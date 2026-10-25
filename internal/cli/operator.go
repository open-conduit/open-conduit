package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/client"
)

// Env carries process inputs so operator commands are testable.
type Env struct {
	Getenv func(string) string
	Cwd    string
}

type operator struct {
	out, errOut io.Writer
	env         Env
	endpoint    string
	tokenFile   string
	socket      string
	dataDir     string
}

func usageError(errOut io.Writer, msg string) int {
	fmt.Fprintf(errOut, "open-conduit: %s; use --help\n", msg)
	return 2
}

const operatorHelp = `open-conduit — Open Conduit operator client

Usage: open-conduit [--endpoint unix:///path] [--token-file PATH] [--socket PATH] [--data-dir DIR] COMMAND

Commands:
  health
  scopes list [--parent-id ID] [--enabled true|false] | scopes get ID
  work list [--scope-id --state --kind --parent-id] | work get ID
  work create --scope-id S --kind K --binding B [--schema S --data JSON] [--key K]
  work cancel|retry ID --revision N [--reason TEXT] [--key K]
  work wake ID --revision N --type T --delivery-id D --key-source S --key-external-id X [--schema S --data JSON] [--key K]
  events list [--scope-id --work-id --type --after-sequence] | events get ID | events chain ID
  events emit --scope-id S --type T --delivery-id D [--work-id W] [--causation-id E] [--schema S --data JSON] [--key K]
  executions list [--scope-id --work-id --state] | executions get ID | executions cancel ID --revision N [--reason TEXT] [--key K]
  waits list [--scope-id --work-id --execution-id --state] | waits get ID
  artifacts list [--work-id --execution-id --kind] | artifacts cat ID [--range bytes=A-B]
  config validate FILE | config apply FILE --generation N [--key K]
  auth rotate | auth revoke TOKEN_ID [--reason TEXT] [--key K]
  version

Lists accept --limit and --cursor. Mutations accept --key to retry safely with the same Idempotency-Key.
`

func defaultStateDir(env Env) string {
	if v := env.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "open-conduit")
	}
	return filepath.Join(env.Getenv("HOME"), ".local", "state", "open-conduit")
}
func (o *operator) abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(o.env.Cwd, p)
}

// RunOperator dispatches operator commands through the public client only.
func RunOperator(version, commit string, args []string, env Env, out, errOut io.Writer) int {
	o := &operator{out: out, errOut: errOut, env: env}
	global := flag.NewFlagSet("open-conduit", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	global.StringVar(&o.endpoint, "endpoint", "", "")
	global.StringVar(&o.tokenFile, "token-file", "", "")
	global.StringVar(&o.socket, "socket", "", "")
	global.StringVar(&o.dataDir, "data-dir", "", "")
	if err := global.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(out, operatorHelp)
		return 0
	} else if err != nil {
		return usageError(errOut, "invalid global flag")
	}
	rest := global.Args()
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
		fmt.Fprint(out, operatorHelp)
		return 0
	}
	if rest[0] == "version" || rest[0] == "--version" {
		fmt.Fprintf(out, "open-conduit %s (%s)\n", version, commit)
		return 0
	}
	commands := map[string]func([]string) int{"health": o.health, "scopes": o.scopes, "work": o.work, "events": o.events, "executions": o.executions, "waits": o.waits, "artifacts": o.artifacts, "config": o.config, "auth": o.auth}
	cmd, ok := commands[rest[0]]
	if !ok {
		// Do not echo arguments: a mistaken invocation could contain credentials.
		return usageError(errOut, "unknown command")
	}
	return cmd(rest[1:])
}

func (o *operator) stateDir() string {
	if o.dataDir != "" {
		return o.abs(o.dataDir)
	}
	if v := o.env.Getenv("OPEN_CONDUIT_DATA_DIR"); v != "" {
		return o.abs(v)
	}
	return defaultStateDir(o.env)
}
func (o *operator) tokenPath() string {
	if o.tokenFile != "" {
		return o.abs(o.tokenFile)
	}
	if v := o.env.Getenv("OPEN_CONDUIT_TOKEN_FILE"); v != "" {
		return o.abs(v)
	}
	return filepath.Join(o.stateDir(), "private", "operator.token")
}
func (o *operator) client() (*client.Client, client.TokenFile, error) {
	file, err := client.ReadTokenFile(o.tokenPath())
	if err != nil {
		return nil, file, err
	}
	endpoint := client.DiscoverEndpoint(o.endpoint, o.abs(o.socket), o.stateDir(), o.env.Getenv)
	c, err := client.New(endpoint, file.Token)
	return c, file, err
}

