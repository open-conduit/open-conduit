package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const managementConfig = `{
  "version": 1,
  "settings": {},
  "connections": [],
  "scopes": [{
    "id": "ops", "name": "Operations",
    "authorization": {"connections": [], "operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "read_scopes": []},
    "bindings": [{"name": "run", "executor": "fixture", "requires": {"toolbelt": "cli"}, "operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "secret_names": ["api"], "config": {}}],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}],
    "secret_refs": {"api": "cmd:printf super-secret-value", "other": "env:SECRET_FIXTURE_TOKEN"}
  }]
}`

type managementHarness struct {
	t          *testing.T
	state      string
	configPath string
	store      storage.Store
	clock      *conformance.Clock
	scheduler  *runtime.Scheduler
	router     *runtime.Router
	executor   *schedulerExecutor
	belt       *runtime.Toolbelt
	configs    *management.ConfigService
	artifacts  *artifacts.Store
	endpoint   string
	// endpoints serves each execution its own toolbelt socket and token
	// directory under state/runs, as the daemon does.
	endpoints *management.Endpoints
	server    *management.Server
	tokens    chan string
	secrets   chan map[string]string
	lock      *management.Lock
	actions   map[string]extension.ActionProvider
	// providerTimeout and viewDeadline, when set, time action provider
	// calls in place of the real timers.
	providerTimeout func(provider string) <-chan time.Time
	viewDeadline    func() <-chan time.Time
}

func newManagementHarness(t *testing.T) *managementHarness {
	t.Helper()
	return newManagementHarnessWith(t, managementConfig, nil)
}

