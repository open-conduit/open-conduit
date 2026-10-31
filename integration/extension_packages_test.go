package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
	shellext "github.com/open-conduit/open-conduit/packages/shell"
	slackext "github.com/open-conduit/open-conduit/packages/slack"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// 07-03 round trips through the subprocess boundary with the first-party
// workloads themselves (Slack, shell), and the surfaces 07-03 adapted
// (webhook.handle, provider.*, telemetry.export, work.annotate, setup.*,
// validate) through the example package.

// TestExternalSlackRoundTrip runs the first-party Slack extension as a
// subprocess: a Socket Mode envelope is delivered through ingress.deliver
// and acknowledged only after durable acceptance, the remote route starts a
// conversation, the slack.reply tool binds the Work through state reads and
// posts through the projector (KV over sdk.Store), and the projection task
// runs over the protocol.
func TestExternalSlackRoundTrip(t *testing.T) {
	bins := packageBinaries(t)
	const team, channel = "T0TEAM", "C0SUPPORT"
	fake := slacktest.New(team, "xoxb-test")
	t.Cleanup(fake.Close)
	fake.AddAppToken("xapp-test")
	fake.SetClock(time.Now().UTC().Truncate(time.Second).Add(-time.Minute))
	fake.Join(channel, "UBOB")

	e := newExtEnv(t)
	entry := json.RawMessage(`{"namespace":"slack/` + team + `","scope":"support","team":"` + team + `","channels":["` + channel + `"],"bot_user_id":"UBOT"}`)
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{{"name": "helpdesk", "type": "slack", "connection": "slack", "config": entry, "secret_names": []string{}}}})
	if err := e.store.Transact(context.Background(), storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "support", Name: "support", Bindings: []domain.Binding{{Name: "agent", Executor: "fixture"}},
			Routes: []domain.Route{{Name: "threads", Handler: "slack", Binding: "agent", ActivePolicy: "queue"}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	ingress, err := runtime.NewIngress(e.store, time.Now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deps := e.deps()
	deps.Now = time.Now
	deps.Scopes = func() []domain.ID { return []domain.ID{"support"} }
	deps.Secrets = func(_ context.Context, scope domain.ID, connection string, names []string) (map[string]string, error) {
		values := map[string]string{sl.SecretBotToken: "xoxb-test", sl.SecretAppToken: "xapp-test"}
		out := map[string]string{}
		for _, n := range names {
			if scope != "support" || connection != "slack" || values[n] == "" {
				return nil, extension.ErrUnauthorized
			}
			out[n] = values[n]
		}
		return out, nil
	}
	deps.Deliver = func(ctx context.Context, name string, d extension.Delivery) ([]domain.AcceptedEvent, error) {
		return ingress.Push(ctx, name, d)
	}
	o := e.options(slackext.Name)
	o.Configure = func() protocol.ConfigureParams {
		return protocol.ConfigureParams{Generation: "1", Entries: []protocol.ConfigEntry{{ScopeID: "support", Type: "slack", Name: "helpdesk", Config: entry,
			Connection: &protocol.ConnectionEntry{ID: "slack", Type: "slack", Config: json.RawMessage(`{"api_url":"` + fake.URL + `"}`), Secrets: []string{sl.SecretAppToken, sl.SecretBotToken}}}}}
	}
	r := e.load(e.install(slackext.Manifest(), bins["slack-extension"]), deps, o)
	srcs := r.Process().Configured().Sources
	if len(srcs) != 1 || srcs[0].Name != "slack/support/helpdesk" || srcs[0].Source.Namespace != "slack/"+team {
		t.Fatalf("configure registered %+v", srcs)
	}
	if err := ingress.Register(context.Background(), srcs[0].Name, srcs[0].Source.Extension(), r.Ingress(srcs[0]), 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "socket connected", func() bool { return fake.SocketsOpen() == 1 })

	root := fake.Post(channel, "UALICE", "the export is broken")
	envelope := fake.SocketSend(fake.MessageEvent(channel, root))
	waitFor(t, "envelope acknowledged after acceptance", func() bool { return fake.Acked(envelope) })
	var accepted domain.AcceptedEvent
	if err := e.store.Transact(context.Background(), storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		events, err := tx.Events(0, 0)
		for _, a := range events {
			if a.Event.Type == sl.TypeMessageHuman {
				accepted = a
			}
		}
		return err
	}); err != nil || accepted.Event.ID == "" {
		t.Fatalf("socket delivery not accepted: %v", err)
	}

	router, err := runtime.NewRouter(e.store, time.Now, runtime.RouterOptions{Generation: "1", Handlers: map[string]extension.RouteHandler{"slack": r.RouteHandler("slack")}, Bindings: map[string]runtime.RouteBinding{"fixture": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Route(context.Background(), accepted.Event.ID); err != nil {
		t.Fatal(err)
	}
	var work domain.Work
	if err := e.store.Transact(context.Background(), storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		for _, w := range works {
			if w.Kind == sl.WorkKind {
				work = w
			}
		}
		return err
	}); err != nil || work.ID == "" {
		t.Fatalf("the remote route started no conversation: %v", err)
	}

	// The package offers trace samples through route.samples, written as of
	// the host's clock rather than its own.
	hostNow := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	var scope domain.Scope
	if err := e.store.Transact(context.Background(), storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		var err error
		scope, err = tx.Scope("support")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sampler, ok := r.RouteHandler("slack").(extension.RouteSampler)
	if !ok {
		t.Fatal("a handler declared with samples is not a sampler")
	}
	samples := sampler.Samples(context.Background(), extension.SampleInput{Scope: scope, Route: scope.Routes[0], Now: hostNow})
	if len(samples) == 0 || samples[0].Event.Type != sl.TypeMessageHuman {
		t.Fatalf("samples through the subprocess: %+v", samples)
	}
	var mention sl.MessagePayload
	if err := json.Unmarshal(samples[0].Event.Data.Data, &mention); err != nil || mention.TS != sl.TS(hostNow) {
		t.Fatalf("the sample is not written by the host's clock: %+v %v", mention, err)
	}
	if listed, err := router.Samples(context.Background(), "support"); err != nil || len(listed) != len(samples) || listed[0].Handler != "slack" {
		t.Fatalf("router samples: %+v %v", listed, err)
	}

	var reply extension.Operation
	for _, op := range r.Registration().Operations {
		if op.Name == sl.OpReply {
			reply = op
		}
	}
	ctx := extension.WithCallRequestID(context.Background(), "reply-1")
	out, err := reply.Handler(ctx, extension.Authority{ScopeID: "support", WorkID: work.ID, ExecutionID: "exec-1"}, domain.Payload{Schema: "slack.tool.v1", Data: json.RawMessage(`{"text":"looking into it"}`)})
	if err != nil || !strings.Contains(string(out.Data), `"posted"`) {
		t.Fatalf("slack.reply through the subprocess: %s %v", out.Data, err)
	}
	replies := fake.AgentReplies(channel, root)
	if len(replies) != 1 || replies[0].Text != "looking into it" {
		t.Fatalf("agent replies in the fake: %+v", replies)
	}
	// A retried call (same request ID) never posts twice.
	if _, err := reply.Handler(ctx, extension.Authority{ScopeID: "support", WorkID: work.ID, ExecutionID: "exec-1"}, domain.Payload{Schema: "slack.tool.v1", Data: json.RawMessage(`{"text":"looking into it"}`)}); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.AgentReplies(channel, root)); n != 1 {
		t.Fatalf("retried reply posted %d times", n)
	}
	// Another scope's Work is refused (state reads are scoped).
	if _, err := reply.Handler(ctx, extension.Authority{ScopeID: "other-scope", WorkID: work.ID, ExecutionID: "exec-1"}, domain.Payload{Data: json.RawMessage(`{"text":"x"}`)}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("foreign authority: %v", err)
	}
	if err := r.Task(context.Background(), slackext.ProjectTask); err != nil {
		t.Fatalf("projection task: %v", err)
	}
	if res, err := r.Check(context.Background(), "slack", false); err != nil || !res.OK || !strings.Contains(res.Detail, "receiver socket") {
		t.Fatalf("check: %+v %v", res, err)
	}
	if text, ok := r.Render(context.Background(), accepted.Event); !ok || !strings.Contains(text, "the export is broken") {
		t.Fatalf("render: %q %v", text, ok)
	}
	if n := r.Service().Refused(); n != 0 {
		t.Fatalf("%d unauthorized host calls", n)
	}
}

// TestExternalShellAdoptionAndArtifacts runs the first-party shell executor
// as a subprocess: its process group outlives a crash of the extension, the
// host re-adopts the run through executor.recover, and the combined log is
// written to the host's artifact store through artifact.*.
func TestExternalShellAdoptionAndArtifacts(t *testing.T) {
	bins := packageBinaries(t)
	e := newExtEnv(t)
	arts, err := artifacts.Open(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = arts.Close() })
	deps := e.deps()
	deps.Artifacts = arts
	f := e.install(shellext.Manifest(), bins["shell-extension"])
	r := e.load(f, deps, e.options(shellext.Name))
	x, ok := r.Executor(shellext.Name)
	if !ok {
		t.Fatal("shell executor not declared")
	}
	gate := filepath.Join(t.TempDir(), "go")
	cfg, _ := json.Marshal(map[string]string{"command": "while [ ! -e '" + gate + "' ]; do sleep 0.02; done; printf adopted"})
	var mu sync.Mutex
	var persisted []domain.Payload
	req := extension.RunRequest{Work: domain.Work{ID: "work", ScopeID: extensionScope}, Execution: domain.Execution{ID: "shell-exec", WorkID: "work", Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Context: extension.ExecutionContext{Config: domain.Payload{Schema: "shell.config.v1", Data: cfg}},
		PersistRecovery: func(p domain.Payload) error {
			mu.Lock()
			persisted = append(persisted, p)
			mu.Unlock()
			return nil
		}}
	h, err := x.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	crash(t, r)
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.Wait(ctx)
	if err != nil {
		t.Fatalf("wait across the extension crash: %v", err)
	}
	if res.Outcome.Kind != domain.OutcomeCompleted || len(res.Artifacts) == 0 {
		t.Fatalf("result: %+v", res)
	}
	mu.Lock()
	n := len(persisted)
	same := n >= 2 && bytes.Equal(persisted[0].Data, persisted[n-1].Data)
	mu.Unlock()
	if !same {
		t.Fatalf("re-adoption must re-acknowledge the same recovery metadata: %d persisted", n)
	}
	rc, err := arts.Open(context.Background(), res.Artifacts[0])
	if err != nil {
		t.Fatal(err)
	}
	log, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(log), "adopted") {
		t.Fatalf("log artifact: %q", log)
	}
	if err := x.(extension.ForgetExecutor).Forget(context.Background(), "shell-exec"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	// The executor's binding config is validated by the package.
	if err := x.(interface {
		ValidateConfig(json.RawMessage, []string) error
	}).ValidateConfig(json.RawMessage(`{"command":"a","argv":["b"]}`), nil); err == nil {
		t.Fatal("invalid shell config accepted")
	}
}

type setupHostFake struct {
	printed []string
	answer  string
}

func (h *setupHostFake) Printf(format string, args ...any) { h.printed = append(h.printed, format) }
func (h *setupHostFake) Prompt(string, extension.Prompt) (string, error) {
	return h.answer, nil
}
func (h *setupHostFake) Choose(string, []string) (int, error)                 { return 0, extension.ErrSetupNoInput }
func (h *setupHostFake) OpenURL(string) error                                 { return nil }
func (h *setupHostFake) StoreSecret(name, _ string) (string, error)           { return "keyring:" + name, nil }
func (h *setupHostFake) LookPath(string) (string, bool)                       { return "", false }
func (h *setupHostFake) Run(context.Context, string, string, ...string) error { return nil }
func (h *setupHostFake) Config() []byte                                       { return nil }
func (h *setupHostFake) Cwd() string                                          { return "/work" }
func (h *setupHostFake) StateDir() string                                     { return "/state" }
func (h *setupHostFake) DryRun() bool                                         { return false }

// TestExternalSurfacesRoundTrip exercises the surfaces 07-03 adapted
// through the example package's process.
func TestExternalSurfacesRoundTrip(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	e := newExtEnv(t)
	var delivered []extension.Delivery
	var mu sync.Mutex
	deps := e.deps()
	deps.Deliver = func(_ context.Context, name string, d extension.Delivery) ([]domain.AcceptedEvent, error) {
		if name != notes.Name+"/"+string(extensionScope) {
			return nil, errors.New("wrong source")
		}
		mu.Lock()
		delivered = append(delivered, d)
		mu.Unlock()
		return []domain.AcceptedEvent{{Event: domain.Event{ID: "accepted"}}}, nil
	}
	o := e.options(notes.Name)
	r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), deps, o)
	ctx := context.Background()

	t.Run("validate", func(t *testing.T) {
		v := r.Validator(protocol.ValidateExtension, "notes")
		if err := v(json.RawMessage(`{}`), nil); !errors.Is(err, extproc.ErrInvalidConfig) {
			t.Fatalf("an entry without a service file: %v", err)
		}
		if err := v(json.RawMessage(`{"service":"/x.json"}`), nil); err != nil {
			t.Fatalf("a valid entry: %v", err)
		}
	})
	t.Run("webhook", func(t *testing.T) {
		paths := r.Process().Configured().WebhookPaths
		if len(paths) != 1 || paths[0] != "/notes/"+string(extensionScope) {
			t.Fatalf("webhook paths %v", paths)
		}
		h := r.Webhook(paths[0], 0)
		body, _ := json.Marshal(map[string]any{"note": notes.Note{ID: "c", Rev: "r1", Text: "pushed", At: extensionNoteTime}})
		for _, c := range []struct {
			token string
			want  int
		}{{"wrong", http.StatusUnauthorized}, {notes.WebhookToken, http.StatusAccepted}} {
			req := httptest.NewRequest(http.MethodPost, paths[0], bytes.NewReader(body))
			req.Header.Set("X-Notes-Token", c.token)
			req.Header.Set("X-Notes-Delivery", "hook-1")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != c.want {
				t.Fatalf("token %q: status %d, want %d (%s)", c.token, w.Code, c.want, w.Body)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 1 || delivered[0].ID != "hook-1" || !bytes.Equal(delivered[0].Body, body) {
			t.Fatalf("delivered %+v", delivered)
		}
	})
	t.Run("provider", func(t *testing.T) {
		p, ok := r.Provider(notes.Scratch)
		if !ok {
			t.Fatal("provider not declared")
		}
		owner := extension.ResourceOwner{ScopeID: extensionScope, WorkID: "work-1", ExecutionID: "exec-1"}
		var log bytes.Buffer
		res, err := p.Acquire(ctx, extension.ResourceAcquisition{Owner: owner, Request: extension.ResourceRequest{Mode: extension.ResourceWork, Log: &log}})
		if err != nil || res.Dir == "" || !strings.Contains(log.String(), "scratch") {
			t.Fatalf("acquire: %+v %v %q", res, err, log.String())
		}
		if _, err := os.Stat(res.Dir); err != nil {
			t.Fatal(err)
		}
		again, err := p.Acquire(ctx, extension.ResourceAcquisition{Owner: owner, Request: extension.ResourceRequest{Mode: extension.ResourceWork}})
		if err != nil || again.Ref != res.Ref || again.Dir != res.Dir {
			t.Fatalf("acquire is idempotent per owner: %+v %v", again, err)
		}
		if got, err := p.Lookup(ctx, owner, res.Ref); err != nil || got.Dir != res.Dir {
			t.Fatalf("lookup: %+v %v", got, err)
		}
		active, err := p.Reconcile(ctx, func(context.Context, extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
			return extension.ResourceOwnerStatus{Known: true}, nil
		})
		if err != nil || active.Active != 1 || active.Released != 0 {
			t.Fatalf("reconcile while running: %+v %v", active, err)
		}
		done, err := p.Reconcile(ctx, func(_ context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
			if o != owner {
				return extension.ResourceOwnerStatus{}, errors.New("wrong owner")
			}
			return extension.ResourceOwnerStatus{Known: true, WorkDone: true, ExecutionDone: true}, nil
		})
		if err != nil || done.Released != 1 {
			t.Fatalf("reconcile after the owner finished: %+v %v", done, err)
		}
		if _, err := os.Stat(res.Dir); !os.IsNotExist(err) {
			t.Fatalf("released scratch directory still exists: %v", err)
		}
		// owner_status is answered only while a reconcile runs.
		host := pipeHost(t, r.Service())
		if err := host.Call(ctx, "provider.owner_status", protocol.OwnerStatusParams{Reconcile: "stale", Owner: protocol.FromOwner(owner)}, nil); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("owner_status outside a reconcile: %v", err)
		}
	})
	t.Run("telemetry", func(t *testing.T) {
		x, ok := r.Exporter()
		if !ok {
			t.Fatal("exporter not declared")
		}
		if err := x.Export(ctx, []observability.Record{{Name: "execution.completed", At: time.Now(), Correlation: observability.Correlation{WorkID: "work-9", ScopeID: extensionScope, EventID: "e", TraceID: "t"}}}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(o.StateDir, "telemetry.log"))
		if err != nil || !strings.Contains(string(raw), "execution.completed work-9") {
			t.Fatalf("exported records: %q %v", raw, err)
		}
	})
	t.Run("annotate", func(t *testing.T) {
		got := r.Annotate(ctx, domain.Work{ID: "work-a", ScopeID: extensionScope, CorrelationKeys: []domain.CorrelationKey{{Source: notes.Name, ExternalID: "a"}}})
		if len(got) != 1 || got[0].Name != "note" {
			t.Fatalf("annotations: %+v", got)
		}
		// A Work's display reaches an annotating extension only for Work
		// in the extension's scopes.
		shown := &domain.WorkDisplay{Title: "Note a"}
		got = r.Annotate(ctx, domain.Work{ID: "work-a", ScopeID: extensionScope, Display: shown, CorrelationKeys: []domain.CorrelationKey{{Source: notes.Name, ExternalID: "a"}}})
		if len(got) != 2 || got[1].Name != "shown" || got[1].Value != "Note a" {
			t.Fatalf("annotations in scope: %+v", got)
		}
		got = r.Annotate(ctx, domain.Work{ID: "work-b", ScopeID: "another-scope", Display: shown, CorrelationKeys: []domain.CorrelationKey{{Source: notes.Name, ExternalID: "a"}}})
		if len(got) != 1 || got[0].Name != "note" {
			t.Fatalf("another scope's display reached the extension: %+v", got)
		}
	})
	t.Run("run_environment", func(t *testing.T) {
		env, err := r.RunEnvironment(ctx, extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: extensionScope, WorkID: "work-e", ExecutionID: "work-e/1"}, Connection: "notes-service"})
		if err != nil || env["NOTES_WORK"] != "work-e" || env["NOTES_CONNECTION"] != "notes-service" {
			t.Fatalf("run environment: %v %v", env, err)
		}
	})
	t.Run("setup", func(t *testing.T) {
		s, ok := r.Setup()
		if !ok || s.SetupInfo().Type != notes.Name {
			t.Fatal("setup not declared")
		}
		host := &setupHostFake{answer: "service.json"}
		res, err := s.Setup(ctx, host, nil)
		if err != nil || !strings.Contains(string(res.Scope), "/work/service.json") || len(host.printed) == 0 {
			t.Fatalf("setup: %s %v %v", res.Scope, err, host.printed)
		}
		// setup.* callbacks quoting no running setup are refused.
		hc := pipeHost(t, r.Service())
		if err := hc.Call(ctx, "setup.print", protocol.SetupPrintParams{Run: "stale", Text: "x"}, nil); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("setup callback outside a run: %v", err)
		}
	})
	if n := r.Service().Refused(); n != 0 {
		t.Fatalf("%d unauthorized host calls", n)
	}
}

