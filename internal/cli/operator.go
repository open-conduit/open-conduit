// Package cli provides the operator commands (RunOperator) and the execution
// toolbelt CLI used by os (RunToolbelt), both over the public local client.
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
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
)

// Env carries process inputs so operator commands are testable.
type Env struct {
	Getenv func(string) string
	Cwd    string
	// TUI runs the interactive history view; the open-conduit entrypoint
	// supplies it so the os toolbelt binary carries no terminal UI.
	TUI func(*client.Client) error
	// Web serves the browser view until interrupted; the open-conduit
	// entrypoint supplies it so the os toolbelt binary carries no web UI.
	Web func(WebRequest) error
	// Serve runs the daemon; only the open-conduit entrypoint supplies it.
	Serve func(ServeRequest) int
	// Stdin is the terminal input for interactive commands (default os.Stdin).
	Stdin io.Reader
	// Attach runs an attach command (default: with the terminal).
	Attach func(argv []string, dir string) error
	// ValidateConfig validates a config file offline against the daemon's
	// registrations; only the open-conduit entrypoint supplies it.
	ValidateConfig func(data []byte) ([]client.Detail, error)
	// ValidateConfigAt is ValidateConfig for a config file at path, so the
	// extension packages installed beside it (07-03) are known; it takes
	// precedence when set.
	ValidateConfigAt func(path string, data []byte) ([]client.Detail, error)
	// ConfigWarnings names what in the config is accepted but suspect: cmd:
	// secret references whose program is not found on the resolver's PATH,
	// settings that have no effect yet, and included folders that have gone.
	// path is the config file's. Only the open-conduit entrypoint supplies
	// it. Warnings never fail a check.
	ConfigWarnings func(path string, data []byte) []string
	// OfflineChecks are the doctor checks that need no daemon (the config,
	// its bindings, the agent CLIs); doctor runs them when no daemon is
	// reachable. Only the open-conduit entrypoint supplies it.
	OfflineChecks func(ctx context.Context, configPath string, data []byte) []client.Check
	// Extensions manages installed extension packages and runs the
	// conformance runner (07-03); only the open-conduit entrypoint
	// supplies it.
	Extensions *ExtensionOps
	// Setup starts a coding agent with the setup guide (init --with).
	Setup func(SetupRequest) int
	// Setups are the registered extensions' optional setup flows that
	// `init <type>` hosts; only the open-conduit entrypoint supplies them,
	// so this package never imports an extension.
	Setups []extension.Setup
	// Adapters is the registered integrations' catalog (`adapters`, the
	// config editor); only the open-conduit entrypoint supplies it.
	Adapters func() []Adapter
	// EditConfig runs the full-screen config editor and reports whether it
	// saved; only the open-conduit entrypoint supplies it (the os binary
	// carries no terminal UI).
	EditConfig func(EditRequest) (bool, error)
	// Migrate converts an Open Supervisor v1 config (06-02); State runs the
	// offline backup, restore, diagnosis and credential recovery. Only the
	// open-conduit entrypoint supplies them.
	Migrate func(MigrateRequest) (MigrateResult, error)
	// Upgrade renames the extension settings a release renamed in an Open
	// Conduit config (`migrate upgrade`), keeping the rest of the file.
	Upgrade func(config []byte) (MigrateResult, error)
	State   *StateOps
	// Service runs the background service commands and doctor's service
	// check; only the open-conduit entrypoint supplies it.
	Service *ServiceHost
	// Updater checks for and installs releases (`upgrade`); only the
	// open-conduit entrypoint supplies it.
	Updater *upgrade.Updater
	// OpenURL, LookPath and RunCommand replace the setup host's browser
	// opener, PATH lookup and attached command runner (tests).
	OpenURL    func(string) error
	LookPath   func(string) (string, bool)
	RunCommand func(ctx context.Context, dir, name string, args ...string) error
}

type operator struct {
	out, errOut io.Writer
	env         Env
	endpoint    string
	tokenFile   string
	socket      string
	dataDir     string
	build       string
	version     string
}

