package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/client"
)

const toolbeltHelp = `os — Open Conduit execution toolbelt (CLI transport)

Usage: os COMMAND [flags]

Commands:
  context                                   read this Work's accumulated context
  emit --type T --delivery-id D [--schema S] [--data JSON | --data-file PATH|-]
       [--key-source S --key-external-id X] [--occurred-at RFC3339]
  work create --kind K [--id ID] [--key-source S --key-external-id X]... [--schema S --data JSON]
  wait --type T[,T...] [--key-source S --key-external-id X]... [--timeout SECONDS] [--after-sequence N]
  outcome completed|failed|handoff [--summary TEXT] [--schema S --data JSON]
  call OPERATION [--schema S] [--data JSON | --data-file PATH|-]
  mcp                                       serve these operations as an MCP server on stdio
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
		return t.usage("expected work create")
	case "wait":
		return t.wait(args[1:])
	case "outcome":
		return t.outcome(args[1:])
	case "call":
		return t.call(args[1:])
	case "mcp":
		return t.mcp(args[1:])
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
	if path := t.env.Getenv("OS_TOKEN_FILE"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		token = t.env.Getenv("OS_TOKEN")
	}
	if endpoint == "" || token == "" {
		return nil, errors.New("OS_ENDPOINT and OS_TOKEN are not set; os runs inside an execution with a toolbelt")
	}
	return client.New(endpoint, token)
}

func (t *toolbelt) do(f *toolFlags, operation string, input any, pick func(client.ToolResult) any) int {
	c, err := t.client()
	if err != nil {
		fmt.Fprintf(t.errOut, "os: %s\n", err)
		return 2
	}
	call := client.ToolCall{Operation: operation, RequestID: f.get("request-id")}
	if input != nil {
		if call.Input, err = json.Marshal(input); err != nil {
			return t.usage("invalid input")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := c.ToolbeltCall(ctx, call)
	if err != nil {
		var api *client.Error
		if errors.As(err, &api) {
			fmt.Fprintf(t.errOut, "os: %s\n", api.Error())
		} else {
			fmt.Fprintln(t.errOut, "os: management endpoint unreachable")
		}
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

func (t *toolbelt) wait(args []string) int {
	f := newToolFlags("type", "timeout", "after-sequence")
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

func (t *toolbelt) call(args []string) int {
	f := newToolFlags("schema", "data", "data-file")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 || pos[0] == "" {
		return t.usage("call requires one operation")
	}
	data, err := t.payload(f)
	if err != nil {
		return t.usage(err.Error())
	}
	var in any
	if data != nil {
		in = data
	}
	return t.do(f, pos[0], in, func(r client.ToolResult) any {
		if r.Output == nil {
			return client.Payload{}
		}
		return r.Output
	})
}