// newManagementHarnessWith starts the harness on cfg, with registry
// additions made by register (nil for none).
func newManagementHarnessWith(t *testing.T, cfg string, register func(*config.Registry)) *managementHarness {
	t.Helper()
	ctx := context.Background()
	state, err := os.MkdirTemp("/tmp", "oc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	h := &managementHarness{t: t, state: state, clock: conformance.NewClock(), tokens: make(chan string, 8), secrets: make(chan map[string]string, 8)}
	h.lock, err = management.AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.lock.Close() })
	db, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.store = db
	if err = os.MkdirAll(filepath.Join(state, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if h.artifacts, err = artifacts.Open(filepath.Join(state, "artifacts")); err != nil {
		t.Fatal(err)
	}
	h.executor = &schedulerExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}
	h.executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		h.tokens <- r.Context.ToolbeltToken
		h.secrets <- r.Context.Secrets
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	h.configPath = filepath.Join(state, "config", "config.json")
	if err = config.SaveAtomic(h.configPath, []byte(cfg)); err != nil {
		t.Fatal(err)
	}
	registry := config.Registry{Executors: map[string]extension.Executor{"fixture": h.executor}, Handlers: map[string]bool{"manual": true}}
	if register != nil {
		register(&registry)
	}
	h.configs, err = management.OpenConfig(ctx, db, h.clock.Now, registry, h.configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	router, err := runtime.NewRouter(db, h.clock.Now, runtime.RouterOptions{Generation: "1", Handlers: map[string]extension.RouteHandler{"manual": routingHandlerFunc(routingDecision)}, Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: h.executor, Capabilities: h.executor.Capabilities()}}})
	if err != nil {
		t.Fatal(err)
	}
	h.router = router
	h.belt, err = runtime.NewToolbelt(db, h.clock.Now, router, runtime.ToolbeltOptions{Authorize: h.configs.Authorize, Grant: h.configs.Grant(func() []string { return h.belt.EventTypes() })})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "fixture", EventTypes: []string{"fixture.reported.v1"}}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(state, "management.sock")
	h.endpoint = "unix://" + socket
	h.openEndpoints(func(id domain.ID) http.Handler { return h.server.ExecutionHandler(id) })
	serve := func(ctx context.Context, id domain.ID, token string) (string, string, error) {
		return h.endpoints.Serve(ctx, id, token)
	}
	h.scheduler, err = runtime.NewScheduler(db, h.clock.Now, router, runtime.SchedulerOptions{Owner: "daemon", Generation: "1", Executors: map[string]extension.Executor{"fixture": h.executor}, Toolbelt: h.belt, ToolbeltEndpoint: h.endpoint, ToolbeltEndpoints: serve, Secrets: h.configs.Secrets(&config.Resolver{})})
	if err != nil {
		t.Fatal(err)
	}
	control, err := runtime.NewControl(db, h.clock.Now, router, h.scheduler, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	server, err := management.NewServer(management.Options{Store: db, Artifacts: h.artifacts, Now: h.clock.Now, Control: control, Toolbelt: h.belt, Config: h.configs, Version: "test", InstanceID: "instance-1",
		Actions: func(context.Context, domain.ID) map[string]extension.ActionProvider { return h.actions },
		ProviderTimeout: func(p string) <-chan time.Time {
			if h.providerTimeout != nil {
				return h.providerTimeout(p)
			}
			return nil
		},
		ViewDeadline: func() <-chan time.Time {
			if h.viewDeadline != nil {
				return h.viewDeadline()
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err = management.BootstrapOperator(ctx, db, h.clock.Now, filepath.Join(state, "private", "operator.token")); err != nil {
		t.Fatal(err)
	}
	listener, err := management.Listen(h.lock, socket)
	if err != nil {
		t.Fatal(err)
	}
	h.server = server
	srv := &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })
	return h
}

// openEndpoints (re)opens the per-execution endpoints, as a daemon start does.
func (h *managementHarness) openEndpoints(handler func(domain.ID) http.Handler) {
	h.t.Helper()
	e, err := management.NewEndpoints(h.lock, h.state, "", handler)
	if err != nil {
		h.t.Fatal(err)
	}
	h.endpoints = e
	h.t.Cleanup(func() { e.Shutdown(context.Background()) })
}

func (h *managementHarness) cli(args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	env := map[string]string{"OPEN_CONDUIT_DATA_DIR": h.state, "HOME": h.state}
	code := cli.RunOperator("test", "abc", args, cli.Env{Getenv: func(k string) string { return env[k] }, Cwd: h.state}, &out, &errOut)
	return out.String(), errOut.String(), code
}
func (h *managementHarness) mustCLI(v any, args ...string) {
	h.t.Helper()
	out, errOut, code := h.cli(args...)
	if code != 0 {
		h.t.Fatalf("%v: exit %d: %s", args, code, errOut)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(out), v); err != nil {
			h.t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
}
func (h *managementHarness) operatorClient() *client.Client {
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

func TestManagementSocketAndLock(t *testing.T) {
	h := newManagementHarness(t)
	info, err := os.Stat(filepath.Join(h.state, "management.sock"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions: %v %v", info.Mode(), err)
	}
	if _, err = management.AcquireLock(h.state); err == nil {
		t.Fatal("second instance acquired the state lock")
	}
	open, err := os.MkdirTemp("/tmp", "oc-open-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(open)
	if err = os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err = management.AcquireLock(open); err == nil {
		t.Fatal("world-readable state directory accepted")
	}
	if err = management.BootstrapOperator(context.Background(), h.store, h.clock.Now, filepath.Join(h.state, "private", "operator.token")); !errors.Is(err, management.ErrCredentialExists) {
		t.Fatalf("bootstrap replaced credential: %v", err)
	}
	tokenInfo, _ := os.Stat(filepath.Join(h.state, "private", "operator.token"))
	if tokenInfo.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", tokenInfo.Mode())
	}
}

// TestManagementOperatorWorkflow drives a causal chain and work control using
// only the CLI/client against a real Unix socket server.
func TestManagementOperatorWorkflow(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	var health client.Health
	h.mustCLI(&health, "health")
	if health.State != "ready" || health.ConfigGeneration != "1" {
		t.Fatalf("health: %+v", health)
	}
	// Manual trigger enters the event stream and routes to Work.
	var root client.Event
	h.mustCLI(&root, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "d1", "--schema", "fixture.v1", "--data", `{"n":1}`, "--key", "k1")
	if root.Source != "core.manual" || root.CausationID != "" || !root.Routed {
		t.Fatalf("manual event: %+v", root)
	}
	var dup client.Event
	h.mustCLI(&dup, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "d1", "--schema", "fixture.v1", "--data", `{"n":1}`, "--key", "k1")
	if dup.ID != root.ID || dup.Sequence != root.Sequence {
		t.Fatal("idempotent replay changed result")
	}
	if _, errOut, code := h.cli("events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "d2", "--key", "k1"); code != 1 || !strings.Contains(errOut, "idempotency_conflict") {
		t.Fatalf("changed request with reused key: %d %s", code, errOut)
	}
	workID := root.RoutedWorkID
	attempt, err := h.scheduler.Start(ctx, domain.ID(workID))
	if err != nil {
		t.Fatal(err)
	}
	token := <-h.tokens
	secrets := <-h.secrets
	if len(secrets) != 1 || secrets["api"] != "super-secret-value" {
		t.Fatalf("executor did not receive exactly its binding secrets: %d", len(secrets))
	}
	exec, err := client.New(h.endpoint, token)
	if err != nil {
		t.Fatal(err)
	}
	// Credential classes are separated in both directions.
	if _, err = exec.Health(ctx); !isStatus(err, 403) {
		t.Fatalf("execution token read management: %v", err)
	}
	if _, err = h.operatorClient().ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); !isStatus(err, 403) {
		t.Fatalf("operator masqueraded as execution: %v", err)
	}
	emitted, err := exec.ToolbeltCall(ctx, client.ToolCall{Operation: "emit", RequestID: "r1", Input: json.RawMessage(`{"type":"fixture.reported.v1","delivery_id":"report-1","data":{"schema":"fixture.v1","data":{"ok":true}}}`)})
	if err != nil || emitted.Event == nil {
		t.Fatalf("toolbelt emit: %v", err)
	}
	// The host routes accepted execution emissions like any other event.
	if err = h.router.Route(ctx, domain.ID(emitted.Event.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = exec.ToolbeltCall(ctx, client.ToolCall{Operation: "emit", Input: json.RawMessage(`{"type":"undeclared.v1","delivery_id":"x"}`)}); !isStatus(err, 403) {
		t.Fatalf("undeclared emit: %v", err)
	}
	if _, err = exec.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context", WorkID: "other"}); !isStatus(err, 403) {
		t.Fatalf("cross-work assertion: %v", err)
	}
	var chain []client.Event
	h.mustCLI(&chain, "events", "chain", emitted.Event.ID)
	if len(chain) != 2 || chain[0].ID != emitted.Event.ID || chain[1].ID != root.ID || chain[0].ExecutionID != string(attempt.Lease.ExecutionID) {
		t.Fatalf("causal chain: %+v", chain)
	}
	var events client.Page[client.Event]
	h.mustCLI(&events, "events", "list", "--work-id", workID)
	if len(events.Items) != 2 {
		t.Fatalf("events by work: %d", len(events.Items))
	}
	// Operator cancels the execution; the work is held until manual retry.
	var x client.Execution
	h.mustCLI(&x, "executions", "get", string(attempt.Lease.ExecutionID))
	if _, errOut, code := h.cli("executions", "cancel", x.ID, "--revision", "999", "--key", "c0"); code != 1 || !strings.Contains(errOut, "revision_conflict") {
		t.Fatalf("stale revision: %s", errOut)
	}
	var cancelled client.ControlResult
	h.mustCLI(&cancelled, "executions", "cancel", x.ID, "--revision", x.Revision, "--reason", "operator", "--key", "c1")
	var again client.ControlResult
	h.mustCLI(&again, "executions", "cancel", x.ID, "--revision", x.Revision, "--reason", "operator", "--key", "c1")
	if again != cancelled || cancelled.EventID == "" {
		t.Fatalf("response-loss replay differs: %+v %+v", cancelled, again)
	}
	if _, err = exec.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); !isStatus(err, 403) {
		t.Fatalf("cancelled execution kept authority: %v", err)
	}
	if _, err = h.scheduler.Start(ctx, domain.ID(workID)); err == nil {
		t.Fatal("operator-cancelled work scheduled without manual retry")
	}
	var w client.Work
	h.mustCLI(&w, "work", "get", workID)
	if w.State != "ready" {
		t.Fatalf("work after execution cancel: %s", w.State)
	}
	var retried client.ControlResult
	h.mustCLI(&retried, "work", "retry", workID, "--revision", w.Revision, "--key", "r1")
	// Physical stop evidence is trusted host input, never implied by cancel.
	if _, err = h.scheduler.Start(ctx, domain.ID(workID)); !errors.Is(err, runtime.ErrResourceHeld) {
		t.Fatalf("unconfirmed stop released resources: %v", err)
	}
	if err = h.scheduler.ConfirmStopped(ctx, attempt.Lease.ExecutionID, attempt.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	second, err := h.scheduler.Start(ctx, domain.ID(workID))
	if err != nil {
		t.Fatalf("manual retry did not release hold: %v", err)
	}
	found := false
	for _, in := range second.Request.Inputs {
		found = found || in.ID == domain.ID(retried.EventID)
	}
	if !found {
		t.Fatalf("retry event not in attempt inputs: %+v", second.Request.Inputs)
	}
	<-h.tokens
	<-h.secrets
	// Artifact/log access through the authenticated stream.
	writer, err := h.artifacts.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	writer.Write([]byte("hello log output"))
	ref, err := writer.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.scheduler.Complete(ctx, second.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "boom"}, Artifacts: []domain.ArtifactRef{ref}}); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.cli("artifacts", "cat", ref.ID, "--range", "bytes=6-8")
	if code != 0 || out != "log" {
		t.Fatalf("artifact range: %q %s", out, errOut)
	}
	// Work cancellation terminalizes through a control event.
	h.mustCLI(&w, "work", "get", workID)
	var workCancel client.ControlResult
	h.mustCLI(&workCancel, "work", "cancel", workID, "--revision", w.Revision, "--key", "wc")
	h.mustCLI(&w, "work", "get", workID)
	if w.State != "cancelled" {
		t.Fatalf("work cancel: %s", w.State)
	}
	var executions client.Page[client.Execution]
	h.mustCLI(&executions, "executions", "list", "--work-id", workID)
	if len(executions.Items) != 2 {
		t.Fatalf("executions: %d", len(executions.Items))
	}
	for _, args := range [][]string{{"executions", "list"}, {"events", "list"}, {"scopes", "list"}, {"work", "list"}, {"waits", "list"}} {
		out, _, _ := h.cli(args...)
		if strings.Contains(out, "super-secret") || strings.Contains(out, "printf") || strings.Contains(out, token) {
			t.Fatalf("%v leaked a secret, reference or token", args)
		}
	}
	// Plaintext secrets and tokens are absent from coordination storage.
	if err = h.store.Transact(ctx, storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
		for _, ns := range []string{"core.toolbelt.tokens", "core.scheduler.attempts", "core.management.idempotency", "core.management.auth"} {
			rows, e := tx.List(ns, "")
			if e != nil {
				return e
			}
			for _, row := range rows {
				if strings.Contains(string(row.Value), "super-secret") || strings.Contains(string(row.Value), token) {
					t.Fatalf("plaintext persisted in %s", ns)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func isStatus(err error, status int) bool {
	var api *client.Error
	return errors.As(err, &api) && api.Status == status
}

func TestManagementAuthRotationRevocationAndLease(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	old := h.operatorClient()
	if _, err := old.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := h.cli("auth", "rotate"); code != 0 {
		t.Fatal(errOut)
	}
	if _, err := old.Health(ctx); !isStatus(err, 401) {
		t.Fatalf("rotated token still valid: %v", err)
	}
	fresh := h.operatorClient()
	if _, err := fresh.Health(ctx); err != nil {
		t.Fatal(err)
	}
	bad, _ := client.New(h.endpoint, "not-a-token")
	if _, err := bad.Health(ctx); !isStatus(err, 401) {
		t.Fatalf("unknown token: %v", err)
	}
	// Execution token revocation by ID and lease expiry.
	var root client.Event
	h.mustCLI(&root, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "lease")
	attempt, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
	if err != nil {
		t.Fatal(err)
	}
	token := <-h.tokens
	<-h.secrets
	exec, _ := client.New(h.endpoint, token)
	if _, err = exec.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(2 * time.Minute)
	if _, err = exec.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); !isStatus(err, 403) {
		t.Fatalf("expired lease accepted: %v", err)
	}
	recovered, err := h.scheduler.Recover(ctx, attempt.Lease.ExecutionID)
	if err != nil && !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatal(err)
	}
	_ = recovered
	id, fresher, err := h.belt.IssueWithID(ctx, extension.Authority{ScopeID: "ops", WorkID: domain.ID(root.RoutedWorkID), ExecutionID: attempt.Lease.ExecutionID, Lease: domain.LeaseRef{}, ExpiresAt: h.clock.Now().Add(time.Minute)})
	if err == nil {
		t.Fatalf("token issued for stale lease: %s %s", id, fresher)
	}
	// Revoke a live execution token through the management API.
	var second client.Event
	h.mustCLI(&second, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "revoke")
	a2, err := h.scheduler.Start(ctx, domain.ID(second.RoutedWorkID))
	if err != nil {
		t.Fatal(err)
	}
	<-h.tokens
	<-h.secrets
	id, live, err := h.belt.IssueWithID(ctx, extension.Authority{ScopeID: "ops", WorkID: domain.ID(second.RoutedWorkID), ExecutionID: a2.Lease.ExecutionID, Lease: a2.Lease, ExpiresAt: h.clock.Now().Add(time.Minute), Operations: []string{"read_context"}})
	if err != nil {
		t.Fatal(err)
	}
	liveClient, _ := client.New(h.endpoint, live)
	if _, err = liveClient.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); err != nil {
		t.Fatal(err)
	}
	var revoked client.Revocation
	h.mustCLI(&revoked, "auth", "revoke", id, "--key", "rv")
	if _, err = liveClient.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); err == nil {
		t.Fatal("revoked execution token accepted")
	}
}

func TestManagementConfigReloadAndRedaction(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	write := func(name, body string) string {
		p := filepath.Join(h.state, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var validation client.ConfigValidation
	h.mustCLI(&validation, "config", "validate", write("same.json", managementConfig))
	var scope client.Scope
	h.mustCLI(&scope, "scopes", "get", "ops")
	for _, leaked := range []string{string(validation.Config), string(scope.Config), scope.SecretRefs["api"]} {
		for _, needle := range []string{"printf", "super-secret", "cmd:", "env:", "SECRET_FIXTURE"} {
			if strings.Contains(leaked, needle) {
				t.Fatalf("secret reference leaked: %s", needle)
			}
		}
	}
	// Invalid edit: diagnostics, last good generation stays active.
	invalid := strings.Replace(managementConfig, `"active_policy": "queue"`, `"active_policy": "inject"`, 1)
	_, errOut, code := h.cli("config", "apply", write("bad.json", invalid), "--generation", "1")
	if code != 1 || !strings.Contains(errOut, "unsupported_policy") || strings.Contains(errOut, "printf") {
		t.Fatalf("invalid config: %d %s", code, errOut)
	}
	if _, gen := h.configs.Current(); gen != 1 {
		t.Fatalf("generation moved on invalid apply: %d", gen)
	}
	narrowed := strings.Replace(managementConfig, `"operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "secret_names"`, `"operations": ["emit", "declare_outcome", "create_work", "wait"], "secret_names"`, 1)
	if narrowed == managementConfig {
		t.Fatal("fixture replace failed")
	}
	// A live token loses a revoked operation immediately after reload.
	var root client.Event
	h.mustCLI(&root, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "reload")
	if _, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID)); err != nil {
		t.Fatal(err)
	}
	exec, _ := client.New(h.endpoint, <-h.tokens)
	<-h.secrets
	if _, err := exec.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code = h.cli("config", "apply", write("stale.json", narrowed), "--generation", "0"); code != 1 || !strings.Contains(errOut, "generation_conflict") {
		t.Fatalf("stale generation: %s", errOut)
	}
	var applied client.ConfigApplied
	h.mustCLI(&applied, "config", "apply", write("narrow.json", narrowed), "--generation", "1", "--key", "apply-1")
	if applied.Generation != "2" {
		t.Fatalf("generation %s", applied.Generation)
	}
	if _, err := exec.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); !isStatus(err, 403) {
		t.Fatalf("narrowed operation still authorized: %v", err)
	}
	// config apply sends the file's text, which is saved as written.
	saved, _ := os.ReadFile(h.configPath)
	backup, _ := os.ReadFile(h.configPath + ".last-good")
	if string(saved) != narrowed || string(backup) != managementConfig {
		t.Fatalf("atomic save/last-good backup mismatch:\n%s\n---\n%s", saved, backup)
	}
	// Restart: a disk edit alone never changes the committed generation.
	if err := config.SaveAtomic(h.configPath, []byte(invalid)); err != nil {
		t.Fatal(err)
	}
	registry := config.Registry{Executors: map[string]extension.Executor{"fixture": h.executor}, Handlers: map[string]bool{"manual": true}}
	reopened, err := management.OpenConfig(ctx, h.store, h.clock.Now, registry, h.configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap, gen := reopened.Current(); gen != 2 || string(snap.Normalized) == "" {
		t.Fatalf("restart generation %d", gen)
	}
	if err = reopened.Authorize(ctx, runtime.AuthorityCheck{ScopeID: "ops", Binding: "run", Operation: "read_context"}); err == nil {
		t.Fatal("restart restored a revoked grant")
	}
}

// A stored generation that an upgrade stopped validating (it names an
// operation no longer registered) is replaced by a valid config file at start,
// and the error names the operation while the file is still stale.
func TestManagementAdoptsFileOverInvalidStoredGeneration(t *testing.T) {
	ctx := context.Background()
	clock := conformance.NewClock()
	state := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	path := filepath.Join(state, "config.json")
	withOld := strings.Replace(managementConfig, `"operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "read_scopes"`, `"operations": ["emit", "read_context", "declare_outcome", "create_work", "wait", "ext.removed"], "read_scopes"`, 1)
	if withOld == managementConfig {
		t.Fatal("fixture replace failed")
	}
	if err = config.SaveAtomic(path, []byte(withOld)); err != nil {
		t.Fatal(err)
	}
	executors := map[string]extension.Executor{"fixture": &schedulerExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}}
	older := config.Registry{Executors: executors, Handlers: map[string]bool{"manual": true}, Operations: []string{"ext.removed"}}
	if _, err = management.OpenConfig(ctx, db, clock.Now, older, path, nil); err != nil {
		t.Fatal(err)
	}
	upgraded := config.Registry{Executors: executors, Handlers: map[string]bool{"manual": true}}
	if _, err = management.OpenConfig(ctx, db, clock.Now, upgraded, path, nil); err == nil || !strings.Contains(err.Error(), "ext.removed") || !strings.Contains(err.Error(), "stored generation 1") {
		t.Fatalf("stale file: %v", err)
	}
	if err = config.SaveAtomic(path, []byte(managementConfig)); err != nil {
		t.Fatal(err)
	}
	adopted, err := management.OpenConfig(ctx, db, clock.Now, upgraded, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, gen := adopted.Current(); gen != 2 || adopted.Adopted == nil || adopted.Adopted.Stored != 1 {
		t.Fatalf("adopted generation %d %+v", gen, adopted.Adopted)
	}
	reopened, err := management.OpenConfig(ctx, db, clock.Now, upgraded, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, gen := reopened.Current(); gen != 2 || reopened.Adopted != nil {
		t.Fatalf("restart after adoption: generation %d %+v", gen, reopened.Adopted)
	}
}

func TestManagementPaginationAndRequestValidation(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	c := h.operatorClient()
	for _, d := range []string{"p1", "p2", "p3", "p4", "p5"} {
		if _, err := c.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: d}, "key-"+d); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		q := map[string][]string{"limit": {"2"}, "type": {"operator.requested"}}
		if cursor != "" {
			q["cursor"] = []string{cursor}
		}
		page, err := c.Events(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range page.Items {
			if seen[e.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[e.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		// A new item after traversal start is outside the captured bound.
		if pages == 1 {
			if _, err = c.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: "late"}, "key-late"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("pagination saw %d items over %d pages", len(seen), pages)
	}
	if _, err := c.Events(ctx, map[string][]string{"cursor": {cursor + "x"}}); !isStatus(err, 400) {
		t.Fatalf("tampered cursor: %v", err)
	}
	if _, err := c.Events(ctx, map[string][]string{"bogus": {"1"}}); !isStatus(err, 400) {
		t.Fatalf("unknown filter: %v", err)
	}
	work, err := c.Works(ctx, map[string][]string{"limit": {"2"}})
	if err != nil || len(work.Items) != 2 || work.NextCursor == "" {
		t.Fatalf("work pagination: %v", err)
	}
	if _, err = c.Works(ctx, map[string][]string{"cursor": {work.NextCursor}, "state": {"ready"}}); !isStatus(err, 400) {
		t.Fatalf("cursor reused with different filters: %v", err)
	}
	// Strict request validation.
	raw := func(body, key, contentType string) int {
		f, _ := client.ReadTokenFile(filepath.Join(h.state, "private", "operator.token"))
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", strings.TrimPrefix(h.endpoint, "unix://"))
		}}
		req, _ := http.NewRequest("POST", "http://localhost/v1/events", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.Token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	valid := `{"scope_id":"ops","type":"operator.requested","delivery_id":"v"}`
	if s := raw(valid, "", "application/json"); s != 400 {
		t.Fatalf("missing idempotency key: %d", s)
	}
	if s := raw(valid, "k", "text/plain"); s != 415 {
		t.Fatalf("media type: %d", s)
	}
	if s := raw(`{"scope_id":"ops","scope_id":"ops","type":"operator.requested","delivery_id":"v"}`, "k2", "application/json"); s != 400 {
		t.Fatalf("duplicate key: %d", s)
	}
	if s := raw(`{"scope_id":"ops","type":"operator.requested","delivery_id":"v","extra":1}`, "k3", "application/json"); s != 400 {
		t.Fatalf("unknown field: %d", s)
	}
	if s := raw(`{"scope_id":"ops","type":"x","delivery_id":"`+strings.Repeat("a", 1<<20)+`"}`, "k4", "application/json"); s != 413 {
		t.Fatalf("body limit: %d", s)
	}
	if s := raw(`{"scope_id":"ops","type":"core.forged","delivery_id":"v"}`, "k5", "application/json"); s != 422 {
		t.Fatalf("forged core family: %d", s)
	}
}

// TestManagementDeclaredWaitAndOperatorWake parks via the HTTP toolbelt and
// wakes through the operator wake endpoint, which only accepts ordinary events.
func TestManagementDeclaredWaitAndOperatorWake(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	c := h.operatorClient()
	key := &client.Key{Source: "ops", ExternalID: "thread-1"}
	root, err := c.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: "thread", CorrelationKey: key}, "wake-root")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
	if err != nil {
		t.Fatal(err)
	}
	exec, _ := client.New(h.endpoint, <-h.tokens)
	<-h.secrets
	declared, err := exec.ToolbeltCall(ctx, client.ToolCall{Operation: "wait", Input: json.RawMessage(`{"match":{"keys":[{"source":"ops","external_id":"thread-1"}],"types":["operator.reply"]},"timeout_seconds":3600}`)})
	if err != nil || declared.Wait == nil || declared.Wait.TimeoutTimerID == "" {
		t.Fatalf("declared wait: %v", err)
	}
	if _, err = exec.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"}); !isStatus(err, 403) {
		t.Fatalf("authority survived wait: %v", err)
	}
	if err = h.scheduler.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	var waits client.Page[client.Wait]
	h.mustCLI(&waits, "waits", "list", "--work-id", root.RoutedWorkID, "--state", "active")
	if len(waits.Items) != 1 || waits.Items[0].ID != declared.Wait.ID {
		t.Fatalf("active waits: %+v", waits.Items)
	}
	var w client.Work
	h.mustCLI(&w, "work", "get", root.RoutedWorkID)
	if w.State != "waiting" {
		t.Fatalf("work state %s", w.State)
	}
	var miss client.WakeResult
	h.mustCLI(&miss, "work", "wake", w.ID, "--revision", w.Revision, "--type", "operator.note", "--delivery-id", "n1", "--key-source", "ops", "--key-external-id", "thread-1", "--key", "wk1")
	if miss.Matched {
		t.Fatal("nonmatching event woke the wait")
	}
	h.mustCLI(&w, "work", "get", root.RoutedWorkID)
	var hit client.WakeResult
	h.mustCLI(&hit, "work", "wake", w.ID, "--revision", w.Revision, "--type", "operator.reply", "--delivery-id", "r1", "--key-source", "ops", "--key-external-id", "thread-1", "--key", "wk2")
	if !hit.Matched {
		t.Fatalf("matching event did not wake: %+v", hit)
	}
	var woken client.Wait
	h.mustCLI(&woken, "waits", "get", declared.Wait.ID)
	if woken.State != "woken" || woken.WakeEventID != hit.Event.ID {
		t.Fatalf("wait after wake: %+v", woken)
	}
	if _, err = h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID)); err != nil {
		t.Fatalf("woken work did not continue: %v", err)
	}
}

// An included folder's scopes join the generation; deleting the folder
// leaves a configuration that still applies, with those scopes kept but
// disabled, and putting it back enables them again.
func TestManagementIncludedFolderSurvivesDeletion(t *testing.T) {
	ctx := context.Background()
	clock := conformance.NewClock()
	state := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	folder := filepath.Join(state, "web")
	fragment := `{"version": 1, "scopes": [{"id": "web", "name": "Web",
		"bindings": [{"name": "run", "executor": "fixture", "requires": {"toolbelt": "cli"}, "operations": ["emit"], "secret_names": ["api"], "config": {}}],
		"routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}]}]}`
	writeFragment := func() {
		if err := os.MkdirAll(filepath.Join(folder, ".open-conduit"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, config.FragmentFile), []byte(fragment), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFragment()
	path := filepath.Join(state, "config.json")
	data := []byte(strings.Replace(managementConfig, `"connections": [],`, `"connections": [], "includes": [{"path": "web", "parent": "ops"}],`, 1))
	if err = config.SaveAtomic(path, data); err != nil {
		t.Fatal(err)
	}
	registry := config.Registry{Executors: map[string]extension.Executor{"fixture": &schedulerExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}}, Handlers: map[string]bool{"manual": true}}
	configs, err := management.OpenConfig(ctx, db, clock.Now, registry, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !configs.Enabled("web") || configs.DiskStatus() != "current" {
		t.Fatalf("included scope not active: %s", configs.DiskStatus())
	}
	apply := func(want bool) {
		t.Helper()
		snapshot, err := configs.Validate(data)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		_, gen := configs.Current()
		if _, _, err = configs.Apply(ctx, data, snapshot.Hash, gen); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if configs.Enabled("web") != want || !configs.Enabled("ops") {
			t.Fatalf("web enabled %v, want %v", configs.Enabled("web"), want)
		}
	}
	if err = os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	if configs.DiskStatus() != "pending_edit" {
		t.Fatalf("deleted folder status %s", configs.DiskStatus())
	}
	apply(false)
	reopened, err := management.OpenConfig(ctx, db, clock.Now, registry, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap, _ := reopened.Current(); reopened.Enabled("web") || snap.Scopes["web"] == nil {
		t.Fatal("restart lost the disabled scope or enabled it")
	}
	writeFragment()
	apply(true)
}

// TestManagementGrantsBindingGrantsTheWorkTook: an attempt's toolbelt grant
// is the binding's operations plus those of the grants its Work took, never
// another grant's, and every granted operation passes the current ceiling.
func TestManagementGrantsBindingGrantsTheWorkTook(t *testing.T) {
	cfg := strings.Replace(managementConfig, `"operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "secret_names": ["api"], "config": {}`,
		`"operations": ["read_context", "declare_outcome"], "secret_names": ["api"], "config": {}, "grants": [{"name": "children", "operations": ["create_work", "wait"]}, {"name": "events", "operations": ["emit"]}]`, 1)
	h := newManagementHarnessWith(t, cfg, nil)
	grant := h.configs.Grant(nil)
	for _, c := range []struct {
		grants []string
		want   []string
	}{
		{nil, []string{"read_context", "declare_outcome"}},
		{[]string{"children"}, []string{"read_context", "declare_outcome", "create_work", "wait"}},
		{[]string{"children", "events"}, []string{"read_context", "declare_outcome", "create_work", "wait", "emit"}},
	} {
		g, err := grant(context.Background(), runtime.AttemptState{ScopeID: "ops", Generation: "1", Binding: domain.Binding{Name: "run"}, Grants: c.grants})
		if err != nil || !slices.Equal(g.Operations, c.want) {
			t.Errorf("grants %v: %v (%v), want %v", c.grants, g.Operations, err, c.want)
		}
	}
	if err := h.configs.Authorize(context.Background(), runtime.AuthorityCheck{ScopeID: "ops", Binding: "run", Operation: "emit"}); err != nil {
		t.Fatalf("a grant's operation fails the ceiling: %v", err)
	}
}