func usageError(errOut io.Writer, msg string) int {
	fmt.Fprintf(errOut, "open-conduit: %s; use --help\n", msg)
	return 2
}

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
	o := &operator{out: out, errOut: errOut, env: env, build: version + " (" + commit + ")", version: version}
	global := flag.NewFlagSet("open-conduit", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	global.StringVar(&o.endpoint, "endpoint", "", "")
	global.StringVar(&o.tokenFile, "token-file", "", "")
	global.StringVar(&o.socket, "socket", "", "")
	global.StringVar(&o.dataDir, "data-dir", "", "")
	showVersion := global.Bool("version", false, "")
	if err := global.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(out, operatorHelp)
		return 0
	} else if err != nil {
		return usageError(errOut, "invalid global flag")
	}
	rest := global.Args()
	if *showVersion && len(rest) == 0 {
		rest = []string{"version"}
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
		fmt.Fprint(out, operatorHelp)
		return 0
	}
	if *showVersion || rest[0] == "version" || rest[0] == "--version" {
		fmt.Fprintf(out, "open-conduit %s (%s)\n", version, commit)
		return 0
	}
	commands := map[string]func([]string) int{"health": o.health, "scopes": o.scopes, "work": o.work, "events": o.events, "executions": o.executions, "waits": o.waits, "artifacts": o.artifacts, "config": o.config, "auth": o.auth, "runs": o.runs, "doctor": o.doctor, "tui": o.tui, "web": o.web, "serve": o.serve, "claim": o.claim, "actions": o.actions, "init": o.init, "secret": o.secret, "adapters": o.adapters, "route": o.route, "migrate": o.migrate, "backup": o.backup, "state": o.state, "extension": o.extension, "dev": o.dev, "service": o.service, "container": o.container, "upgrade": o.selfUpgrade}
	cmd, ok := commands[rest[0]]
	if !ok {
		// Do not echo arguments: a mistaken invocation could contain credentials.
		return usageError(errOut, "unknown command")
	}
	// Help never needs the daemon, whichever command asks for it.
	if helpRequested(rest[1:]) {
		o.printHelp(out, rest[0], rest[1:])
		return 0
	}
	// A command that needs a subcommand shows what it takes.
	if len(rest) == 1 && needsSubcommand[rest[0]] {
		fmt.Fprint(errOut, commandHelp(rest[0]))
		return 2
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
	endpoint := client.DiscoverEndpoint(o.endpoint, o.abs(o.socket), o.stateDir(), o.env.Getenv)
	file, err := client.ReadTokenFile(o.tokenPath())
	if err != nil {
		// The first serve writes the token: with none and nothing listening,
		// say the daemon has never run rather than that a file is missing.
		if _, statErr := os.Lstat(o.tokenPath()); errors.Is(statErr, os.ErrNotExist) {
			if !listening(endpoint) {
				return nil, file, &notRunningError{neverStarted: true}
			}
			return nil, file, fmt.Errorf("operator token file unavailable: %s does not exist, but a daemon is running at %s; pass that daemon's token with --token-file or OPEN_CONDUIT_TOKEN_FILE", o.tokenPath(), endpoint)
		}
		return nil, file, err
	}
	c, err := client.New(endpoint, file.Token)
	return c, file, err
}

// notRunningError reports that no daemon answers on the management endpoint.
type notRunningError struct{ neverStarted bool }

func (e *notRunningError) Error() string {
	if e.neverStarted {
		return "no Open Conduit daemon is running (it has never been started on this machine); start one with `open-conduit serve`"
	}
	return "no Open Conduit daemon is running; start one with `open-conduit serve`"
}

