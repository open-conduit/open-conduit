package inprocsdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
	"github.com/open-conduit/open-conduit/storage"
)

// acme is an in-process extension with a source per entry (a webhook for
// those that ask for one), one operation it registers only once started, a
// task and a check.
type acme struct {
	mu     sync.Mutex
	host   *inproc.Host
	starts int
	// failStarts is how many Starts fail before one succeeds.
	failStarts int
	entries    []protocol.ConfigEntry
	ran        []string
}

var (
	_ inproc.Starter    = (*acme)(nil)
	_ inproc.Configurer = (*acme)(nil)
	_ inproc.Ingester   = (*acme)(nil)
	_ inproc.Webhooker  = (*acme)(nil)
	_ inproc.Operator   = (*acme)(nil)
	_ inproc.Declarer   = (*acme)(nil)
	_ inproc.Tooled     = (*acme)(nil)
	_ inproc.Tasker     = (*acme)(nil)
	_ inproc.Checker    = (*acme)(nil)
	_ inproc.Typed      = (*acme)(nil)
)

func (*acme) Name() string { return "acme" }

func (x *acme) Types() []inproc.ConfigType {
	return []inproc.ConfigType{{Kind: protocol.ValidateExtension, Type: "acme", Ingress: true, Validate: func(c json.RawMessage, _ []string) error {
		if strings.Contains(string(c), "bad") {
			return extension.ErrInvalid
		}
		return nil
	}}}
}

func (x *acme) Start(_ context.Context, h inproc.Host) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.starts++
	if x.failStarts > 0 {
		x.failStarts--
		return errors.New("no credentials yet")
	}
	x.host = &h
	return nil
}

func (x *acme) Configure(_ context.Context, p protocol.ConfigureParams) {
	x.mu.Lock()
	x.entries = p.Entries
	x.mu.Unlock()
}

type ingress struct{}

func (ingress) Spec() extension.IngressSpec {
	return extension.IngressSpec{Families: []extension.EventFamily{{Type: "acme.thing", ReDerivable: true}}}
}
func (ingress) Push(context.Context, extension.Source, extension.Delivery) (extension.PushResult, error) {
	return extension.PushResult{}, nil
}
func (ingress) Reconcile(context.Context, extension.Scan) (extension.Snapshot, error) {
	return extension.Snapshot{Complete: true}, nil
}

func (x *acme) Sources() []inproc.Source {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []inproc.Source
	for _, e := range x.entries {
		out = append(out, inproc.Source{Name: "acme/" + e.ScopeID + "/" + e.Name, Source: extension.Source{Namespace: "acme/" + e.Name, ScopeID: domain.ID(e.ScopeID)}, Ingress: ingress{}, Interval: time.Minute})
	}
	return out
}

// Webhook serves an entry whose name ends -hook.
func (x *acme) Webhook(path string) http.Handler {
	if !strings.HasSuffix(path, "-hook") {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
}

func (*acme) Operations() []string { return []string{"acme.ping"} }

func (*acme) Declarations() []extension.Operation {
	return []extension.Operation{{Name: "acme.ping", Summary: "Answer pong.", Input: json.RawMessage(`{"type":"object"}`)}}
}

func (x *acme) Registration() extension.Registration {
	x.mu.Lock()
	started := x.host != nil
	x.mu.Unlock()
	if !started {
		return extension.Registration{}
	}
	return extension.Registration{Version: extension.ContractVersion, Name: "acme", Operations: []extension.Operation{{Name: "acme.ping",
		Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
			return domain.Payload{Schema: "acme.pong.v1", Data: json.RawMessage(`{"pong":true}`)}, nil
		}}}}
}

func (x *acme) Tasks() []inproc.Task {
	return []inproc.Task{{Name: "sweep_things", Pace: 30 * time.Second, Run: func(context.Context) error {
		x.mu.Lock()
		x.ran = append(x.ran, "sweep")
		x.mu.Unlock()
		return nil
	}}}
}

func (*acme) Checks() []inproc.Check {
	return []inproc.Check{{Run: func(context.Context, protocol.CheckConfig) inproc.CheckResult {
		return inproc.CheckResult{Name: "acme", Status: inproc.CheckWarn, Code: "half_set_up", Items: []string{"one entry"}}
	}}}
}

