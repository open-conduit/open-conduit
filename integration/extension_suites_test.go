package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/examples/runner"
	"github.com/open-conduit/open-conduit/storage"
)

// The shared ingress, executor, toolbelt and KV suites run against the same
// example code in-process and as a subprocess behind internal/extproc
// (07-02): an external extension meets the same guarantees.

func notesIngressFixture(e *extEnv, in extension.Ingress) conformance.IngressFixture {
	source := extension.Source{Namespace: "notes/" + string(extensionScope), ScopeID: extensionScope}
	body, _ := json.Marshal(map[string]any{"note": notes.Note{ID: "a", Rev: "r1", Text: "hello world", At: extensionNoteTime}})
	deleted, _ := json.Marshal(map[string]any{"deleted": map[string]any{"id": "b", "at": extensionNoteTime}})
	reference := &notes.Ingress{Service: e.service, Release: 2}
	canonical, err := reference.Push(context.Background(), source, extension.Delivery{ID: "d1", ReceivedAt: extensionNoteTime, Body: body})
	if err != nil || len(canonical.Observations) != 1 {
		e.t.Fatalf("reference push: %v", err)
	}
	return conformance.IngressFixture{Ingress: in, Source: source, Delivery: extension.Delivery{ID: "d1", ReceivedAt: extensionNoteTime, Body: body}, Canonical: canonical.Observations[0], OneShotType: notes.Deleted, OneShot: &extension.Delivery{ID: "d2", ReceivedAt: extensionNoteTime, Body: deleted}}
}

func notesSource(t *testing.T, r *extproc.Remote) protocol.SourceDecl {
	t.Helper()
	srcs := r.Process().Configured().Sources
	if len(srcs) != 1 {
		t.Fatalf("configure registered %d sources", len(srcs))
	}
	return srcs[0]
}

func TestExternalIngressSharedSuite(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	t.Run("in_process", func(t *testing.T) {
		e := newExtEnv(t)
		conformance.RunIngress(t, func() conformance.IngressFixture {
			return notesIngressFixture(e, &notes.Ingress{Service: e.service, Release: 2})
		})
	})
	t.Run("subprocess", func(t *testing.T) {
		e := newExtEnv(t)
		r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), e.options(notes.Name))
		conformance.RunIngress(t, func() conformance.IngressFixture {
			return notesIngressFixture(e, r.Ingress(notesSource(t, r)))
		})
	})
}

func runnerRequest(id domain.ID, auto bool) extension.RunRequest {
	r := extension.RunRequest{Work: domain.Work{ID: "work"}, Execution: domain.Execution{ID: id, WorkID: "work", Lease: domain.Lease{Epoch: 1}}}
	if auto {
		r.Context.Config = domain.Payload{Schema: "runner.config.v1", Data: []byte(`{"auto":true}`)}
	}
	return r
}

func waitForFile(path string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timed out waiting for " + path)
}

func runnerInjectFixture(e extension.Executor, stateDir string) conformance.InjectFixture {
	req := runnerRequest("inject-execution", false)
	dir := runner.RunDir(stateDir, req.Execution.ID)
	return conformance.InjectFixture{Executor: e, Request: req,
		Ready:  func() error { return waitForFile(filepath.Join(dir, "ready")) },
		Finish: func() error { return os.WriteFile(filepath.Join(dir, "finish"), nil, 0o600) },
	}
}

func TestExternalExecutorSharedSuites(t *testing.T) {
	_, runnerPath := extensionBinaries(t)
	inProcess := func(t *testing.T) (extension.Executor, string) {
		dir := t.TempDir()
		return &runner.Executor{StateDir: func() string { return dir }, Job: []string{runnerPath, "job"}}, dir
	}
	subprocess := func(t *testing.T) (extension.Executor, string) {
		e := newExtEnv(t)
		o := e.options(runner.Name)
		r := e.load(e.install(runner.Manifest(), runnerPath), e.deps(), o)
		x, ok := r.Executor(runner.Name)
		if !ok {
			t.Fatal("runner executor not declared")
		}
		return x, o.StateDir
	}
	for name, make := range map[string]func(*testing.T) (extension.Executor, string){"in_process": inProcess, "subprocess": subprocess} {
		t.Run(name, func(t *testing.T) {
			t.Run("executor", func(t *testing.T) {
				x, _ := make(t)
				conformance.RunExecutor(t, func() extension.Executor { return x }, runnerRequest("execution", true))
			})
			t.Run("inject", func(t *testing.T) {
				x, dir := make(t)
				conformance.RunInjectExecutor(t, func() conformance.InjectFixture { return runnerInjectFixture(x, dir) })
			})
		})
	}
}

