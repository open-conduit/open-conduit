package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// Host is what the bridge asks of Open Conduit: a sandboxed launch of a
// stdio server for its entry, the entry's secrets, and its own KV, where a
// call that isn't safe to retry is marked before it is sent.
// *sdk.Host serves it.
type Host interface {
	EntrySandbox(ctx context.Context, scope domain.ID, entry string, needs extension.SandboxNeeds, launch extension.SandboxLaunch) (extension.SandboxPlan, string, error)
	Secrets(ctx context.Context, scope domain.ID, names ...string) (map[string]string, error)
	Update(ctx context.Context, namespace, key string, fn func(old []byte, found bool) ([]byte, error)) error
}

// CallsNamespace holds the marks of calls sent that aren't safe to retry.
const CallsNamespace = Name + ".calls"

// Call is one call of a bridged operation: the entry's type, its scope and
// name, its config as that scope has it, the operation's name within the
// entry, the toolbelt request ID ("" when the caller gave none) and the
// input.
type Call struct {
	Type, Entry string
	Scope       domain.ID
	Config      json.RawMessage
	Operation   string
	RequestID   string
	Input       domain.Payload
}

// ResultSchema is the schema of a call's output: the tool's content
// blocks, and its structured content when it gave any.
const ResultSchema = "mcp.result.v1"

// Result is a call's output.
type Result struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structured_content,omitempty"`
}

// Options are the bridge's host-trusted settings, never from config.
type Options struct {
	// Environ is the host's environment, for a stdio server's PATH, HOME
	// and LANG (os.Environ when nil). Nothing else of it reaches a server.
	Environ func() []string
	// HTTPClient reaches remote servers (a client with no overall timeout
	// when nil; each call is bounded by its entry's timeout). The bridge
	// uses a copy that never follows a redirect.
	HTTPClient *http.Client
	// Now stamps the marks of calls sent (time.Now when nil).
	Now func() time.Time
}

// Bridge serves bridged operations. It keeps one session per entry in each
// scope and starts a server on first use; a session whose entry's config
// has changed is closed and started again.
type Bridge struct {
	o        Options
	mu       sync.Mutex
	sessions map[string]*entrySession
}

type entrySession struct {
	// digest identifies the entry's config, and secrets the secret
	// values the session was started with: a change to either starts
	// it again.
	digest, secrets string
	once            sync.Once
	s               *session
	err             error
}

// New returns a bridge.
func New(o Options) *Bridge {
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	// A redirect would send the entry's secrets on to wherever the server
	// points, perhaps another host or plain http; streamable HTTP needs
	// none, so a 3xx is the answer, refused as a non-2xx.
	client := &http.Client{}
	if o.HTTPClient != nil {
		c := *o.HTTPClient
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTPClient = client
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Bridge{o: o, sessions: map[string]*entrySession{}}
}

func refuse(code, msg string) error { return &extension.OperationError{Code: code, Message: msg} }

func sessionKey(scope domain.ID, entry string) string { return string(scope) + "\x00" + entry }

func digest(typ string, config json.RawMessage) string {
	sum := sha256.Sum256(append([]byte(typ+"\x00"), config...))
	return hex.EncodeToString(sum[:])
}

// Serve serves one call: it finds the tool the operation names in the
// entry's config, checks that the server still offers it exactly as
// pinned, guards a call that isn't safe to retry, and calls it.
func (b *Bridge) Serve(ctx context.Context, h Host, c Call) (domain.Payload, error) {
	cfg, err := Parse(c.Type, c.Config)
	if err != nil {
		return domain.Payload{}, refuse("undeclared", "the entry's config no longer declares this operation")
	}
	tool, ok := cfg.tool(c.Operation)
	if !ok {
		return domain.Payload{}, refuse("undeclared", "the entry's config no longer declares this operation")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.timeout())*time.Second)
	defer cancel()
	secrets, err := resolve(ctx, h, c, cfg)
	if err != nil {
		return domain.Payload{}, err
	}
	s, err := b.session(ctx, h, c, cfg, secrets)
	if err != nil {
		return domain.Payload{}, err
	}
	listed, err := s.tools(ctx)
	if err != nil {
		b.drop(c, s)
		return domain.Payload{}, refuse("server_unavailable", "the MCP server didn't list its tools: "+err.Error())
	}
	if err := pinned(tool, listed); err != nil {
		return domain.Payload{}, err
	}
	guard := !tool.RetrySafe && c.RequestID != ""
	if guard {
		if out, err, done := b.mark(ctx, h, c); done {
			return out, err
		}
	}
	out, err := b.send(ctx, c, s, tool)
	if guard {
		b.settle(ctx, h, c, out, err)
	}
	return out, err
}

