package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extension"
)

const toolbeltHelp = `os — Open Conduit execution toolbelt (CLI transport)

Usage: os COMMAND [flags]

Commands:
  context                                   read this Work's accumulated context
  emit --type T --delivery-id D [--schema S] [--data JSON | --data-file PATH|-]
       [--key-source S --key-external-id X] [--occurred-at RFC3339]
  work create --kind K [--id ID] [--key-source S --key-external-id X]... [--schema S --data JSON]
  work cancel --id ID [--reason TEXT]       cancel child Work this Work asked for
  wait --type T[,T...] [--key-source S --key-external-id X]... [--timeout SECONDS] [--after-sequence N] [--description TEXT]
  outcome completed|failed|handoff [--summary TEXT] [--schema S --data JSON]
  call OPERATION [--schema S] [--data JSON | --data-file PATH|-]
  inbox                                     print input injected into this running execution since the last
                                            read (JSON lines) and acknowledge it as taken in (OS_INBOX)
  mcp                                       serve these operations as an MCP server on stdio
  git-credential get|store|erase            git's credential helper: a token for the Work's repository
  version

Every command accepts --request-id ID to retry safely. Results print as JSON.
Runs inside an execution: OS_ENDPOINT selects the local endpoint and the
credential is read from OS_TOKEN_FILE when present (it is replaced when a
restarted daemon adopts this process), otherwise from OS_TOKEN.
`

// list collects a repeatable flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

type toolbelt struct {
	env         Env
	stdin       io.Reader
	out, errOut io.Writer
}

func (t *toolbelt) usage(msg string) int {
	fmt.Fprintf(t.errOut, "os: %s; use --help\n", msg)
	return 2
}

// RunToolbelt implements the `os` CLI over POST /v1/toolbelt/call. It never
// echoes arguments or credentials in errors.
func RunToolbelt(version, commit string, args []string, env Env, stdin io.Reader, out, errOut io.Writer) int {
	t := &toolbelt{env: env, stdin: stdin, out: out, errOut: errOut}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(out, toolbeltHelp)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintf(out, "os %s (%s)\n", version, commit)
		return 0
	}
	switch args[0] {
	case "context":
		return t.context(args[1:])
	case "emit":
		return t.emit(args[1:])
	case "work":
		if len(args) > 1 && args[1] == "create" {
			return t.createWork(args[2:])
		}
		if len(args) > 1 && args[1] == "cancel" {
			return t.cancelWork(args[2:])
		}
		return t.usage("expected work create or work cancel")
	case "wait":
		return t.wait(args[1:])
	case "outcome":
		return t.outcome(args[1:])
	case "call":
		return t.call(args[1:])
	case "mcp":
		return t.mcp(args[1:])
	case "inbox":
		return t.inbox(args[1:])
	case "git-credential":
		return t.gitCredential(args[1:])
	}
	return t.usage("unknown command")
}

type toolFlags struct {
	fs                *flag.FlagSet
	s                 map[string]*string
	sources, external list
}

func newToolFlags(names ...string) *toolFlags {
	f := &toolFlags{fs: flag.NewFlagSet("os", flag.ContinueOnError), s: map[string]*string{}}
	f.fs.SetOutput(io.Discard)
	for _, n := range append(names, "request-id") {
		f.s[n] = f.fs.String(n, "", "")
	}
	f.fs.Var(&f.sources, "key-source", "")
	f.fs.Var(&f.external, "key-external-id", "")
	return f
}
func (f *toolFlags) get(n string) string { return *f.s[n] }