func (o *operator) emit(v any, err error) int {
	if err != nil {
		var api *client.Error
		if errors.As(err, &api) {
			fmt.Fprintf(o.errOut, "open-conduit: %s\n", api.Error())
		} else {
			fmt.Fprintf(o.errOut, "open-conduit: %s\n", err)
		}
		return 1
	}
	enc := json.NewEncoder(o.out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return 0
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func randomKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "cli-" + hex.EncodeToString(b)
}

type flags struct {
	fs     *flag.FlagSet
	values map[string]*string
}

func newFlags(name string, names ...string) *flags {
	f := &flags{fs: flag.NewFlagSet(name, flag.ContinueOnError), values: map[string]*string{}}
	f.fs.SetOutput(io.Discard)
	for _, n := range names {
		f.values[n] = f.fs.String(n, "", "")
	}
	return f
}
func (f *flags) get(n string) string { return *f.values[n] }

// parse permits flags before and after positional arguments.
func (f *flags) parse(args []string) ([]string, error) {
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
func (f *flags) query(names ...string) url.Values {
	q := url.Values{}
	for _, n := range append(names, "limit", "cursor") {
		if v := *f.values[n]; v != "" {
			q.Set(strings.ReplaceAll(n, "-", "_"), v)
		}
	}
	return q
}
func (f *flags) key() string {
	if k := f.get("key"); k != "" {
		return k
	}
	return randomKey()
}
func (f *flags) payload() (*client.Payload, error) {
	schema, data := f.get("schema"), f.get("data")
	if schema == "" && data == "" {
		return nil, nil
	}
	if data != "" && !json.Valid([]byte(data)) {
		return nil, fmt.Errorf("--data must be JSON")
	}
	return &client.Payload{Schema: schema, Data: json.RawMessage(data)}, nil
}

// listOrGet implements "<noun> list|get" with shared filter flags.
func (o *operator) listOrGet(args []string, filters []string, list func(*client.Client, url.Values) (any, error), get func(*client.Client, string) (any, error)) int {
	if len(args) == 0 {
		return usageError(o.errOut, "missing subcommand")
	}
	f := newFlags(args[0], append(filters, "limit", "cursor")...)
	pos, err := f.parse(args[1:])
	if err != nil {
		return usageError(o.errOut, "invalid flag")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	switch {
	case args[0] == "list" && len(pos) == 0:
		return o.emit(list(c, f.query(filters...)))
	case args[0] == "get" && len(pos) == 1:
		return o.emit(get(c, pos[0]))
	}
	return usageError(o.errOut, "expected list or get ID")
}

func (o *operator) health(args []string) int {
	if len(args) != 0 {
		return usageError(o.errOut, "health takes no arguments")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	return o.emit(c.Health(cx))
}

func (o *operator) scopes(args []string) int {
	return o.listOrGet(args, []string{"parent-id", "enabled"}, func(c *client.Client, q url.Values) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Scopes(cx, q)
	}, func(c *client.Client, id string) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Scope(cx, id)
	})
}

func (o *operator) control(args []string, run func(c *client.Client, id, revision, reason, key string) (any, error)) int {
	f := newFlags("control", "revision", "reason", "key")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 || f.get("revision") == "" {
		return usageError(o.errOut, "expected ID --revision N")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	return o.emit(run(c, pos[0], f.get("revision"), f.get("reason"), f.key()))
}

func (o *operator) work(args []string) int {
	if len(args) == 0 {
		return usageError(o.errOut, "missing work subcommand")
	}
	switch args[0] {
	case "create":
		f := newFlags("create", "scope-id", "kind", "binding", "parent-id", "schema", "data", "key")
		pos, err := f.parse(args[1:])
		if err != nil || len(pos) != 0 {
			return usageError(o.errOut, "invalid work create flags")
		}
		p, err := f.payload()
		if err != nil {
			return usageError(o.errOut, err.Error())
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		return o.emit(c.CreateWork(cx, client.WorkRequest{ScopeID: f.get("scope-id"), Kind: f.get("kind"), Binding: f.get("binding"), ParentID: f.get("parent-id"), Context: p}, f.key()))
	case "cancel":
		return o.control(args[1:], func(c *client.Client, id, rev, reason, key string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.CancelWork(cx, id, rev, reason, key)
		})
	case "retry":
		return o.control(args[1:], func(c *client.Client, id, rev, reason, key string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.RetryWork(cx, id, rev, reason, key)
		})
	case "wake":
		f := newFlags("wake", "revision", "type", "delivery-id", "key-source", "key-external-id", "schema", "data", "key")
		pos, err := f.parse(args[1:])
		if err != nil || len(pos) != 1 {
			return usageError(o.errOut, "expected work wake ID with flags")
		}
		p, err := f.payload()
		if err != nil {
			return usageError(o.errOut, err.Error())
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		return o.emit(c.Wake(cx, pos[0], client.WakeRequest{ExpectedRevision: f.get("revision"), Type: f.get("type"), DeliveryID: f.get("delivery-id"), Data: p, CorrelationKey: &client.Key{Source: f.get("key-source"), ExternalID: f.get("key-external-id")}}, f.key()))
	}
	return o.listOrGet(args, []string{"scope-id", "state", "kind", "parent-id"}, func(c *client.Client, q url.Values) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Works(cx, q)
	}, func(c *client.Client, id string) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Work(cx, id)
	})
}

func (o *operator) events(args []string) int {
	if len(args) > 0 && args[0] == "chain" {
		if len(args) != 2 {
			return usageError(o.errOut, "expected events chain ID")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		return o.emit(c.EventChain(cx, args[1]))
	}
	if len(args) > 0 && args[0] == "emit" {
		f := newFlags("emit", "scope-id", "type", "delivery-id", "work-id", "causation-id", "schema", "data", "key")
		pos, err := f.parse(args[1:])
		if err != nil || len(pos) != 0 {
			return usageError(o.errOut, "invalid events emit flags")
		}
		p, err := f.payload()
		if err != nil {
			return usageError(o.errOut, err.Error())
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		return o.emit(c.SubmitEvent(cx, client.EventRequest{ScopeID: f.get("scope-id"), Type: f.get("type"), DeliveryID: f.get("delivery-id"), WorkID: f.get("work-id"), CausationID: f.get("causation-id"), Data: p}, f.key()))
	}
	return o.listOrGet(args, []string{"scope-id", "work-id", "type", "after-sequence"}, func(c *client.Client, q url.Values) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Events(cx, q)
	}, func(c *client.Client, id string) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Event(cx, id)
	})
}

func (o *operator) executions(args []string) int {
	if len(args) > 0 && args[0] == "cancel" {
		return o.control(args[1:], func(c *client.Client, id, rev, reason, key string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.CancelExecution(cx, id, rev, reason, key)
		})
	}
	return o.listOrGet(args, []string{"scope-id", "work-id", "state"}, func(c *client.Client, q url.Values) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Executions(cx, q)
	}, func(c *client.Client, id string) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Execution(cx, id)
	})
}

func (o *operator) waits(args []string) int {
	return o.listOrGet(args, []string{"scope-id", "work-id", "execution-id", "state"}, func(c *client.Client, q url.Values) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Waits(cx, q)
	}, func(c *client.Client, id string) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Wait(cx, id)
	})
}