// send calls the tool and maps its result.
func (b *Bridge) send(ctx context.Context, c Call, s *session, tool Tool) (domain.Payload, error) {
	res, err := s.callTool(ctx, tool.Name, c.Input.Data)
	if err != nil {
		var rpc *rpcError
		if errors.As(err, &rpc) {
			return domain.Payload{}, refuse("tool_failed", "the MCP server refused the call: "+limit(rpc.Message))
		}
		b.drop(c, s)
		return domain.Payload{}, refuse("server_unavailable", "the MCP server didn't answer the call, which may or may not have run: "+limit(err.Error()))
	}
	if res.IsError {
		return domain.Payload{}, refuse("tool_error", limit(text(res.Content)))
	}
	if res.Content == nil {
		res.Content = []json.RawMessage{}
	}
	out, _ := json.Marshal(Result{Content: res.Content, StructuredContent: res.StructuredContent})
	return domain.Payload{Schema: ResultSchema, Data: out}, nil
}

// pinned checks the server still offers the tool with the description
// and input schema that were approved.
func pinned(t Tool, listed []ServerTool) error {
	for _, l := range listed {
		if l.Name != t.Name {
			continue
		}
		if l.Description != t.Description || !sameJSON(l.InputSchema, t.InputSchema) {
			return refuse("drifted", "the MCP server has changed this tool's description or input schema since it was approved, so it isn't called; the operator can review the change and re-apply the entry's config")
		}
		return nil
	}
	return refuse("drifted", "the MCP server no longer offers this tool, so it isn't called; the operator can remove it from the entry's config")
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// callMark is what the bridge keeps of a call that isn't safe to retry:
// when it was sent and, once it has an outcome, the outcome, so the same
// request is answered the same way. Output is kept only up to
// maxKeptOutput bytes.
type callMark struct {
	At      string          `json:"at"`
	Done    bool            `json:"done,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
	Kept    bool            `json:"kept,omitempty"`
}

const maxKeptOutput = 256 << 10

func markKey(c Call) string {
	sum := sha256.Sum256([]byte(string(c.Scope) + "\x00" + c.Entry + "\x00" + c.Operation + "\x00" + c.RequestID))
	return hex.EncodeToString(sum[:])
}

// mark records that a call is being sent, before it is. A request already
// marked isn't sent again: done reports that, with the answer it had, or
// a refusal when its outcome was never recorded or not kept.
func (b *Bridge) mark(ctx context.Context, h Host, c Call) (out domain.Payload, err error, done bool) {
	var prior *callMark
	uerr := h.Update(ctx, CallsNamespace, markKey(c), func(old []byte, found bool) ([]byte, error) {
		if found {
			var m callMark
			if json.Unmarshal(old, &m) != nil {
				m = callMark{}
			}
			prior = &m
			return old, nil
		}
		prior = nil
		return json.Marshal(callMark{At: b.o.Now().UTC().Format(time.RFC3339)})
	})
	switch {
	case uerr != nil:
		return domain.Payload{}, refuse("server_unavailable", "the call couldn't be recorded before it was sent, so it wasn't sent"), true
	case prior == nil:
		return domain.Payload{}, nil, false
	case !prior.Done:
		return domain.Payload{}, refuse("unknown_outcome", "this call was sent to the MCP server once already and its outcome wasn't recorded; the tool isn't marked retry_safe, so it isn't sent again. Check the system it acts on before asking again with a new request"), true
	case prior.Code != "":
		return domain.Payload{}, refuse(prior.Code, prior.Message), true
	case !prior.Kept:
		return domain.Payload{}, refuse("already_done", "this call already succeeded, and its result was too large to keep for a repeat; the tool isn't marked retry_safe, so it isn't sent again"), true
	}
	return domain.Payload{Schema: ResultSchema, Data: prior.Output}, nil, true
}

// settle records a marked call's outcome. A call cut off before it was
// answered keeps its mark without one, so its outcome stays unknown.
func (b *Bridge) settle(ctx context.Context, h Host, c Call, out domain.Payload, err error) {
	var oe *extension.OperationError
	if err != nil && (!errors.As(err, &oe) || oe.Code == "server_unavailable") {
		return
	}
	m := callMark{At: b.o.Now().UTC().Format(time.RFC3339), Done: true}
	if oe != nil {
		m.Code, m.Message = oe.Code, oe.Message
	} else if len(out.Data) <= maxKeptOutput {
		m.Output, m.Kept = out.Data, true
	}
	raw, _ := json.Marshal(m)
	_ = h.Update(context.WithoutCancel(ctx), CallsNamespace, markKey(c), func([]byte, bool) ([]byte, error) { return raw, nil })
}

// Sweep deletes the marks of calls sent more than maxAge ago: a retry
// comes within an attempt or the next, never weeks later.
func Sweep(ctx context.Context, h interface {
	List(ctx context.Context, namespace, prefix string) ([]protocol.KVEntry, error)
	Commit(ctx context.Context, b protocol.KVCommitParams) error
}, now time.Time, maxAge time.Duration) error {
	entries, err := h.List(ctx, CallsNamespace, "")
	if err != nil {
		return err
	}
	var b protocol.KVCommitParams
	for _, e := range entries {
		var m callMark
		_ = json.Unmarshal(e.Value, &m)
		at, perr := time.Parse(time.RFC3339, m.At)
		if perr != nil || now.Sub(at) > maxAge {
			b.Checks = append(b.Checks, protocol.KVCheck{Namespace: CallsNamespace, Key: e.Key, Version: e.Version})
			b.Deletes = append(b.Deletes, protocol.KVDelete{Namespace: CallsNamespace, Key: e.Key})
		}
	}
	if len(b.Deletes) == 0 {
		return nil
	}
	return h.Commit(ctx, b)
}

// resolve resolves the secrets the entry's config names, on every call,
// so a rotated or revoked secret is never used after the host stops
// answering with it.
func resolve(ctx context.Context, h Host, c Call, cfg Config) (map[string]string, error) {
	names := cfg.secrets()
	if len(names) == 0 {
		return nil, nil
	}
	secrets, err := h.Secrets(ctx, c.Scope, names...)
	if err != nil {
		return nil, refuse("server_unavailable", "the entry's secrets couldn't be resolved")
	}
	for _, n := range names {
		if secrets[n] == "" {
			return nil, refuse("server_unavailable", "a secret the entry's config names has no value in this scope")
		}
	}
	return secrets, nil
}

// secretsDigest identifies a set of secret values without keeping them.
func secretsDigest(secrets map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(secrets)) {
		fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(secrets[k]), secrets[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// session is the entry's session in the call's scope, started on first
// use, or again when the entry's config or the values of its secrets
// have changed.
func (b *Bridge) session(ctx context.Context, h Host, c Call, cfg Config, secrets map[string]string) (*session, error) {
	key, d, sd := sessionKey(c.Scope, c.Entry), digest(c.Type, c.Config), secretsDigest(secrets)
	b.mu.Lock()
	es := b.sessions[key]
	if es == nil || es.digest != d || es.secrets != sd {
		if es != nil {
			go es.close()
		}
		es = &entrySession{digest: d, secrets: sd}
		b.sessions[key] = es
	}
	b.mu.Unlock()
	es.once.Do(func() { es.s, es.err = b.start(ctx, h, c, cfg, secrets) })
	if es.err != nil {
		b.mu.Lock()
		if b.sessions[key] == es {
			delete(b.sessions, key)
		}
		b.mu.Unlock()
		return nil, es.err
	}
	return es.s, nil
}

// drop closes a session whose transport failed, so the next call starts
// the server again.
func (b *Bridge) drop(c Call, s *session) {
	key := sessionKey(c.Scope, c.Entry)
	b.mu.Lock()
	es := b.sessions[key]
	if es != nil && es.s == s {
		delete(b.sessions, key)
	} else {
		es = nil
	}
	b.mu.Unlock()
	if es != nil {
		go es.close()
	}
}

// close stops the session's server, after its start if one is under
// way, and fences a start that hasn't begun.
func (es *entrySession) close() {
	es.once.Do(func() { es.err = refuse("server_unavailable", "the entry's server was stopped") })
	if es.s != nil {
		_ = es.s.close()
	}
}

// Configure closes the sessions of entries the generation no longer has
// as they were, given each entry's scope, name, type and config.
func (b *Bridge) Configure(entries []ConfiguredEntry) {
	keep := map[string]string{}
	for _, e := range entries {
		keep[sessionKey(e.Scope, e.Name)] = digest(e.Type, e.Config)
	}
	b.mu.Lock()
	var stale []*entrySession
	for k, es := range b.sessions {
		if keep[k] != es.digest {
			stale = append(stale, es)
			delete(b.sessions, k)
		}
	}
	b.mu.Unlock()
	for _, es := range stale {
		es.close()
	}
}

// ConfiguredEntry is one of the bridge's entries in a generation.
type ConfiguredEntry struct {
	Scope      domain.ID
	Name, Type string
	Config     json.RawMessage
}

// Close stops every server.
func (b *Bridge) Close() {
	b.Configure(nil)
}

// start starts and initializes the entry's server.
func (b *Bridge) start(ctx context.Context, h Host, c Call, cfg Config, secrets map[string]string) (*session, error) {
	var t transport
	var err error
	switch c.Type {
	case StdioType:
		t, err = b.launch(ctx, h, c, cfg, secrets)
	case HTTPType:
		t = b.remote(cfg, secrets)
	default:
		err = refuse("undeclared", "unknown entry type")
	}
	if err != nil {
		return nil, err
	}
	s, err := start(ctx, t)
	if err != nil {
		_ = t.close()
		return nil, refuse("server_unavailable", "the MCP server didn't start: "+limit(err.Error()))
	}
	return s, nil
}

// passEnv are the host variables a stdio server sees; everything else of
// the daemon's environment stays out.
var passEnv = []string{"PATH", "HOME", "LANG", "LC_ALL"}

// launch starts a stdio server in the sandbox its entry names, through
// the host, which plans it with the entry's policy and makes the
// directory it runs in.
func (b *Bridge) launch(ctx context.Context, h Host, c Call, cfg Config, secrets map[string]string) (transport, error) {
	var env []string
	for _, kv := range b.o.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(passEnv, k) {
			env = append(env, kv)
		}
	}
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	for k, name := range cfg.SecretEnv {
		env = append(env, k+"="+secrets[name])
	}
	plan, dir, err := h.EntrySandbox(ctx, c.Scope, c.Entry, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: cfg.Command, Env: env})
	if err != nil {
		return nil, refuse("sandbox_refused", "the MCP server wasn't started: its sandbox refused the launch ("+limit(extension.SandboxReason(err))+"): "+limit(err.Error()))
	}
	if len(plan.Argv) == 0 || dir == "" {
		return nil, refuse("sandbox_refused", "the MCP server wasn't started: its sandbox returned no launch")
	}
	cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = plan.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A server's stderr is its log; it isn't kept, since it may print
	// what it was given.
	cmd.Stderr = nil
	if err := startServer(cmd); err != nil {
		return nil, refuse("server_unavailable", "the MCP server's command couldn't be started")
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = os.RemoveAll(dir)
		close(exited)
	}()
	stop := func() error {
		// Closing stdin asks the server to exit; it is killed if it
		// doesn't promptly.
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		return nil
	}
	return newStdioTransport(stdin, stdout, stop), nil
}

// remote is the transport to an mcp-http entry's server, with its headers
// and the secrets its config sends.
func (b *Bridge) remote(cfg Config, secrets map[string]string) transport {
	headers := http.Header{}
	for k, v := range cfg.Headers {
		headers.Set(k, v)
	}
	for k, name := range cfg.SecretHeaders {
		headers.Set(k, secrets[name])
	}
	if cfg.BearerSecret != "" {
		headers.Set("Authorization", "Bearer "+secrets[cfg.BearerSecret])
	}
	return &httpTransport{url: cfg.URL, headers: headers, client: b.o.HTTPClient}
}

// text joins a result's text blocks, for an error a tool reported.
func text(content []json.RawMessage) string {
	var parts []string
	for _, raw := range content {
		var b struct {
			Type, Text string
		}
		if json.Unmarshal(raw, &b) == nil && b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "the tool reported an error"
	}
	return strings.Join(parts, "\n")
}

// limit bounds a message the caller sees.
func limit(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("… (%d more bytes)", len(s)-max)
}