// parse permits flags before and after positional arguments.
func (f *toolFlags) parse(args []string) ([]string, error) {
	var positional []string
	for {
		if err := f.fs.Parse(args); err != nil {
			return nil, err
		}
		args = f.fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
func (f *toolFlags) keys() ([]client.Key, error) {
	if len(f.sources) != len(f.external) {
		return nil, errors.New("--key-source and --key-external-id must be paired")
	}
	keys := make([]client.Key, len(f.sources))
	for i := range f.sources {
		if f.sources[i] == "" || f.external[i] == "" {
			return nil, errors.New("correlation keys need a source and external id")
		}
		keys[i] = client.Key{Source: f.sources[i], ExternalID: f.external[i]}
	}
	return keys, nil
}

// payload reads --schema with --data or --data-file (- for stdin).
func (t *toolbelt) payload(f *toolFlags) (*client.Payload, error) {
	schema, data := f.get("schema"), f.get("data")
	if file := f.get("data-file"); file != "" {
		if data != "" {
			return nil, errors.New("use --data or --data-file, not both")
		}
		var b []byte
		var err error
		if file == "-" {
			b, err = io.ReadAll(io.LimitReader(t.stdin, 1<<20+1))
		} else {
			b, err = os.ReadFile(file)
		}
		if err != nil {
			return nil, errors.New("--data-file is unreadable")
		}
		data = strings.TrimSpace(string(b))
	}
	if schema == "" && data == "" {
		return nil, nil
	}
	if data != "" && !json.Valid([]byte(data)) {
		return nil, errors.New("--data must be JSON")
	}
	return &client.Payload{Schema: schema, Data: json.RawMessage(data)}, nil
}

func (t *toolbelt) client() (*client.Client, error) {
	endpoint := t.env.Getenv("OS_ENDPOINT")
	token := ""
	path := t.env.Getenv("OS_TOKEN_FILE")
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		token = t.env.Getenv("OS_TOKEN")
	}
	if endpoint != "" && token == "" && path != "" {
		// The host keeps the token file until the execution has ended and
		// its process is stopped, so a missing file means this process is
		// outliving its execution.
		return nil, fmt.Errorf("the toolbelt token file %s is gone: this execution has ended (timed out, cancelled or finished) and its toolbelt is closed; the run's history says why", path)
	}
	if endpoint == "" || token == "" {
		return nil, errors.New("OS_ENDPOINT and OS_TOKEN are not set; os runs inside an execution with a toolbelt")
	}
	return client.New(endpoint, token)
}

// recoveryBudget bounds how long a call refused with authority_recovering
// is made again: the daemon recovers a lapsed lease on its next poll (about
// a second), so a refusal that outlasts this is not recovery.
const recoveryBudget = 30 * time.Second

// recoverySleep waits between those attempts; tests replace it.
var recoverySleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// withRecovery runs one toolbelt request with the execution's current
// credential. A refusal with authority_recovering says the request did not
// run because the run's lease lapsed (the host slept or stalled) and
// recovery is adopting this process with fresh authority, so it re-reads
// the credential (recovery replaces the token file) and makes the request
// again, backing off, until it is answered, ctx ends or recoveryBudget is
// spent. Any other answer or error is returned as it came.
func (t *toolbelt) withRecovery(ctx context.Context, request func(*client.Client) error) error {
	delay, waited := 250*time.Millisecond, time.Duration(0)
	for {
		c, err := t.client()
		if err != nil {
			return err
		}
		err = request(c)
		var api *client.Error
		if !errors.As(err, &api) || api.Code != "authority_recovering" || waited+delay > recoveryBudget {
			return err
		}
		if e := recoverySleep(ctx, delay); e != nil {
			return err
		}
		waited += delay
		delay = min(2*delay, 4*time.Second)
	}
}

// toolbeltCall is one toolbelt call made through withRecovery.
func (t *toolbelt) toolbeltCall(ctx context.Context, call client.ToolCall) (result client.ToolResult, err error) {
	err = t.withRecovery(ctx, func(c *client.Client) (e error) {
		result, e = c.ToolbeltCall(ctx, call)
		return e
	})
	return result, err
}

func (t *toolbelt) do(f *toolFlags, operation string, input any, pick func(client.ToolResult) any) int {
	if _, err := t.client(); err != nil {
		fmt.Fprintf(t.errOut, "os: %s\n", err)
		return 2
	}
	call := client.ToolCall{Operation: operation, RequestID: f.get("request-id")}
	if input != nil {
		var err error
		if call.Input, err = json.Marshal(input); err != nil {
			return t.usage("invalid input")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	result, err := t.toolbeltCall(ctx, call)
	if err != nil {
		fmt.Fprintf(t.errOut, "os: %s\n", callError(err))
		return 1
	}
	enc := json.NewEncoder(t.out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(pick(result))
	return 0
}

func (t *toolbelt) context(args []string) int {
	f := newToolFlags()
	if pos, err := f.parse(args); err != nil || len(pos) != 0 {
		return t.usage("context takes no arguments")
	}
	return t.do(f, "read_context", nil, func(r client.ToolResult) any {
		if r.Context == nil {
			return client.Payload{}
		}
		return r.Context
	})
}

func (t *toolbelt) emit(args []string) int {
	f := newToolFlags("type", "delivery-id", "schema", "data", "data-file", "occurred-at")
	if pos, err := f.parse(args); err != nil || len(pos) != 0 {
		return t.usage("invalid emit flags")
	}
	if f.get("type") == "" || f.get("delivery-id") == "" {
		return t.usage("emit requires --type and --delivery-id")
	}
	keys, err := f.keys()
	if err != nil || len(keys) > 1 {
		return t.usage("emit accepts at most one paired correlation key")
	}
	if at := f.get("occurred-at"); at != "" {
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			return t.usage("--occurred-at must be RFC3339")
		}
	}
	data, err := t.payload(f)
	if err != nil {
		return t.usage(err.Error())
	}
	in := map[string]any{"type": f.get("type"), "delivery_id": f.get("delivery-id")}
	if data != nil {
		in["data"] = data
	}
	if len(keys) == 1 {
		in["correlation_key"] = keys[0]
	}
	if at := f.get("occurred-at"); at != "" {
		in["occurred_at"] = at
	}
	return t.do(f, "emit", in, func(r client.ToolResult) any { return r.Event })
}

func (t *toolbelt) createWork(args []string) int {
	f := newToolFlags("kind", "id", "schema", "data", "data-file")
	if pos, err := f.parse(args); err != nil || len(pos) != 0 {
		return t.usage("invalid work create flags")
	}
	if f.get("kind") == "" {
		return t.usage("work create requires --kind")
	}
	keys, err := f.keys()
	if err != nil {
		return t.usage(err.Error())
	}
	data, err := t.payload(f)
	if err != nil {
		return t.usage(err.Error())
	}
	in := map[string]any{"kind": f.get("kind"), "correlation_keys": keys}
	if id := f.get("id"); id != "" {
		in["id"] = id
	}
	if data != nil {
		in["context"] = data
	}
	return t.do(f, "create_work", in, func(r client.ToolResult) any { return r.Work })
}

func (t *toolbelt) cancelWork(args []string) int {
	f := newToolFlags("id", "reason")
	if pos, err := f.parse(args); err != nil || len(pos) != 0 {
		return t.usage("invalid work cancel flags")
	}
	if f.get("id") == "" {
		return t.usage("work cancel requires --id")
	}
	in := map[string]any{"work_id": f.get("id")}
	if r := f.get("reason"); r != "" {
		in["reason"] = r
	}
	return t.do(f, "cancel_work", in, func(r client.ToolResult) any { return r.Work })
}

func (t *toolbelt) wait(args []string) int {
	f := newToolFlags("type", "timeout", "after-sequence", "description")
	if pos, err := f.parse(args); err != nil || len(pos) != 0 {
		return t.usage("invalid wait flags")
	}
	keys, err := f.keys()
	if err != nil {
		return t.usage(err.Error())
	}
	types := []string{}
	for _, ty := range strings.Split(f.get("type"), ",") {
		if ty = strings.TrimSpace(ty); ty != "" {
			types = append(types, ty)
		}
	}
	if len(types) == 0 && len(keys) == 0 {
		return t.usage("wait requires --type or a correlation key")
	}
	match := map[string]any{"keys": keys, "types": types}
	in := map[string]any{"match": match}
	if s := f.get("timeout"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return t.usage("--timeout must be positive seconds")
		}
		in["timeout_seconds"] = n
	}
	if s := f.get("after-sequence"); s != "" {
		if _, err := strconv.ParseUint(s, 10, 64); err != nil {
			return t.usage("--after-sequence must be a decimal sequence")
		}
		in["after_sequence"] = s
	}
	if s := f.get("description"); s != "" {
		in["description"] = s
	}
	return t.do(f, "wait", in, func(r client.ToolResult) any { return r.Wait })
}

func (t *toolbelt) outcome(args []string) int {
	f := newToolFlags("summary", "schema", "data", "data-file")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 {
		return t.usage("outcome requires one kind")
	}
	switch pos[0] {
	case "completed", "failed", "handoff":
	default:
		return t.usage("outcome kind must be completed, failed or handoff (use wait to suspend)")
	}
	data, err := t.payload(f)
	if err != nil {
		return t.usage(err.Error())
	}
	in := map[string]any{"kind": pos[0]}
	if s := f.get("summary"); s != "" {
		in["summary"] = s
	}
	if data != nil {
		in["data"] = data
	}
	return t.do(f, "declare_outcome", in, func(client.ToolResult) any { return map[string]string{"declared": pos[0]} })
}

// builtinCommands are the commands that serve the builtin operations, which
// `os call` refuses.
var builtinCommands = map[string]string{
	"read_context":    "context",
	"emit":            "emit",
	"create_work":     "work create",
	"cancel_work":     "work cancel",
	"wait":            "wait",
	"declare_outcome": "outcome",
}

func (t *toolbelt) call(args []string) int {
	f := newToolFlags("schema", "data", "data-file")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 || pos[0] == "" {
		return t.usage("call requires one operation")
	}
	if command, ok := builtinCommands[pos[0]]; ok {
		// call shows an extension operation's output, which a builtin has
		// none of: refuse rather than print an empty result.
		return t.usage(pos[0] + " is a command of its own: use os " + command)
	}
	data, err := t.payload(f)
	if err != nil {
		return t.usage(err.Error())
	}
	var in any
	if data != nil {
		if data.Data, err = fileInput(data.Data); err != nil {
			return t.usage(err.Error())
		}
		in = data
	}
	return t.do(f, pos[0], in, func(r client.ToolResult) any {
		if r.Output == nil {
			return client.Payload{}
		}
		shown, err := fileOutput(r.Output)
		if err != nil {
			return map[string]string{"error": err.Error()}
		}
		return shown
	})
}

// gitCredential is git's credential helper protocol over the git.credential
// capability: for `get` it reads git's attributes from stdin, asks each
// granted operation serving the capability in turn (GitHub's, Bitbucket's,
// an installed code host's), and prints the first username and password one
// answers for that URL, or nothing when none does. `store` and `erase` do
// nothing: the host owns the token and revokes it. It sends no request ID,
// so the credential is never kept for replay.
func (t *toolbelt) gitCredential(args []string) int {
	if len(args) != 1 {
		return t.usage("git-credential takes get, store or erase")
	}
	if args[0] != "get" {
		_, _ = io.Copy(io.Discard, t.stdin)
		return 0
	}
	in := map[string]string{}
	sc := bufio.NewScanner(t.stdin)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok && (k == "protocol" || k == "host" || k == "path") {
			in[k] = v
		}
	}
	if _, err := t.client(); err != nil {
		fmt.Fprintf(t.errOut, "os: %s\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var ops []string
	err := t.withRecovery(ctx, func(c *client.Client) (e error) {
		ops, e = c.ToolbeltServing(ctx, extension.CapabilityGitCredential)
		return e
	})
	if err != nil {
		t.noCredential(err)
		return 1
	}
	if len(ops) == 0 {
		// Say why, but exit 0 so git still asks any later helper.
		fmt.Fprintf(t.errOut, "os: no git credential: no granted operation serves %s\n", extension.CapabilityGitCredential)
		return 0
	}
	// An extension operation's input is a payload envelope; git's
	// attributes are its data.
	attrs, _ := json.Marshal(in)
	data, _ := json.Marshal(client.Payload{Data: attrs})
	var refused error
	for _, op := range ops {
		result, err := t.toolbeltCall(ctx, client.ToolCall{Operation: op, Input: data, Capability: extension.CapabilityGitCredential})
		if err != nil {
			// Another operation may still own the URL; report the
			// refusal only if none does.
			refused = err
			continue
		}
		if result.Wait != nil {
			// A gate parked the run on the sign-in (a person's approval):
			// git gets nothing, and the agent is told what the gate said.
			t.heldCredential(result)
			return 1
		}
		var cred struct{ Username, Password string }
		if result.Output != nil {
			_ = json.Unmarshal(result.Output.Data, &cred)
		}
		if cred.Username == "" || cred.Password == "" || strings.ContainsAny(cred.Username+cred.Password, "\n\x00") {
			continue
		}
		fmt.Fprintf(t.out, "username=%s\npassword=%s\n", cred.Username, cred.Password)
		return 0
	}
	if refused != nil {
		t.noCredential(refused)
		return 1
	}
	return 0
}

// heldCredential reports that git's sign-in parked the run: the gate's own
// instruction (its output's next), else what the Wait is for. Git prints
// it among its own errors, so the agent reads it.
func (t *toolbelt) heldCredential(result client.ToolResult) {
	var out struct{ Next string }
	if result.Output != nil {
		_ = json.Unmarshal(result.Output.Data, &out)
	}
	why := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, out.Next)
	if why == "" {
		why = "it waits for " + result.Wait.Description + "; end your turn now, and once resumed run the same git command again"
	}
	fmt.Fprintf(t.errOut, "os: no git credential yet: %s\n", why)
}

// noCredential reports why git gets no credential.
func (t *toolbelt) noCredential(err error) {
	fmt.Fprintf(t.errOut, "os: no git credential: %s\n", callError(err))
}

// callTimeout bounds one toolbelt call. An operation can make several
// requests of its own (an issue and every page of its comments), so a slow
// upstream can use it up; callError then says so rather than blaming the
// connection.
const callTimeout = 60 * time.Second

// callError says why a toolbelt call failed, with its cause, so an agent can
// tell an operation the daemon refused from one that ran out of time and from
// a daemon it could not reach. It names no argument or credential.
func callError(err error) string {
	var api *client.Error
	var errno syscall.Errno
	var op *net.OpError
	switch {
	case errors.As(err, &api):
		return api.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("no answer from the management endpoint within %s; the operation may still finish, and a retry with the same request ID never repeats it", callTimeout)
	case errors.Is(err, client.ErrUnreachable) && errors.As(err, &errno):
		return "management endpoint unreachable: " + errno.Error()
	case errors.Is(err, client.ErrUnreachable) && errors.As(err, &op) && op.Err != nil:
		return "management endpoint unreachable: " + op.Err.Error()
	case errors.Is(err, client.ErrUnreachable):
		return "management endpoint unreachable"
	}
	return "management call failed: " + err.Error()
}

// inbox reads the executor's inject inbox (OS_INBOX; the shell executor's
// file protocol): each delivered <order>-<digest>.json not yet acknowledged is
// printed as one JSON line and acknowledged with consumed/<digest>, which the
// executor reports as consumption with the attempt's result. Nothing goes
// through the endpoint, so it also works for an adopted process.
func (t *toolbelt) inbox(args []string) int {
	if len(args) != 0 {
		return t.usage("inbox takes no arguments")
	}
	dir := t.env.Getenv("OS_INBOX")
	if dir == "" {
		fmt.Fprintln(t.errOut, "os: OS_INBOX is not set: this execution has no inject inbox")
		return 1
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(t.errOut, "os: the inbox is unreadable")
		return 1
	}
	if err = os.MkdirAll(filepath.Join(dir, "consumed"), 0o700); err != nil {
		fmt.Fprintln(t.errOut, "os: the inbox is not writable")
		return 1
	}
	names := []string{}
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") && strings.Contains(n, "-") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		digest := n[strings.LastIndex(n, "-")+1 : len(n)-len(".json")]
		ack := filepath.Join(dir, "consumed", digest)
		if _, err := os.Stat(ack); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || !json.Valid(data) {
			fmt.Fprintln(t.errOut, "os: an inbox entry is unreadable")
			return 1
		}
		var line bytes.Buffer
		if err = json.Compact(&line, data); err != nil {
			return 1
		}
		line.WriteByte('\n')
		if _, err = t.out.Write(line.Bytes()); err != nil {
			return 1
		}
		// Acknowledge only once the event was handed over.
		if err = os.WriteFile(ack, nil, 0o600); err != nil {
			fmt.Fprintln(t.errOut, "os: could not acknowledge an inbox entry")
			return 1
		}
	}
	return 0
}
