// Package mcp is the MCP bridge: it turns the tools an operator lists from
// an MCP server into toolbelt operations, so every executor reaches them
// through the toolbelt, with the host's authority, secrets, gates and
// trail, instead of loading the server into one agent.
//
// An entry of type mcp-stdio runs a server as a command in the sandbox the
// entry names; one of type mcp-http talks to a remote server over MCP's
// streamable HTTP transport. Each tool the entry's config lists becomes the
// entry operation mcp.<entry>.<operation> (extension.EntryOperationDecl),
// with the description and input schema the config pins. A call is
// refused when the server no longer offers the tool exactly as pinned, so
// a server that adds a tool grants nothing and one that rewrites a
// description or schema is refused until the operator re-applies the
// change.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/extension"
)

const (
	// Name is the package's registration name, the root of every bridged
	// operation's name.
	Name = "mcp"
	// StdioType is the entry type of a server run as a local command.
	StdioType = "mcp-stdio"
	// HTTPType is the entry type of a remote server.
	HTTPType = "mcp-http"
	// DefaultToolset is the toolset within an entry a tool joins unless it
	// names another, so @mcp.<entry>.tools grants every tool of an entry.
	DefaultToolset = "tools"
	// DefaultTimeoutSeconds bounds one call, and a server's start.
	DefaultTimeoutSeconds = 60
	maxTimeoutSeconds     = 3600
)