// newExternalToolbeltHarness is newToolbeltHarness with the fixture
// registration served by the example extension's subprocess.
func newExternalToolbeltHarness(t *testing.T, mode string, reg extension.Registration, ops []string) *toolbeltHarness {
	t.Helper()
	o := routingOptions(routingHandlerFunc(routingDecision), "queue")
	o.Capabilities = domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
	f, executor := schedulerFixture(t, mode, o)
	base := f.Driver.(*schedulerDriver)
	h := &toolbeltHarness{f: f, executor: executor, echoes: new(int), deny: new(bool), mu: &sync.Mutex{}}
	belt, err := runtime.NewToolbelt(f.Store, f.Clock.Now, base.Router, runtime.ToolbeltOptions{
		Grant: func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			return runtime.ToolbeltGrant{Operations: ops, EventTypes: []string{"fixture.changed.v1"}}, nil
		},
		Authorize: func(_ context.Context, c runtime.AuthorityCheck) error {
			if c.Binding != "fixture" || c.Generation != "fixture-generation" {
				return errors.New("denied")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = belt.Register(reg); err != nil {
		t.Fatal(err)
	}
	options := base.Options
	options.Toolbelt, options.ToolbeltEndpoints = belt, fixtureEndpoint("unix:///fixture/toolbelt.sock")
	options.AliasFilter = belt.DependencyFilter
	h.scheduler = newTestScheduler(t, f, options)
	h.belt = belt
	executor.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		h.mu.Lock()
		h.token = r.Context.ToolbeltToken
		h.mu.Unlock()
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		handle := &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
		executor.Mu.Lock()
		if executor.Handles == nil {
			executor.Handles = map[domain.ID]extension.RunHandle{}
		}
		executor.Handles[r.Execution.ID] = handle
		executor.Mu.Unlock()
		return handle, nil
	}
	key := toolbeltKey
	requireAccept(t, f, "trigger", &key)
	requireRoute(t, f.Driver, "trigger")
	if h.attempt, err = h.scheduler.Start(context.Background(), "work/item"); err != nil {
		t.Fatal(err)
	}
	return h
}

func fixtureRemote(t *testing.T) *extproc.Remote {
	t.Helper()
	notesPath, _ := extensionBinaries(t)
	e := newExtEnv(t)
	deps := e.deps()
	deps.Scopes = func() []domain.ID { return []domain.ID{"routing-scope"} }
	o := e.options("fixture")
	o.Configure = nil
	return e.load(e.install(notes.Manifest(notes.Options{Fixture: true}), notesPath), deps, o)
}

func TestExternalToolbeltSharedSuite(t *testing.T) {
	for _, mode := range []string{"persistent", "reopen"} {
		t.Run(mode, func(t *testing.T) {
			conformance.RunToolbelt(t, func() conformance.ToolbeltFixture {
				r := fixtureRemote(t)
				h := newExternalToolbeltHarness(t, mode, r.Registration(), allToolbeltOperations())
				return conformance.ToolbeltFixture{Belt: h.belt, Issuer: h.belt, Registrar: h.belt, Token: h.token, Authority: h.authority(), Advance: h.f.Clock.Advance, LoseLease: func() error {
					h.f.Clock.Advance(conformance.RuntimeLease + time.Second)
					_, err := h.scheduler.Recover(context.Background(), h.attempt.Lease.ExecutionID)
					return err
				}}
			})
		})
	}
}

func TestExternalExtensionKVSharedSuite(t *testing.T) {
	fixture := func(t *testing.T, wire bool) conformance.ExtensionKVFixture {
		e := newExtEnv(t)
		svc, err := extproc.NewService(notes.Manifest(notes.Options{Release: 2}), e.deps())
		if err != nil {
			t.Fatal(err)
		}
		var kv conformance.ExtensionKV = extproc.LocalKV{S: svc}
		if wire {
			kv = pipeHost(t, svc)
		}
		return conformance.ExtensionKVFixture{KV: kv, Name: notes.Name, Peek: func(ns, key string) ([]byte, error) {
			var v []byte
			err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
				var err error
				v, err = tx.Get(ns, key)
				return err
			})
			return v, err
		}}
	}
	t.Run("in_process", func(t *testing.T) {
		conformance.RunExtensionKV(t, func() conformance.ExtensionKVFixture { return fixture(t, false) })
	})
	t.Run("protocol", func(t *testing.T) {
		conformance.RunExtensionKV(t, func() conformance.ExtensionKVFixture { return fixture(t, true) })
	})
}

