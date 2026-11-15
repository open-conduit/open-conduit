package chatcompletions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
)

// Config is the strictly checked binding `config` object. base_url and
// model are required; every other field is optional.
type Config struct {
	// BaseURL is the endpoint's API root: requests go to
	// <base_url>/chat/completions (https://openrouter.ai/api/v1,
	// http://localhost:11434/v1).
	BaseURL string `json:"base_url"`
	// Model is the request's model.
	Model string `json:"model"`
	// APIKeySecret names a secret the binding selects, sent as the bearer
	// token. Unset sends no Authorization header (a local Ollama).
	APIKeySecret string `json:"api_key_secret,omitempty"`
	// Headers are added to every request. They may not carry credentials:
	// a header whose name is an authorization, cookie, key, token or secret
	// header is refused, and so is any value holding a selected secret.
	Headers map[string]string `json:"headers,omitempty"`
	// Playbook is the playbook a turn starts on: its text is the start of
	// the turn's first message. "-" or empty means none.
	Playbook string `json:"playbook,omitempty"`
	// PluginDir holds the playbooks, as <dir>/skills/<playbook>/SKILL.md.
	// Default: the host's own (ExecutionContext.PlaybookDir).
	PluginDir string `json:"plugin_dir,omitempty"`
	// Args is the playbook's argument template (default {number}).
	Args string `json:"args,omitempty"`
	// Operations narrows which granted operations become tools: exact
	// names, or a namespace as "github.*". Unset offers every granted
	// operation. wait and declare_outcome are always offered when granted.
	// It grants nothing: the host still checks each call.
	Operations []string `json:"operations,omitempty"`
	// MaxRounds bounds the requests one attempt makes (default 30).
	MaxRounds int `json:"max_rounds,omitempty"`
	// TimeoutSeconds bounds one request (default 300). A request that runs
	// out ends the attempt with a retry hint.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// HandoffTokens starts a fresh conversation from the last turn's note
	// when that turn's last request carried more prompt tokens than this
	// (default 100000; 0 always resumes).
	HandoffTokens *int64 `json:"handoff_tokens,omitempty"`
	// Request is passed through in every request's body (temperature,
	// max_tokens, provider routing). The executor's own fields are
	// reserved.
	Request map[string]json.RawMessage `json:"request,omitempty"`
}

// Defaults.
const (
	DefaultMaxRounds      = 30
	DefaultTimeoutSeconds = 300
	DefaultHandoffTokens  = int64(100_000)
	maxRounds             = 500
	maxTimeoutSeconds     = 3600
)

// reservedRequest are the body fields the executor sets itself, which
// request may not.
var reservedRequest = []string{"model", "messages", "tools", "tool_choice", "functions", "function_call", "stream", "stream_options", "n"}

// executorHeaders are the headers the executor sets itself.
var executorHeaders = []string{"Content-Type", "Content-Length", "Host", "Accept", "Accept-Encoding", "Transfer-Encoding", "Connection"}

// credentialHeader matches header names that carry credentials.
var credentialHeader = regexp.MustCompile(`(?i)(auth|cookie|key|token|secret|session|password|signature)`)