// Tool is one tool the operator takes from the server: the server's name
// for it, the operation it becomes (Operation, default Name), and what was
// approved: its description and input schema exactly as the server gave
// them. Summary is the operation's one-line summary (default the
// description's first line). RetrySafe says a call may be sent again when
// its outcome wasn't recorded; by default it isn't.
type Tool struct {
	Name        string          `json:"name"`
	Operation   string          `json:"operation,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	RetrySafe   bool            `json:"retry_safe,omitempty"`
	Toolset     string          `json:"toolset,omitempty"`
}

// Config is an entry's config. Command, Env and SecretEnv are for
// mcp-stdio: the server's argv, its environment, and variables set from
// the entry's secrets (variable to secret name). URL, Headers,
// SecretHeaders and BearerSecret are for mcp-http: the endpoint, its
// headers, headers set from secrets (header to secret name), and a secret
// sent as the bearer token.
type Config struct {
	Command        []string          `json:"command,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	SecretEnv      map[string]string `json:"secret_env,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	SecretHeaders  map[string]string `json:"secret_headers,omitempty"`
	BearerSecret   string            `json:"bearer_secret,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Tools          []Tool            `json:"tools"`
}

var (
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)
	// Headers the transport sets itself.
	reservedHeaders = []string{"accept", "authorization", "content-type", "content-length", "host", "mcp-session-id", "mcp-protocol-version"}
)

// configError refuses a config at field (a slash path within it, "" for
// the whole), with a fixed message that never quotes a configured value.
func configError(field, msg string) error {
	path := ""
	if field != "" {
		path = "/" + field
	}
	return extension.Invalid(path, "invalid_mcp_config", msg)
}

// Parse strictly decodes an entry's config of type typ and checks it on
// its own terms: what it can't see (the entry's secret names) Validate
// checks.
func Parse(typ string, raw json.RawMessage) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return Config{}, configError("", "config must be an object of the fields the "+typ+" type documents")
	}
	switch typ {
	case StdioType:
		if len(c.Command) == 0 || c.Command[0] == "" {
			return Config{}, configError("command", "command is the server's argv: give at least the program")
		}
		if c.URL != "" || len(c.Headers) > 0 || len(c.SecretHeaders) > 0 || c.BearerSecret != "" {
			return Config{}, configError("url", "url, headers, secret_headers and bearer_secret are for mcp-http entries")
		}
		for k := range c.Env {
			if !envName.MatchString(k) {
				return Config{}, configError("env", "env names must be letters, digits and _")
			}
		}
		for k, v := range c.SecretEnv {
			if !envName.MatchString(k) || v == "" {
				return Config{}, configError("secret_env", "secret_env maps a variable name to a secret name")
			}
			if _, both := c.Env[k]; both {
				return Config{}, configError("secret_env", "a variable is set by env or secret_env, not both")
			}
		}
	case HTTPType:
		if len(c.Command) > 0 || len(c.Env) > 0 || len(c.SecretEnv) > 0 {
			return Config{}, configError("command", "command, env and secret_env are for mcp-stdio entries")
		}
		if err := checkURL(c.URL); err != nil {
			return Config{}, err
		}
		for k := range c.Headers {
			if !headerName.MatchString(k) || slices.Contains(reservedHeaders, strings.ToLower(k)) {
				return Config{}, configError("headers", "a header name must be a valid name the transport doesn't set itself")
			}
		}
		for k, v := range c.SecretHeaders {
			if !headerName.MatchString(k) || slices.Contains(reservedHeaders, strings.ToLower(k)) || v == "" {
				return Config{}, configError("secret_headers", "secret_headers maps a header name the transport doesn't set itself to a secret name")
			}
			if _, both := c.Headers[http.CanonicalHeaderKey(k)]; both {
				return Config{}, configError("secret_headers", "a header is set by headers or secret_headers, not both")
			}
		}
	default:
		return Config{}, configError("", "unknown entry type "+typ)
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > maxTimeoutSeconds {
		return Config{}, configError("timeout_seconds", fmt.Sprintf("timeout_seconds is between 1 and %d (default %d)", maxTimeoutSeconds, DefaultTimeoutSeconds))
	}
	if len(c.Tools) == 0 {
		return Config{}, configError("tools", "list the tools to take from the server; a tool not listed is never offered")
	}
	names, ops := map[string]bool{}, map[string]bool{}
	for i := range c.Tools {
		t := &c.Tools[i]
		field := fmt.Sprintf("tools/%d", i)
		if t.Name == "" || names[t.Name] {
			return Config{}, configError(field+"/name", "each tool names a tool of the server, once")
		}
		names[t.Name] = true
		op := t.op()
		if !extension.EntryOperationSegment(op) {
			return Config{}, configError(field+"/operation", "the tool's name can't name an operation: set operation to a lower-case letter then letters, digits, - or _")
		}
		if ops[op] {
			return Config{}, configError(field+"/operation", "two tools become the same operation; give one another operation")
		}
		ops[op] = true
		if !validSchema(t.InputSchema) {
			return Config{}, configError(field+"/input_schema", "input_schema is the tool's inputSchema exactly as the server lists it, a JSON Schema object")
		}
		if _, err := t.decl(); err != nil {
			return Config{}, configError(field, err.Error())
		}
	}
	return c, nil
}

// checkURL takes an https URL, or http to a loopback host: a server
// that is sent the entry's secrets is reached privately.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return configError("url", "url is the server's streamable HTTP endpoint, an absolute https URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return configError("url", "url must be https (http only to a loopback address)")
}

func validSchema(raw json.RawMessage) bool {
	var m map[string]any
	return len(raw) > 0 && json.Unmarshal(raw, &m) == nil && m != nil
}

func (t Tool) op() string {
	if t.Operation != "" {
		return t.Operation
	}
	return t.Name
}

func (t Tool) toolset() string {
	if t.Toolset != "" {
		return t.Toolset
	}
	return DefaultToolset
}

// summary is the configured summary, or the description's first line cut
// to the toolbelt's limit at a character boundary.
func (t Tool) summary() string {
	if t.Summary != "" {
		return t.Summary
	}
	line, _, _ := strings.Cut(strings.TrimSpace(t.Description), "\n")
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if len(line) <= extension.MaxSummary {
		return line
	}
	cut := extension.MaxSummary - len("…")
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
}

// decl is the operation the tool becomes: its summary, the description as
// its guidance (the agent sees what was approved, in full) and the pinned
// input schema.
func (t Tool) decl() (extension.EntryOperationDecl, error) {
	d := extension.EntryOperationDecl{Name: t.op(), Summary: t.summary(), Guidance: t.Description, Input: t.InputSchema, Toolset: t.toolset()}
	if d.Summary == "" {
		return d, errors.New("the tool has no description to take a summary from: set summary")
	}
	if len(t.Description) > extension.MaxGuidance {
		return d, fmt.Errorf("the tool's description is longer than the %d bytes an operation's guidance may be", extension.MaxGuidance)
	}
	if err := extension.CheckEntryOperationDecl(d); err != nil {
		return d, errors.New("the tool can't be an operation: its summary must be one line, its toolset a name")
	}
	return d, nil
}

// Declared are the operations an entry's config declares, read from the
// config alone.
func Declared(typ string, raw json.RawMessage) ([]extension.EntryOperationDecl, error) {
	c, err := Parse(typ, raw)
	if err != nil {
		return nil, err
	}
	out := make([]extension.EntryOperationDecl, 0, len(c.Tools))
	for _, t := range c.Tools {
		d, _ := t.decl()
		out = append(out, d)
	}
	return out, nil
}

// Validate checks an entry's config and that every secret it names is one
// the entry selects.
func Validate(typ string, raw json.RawMessage, secretNames []string) error {
	c, err := Parse(typ, raw)
	if err != nil {
		return err
	}
	check := func(field, name string) error {
		if !slices.Contains(secretNames, name) {
			return configError(field, "names a secret the entry doesn't select: add it to the entry's secret_names")
		}
		return nil
	}
	for _, name := range c.SecretEnv {
		if err := check("secret_env", name); err != nil {
			return err
		}
	}
	for _, name := range c.SecretHeaders {
		if err := check("secret_headers", name); err != nil {
			return err
		}
	}
	if c.BearerSecret != "" {
		return check("bearer_secret", c.BearerSecret)
	}
	return nil
}

// tool finds the tool that operation op names.
func (c Config) tool(op string) (Tool, bool) {
	for _, t := range c.Tools {
		if t.op() == op {
			return t, true
		}
	}
	return Tool{}, false
}

func (c Config) timeout() int {
	if c.TimeoutSeconds > 0 {
		return c.TimeoutSeconds
	}
	return DefaultTimeoutSeconds
}

// secrets are the secret names the config uses.
func (c Config) secrets() []string {
	var out []string
	for _, m := range []map[string]string{c.SecretEnv, c.SecretHeaders} {
		for _, v := range m {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	if c.BearerSecret != "" && !slices.Contains(out, c.BearerSecret) {
		out = append(out, c.BearerSecret)
	}
	slices.Sort(out)
	return out
}
