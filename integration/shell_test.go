package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/shell"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/internal/telemetry"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

// TestMain lets this test binary act as the `os` toolbelt CLI when invoked
// through the symlink placed on a shell execution's PATH.
// It first tries to build the shipped cmd/os binary (without the race
// runtime, whose start-up makes every toolbelt call slow), falling back to the
// symlinked test binary.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "os" {
		cwd, _ := os.Getwd()
		os.Exit(cli.RunToolbelt("test", "test", os.Args[1:], cli.Env{Getenv: os.Getenv, Cwd: cwd}, os.Stdin, os.Stdout, os.Stderr))
	}
	dir, err := os.MkdirTemp("", "oc-osbin-")
	if err == nil {
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "os"), "github.com/open-conduit/open-conduit/cmd/os")
		if build.Run() == nil {
			builtToolbelt = filepath.Join(dir, "os")
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

var builtToolbelt string

func shellConfig(t *testing.T, binding map[string]any) string {
	t.Helper()
	cfg, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	return `{
  "version": 1, "settings": {}, "connections": [],
  "scopes": [{
    "id": "ops", "name": "Operations",
    "authorization": {"connections": [], "operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "read_scopes": []},
    "bindings": [{"name": "run", "executor": "shell", "requires": {"toolbelt": "cli"}, "operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "secret_names": ["api"], "config": ` + string(cfg) + `}],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}],
    "secret_refs": {"api": "cmd:printf shell-secret"}
  }]
}`
}

// shellHost composes the daemon pieces over one state directory. close and a
// new shellHost on the same directory model a daemon restart: only SQLite,
// artifacts, the config file and the shell executor's state survive.
type shellHost struct {
	t         *testing.T
	state     string
	clock     *conformance.Clock
	store     *sqlite.Store
	artifacts *artifacts.Store
	router    *runtime.Router
	scheduler *runtime.Scheduler
	executor  *shell.Executor
	endpoint  string
	srv       *http.Server
	lock      *management.Lock
	closed    bool
	// coord is the coordination store every component uses: SQLite itself,
	// or SQLite behind lifecycle instrumentation when telemetry is enabled.
	coord     storage.Store
	toolbelt  *runtime.Toolbelt
	timers    *runtime.Timers
	ingress   *runtime.Ingress
	pipeline  *telemetry.Pipeline
	memory    *telemetry.Memory
	diagnosed []string
	diagMu    sync.Mutex
}

// shellHostOptions enables optional 02-08 composition: lifecycle telemetry
// (pipeline with an in-memory exporter plus any extra exporters), retention
// and extension doctor checks.
type shellHostOptions struct {
	telemetry bool
	exporters []observability.Exporter
	buffer    int
	kept      int
	checks    []func(context.Context) runtime.Check
	ingress   bool
	// executors and handlers add fake registrations beside the shell.
	executors map[string]extension.Executor
	handlers  map[string]extension.RouteHandler
}

func shellState(t *testing.T, cfg string) (string, string) {
	t.Helper()
	state, err := os.MkdirTemp("/tmp", "oc-shell-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	bin := filepath.Join(state, "bin")
	self := builtToolbelt
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "os")); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveAtomic(filepath.Join(state, "config", "config.json"), []byte(cfg)); err != nil {
		t.Fatal(err)
	}
	return state, bin
}

func newShellHost(t *testing.T, state, bin string, clock *conformance.Clock, owner string, resume bool) *shellHost {
	t.Helper()
	return newShellHostWith(t, state, bin, clock, owner, resume, shellHostOptions{})
}