var (
	// ErrConfig is returned for a binding config the executor rejects.
	// Messages never echo configured values.
	ErrConfig     = errors.New("invalid chat-completions binding config")
	playbookName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	operationName = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+)*(\.\*)?$`)
)

func configError(msg string) error { return fmt.Errorf("%w: %s", ErrConfig, msg) }

// ParseConfig strictly decodes, validates and defaults a binding config.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, configError("malformed config or unknown field")
	}
	if _, err := d.Token(); err != io.EOF {
		return Config{}, configError("trailing data after config object")
	}
	for _, s := range append([]string{c.BaseURL, c.Model, c.APIKeySecret, c.Playbook, c.PluginDir, c.Args}, c.Operations...) {
		if strings.ContainsRune(s, 0) {
			return Config{}, configError("strings must not contain NUL")
		}
	}
	u, err := url.Parse(c.BaseURL)
	if c.BaseURL == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, configError("base_url must be an http or https URL with a host and no credentials, query or fragment")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if strings.TrimSpace(c.Model) == "" {
		return Config{}, configError("model is required")
	}
	for name, value := range c.Headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if name == "" || strings.ContainsAny(name, " \t\r\n:") || strings.ContainsAny(value, "\r\n\x00") {
			return Config{}, configError("headers must be valid header names and single-line values")
		}
		if slices.Contains(executorHeaders, canonical) || credentialHeader.MatchString(name) {
			return Config{}, configError("headers may not set the executor's own headers or carry credentials (authorization, cookie, key, token or secret headers); use api_key_secret")
		}
	}
	if c.Playbook != "" && c.Playbook != "-" && !playbookName.MatchString(c.Playbook) {
		return Config{}, configError("playbook must be a skill name")
	}
	if c.PluginDir != "" && (!filepath.IsAbs(c.PluginDir) || filepath.Clean(c.PluginDir) != c.PluginDir) {
		return Config{}, configError("plugin_dir must be a clean absolute path")
	}
	if c.Args == "" {
		c.Args = "{number}"
	}
	for _, op := range c.Operations {
		if !operationName.MatchString(op) {
			return Config{}, configError("operations must be operation names or namespaces as name.*")
		}
	}
	if c.MaxRounds == 0 {
		c.MaxRounds = DefaultMaxRounds
	}
	if c.MaxRounds < 1 || c.MaxRounds > maxRounds {
		return Config{}, configError("max_rounds must be between 1 and 500")
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > maxTimeoutSeconds {
		return Config{}, configError("timeout_seconds must be between 1 and 3600")
	}
	if c.HandoffTokens == nil {
		n := DefaultHandoffTokens
		c.HandoffTokens = &n
	}
	if *c.HandoffTokens < 0 {
		return Config{}, configError("handoff_tokens must not be negative")
	}
	for k := range c.Request {
		if slices.Contains(reservedRequest, k) {
			return Config{}, configError("request may not set " + strings.Join(reservedRequest, ", "))
		}
	}
	return c, nil
}

// ValidateConfig rejects an invalid binding before any execution is
// scheduled. secretNames are the binding's selected secrets.
func ValidateConfig(raw json.RawMessage, secretNames []string) error {
	c, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	if c.APIKeySecret != "" && !slices.Contains(secretNames, c.APIKeySecret) {
		return configError("api_key_secret names a secret the binding does not select")
	}
	return nil
}

// Playbook is the playbook a binding config starts turns on
// (extension.PlaybookNamer), "" for none.
func Playbook(raw json.RawMessage) string {
	c, err := ParseConfig(raw)
	if err != nil || c.Playbook == "-" {
		return ""
	}
	return c.Playbook
}

// offers reports whether the config's operations narrowing lets op become
// a tool.
func (c Config) offers(op string) bool {
	if len(c.Operations) == 0 {
		return true
	}
	for _, pattern := range c.Operations {
		if prefix, ok := strings.CutSuffix(pattern, ".*"); ok && strings.HasPrefix(op, prefix+".") || pattern == op {
			return true
		}
	}
	return false
}

// BindingFields describe a binding's config for the operator's catalogue
// (manifest fields).
func BindingFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "base_url", Help: "the endpoint's API root; requests go to <base_url>/chat/completions", Required: true},
		{Name: "model", Help: "the request's model", Required: true},
		{Name: "api_key_secret", Help: "a selected secret sent as the bearer token"},
		{Name: "headers", Help: "added to every request; may not carry credentials"},
		{Name: "playbook", Help: "the playbook a turn starts on; none (or -) starts it on the rendered Work alone"},
		{Name: "plugin_dir", Help: "holds the playbooks as <dir>/skills/<playbook>/SKILL.md; the host's own by default"},
		{Name: "args", Help: "the playbook's argument template", Default: "{number}"},
		{Name: "operations", Help: "narrows which granted operations become tools: names, or name.* for a namespace"},
		{Name: "max_rounds", Help: "the most requests one attempt makes", Default: "30"},
		{Name: "timeout_seconds", Help: "one request's time limit", Default: "300"},
		{Name: "handoff_tokens", Help: "a turn after one whose last request carried more prompt tokens starts afresh from the note; 0 turns it off", Default: "100000"},
		{Name: "request", Help: "passed through in every request's body (temperature, max_tokens); the executor's own fields are reserved"},
	}
}

// expand fills an argument template: {key} is the Work's first
// correlation key other than its lineage key (domain.SubjectKey), {number}
// the first number in it (the key when it has none) and {work} the Work ID.
func expand(template string, w domain.Work) string {
	key := ""
	if k, ok := domain.SubjectKey(w.CorrelationKeys); ok {
		key = k.ExternalID
	}
	number := regexp.MustCompile(`\d+`).FindString(key)
	if number == "" {
		number = key
	}
	return strings.NewReplacer("{key}", key, "{number}", number, "{work}", string(w.ID)).Replace(template)
}