func notesActionFixture(p extension.ActionProvider) conformance.ActionFixture {
	scope := domain.Scope{ID: extensionScope}
	open := domain.Work{ID: "work-note", ScopeID: extensionScope, State: domain.WorkWaiting, CorrelationKeys: []domain.CorrelationKey{{Source: notes.Name, ExternalID: "a"}}}
	done := open
	done.State = domain.WorkCompleted
	empty := ""
	return conformance.ActionFixture{Provider: p, Available: extension.ActionTarget{Scope: scope, Work: &open}, Unavailable: extension.ActionTarget{Scope: scope, Work: &done},
		Action: "remark", Input: "looks right", BadInput: &empty, Caller: extension.LocalOperator, Stranger: extension.Caller{Kind: "slack_user", ID: "U1"}, EventType: notes.Annotated}
}

func TestExternalActionsSharedSuite(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	t.Run("in_process", func(t *testing.T) {
		conformance.RunActions(t, func() conformance.ActionFixture { return notesActionFixture(notes.Actions{}) })
	})
	t.Run("subprocess", func(t *testing.T) {
		e := newExtEnv(t)
		r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), e.options(notes.Name))
		p, ok := r.Actions()
		if !ok {
			t.Fatal("notes declares no actions")
		}
		conformance.RunActions(t, func() conformance.ActionFixture { return notesActionFixture(p) })
	})
}

func notesPanelFixture(t *testing.T, p extension.ActionProvider) conformance.PanelFixture {
	a := notesActionFixture(p)
	pp, ok := p.(interface {
		extension.ActionProvider
		extension.PanelProvider
	})
	if !ok {
		t.Fatal("notes shows no panels")
	}
	return conformance.PanelFixture{Provider: pp, Shown: a.Available, Hidden: a.Unavailable}
}

func TestExternalPanelsSharedSuite(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	t.Run("in_process", func(t *testing.T) {
		conformance.RunPanels(t, func() conformance.PanelFixture { return notesPanelFixture(t, notes.Actions{}) })
	})
	t.Run("subprocess", func(t *testing.T) {
		e := newExtEnv(t)
		r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), e.options(notes.Name))
		p, ok := r.Actions()
		if !ok {
			t.Fatal("notes declares no actions")
		}
		conformance.RunPanels(t, func() conformance.PanelFixture { return notesPanelFixture(t, p) })
	})
}

// TestExternalOperationActsAsBindingConnection: over the subprocess
// protocol, an operation reads the calling binding's connection from its
// authority and resolves that connection's secrets through secret.resolve;
// the host's refusal of any other connection reaches it, and an authority
// without a connection names none.
func TestExternalOperationActsAsBindingConnection(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	e := newExtEnv(t)
	deps := e.deps()
	deps.Scopes = func() []domain.ID { return []domain.ID{"routing-scope"} }
	var mu sync.Mutex
	var asked []string
	deps.Secrets = func(_ context.Context, scope domain.ID, connection string, names []string) (map[string]string, error) {
		mu.Lock()
		asked = append(asked, string(scope)+"/"+connection)
		mu.Unlock()
		if scope != "routing-scope" || connection != "acme-reviewer" {
			return nil, extension.ErrUnauthorized
		}
		out := map[string]string{}
		for _, n := range names {
			if n != "token" {
				return nil, extension.ErrUnauthorized
			}
			out[n] = "reviewer-token"
		}
		return out, nil
	}
	o := e.options("fixture")
	o.Configure = nil
	r := e.load(e.install(notes.Manifest(notes.Options{Fixture: true}), notesPath), deps, o)
	var op extension.Operation
	for _, x := range r.Registration().Operations {
		if x.Name == "fixture.connection" {
			op = x
		}
	}
	if op.Handler == nil {
		t.Fatal("fixture.connection is not registered")
	}
	call := func(connection string) (domain.Payload, error) {
		a := extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: "execution-1", ExpiresAt: e.clock.Now().Add(time.Minute), Connection: connection}
		return op.Handler(context.Background(), a, domain.Payload{Schema: "fixture.connection.input.v1", Data: []byte(`{"names":["token"]}`)})
	}
	out, err := call("acme-reviewer")
	if err != nil || string(out.Data) != `{"connection":"acme-reviewer","resolved":["token"]}` {
		t.Fatalf("acting as the binding's connection: %s %v", out.Data, err)
	}
	if _, err := call("acme-spare"); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("a connection the host refuses: %v", err)
	}
	out, err = call("")
	if err != nil || string(out.Data) != `{"connection":"","resolved":[]}` {
		t.Fatalf("a binding without a connection: %s %v", out.Data, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"routing-scope/acme-reviewer", "routing-scope/acme-spare"}; !slices.Equal(asked, want) {
		t.Fatalf("secret.resolve calls %v, want %v", asked, want)
	}
}