func (o *operator) artifacts(args []string) int {
	if len(args) > 0 && args[0] == "cat" {
		f := newFlags("cat", "range")
		pos, err := f.parse(args[1:])
		if err != nil || len(pos) != 1 {
			return usageError(o.errOut, "expected artifacts cat ID")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		if err = c.ArtifactContent(cx, pos[0], f.get("range"), o.out); err != nil {
			return o.emit(nil, err)
		}
		return 0
	}
	if len(args) == 0 || args[0] != "list" {
		return usageError(o.errOut, "expected artifacts list or cat ID")
	}
	f := newFlags("list", "work-id", "execution-id", "kind", "limit", "cursor")
	if pos, err := f.parse(args[1:]); err != nil || len(pos) != 0 {
		return usageError(o.errOut, "invalid artifacts list flags")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	return o.emit(c.Artifacts(cx, f.query("work-id", "execution-id", "kind")))
}

func (o *operator) config(args []string) int {
	if len(args) == 0 {
		return usageError(o.errOut, "missing config subcommand")
	}
	f := newFlags(args[0], "generation", "key")
	pos, err := f.parse(args[1:])
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected config validate|apply FILE")
	}
	data, err := os.ReadFile(o.abs(pos[0]))
	if err != nil {
		return o.emit(nil, fmt.Errorf("config file unreadable"))
	}
	if !json.Valid(data) {
		return o.emit(nil, fmt.Errorf("config file is not valid JSON"))
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	switch args[0] {
	case "validate":
		return o.emit(c.ValidateConfig(cx, data))
	case "apply":
		if f.get("generation") == "" {
			return usageError(o.errOut, "config apply requires --generation (the current committed generation)")
		}
		v, err := c.ValidateConfig(cx, data)
		if err != nil {
			return o.emit(nil, err)
		}
		return o.emit(c.ApplyConfig(cx, data, v.CandidateHash, f.get("generation"), f.key()))
	}
	return usageError(o.errOut, "expected config validate|apply FILE")
}

func (o *operator) auth(args []string) int {
	if len(args) == 0 {
		return usageError(o.errOut, "missing auth subcommand")
	}
	switch args[0] {
	case "rotate":
		if len(args) != 1 {
			return usageError(o.errOut, "auth rotate takes no arguments")
		}
		c, file, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		replacement, err := c.Rotate(cx, file.TokenID)
		if err != nil {
			return o.emit(nil, err)
		}
		if err = writePrivateJSON(o.tokenPath(), replacement); err != nil {
			return o.emit(nil, fmt.Errorf("rotation committed but the credential file could not be saved; recover with the locked local credential reset"))
		}
		return o.emit(map[string]string{"token_id": replacement.TokenID}, nil)
	case "revoke":
		f := newFlags("revoke", "reason", "key")
		pos, err := f.parse(args[1:])
		if err != nil || len(pos) != 1 {
			return usageError(o.errOut, "expected auth revoke TOKEN_ID")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		return o.emit(c.Revoke(cx, pos[0], f.get("reason"), f.key()))
	}
	return usageError(o.errOut, "expected auth rotate|revoke")
}

func writePrivateJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(append(b, '\n')); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