func newShellHostWith(t *testing.T, state, bin string, clock *conformance.Clock, owner string, resume bool, o shellHostOptions) *shellHost {
	t.Helper()
	ctx := context.Background()
	h := &shellHost{t: t, state: state, clock: clock}
	var err error
	if h.lock, err = management.AcquireLock(state); err != nil {
		t.Fatal(err)
	}
	if h.store, err = sqlite.Open(ctx, filepath.Join(state, "conduit.db")); err != nil {
		t.Fatal(err)
	}
	h.coord = h.store
	var observer observability.Observer
	if o.telemetry {
		h.memory = &telemetry.Memory{}
		if h.pipeline, err = telemetry.New(telemetry.Options{Exporters: append([]observability.Exporter{h.memory}, o.exporters...), Buffer: o.buffer, FlushInterval: 10 * time.Millisecond, ExportTimeout: 200 * time.Millisecond, Attempts: 2, Secrets: func() []string { return []string{"shell-secret"} }}); err != nil {
			t.Fatal(err)
		}
		observer = h.pipeline
		if h.coord, err = runtime.Instrument(h.store, runtime.TelemetryOptions{Observer: h.pipeline, Diagnostic: func(code string) {
			h.diagMu.Lock()
			h.diagnosed = append(h.diagnosed, code)
			h.diagMu.Unlock()
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.MkdirAll(filepath.Join(state, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if h.artifacts, err = artifacts.Open(filepath.Join(state, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if h.executor, err = shell.New(shell.Options{StateDir: filepath.Join(state, "executors", "shell"), Artifacts: h.artifacts, ToolbeltDir: bin, Resume: resume, Grace: 500 * time.Millisecond, PollInterval: 20 * time.Millisecond, Environ: func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + state, "OPEN_CONDUIT_TOKEN_FILE=" + filepath.Join(state, "private", "operator.token")}
	}}); err != nil {
		t.Fatal(err)
	}
	executors := map[string]extension.Executor{"shell": h.executor}
	routeBindings := map[string]runtime.RouteBinding{"shell": {Executor: h.executor, Capabilities: h.executor.Capabilities()}}
	for name, e := range o.executors {
		executors[name] = e
		routeBindings[name] = runtime.RouteBinding{Executor: e, Capabilities: e.Capabilities()}
	}
	handlers := map[string]extension.RouteHandler{"manual": routingHandlerFunc(shellRoute)}
	handlerNames := map[string]bool{"manual": true}
	for name, handler := range o.handlers {
		handlers[name] = handler
		handlerNames[name] = true
	}
	registry := config.Registry{Executors: executors, Handlers: handlerNames}
	configs, err := management.OpenConfig(ctx, h.coord, clock.Now, registry, filepath.Join(state, "config", "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.router, err = runtime.NewRouter(h.coord, clock.Now, runtime.RouterOptions{Generation: "1", Handlers: handlers, Bindings: routeBindings}); err != nil {
		t.Fatal(err)
	}
	var belt *runtime.Toolbelt
	belt, err = runtime.NewToolbelt(h.coord, clock.Now, h.router, runtime.ToolbeltOptions{Authorize: configs.Authorize, Grant: configs.Grant(func() []string { return belt.EventTypes() }), Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	if err = belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "fixture", EventTypes: []string{"fixture.reported.v1"}}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(state, "management.sock")
	h.endpoint = "unix://" + socket
	if h.scheduler, err = runtime.NewScheduler(h.coord, clock.Now, h.router, runtime.SchedulerOptions{Owner: owner, Generation: "1", Executors: executors, Toolbelt: belt, ToolbeltEndpoint: h.endpoint, Secrets: configs.Secrets(&config.Resolver{}), KeptExecutions: o.kept, Artifacts: h.artifacts}); err != nil {
		t.Fatal(err)
	}
	h.toolbelt = belt
	if o.ingress {
		if h.ingress, err = runtime.NewIngress(h.coord, clock.Now, runtime.IngressOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if h.timers, err = runtime.NewTimers(h.coord, clock.Now, runtime.TimerOptions{}); err != nil {
		t.Fatal(err)
	}
	control, err := runtime.NewControl(h.coord, clock.Now, h.router, h.scheduler, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	server, err := management.NewServer(management.Options{Store: h.coord, Artifacts: h.artifacts, Now: clock.Now, Control: control, Toolbelt: belt, Config: configs, Version: "test", InstanceID: owner, Scheduler: h.scheduler, Resolver: &config.Resolver{}, Checks: o.checks, Ingress: h.ingress,
		Live: func(ctx context.Context, executor string, id domain.ID, tail int64) ([]extension.LiveOutput, error) {
			l, ok := executors[executor].(extension.LiveExecutor)
			if !ok {
				return nil, management.ErrLiveUnsupported
			}
			return l.Live(ctx, id, tail)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err = management.BootstrapOperator(ctx, h.coord, clock.Now, filepath.Join(state, "private", "operator.token")); err != nil && !errors.Is(err, management.ErrCredentialExists) {
		t.Fatal(err)
	}
	listener, err := management.Listen(h.lock, socket)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second}
	go h.srv.Serve(listener)
	t.Cleanup(h.close)
	return h
}

func (h *shellHost) close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.pipeline != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = h.pipeline.Shutdown(ctx)
		cancel()
	}
	h.srv.Close()
	h.store.Close()
	h.artifacts.Close()
	h.lock.Close()
}

// shellRoute sends operator events to Work keyed by their correlation; other
// events (execution emissions, child creation) are not routed to new Work.
func shellRoute(in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if !strings.HasPrefix(e.Type, "operator.") || e.CorrelationKey == nil {
		return extension.RouteDecision{}, nil
	}
	return extension.RouteDecision{Match: true, CreateID: domain.ID("work/" + e.CorrelationKey.ExternalID), Kind: "fixture.work"}, nil
}

func (h *shellHost) operator() *client.Client {
	h.t.Helper()
	f, err := client.ReadTokenFile(filepath.Join(h.state, "private", "operator.token"))
	if err != nil {
		h.t.Fatal(err)
	}
	c, err := client.New(h.endpoint, f.Token)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// routeAll routes every accepted but unrouted event, as the daemon's routing
// loop would.
func (h *shellHost) routeAll() {
	h.t.Helper()
	var pending []domain.Event
	err := h.coord.Transact(context.Background(), storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
		events, err := tx.Events(0, 0)
		for _, a := range events {
			if !a.Routed {
				pending = append(pending, a.Event)
			}
		}
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, e := range pending {
		if err = h.router.Route(context.Background(), e.ID); err != nil {
			h.t.Fatalf("route %s %s work=%s: %v", e.Type, e.Source, e.WorkID, err)
		}
	}
}

func (h *shellHost) observe(a runtime.Attempt) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := h.scheduler.Observe(ctx, a)
	if errors.Is(err, storage.ErrConflict) {
		// Completion cannot overtake accepted unrouted input (for example the
		// execution's own emission): route it, then reapply the stored result.
		h.routeAll()
		err = h.scheduler.Observe(ctx, a)
	}
	if err != nil {
		h.t.Fatalf("observe: %v", err)
	}
}

func (h *shellHost) submit(typ, delivery, external string) client.Event {
	h.t.Helper()
	e, err := h.operator().SubmitEvent(context.Background(), client.EventRequest{ScopeID: "ops", Type: typ, DeliveryID: delivery, CorrelationKey: &client.Key{Source: "ops", ExternalID: external}}, "submit-"+delivery)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

func (h *shellHost) artifact(id string) string {
	h.t.Helper()
	var b strings.Builder
	if err := h.operator().ArtifactContent(context.Background(), id, "", &b); err != nil {
		h.t.Fatal(err)
	}
	return b.String()
}

func shellData(t *testing.T, o *client.Outcome) map[string]any {
	t.Helper()
	m := map[string]any{}
	if o == nil || o.Data == nil || json.Unmarshal(o.Data.Data, &m) != nil {
		t.Fatalf("outcome without structured data: %+v", o)
	}
	return m
}

const toolbeltScript = `set -e
[ "$API" = shell-secret ] || { echo "secret missing" >&2; exit 8; }
[ -z "$OPEN_CONDUIT_TOKEN_FILE" ] || { echo "operator credential leaked" >&2; exit 7; }
if [ "$OS_TRIGGER_TYPE" = operator.reply ]; then
  os context > "$OS_OUTPUT_DIR/context-after-wake.json"
  os outcome completed --summary "reply handled" --schema fixture.v1 --data '{"handled":true}'
  echo "declared after wake"
  exit 0
fi
os context > "$OS_OUTPUT_DIR/context.json"
os emit --type fixture.reported.v1 --delivery-id report-1 --schema fixture.v1 --data '{"ok":true}' --request-id emit-1 > "$OS_OUTPUT_DIR/emitted.json"
os work create --kind fixture.child --key-source ops --key-external-id child-1 --request-id child-1 > "$OS_OUTPUT_DIR/child.json"
os wait --type operator.reply --key-source ops --key-external-id thread-1 --timeout 3600 --request-id wait-1 > "$OS_OUTPUT_DIR/wait.json"
if os context > /dev/null 2>&1; then echo "authority survived wait" >&2; exit 9; fi
echo "waiting for reply"
`

// TestShellToolbeltWorkflow is the 02-07 acceptance: a no-workspace shell
// execution reads context, emits an event, creates child Work, waits and,
// after the wake, declares an outcome, all through the `os` CLI over the real
// Unix-socket toolbelt. It runs with N+1 wake and with explicit resume.
func TestShellToolbeltWorkflow(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run("resume="+strconv.FormatBool(resume), func(t *testing.T) {
			state, bin := shellState(t, shellConfig(t, map[string]any{"command": toolbeltScript}))
			h := newShellHost(t, state, bin, conformance.NewClock(), "daemon", resume)
			ctx := context.Background()
			c := h.operator()
			root := h.submit("operator.requested", "root", "thread-1")
			work := root.RoutedWorkID
			first, err := h.scheduler.Start(ctx, domain.ID(work))
			if err != nil {
				t.Fatal(err)
			}
			h.observe(first)
			x, err := c.Execution(ctx, string(first.Lease.ExecutionID))
			if err != nil {
				t.Fatal(err)
			}
			w, err := c.Work(ctx, work)
			if err != nil || w.State != "waiting" {
				t.Fatalf("work after declared wait: %+v %v", w, err)
			}
			waits, err := c.Waits(ctx, url.Values{"work_id": {work}, "state": {"active"}})
			if err != nil || len(waits.Items) != 1 || waits.Items[0].Match.Types[0] != "operator.reply" || waits.Items[0].TimeoutTimerID == "" {
				t.Fatalf("declared wait: %+v %v", waits.Items, err)
			}
			// Logs and OS_OUTPUT_DIR files are execution artifacts.
			if len(x.Artifacts) != 5 {
				t.Fatalf("artifacts %+v", x.Artifacts)
			}
			if x.Outcome == nil || x.Outcome.Kind != "waiting" {
				t.Fatalf("first segment outcome %+v", x.Outcome)
			}
			data := shellData(t, x.Outcome)
			outputs := data["outputs"].(map[string]any)
			if log := h.artifact(data["log"].(string)); !strings.Contains(log, "waiting for reply") {
				t.Fatalf("log artifact %q", log)
			}
			var emitted client.Event
			if err = json.Unmarshal([]byte(h.artifact(outputs["emitted.json"].(string))), &emitted); err != nil {
				t.Fatal(err)
			}
			if emitted.Type != "fixture.reported.v1" || emitted.ExecutionID != x.ID || emitted.WorkID != work || emitted.CausationID != root.ID {
				t.Fatalf("emission authority/causation: %+v", emitted)
			}
			children, err := c.Works(ctx, url.Values{"parent_id": {work}})
			if err != nil || len(children.Items) != 1 || children.Items[0].Kind != "fixture.child" || children.Items[0].ScopeID != "ops" {
				t.Fatalf("child work: %+v %v", children.Items, err)
			}
			h.routeAll()
			wake, err := c.Wake(ctx, work, client.WakeRequest{ExpectedRevision: w.Revision, Type: "operator.reply", DeliveryID: "reply-1", CorrelationKey: &client.Key{Source: "ops", ExternalID: "thread-1"}}, "wake-1")
			if err != nil || !wake.Matched {
				t.Fatalf("wake: %+v %v", wake, err)
			}
			second, err := h.scheduler.Start(ctx, domain.ID(work))
			if err != nil {
				t.Fatal(err)
			}
			if resume != (second.Lease.ExecutionID == first.Lease.ExecutionID) {
				t.Fatalf("resume=%v: first %s second %s", resume, first.Lease.ExecutionID, second.Lease.ExecutionID)
			}
			if second.Request.Trigger.ID != domain.ID(wake.Event.ID) {
				t.Fatalf("trigger %s, want wake %s", second.Request.Trigger.ID, wake.Event.ID)
			}
			h.observe(second)
			final, err := c.Execution(ctx, string(second.Lease.ExecutionID))
			if err != nil {
				t.Fatal(err)
			}
			if final.State != "completed" || final.Outcome == nil || final.Outcome.Kind != "completed" || final.Outcome.Summary != "reply handled" {
				t.Fatalf("declared outcome: %+v %+v", final.State, final.Outcome)
			}
			if w, err = c.Work(ctx, work); err != nil || w.State != "completed" {
				t.Fatalf("work after outcome: %+v %v", w.State, err)
			}
			chain, err := c.EventChain(ctx, emitted.ID)
			if err != nil || len(chain) < 2 || chain[0].ID != emitted.ID || chain[len(chain)-1].ID != root.ID {
				t.Fatalf("causal chain: %+v %v", chain, err)
			}
		})
	}
}

// TestShellStructuredFailures drives exit failure, timeout and operator
// cancel with verified stop through the scheduler.
func TestShellStructuredFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("exit", func(t *testing.T) {
		state, bin := shellState(t, shellConfig(t, map[string]any{"command": "echo failing >&2; exit 4"}))
		h := newShellHost(t, state, bin, conformance.NewClock(), "daemon", false)
		root := h.submit("operator.requested", "root", "exit")
		a, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
		if err != nil {
			t.Fatal(err)
		}
		h.observe(a)
		x, _ := h.operator().Execution(ctx, string(a.Lease.ExecutionID))
		d := shellData(t, x.Outcome)
		if x.State != "failed" || x.Outcome.Summary != "exit status 4" || d["reason"] != "exit" || d["exit_code"] != float64(4) {
			t.Fatalf("exit failure: %s %+v %v", x.State, x.Outcome, d)
		}
		if !strings.Contains(h.artifact(d["log"].(string)), "failing") {
			t.Fatal("stderr missing from log artifact")
		}
		st, _ := h.scheduler.RetryStatus(ctx, domain.ID(root.RoutedWorkID))
		if st.FailureCount != 1 {
			t.Fatalf("retry policy not applied: %+v", st)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		state, bin := shellState(t, shellConfig(t, map[string]any{"command": "sleep 30", "timeout_seconds": 1}))
		h := newShellHost(t, state, bin, conformance.NewClock(), "daemon", false)
		root := h.submit("operator.requested", "root", "timeout")
		a, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
		if err != nil {
			t.Fatal(err)
		}
		h.observe(a)
		x, _ := h.operator().Execution(ctx, string(a.Lease.ExecutionID))
		if d := shellData(t, x.Outcome); x.State != "failed" || d["reason"] != "timeout" || d["timeout_seconds"] != float64(1) {
			t.Fatalf("timeout: %s %+v", x.State, x.Outcome)
		}
		if st, _ := h.scheduler.InspectAttempt(ctx, a.Lease.ExecutionID); !st.Excluded {
			t.Fatal("observed structured timeout did not establish exclusion")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		state, bin := shellState(t, shellConfig(t, map[string]any{"command": "sleep 30 & echo $! > \"$OS_OUTPUT_DIR/child\"; wait"}))
		h := newShellHost(t, state, bin, conformance.NewClock(), "daemon", false)
		root := h.submit("operator.requested", "root", "cancel")
		a, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
		if err != nil {
			t.Fatal(err)
		}
		observed := make(chan error, 1)
		go func() { observed <- h.scheduler.Observe(ctx, a) }()
		x, err := h.operator().Execution(ctx, string(a.Lease.ExecutionID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.operator().CancelExecution(ctx, x.ID, x.Revision, "stop", "cancel-1"); err != nil {
			t.Fatal(err)
		}
		if st, _ := h.scheduler.InspectAttempt(ctx, a.Lease.ExecutionID); st.Excluded {
			t.Fatal("cancel alone established exclusion")
		}
		// Trusted host cleanup verifies the tree stopped, then confirms.
		if err = h.executor.Stop(ctx, a.Lease.ExecutionID, a.Lease.Epoch); err != nil {
			t.Fatal(err)
		}
		if err = h.scheduler.ConfirmStopped(ctx, a.Lease.ExecutionID, a.Lease.Epoch); err != nil {
			t.Fatal(err)
		}
		if err = <-observed; err == nil {
			t.Fatal("fenced attempt accepted its late result")
		}
		x, _ = h.operator().Execution(ctx, x.ID)
		if x.State != "cancelled" {
			t.Fatalf("state %s", x.State)
		}
	})
}

// TestShellDetachedAdoptionAcrossRestart tears the host down while the shell
// process runs, restarts on the same state, and adopts it under a new epoch
// without relaunching. The adopted process uses the replaced credential.
func TestShellDetachedAdoptionAcrossRestart(t *testing.T) {
	ctx := context.Background()
	script := `echo launch >> "$HOME/launches"
echo $$ > "$HOME/pid"
while [ ! -e "$HOME/release" ]; do sleep 0.02; done
os emit --type fixture.reported.v1 --delivery-id after-restart --request-id after-restart > "$OS_OUTPUT_DIR/emitted.json"
echo adopted-finish`
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": script}))
	clock := conformance.NewClock()
	first := newShellHost(t, state, bin, clock, "daemon-1", false)
	root := first.submit("operator.requested", "root", "detached")
	a, err := first.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(state, "pid")
	deadline := time.Now().Add(10 * time.Second)
	for !fileHasLine(pidFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	oldToken := a.Request.Context.ToolbeltToken
	first.close() // daemon gone; the process keeps running in its own session
	pid, _ := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	if pid <= 1 || syscall.Kill(pid, 0) != nil {
		t.Fatal("shell process did not survive the host")
	}
	clock.Advance(conformance.RuntimeLease + time.Second)
	second := newShellHost(t, state, bin, clock, "daemon-2", false)
	recovered, err := second.scheduler.Recover(ctx, a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Lease.Epoch <= a.Lease.Epoch || recovered.Request.Context.ToolbeltToken == oldToken {
		t.Fatalf("adoption did not move to a new epoch/token: %d", recovered.Lease.Epoch)
	}
	old, _ := client.New(second.endpoint, oldToken)
	if _, err = old.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); err == nil {
		t.Fatal("pre-restart credential still authorized")
	}
	if err = os.WriteFile(filepath.Join(state, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second.observe(recovered)
	x, err := second.operator().Execution(ctx, string(a.Lease.ExecutionID))
	if err != nil {
		t.Fatal(err)
	}
	if x.State != "completed" || x.Attempt != 1 || x.Lease.Epoch != strconv.FormatUint(recovered.Lease.Epoch, 10) {
		t.Fatalf("adopted execution: %+v", x)
	}
	d := shellData(t, x.Outcome)
	if !strings.Contains(second.artifact(d["log"].(string)), "adopted-finish") {
		t.Fatal("log lost across adoption")
	}
	var emitted client.Event
	if err = json.Unmarshal([]byte(second.artifact(d["outputs"].(map[string]any)["emitted.json"].(string))), &emitted); err != nil || emitted.ExecutionID != x.ID {
		t.Fatalf("post-adoption emission with replaced credential: %v %+v", err, emitted)
	}
	if got := readFile(t, filepath.Join(state, "launches")); got != "launch\n" {
		t.Fatalf("duplicate launch: %q", got)
	}
	executions, err := second.operator().Executions(ctx, url.Values{"work_id": {root.RoutedWorkID}})
	if err != nil || len(executions.Items) != 1 {
		t.Fatalf("executions after adoption: %+v %v", executions.Items, err)
	}
}

// TestShellConfigRejectedAtApply proves executor-owned config validation runs
// at load/apply time with a safe diagnostic.
func TestShellConfigRejectedAtApply(t *testing.T) {
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": "true"}))
	h := newShellHost(t, state, bin, conformance.NewClock(), "daemon", false)
	bad := shellConfig(t, map[string]any{"command": "true", "env": map[string]string{"OS_TOKEN": "forged-secret-value"}})
	v, err := h.operator().ValidateConfig(context.Background(), json.RawMessage(bad))
	if err == nil && v.Valid {
		t.Fatal("reserved toolbelt variable accepted")
	}
	var api *client.Error
	if !errors.As(err, &api) || !strings.Contains(api.Error(), "invalid_config") || strings.Contains(api.Error(), "forged-secret-value") {
		t.Fatalf("diagnostic: %v", err)
	}
}

func fileHasLine(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.HasSuffix(string(b), "\n")
}
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestToolbeltCLIUsage checks exit codes and that arguments or credentials are
// never echoed.
func TestToolbeltCLIUsage(t *testing.T) {
	run := func(env map[string]string, args ...string) (int, string, string) {
		var out, errOut strings.Builder
		code := cli.RunToolbelt("v", "c", args, cli.Env{Getenv: func(k string) string { return env[k] }}, strings.NewReader(""), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run(nil, "--help"); code != 0 || !strings.Contains(out, "OS_ENDPOINT") {
		t.Fatalf("help: %d", code)
	}
	if code, out, _ := run(nil, "version"); code != 0 || out != "os v (c)\n" {
		t.Fatalf("version: %q", out)
	}
	for _, args := range [][]string{{"emit"}, {"emit", "--type", "x"}, {"emit", "--type", "t", "--delivery-id", "d", "--data", "secret-value{"}, {"wait"}, {"outcome", "waiting"}, {"work", "create"}, {"bogus", "secret-value"}, {"emit", "--key-source", "a", "--type", "t", "--delivery-id", "d"}} {
		code, _, errOut := run(nil, args...)
		if code != 2 || strings.Contains(errOut, "secret-value") {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
	// Well-formed but outside an execution: no endpoint/token.
	if code, _, errOut := run(nil, "context"); code != 2 || !strings.Contains(errOut, "OS_ENDPOINT") {
		t.Fatalf("missing env: %d %q", code, errOut)
	}
	// A rejected credential is a call failure with a safe code.
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": "true"}))
	h := newShellHost(t, state, bin, conformance.NewClock(), "daemon", false)
	code, _, errOut := run(map[string]string{"OS_ENDPOINT": h.endpoint, "OS_TOKEN": "forged-token-value"}, "context")
	if code != 1 || strings.Contains(errOut, "forged-token-value") {
		t.Fatalf("forged token: %d %q", code, errOut)
	}
}