// TestSDKStoreOverProtocol: sdk.Store is storage.Store over the host: KV
// writes commit atomically with preconditions on what was read, core
// records are read-only, and state reads are scoped.
func TestSDKStoreOverProtocol(t *testing.T) {
	e := newExtEnv(t)
	svc, err := extproc.NewService(notes.Manifest(notes.Options{Release: 2}), e.deps())
	if err != nil {
		t.Fatal(err)
	}
	host := pipeHost(t, svc)
	store := sdk.NewStore(host)
	ctx := context.Background()
	g := storage.Guard{Now: time.Now()}
	if err := store.Transact(ctx, g, func(tx storage.Tx) error {
		if _, err := tx.Get(notes.IndexNS, "k"); !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err := tx.Put(notes.IndexNS, "k", []byte("1")); err != nil {
			return err
		}
		v, err := tx.Get(notes.IndexNS, "k") // read-your-writes
		if err != nil || string(v) != "1" {
			return errors.New("own write not visible")
		}
		return tx.Put(notes.IndexNS, "j", []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	// A concurrent change to a key the transaction read is a conflict and
	// applies nothing.
	err = store.Transact(ctx, g, func(tx storage.Tx) error {
		if _, err := tx.Get(notes.IndexNS, "k"); err != nil {
			return err
		}
		if err := host.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{{Namespace: notes.IndexNS, Key: "k", Value: []byte("other")}}}); err != nil {
			return err
		}
		return tx.Put(notes.IndexNS, "j", []byte("3"))
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("concurrent change: %v", err)
	}
	if v, _, _ := host.Get(ctx, notes.IndexNS, "j"); string(v) != "2" {
		t.Fatalf("a conflicting batch applied a write: %q", v)
	}
	if err := store.Transact(ctx, g, func(tx storage.Tx) error {
		rows, err := tx.List(notes.IndexNS, "")
		if err != nil || len(rows) != 2 {
			return errors.New("list")
		}
		if err := tx.PutWork(domain.Work{ID: "w"}, 0); !errors.Is(err, sdk.ErrReadOnly) {
			return errors.New("core write allowed")
		}
		if _, err := tx.Scope("other-scope"); !errors.Is(err, storage.ErrNotFound) {
			return errors.New("an unconfigured scope is visible")
		}
		scopes, err := tx.ListScopes()
		if err != nil || len(scopes) != 1 || scopes[0].ID != extensionScope {
			return errors.New("scoped list")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A foreign namespace is refused at commit.
	if err := store.Transact(ctx, g, func(tx storage.Tx) error { return tx.Put("slack.projection.x", "k", []byte("x")) }); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("foreign namespace: %v", err)
	}
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWaitProgressOverProtocol: wait.progress is enabled by a wait
// operation, reaches the host as the extension's own manifest name, and the
// host's refusal of another's Wait crosses as unauthorized.
func TestWaitProgressOverProtocol(t *testing.T) {
	e := newExtEnv(t)
	type call struct {
		owner    string
		work     domain.ID
		progress string
	}
	var calls []call
	deps := e.deps()
	deps.WaitProgress = func(_ context.Context, owner string, work domain.ID, progress string) error {
		calls = append(calls, call{owner, work, progress})
		if work == "work/other" {
			return extension.ErrUnauthorized
		}
		return nil
	}
	ctx := context.Background()
	// Only an extension with a wait operation can own a Wait.
	plain, err := extproc.NewService(notes.Manifest(notes.Options{Release: 2}), deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeHost(t, plain).WaitProgress(ctx, "work/example", "1 of 2 example checks finished"); !errors.Is(err, extension.ErrUndeclared) {
		t.Fatalf("without a wait operation: %v", err)
	}
	m := notes.Manifest(notes.Options{Fixture: true})
	svc, err := extproc.NewService(m, deps)
	if err != nil {
		t.Fatal(err)
	}
	host := pipeHost(t, svc)
	if err := host.WaitProgress(ctx, "work/example", "1 of 2 example checks finished"); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitProgress(ctx, "work/other", "1 of 2 example checks finished"); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("another's wait: %v", err)
	}
	want := []call{{m.Name, "work/example", "1 of 2 example checks finished"}, {m.Name, "work/other", "1 of 2 example checks finished"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %+v, want %+v", calls, want)
	}
}

// TestPermissionsOverProtocol runs the example package, which declares
// every permission, through the methods they gate: it records telemetry
// events and runs the host's probes from a task, and receives the host's
// telemetry events as a sink. A manifest without the permissions is refused
// each call, and the host checks what a permitted call asks.
func TestPermissionsOverProtocol(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	e := newExtEnv(t)
	type captured struct{ scope, name string }
	var mu sync.Mutex
	var captures []captured
	deps := e.deps()
	deps.Capture = func(_ context.Context, scope, name string, at time.Time, props map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		if at.IsZero() || props["release"] != 2.0 {
			return errors.New("an event without its time or properties")
		}
		captures = append(captures, captured{scope, name})
		return nil
	}
	deps.Probes = func() map[string]extension.SetupProbe {
		return map[string]extension.SetupProbe{
			notes.ProbeName: {Name: "echo", Summary: "says its argument back", Args: []extension.ProbeArg{{Name: "say", Required: true}},
				Run: func(_ context.Context, args map[string]string) (extension.ProbeResult, error) {
					return extension.ProbeResult{Command: "echo " + args["say"], Output: map[string]string{"said": args["say"]}}, nil
				}},
			"host.broken": {Name: "broken", Summary: "fails", Run: func(context.Context, map[string]string) (extension.ProbeResult, error) {
				return extension.ProbeResult{Command: "false"}, errors.New("gh is not signed in")
			}},
		}
	}
	o := e.options(notes.Name)
	r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), deps, o)
	ctx := context.Background()

	t.Run("capture_and_probes", func(t *testing.T) {
		if err := r.Task(ctx, notes.TelemetryTask); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := append([]captured(nil), captures...)
		mu.Unlock()
		if want := []captured{{string(extensionScope), notes.PassEvent}, {"", notes.PassEvent}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("captured %+v, want %+v", got, want)
		}
		raw, err := os.ReadFile(filepath.Join(o.StateDir, "probes.json"))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Probes []protocol.ProbeInfo    `json:"probes"`
			Result protocol.ProbeRunResult `json:"result"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Probes) != 2 || out.Probes[1].Name != notes.ProbeName || !out.Probes[1].Args[0].Required {
			t.Fatalf("probes %+v", out.Probes)
		}
		if out.Result.Command != "echo hello" || string(out.Result.Output) != `{"said":"hello"}` || out.Result.Error != "" {
			t.Fatalf("probe result %+v", out.Result)
		}
	})
	t.Run("sink", func(t *testing.T) {
		if !r.Sinks() {
			t.Fatal("the package declares telemetry.sink")
		}
		if err := r.Capture(ctx, "any-scope", "agent_turn", time.Now(), map[string]any{"outcome": "completed"}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(o.StateDir, "sink.log"))
		if err != nil || string(raw) != "any-scope agent_turn\n" {
			t.Fatalf("sink log %q %v", raw, err)
		}
	})
	t.Run("checked", func(t *testing.T) {
		host := pipeHost(t, r.Service())
		if err := host.Capture(ctx, "other-scope", notes.PassEvent, time.Time{}, nil); !errors.Is(err, extension.ErrUnauthorized) {
			t.Fatalf("capture for a scope that does not use the package: %v", err)
		}
		if err := host.Capture(ctx, "", notes.PassEvent, time.Time{}, map[string]any{"scope": "x"}); !errors.Is(err, extension.ErrInvalid) {
			t.Fatalf("a scope among the properties: %v", err)
		}
		if _, err := host.Probe(ctx, notes.ProbeName, nil); !errors.Is(err, extension.ErrInvalid) {
			t.Fatalf("a probe without its required argument: %v", err)
		}
		if _, err := host.Probe(ctx, notes.ProbeName, map[string]string{"say": "x", "cmd": "rm"}); !errors.Is(err, extension.ErrInvalid) {
			t.Fatalf("a probe with an undeclared argument: %v", err)
		}
		if _, err := host.Probe(ctx, "host.missing", nil); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("an unknown probe: %v", err)
		}
		res, err := host.Probe(ctx, "host.broken", nil)
		if err != nil || res.Error != "gh is not signed in" || res.Command != "false" {
			t.Fatalf("a failing probe answers its reason: %+v %v", res, err)
		}
	})
	t.Run("undeclared", func(t *testing.T) {
		m := notes.Manifest(notes.Options{Fixture: true})
		if len(m.Permissions) != 0 {
			t.Fatal("the fixture profile declares no permissions")
		}
		svc, err := extproc.NewService(m, deps)
		if err != nil {
			t.Fatal(err)
		}
		host := pipeHost(t, svc)
		if err := host.Capture(ctx, "", "fixture_event", time.Time{}, nil); !errors.Is(err, extension.ErrUndeclared) {
			t.Fatalf("capture without telemetry.capture: %v", err)
		}
		if _, err := host.Probes(ctx); !errors.Is(err, extension.ErrUndeclared) {
			t.Fatalf("probe.list without probes: %v", err)
		}
		if _, err := host.Probe(ctx, notes.ProbeName, map[string]string{"say": "x"}); !errors.Is(err, extension.ErrUndeclared) {
			t.Fatalf("probe.run without probes: %v", err)
		}
	})
}