// listening reports whether something accepts connections on a unix
// endpoint; an endpoint that does not parse is left to the client to report.
func listening(endpoint string) bool {
	socket, err := client.ParseEndpoint(endpoint)
	if err != nil {
		return true
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// daemonDown reports whether err means no daemon is reachable at all: never
// started, or nothing listening on its socket.
func daemonDown(err error) (*notRunningError, bool) {
	var nr *notRunningError
	if errors.As(err, &nr) {
		return nr, true
	}
	if errors.Is(err, client.ErrUnreachable) && (errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)) {
		return &notRunningError{}, true
	}
	return nil, false
}

func (o *operator) emit(v any, err error) int {
	if err != nil {
		var api *client.Error
		if nr, ok := daemonDown(err); ok {
			fmt.Fprintf(o.errOut, "open-conduit: %s\n", nr)
		} else if errors.As(err, &api) {
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
	// --scope and --scope-id name the same thing on every command.
	for n, alias := range map[string]string{"scope-id": "scope", "scope": "scope-id"} {
		if v, ok := f.values[n]; ok && f.values[alias] == nil {
			f.fs.StringVar(v, alias, "", "")
		}
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

// listOrGet implements "<noun> list|get" with shared filter flags. A list
// prints a table of columns (JSON field names, dotted for nested ones)
// unless --json asks for the records.
func (o *operator) listOrGet(args []string, filters, columns []string, list func(*client.Client, url.Values) (any, error), get func(*client.Client, string) (any, error)) int {
	if len(args) == 0 {
		return usageError(o.errOut, "missing subcommand")
	}
	f := newFlags(args[0], append(filters, "limit", "cursor")...)
	asJSON := f.fs.Bool("json", false, "")
	pos, err := f.parse(args[1:])
	if err != nil {
		return usageError(o.errOut, "invalid flag")
	}
	switch {
	case args[0] == "list" && len(pos) == 0:
	case args[0] == "get" && len(pos) == 1:
	default:
		return usageError(o.errOut, "expected list or get ID")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	if args[0] == "get" {
		return o.emit(get(c, pos[0]))
	}
	v, err := list(c, f.query(filters...))
	if err != nil || *asJSON {
		return o.emit(v, err)
	}
	return o.table(v, columns)
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
	if len(args) > 0 && args[0] == "reconcile" {
		return o.reconcileScope(args[1:])
	}
	return o.listOrGet(args, []string{"parent-id", "enabled"}, scopeColumns, func(c *client.Client, q url.Values) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Scopes(cx, q)
	}, func(c *client.Client, id string) (any, error) {
		cx, cancel := ctx()
		defer cancel()
		return c.Scope(cx, id)
	})
}

// control runs a revisioned control on ID. Without --revision it acts on
// the current revision (current fetches it, refetching once if the record
// moved meanwhile); with it, only if that revision is still current.
func (o *operator) control(args []string, current func(c *client.Client, id string) (string, error), run func(c *client.Client, id, revision, reason, key string) (any, error)) int {
	f := newFlags("control", "revision", "reason", "key")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected ID [--revision N]")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	key := f.key()
	return o.emit(withRevision(c, pos[0], f.get("revision"), current, func(rev string) (any, error) {
		return run(c, pos[0], rev, f.get("reason"), key)
	}))
}

// withRevision runs fn at revision, or, when revision is "", at the
// record's current one, fetching it again if a concurrent change made it
// stale before fn ran.
func withRevision(c *client.Client, id, revision string, current func(c *client.Client, id string) (string, error), fn func(rev string) (any, error)) (any, error) {
	if revision != "" {
		return fn(revision)
	}
	var (
		v   any
		err error
	)
	for attempt := 0; attempt < 3; attempt++ {
		var rev string
		if rev, err = current(c, id); err != nil {
			return nil, err
		}
		v, err = fn(rev)
		var api *client.Error
		if !errors.As(err, &api) || api.Code != "revision_conflict" {
			return v, err
		}
	}
	return v, err
}

func workRevision(c *client.Client, id string) (string, error) {
	cx, cancel := ctx()
	defer cancel()
	w, err := c.Work(cx, id)
	return w.Revision, err
}

func executionRevision(c *client.Client, id string) (string, error) {
	cx, cancel := ctx()
	defer cancel()
	e, err := c.Execution(cx, id)
	return e.Revision, err
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
		return o.control(args[1:], workRevision, func(c *client.Client, id, rev, reason, key string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.CancelWork(cx, id, rev, reason, key)
		})
	case "retry":
		return o.control(args[1:], workRevision, func(c *client.Client, id, rev, reason, key string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.RetryWork(cx, id, rev, reason, key)
		})
	case "hold", "release", "rebind", "attach":
		return o.workControl(args[0], args[1:])
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
		key := f.key()
		return o.emit(withRevision(c, pos[0], f.get("revision"), workRevision, func(rev string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.Wake(cx, pos[0], client.WakeRequest{ExpectedRevision: rev, Type: f.get("type"), DeliveryID: f.get("delivery-id"), Data: p, CorrelationKey: &client.Key{Source: f.get("key-source"), ExternalID: f.get("key-external-id")}}, key)
		}))
	}
	return o.listOrGet(args, []string{"scope-id", "state", "kind", "parent-id"}, workColumns, func(c *client.Client, q url.Values) (any, error) {
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
		f := newFlags("emit", "scope-id", "type", "delivery-id", "work-id", "causation-id", "schema", "data", "key", "key-source", "key-external-id")
		pos, err := f.parse(args[1:])
		if err != nil || len(pos) != 0 || (f.get("key-source") == "") != (f.get("key-external-id") == "") {
			return usageError(o.errOut, "invalid events emit flags")
		}
		var correlation *client.Key
		if f.get("key-source") != "" {
			correlation = &client.Key{Source: f.get("key-source"), ExternalID: f.get("key-external-id")}
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
		return o.emit(c.SubmitEvent(cx, client.EventRequest{ScopeID: f.get("scope-id"), Type: f.get("type"), DeliveryID: f.get("delivery-id"), WorkID: f.get("work-id"), CausationID: f.get("causation-id"), CorrelationKey: correlation, Data: p}, f.key()))
	}
	return o.listOrGet(args, []string{"scope-id", "work-id", "type", "after-sequence"}, eventColumns, func(c *client.Client, q url.Values) (any, error) {
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
		return o.control(args[1:], executionRevision, func(c *client.Client, id, rev, reason, key string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.CancelExecution(cx, id, rev, reason, key)
		})
	}
	return o.listOrGet(args, []string{"scope-id", "work-id", "state"}, executionColumns, func(c *client.Client, q url.Values) (any, error) {
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
	return o.listOrGet(args, []string{"scope-id", "work-id", "execution-id", "state"}, waitColumns, func(c *client.Client, q url.Values) (any, error) {
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
	asJSON := f.fs.Bool("json", false, "")
	if pos, err := f.parse(args[1:]); err != nil || len(pos) != 0 {
		return usageError(o.errOut, "invalid artifacts list flags")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	page, err := c.Artifacts(cx, f.query("work-id", "execution-id", "kind"))
	if err != nil || *asJSON {
		return o.emit(page, err)
	}
	return o.table(page, artifactColumns)
}

func (o *operator) config(args []string) int {
	const configUsage = "expected config check|path|show|get|set|unset|include|edit (local file) or config validate|apply FILE (running daemon)"
	if len(args) == 0 {
		return usageError(o.errOut, configUsage)
	}
	if args[0] == "edit" {
		return o.editConfig(args[1:])
	}
	if code, ok := o.localConfig(args); ok {
		return code
	}
	f := newFlags(args[0], "generation", "key")
	pos, err := f.parse(args[1:])
	if args[0] != "validate" && args[0] != "apply" {
		return usageError(o.errOut, configUsage)
	}
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected config "+args[0]+" FILE")
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
		// Without --generation, apply over whatever is current; with it,
		// only if that generation is still current (compare-and-swap).
		generation := f.get("generation")
		if generation == "" {
			current, err := c.Config(cx)
			if err != nil {
				return o.emit(nil, err)
			}
			generation = current.Generation
		}
		v, err := c.ValidateConfig(cx, data)
		if err != nil {
			return o.emit(nil, err)
		}
		return o.emit(c.ApplyConfigText(cx, data, v.CandidateHash, generation, f.key()))
	}
	return usageError(o.errOut, configUsage)
}

func (o *operator) auth(args []string) int {
	if len(args) == 0 {
		return usageError(o.errOut, "missing auth subcommand")
	}
	switch args[0] {
	case "recover":
		if len(args) != 1 {
			return usageError(o.errOut, "auth recover takes no arguments")
		}
		return o.recoverOperator()
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
			return o.emit(nil, fmt.Errorf("rotation committed but the credential file could not be saved; stop the daemon and run open-conduit auth recover"))
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
	return usageError(o.errOut, "expected auth rotate|revoke|recover")
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