func base() protocol.Manifest {
	return protocol.Manifest{Version: "0.1.0", Command: []string{"acme-extension"}, Provides: protocol.Provides{
		Families:   []protocol.Family{{Type: "acme.thing", ReDerivable: true}},
		Schemas:    []protocol.SchemaDecl{{Name: "acme.pong", Versions: []int{1}}},
		Operations: []protocol.OperationDecl{{Name: "acme.ping", Kind: protocol.OperationHandler}},
		Checks:     []protocol.CheckDecl{{Name: "acme"}},
	}}
}

// The bridge needs from the base manifest what the surfaces can't say.
func TestNewRefusesAnIncompleteBase(t *testing.T) {
	b := base()
	b.Provides.Operations = nil
	if _, err := inprocsdk.New(&acme{}, b); err == nil || !strings.Contains(err.Error(), "acme.ping") {
		t.Fatalf("an operation without a kind: %v", err)
	}
	b = base()
	b.Provides.Checks = nil
	if _, err := inprocsdk.New(&acme{}, b); err == nil {
		t.Fatal("unnamed checks accepted")
	}
	b = base()
	b.Name = "other"
	if _, err := inprocsdk.New(&acme{}, b); err == nil {
		t.Fatal("a base manifest of another name accepted")
	}
}

// The manifest takes what the surfaces say, with tasks named after the
// package; the extension starts once, at the first configure; its sources
// and their webhook paths come back from configure; its operation, task,
// check and config validation are served.
func TestBridgeServesTheInprocContract(t *testing.T) {
	x := &acme{}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	m := b.Extension().Manifest
	if m.Name != "acme" || !m.Provides.Webhooks || len(m.Provides.Extensions) != 1 || !m.Provides.Extensions[0].Ingress {
		t.Fatalf("manifest %+v", m.Provides)
	}
	if len(m.Provides.Tasks) != 1 || m.Provides.Tasks[0].Name != "acme-sweep-things" || m.Provides.Tasks[0].IntervalSeconds != 30 {
		t.Fatalf("tasks %+v", m.Provides.Tasks)
	}
	ctx := context.Background()
	h := sdktest.New(t, b.Extension())

	// Before the first configure nothing has started, and the operation
	// isn't registered yet.
	if _, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("an operation answered before the extension started")
	}
	entries := []protocol.ConfigEntry{{ScopeID: "eng", Type: "acme", Name: "plain", Config: json.RawMessage(`{}`)}, {ScopeID: "eng", Type: "acme", Name: "with-hook", Config: json.RawMessage(`{}`)}}
	for range 2 {
		res, err := h.Configure(ctx, protocol.ConfigureParams{Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range res.Sources {
			names = append(names, s.Name)
			if len(s.Families) != 1 || s.Families[0].Type != "acme.thing" || s.IntervalSeconds != 60 {
				t.Fatalf("source %+v", s)
			}
		}
		if !slices.Equal(names, []string{"acme/eng/plain", "acme/eng/with-hook"}) || !slices.Equal(res.WebhookPaths, []string{"/acme/eng/with-hook"}) {
			t.Fatalf("sources %v webhook paths %v", names, res.WebhookPaths)
		}
	}
	x.mu.Lock()
	starts, host := x.starts, x.host
	x.mu.Unlock()
	if starts != 1 || host == nil || host.Store == nil || host.Secrets == nil || host.Deliver == nil || host.Authorize == nil || host.WaitTarget == nil {
		t.Fatalf("started %d times with host %+v", starts, host)
	}
	out, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)})
	if err != nil || out.Output.Schema != "acme.pong.v1" {
		t.Fatalf("operation: %+v %v", out, err)
	}
	if err := h.RunTask(ctx, "acme-sweep-things"); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	ran := slices.Clone(x.ran)
	x.mu.Unlock()
	if !slices.Equal(ran, []string{"sweep"}) {
		t.Fatalf("task ran %v", ran)
	}
	c, err := h.Check(ctx, "acme")
	if err != nil || c.OK || c.Status != inproc.CheckWarn || c.Code != "half_set_up" || !slices.Equal(c.Items, []string{"one entry"}) {
		t.Fatalf("check %+v %v", c, err)
	}
	var v protocol.ValidateResult
	if err := h.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExtension, Type: "acme", Config: json.RawMessage(`{"bad":true}`)}, &v); err == nil {
		t.Fatal("a bad entry validated")
	}
	if err := h.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExtension, Type: "acme", Config: json.RawMessage(`{}`)}, &v); err != nil {
		t.Fatalf("a good entry refused: %v", err)
	}
}

// A failed Start fails its configure and is tried again by the next one:
// the extension is never configured, registered or served unstarted.
func TestFailedStartIsRetried(t *testing.T) {
	x := &acme{failStarts: 1}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h := sdktest.New(t, b.Extension())
	entries := []protocol.ConfigEntry{{ScopeID: "eng", Type: "acme", Name: "plain", Config: json.RawMessage(`{}`)}}
	if _, err := h.Configure(ctx, protocol.ConfigureParams{Entries: entries}); err == nil {
		t.Fatal("a configure whose Start failed succeeded")
	}
	x.mu.Lock()
	configured := len(x.entries)
	x.mu.Unlock()
	if configured != 0 {
		t.Fatal("an unstarted extension was configured")
	}
	if _, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("an unstarted extension's operation answered")
	}
	res, err := h.Configure(ctx, protocol.ConfigureParams{Entries: entries})
	if err != nil || len(res.Sources) != 1 {
		t.Fatalf("the retried configure: %+v %v", res, err)
	}
	x.mu.Lock()
	starts := x.starts
	x.mu.Unlock()
	if starts != 2 {
		t.Fatalf("started %d times, want 2", starts)
	}
	if _, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("operation after the retried start: %v", err)
	}
}

// Start gets the directories the host named at initialize even when the
// first configure arrives before Attach records the server, as it can over
// a real stdin (#428): a relative state directory made the shell refuse to
// start, and the daemon refused its whole configuration.
func TestStartGetsTheHostsDirectoriesBeforeAttach(t *testing.T) {
	x := &acme{}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	state, exec := t.TempDir(), t.TempDir()
	h := sdktest.New(t, b.Extension(), sdktest.WithStateDir(state), sdktest.WithExecutorDirs(map[string]string{"acme-run": exec}))
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{}); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.host == nil || x.host.StateDir != state || x.host.ExecutorDir == nil || x.host.ExecutorDir("acme-run") != exec {
		t.Fatalf("Start got state %q and executor dir %q", x.host.StateDir, x.host.ExecutorDir("acme-run"))
	}
}

// In-transaction acceptance crosses the wire as kv.commit Emissions (#428):
// the root and derived Events an extension accepts in its transaction land
// with that transaction's writes, a foreign family is refused at once, and
// the services the extension declares nothing for stay nil.
func TestAcceptInTxIsCarried(t *testing.T) {
	x := &acme{}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parent := domain.Event{ID: "parent", Type: "acme.thing", Source: "acme/eng/plain", DeliveryID: "p1"}
	h := sdktest.New(t, b.Extension(), sdktest.WithEvents("eng", parent))
	if _, err := h.Configure(ctx, protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "eng", Type: "acme", Name: "plain", Config: json.RawMessage(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	host := *x.host
	x.mu.Unlock()
	if host.AcceptInTx == nil || host.AcceptDerivedInTx == nil || host.EventScope == nil {
		t.Fatal("in-transaction acceptance is not carried")
	}
	if host.Timers != nil || host.CancelWork != nil || host.Config != nil || host.Probes != nil {
		t.Fatal("a service the manifest declares nothing for is set")
	}
	var root, derived, named domain.AcceptedEvent
	err = host.Store.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		if _, _, err := host.AcceptInTx(tx, "eng", domain.Event{Type: "other.thing", Source: "acme", DeliveryID: "x"}); !errors.Is(err, inproc.ErrNotOwn) {
			t.Errorf("a foreign family: %v, want ErrNotOwn", err)
		}
		var err error
		if root, _, err = host.AcceptInTx(tx, "eng", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "r1"}); err != nil {
			return err
		}
		a, err := tx.Event("parent")
		if err != nil {
			return err
		}
		if scope, err := host.EventScope(tx, a); err != nil || scope != "eng" {
			t.Errorf("EventScope %q %v", scope, err)
		}
		if derived, _, err = host.AcceptDerivedInTx(tx, "eng", "parent", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "d1"}); err != nil {
			return err
		}
		// An Event naming its parent as its cause is accepted as in
		// process; one naming another cause is refused.
		if named, _, err = host.AcceptDerivedInTx(tx, "eng", "parent", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "d2", CausationID: "parent"}); err != nil {
			return err
		}
		if _, _, err := host.AcceptDerivedInTx(tx, "eng", "parent", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "d3", CausationID: "elsewhere"}); !errors.Is(err, storage.ErrInvalid) {
			t.Errorf("another cause: %v, want ErrInvalid", err)
		}
		return tx.Put("acme", "origin/"+string(root.Event.ID), []byte("r1"))
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[domain.ID]domain.AcceptedEvent{}
	for _, a := range h.Events() {
		byID[a.Event.ID] = a
	}
	if _, ok := byID[root.Event.ID]; !ok || root.Event.ID != domain.ID(protocol.EmittedEventID("acme", "r1")) {
		t.Fatalf("the root Event %q was not accepted under the host's ID", root.Event.ID)
	}
	if d, ok := byID[derived.Event.ID]; !ok || d.Event.CausationID != "parent" {
		t.Fatalf("the derived Event: %+v", d)
	}
	if d, ok := byID[named.Event.ID]; !ok || d.Event.CausationID != "parent" {
		t.Fatalf("the derived Event naming its parent: %+v", d)
	}
	if string(h.KV("acme")["origin/"+string(root.Event.ID)]) != "r1" {
		t.Fatal("the transaction's write, naming the Event, did not land with it")
	}

	// Accepting it again, in a later transaction or twice in one, answers
	// the Event as it stands with added false, as in process, so a caller's
	// dedupe holds; nothing is emitted again.
	before := len(h.Events())
	err = host.Store.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		again, added, err := host.AcceptInTx(tx, "eng", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "r1"})
		if err != nil {
			return err
		}
		if added || again.Event.ID != root.Event.ID || again.Sequence == 0 {
			t.Errorf("accepted again: %+v added %v, want the accepted Event, not added", again, added)
		}
		if _, added, err := host.AcceptInTx(tx, "eng", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "r2"}); err != nil || !added {
			t.Errorf("a new Event: added %v %v", added, err)
		}
		if _, added, err := host.AcceptInTx(tx, "eng", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "r2"}); err != nil || added {
			t.Errorf("the same new Event twice in one transaction: added %v %v", added, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(h.Events()); got != before+1 {
		t.Fatalf("%d Events accepted by the second transaction, want 1", got-before)
	}
}

// A Work's Waits are read through state.waits (#428): an extension's
// store sees its own scopes' Waits, seeded with sdktest.WithWaits.
func TestListWaitsIsCarried(t *testing.T) {
	x := &acme{}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	work := domain.Work{ID: "w1", Kind: "acme.thing", ScopeID: "eng", State: domain.WorkWaiting}
	wait := domain.Wait{ID: "wait-1", WorkID: "w1", State: domain.WaitActive, Description: "an answer", Match: domain.EventMatch{Types: []string{"acme.thing"}}}
	h := sdktest.New(t, b.Extension(), sdktest.WithWorks(work), sdktest.WithWaits(wait))
	if _, err := h.Configure(ctx, protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "eng", Type: "acme", Name: "plain", Config: json.RawMessage(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	host := *x.host
	x.mu.Unlock()
	var waits []domain.Wait
	if err := host.Store.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		var err error
		waits, err = tx.ListWaits("w1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0].ID != "wait-1" || waits[0].Description != "an answer" {
		t.Fatalf("waits %+v", waits)
	}
}

// LatestSequence is one state.sequence call over the wire: the latest
// Event's sequence (#428).
func TestLatestSequenceIsCarried(t *testing.T) {
	x := &acme{}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h := sdktest.New(t, b.Extension(), sdktest.WithEvents("eng", domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "1"}, domain.Event{Type: "acme.thing", Source: "acme", DeliveryID: "2"}))
	if _, err := h.Configure(ctx, protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "eng", Type: "acme", Name: "plain", Config: json.RawMessage(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	host := *x.host
	x.mu.Unlock()
	var seq uint64
	if err := host.Store.Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
		var err error
		seq, err = tx.LatestSequence()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("latest sequence %d, want 2", seq)
	}
}

// operated is an extension whose executor offers an operator's live view
// and container attach.
type operated struct{ live liveExecutor }

func (*operated) Name() string { return "operated" }
func (x *operated) Executors() map[string]inproc.Executor {
	return map[string]inproc.Executor{"operated": {Executor: &x.live}}
}

type liveExecutor struct {
	mu      sync.Mutex
	attach  extension.ContainerAttachRequest
	running bool
}

func (*liveExecutor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: "none"}
}
func (*liveExecutor) Run(context.Context, extension.RunRequest) (extension.RunHandle, error) {
	return nil, extension.ErrUnsupported
}
func (*liveExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrNotAdoptable
}
func (e *liveExecutor) Live(_ context.Context, execution domain.ID, tail int64) ([]extension.LiveOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running || execution != "exec-1" {
		return nil, extension.ErrNotLive
	}
	return []extension.LiveOutput{{Name: "activity", MediaType: "text/plain", Size: 11, Content: []byte("hello world")[11-tail:]}}, nil
}
func (e *liveExecutor) ContainerAttach(_ context.Context, r extension.ContainerAttachRequest) (extension.ContainerAttachSpec, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attach = r
	return extension.ContainerAttachSpec{Needs: extension.SandboxNeeds{Write: []string{r.Home}}, Launch: extension.SandboxLaunch{Argv: []string{"agent", "--resume"}, Dir: "/work", TTY: true}}, nil
}

// An executor's live output and container attach cross the wire (#428):
// the bridge declares both controls, a running execution's tail comes back
// and a finished one is ErrNotLive, and the attach request arrives whole.
func TestLiveAndContainerAttachAreCarried(t *testing.T) {
	x := &operated{}
	b, err := inprocsdk.New(x, protocol.Manifest{Version: "0.1.0", Command: []string{"operated"}})
	if err != nil {
		t.Fatal(err)
	}
	decl := b.Extension().Manifest.Provides.Executors
	if len(decl) != 1 || !decl[0].Controls.Live || !decl[0].Controls.ContainerAttach {
		t.Fatalf("executors %+v, want live and container_attach controls", decl)
	}
	h := sdktest.New(t, b.Extension())
	ctx := context.Background()
	var out protocol.LiveResult
	err = h.Call(ctx, "executor.live", protocol.ExecutorTargetParams{Executor: "operated", ExecutionID: "exec-1", Tail: 5}, &out)
	if !errors.Is(err, extension.ErrNotLive) {
		t.Fatalf("live of a finished execution: %v, want ErrNotLive", err)
	}
	x.live.mu.Lock()
	x.live.running = true
	x.live.mu.Unlock()
	if err := h.Call(ctx, "executor.live", protocol.ExecutorTargetParams{Executor: "operated", ExecutionID: "exec-1", Tail: 5}, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Outputs) != 1 || string(out.Outputs[0].Content) != "world" || out.Outputs[0].Size != 11 || out.Outputs[0].MediaType != "text/plain" {
		t.Fatalf("live %+v", out)
	}
	var spec protocol.ContainerAttachSpec
	p := protocol.ContainerAttachParams{Executor: "operated", Work: protocol.FromWork(domain.Work{ID: "work/1", ScopeID: "eng"}), Config: protocol.Payload{Schema: "operated.config.v1", Data: json.RawMessage(`{}`)}, Secrets: map[string]string{"TOKEN": "s"}, Home: "/homes/1"}
	if err := h.Call(ctx, "executor.container_attach", p, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.Launch.TTY || spec.Launch.Dir != "/work" || !slices.Equal(spec.Needs.Write, []string{"/homes/1"}) {
		t.Fatalf("attach spec %+v", spec)
	}
	x.live.mu.Lock()
	defer x.live.mu.Unlock()
	if x.live.attach.Work.ID != "work/1" || x.live.attach.Secrets["TOKEN"] != "s" || x.live.attach.Config.Schema != "operated.config.v1" {
		t.Fatalf("attach request %+v", x.live.attach)
	}
}
