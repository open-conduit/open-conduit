package bootstrap

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const workerFamily = "fixture.changed.v1"

// heldIngress is a provider whose page the test holds: Reconcile says it
// was entered, then waits for release before answering with its page.
type heldIngress struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	page    extension.Snapshot
}

func (h *heldIngress) Spec() extension.IngressSpec {
	return extension.IngressSpec{Families: []extension.EventFamily{{Type: workerFamily, ReDerivable: true}}}
}
func (h *heldIngress) Push(context.Context, extension.Source, extension.Delivery) (extension.PushResult, error) {
	return extension.PushResult{}, nil
}
func (h *heldIngress) Reconcile(ctx context.Context, _ extension.Scan) (extension.Snapshot, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	if h.entered != nil {
		h.entered <- struct{}{}
	}
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return extension.Snapshot{}, ctx.Err()
		}
	}
	return h.page, nil
}
func (h *heldIngress) count() int { h.mu.Lock(); defer h.mu.Unlock(); return h.calls }

// TestSlowIngressSourceHoldsUpNothing (#701): a provider whose page hangs
// holds up neither the pass nor another source. The pass hands each due
// source to a worker and returns; the other source's page is accepted and
// nudges the pass to route it while the slow page is still out; and a pass
// meanwhile doesn't start a second scan of the source still being read.
// Run serially, as ReconcileDue does, the first pass would never return.
func TestSlowIngressSourceHoldsUpNothing(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return at }
	if err = store.Transact(ctx, storage.Guard{Now: at}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	ingress, err := runtime.NewIngress(store, now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	slow := &heldIngress{entered: make(chan struct{}, 1), release: make(chan struct{}), page: extension.Snapshot{ScanID: "slow", Complete: true}}
	fastSource := extension.Source{Namespace: "fixture/fast", ScopeID: "eng"}
	fast := &heldIngress{page: extension.Snapshot{ScanID: "fast", Complete: true, Observations: []extension.Observation{{ID: "item", Version: "v1", Event: domain.Event{
		ID: "fixture/fast:item:v1", Source: fastSource.Namespace, Type: workerFamily, DeliveryID: "item/v1", OccurredAt: at, ReceivedAt: at,
		CorrelationKey: &domain.CorrelationKey{Source: fastSource.Namespace, ExternalID: "item"}}}}}}
	// The slow source sorts first, so a serial pass reads it first.
	if err = ingress.Register(ctx, "a/slow", extension.Source{Namespace: "fixture/slow", ScopeID: "eng"}, slow, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = ingress.Register(ctx, "b/fast", fastSource, fast, time.Minute); err != nil {
		t.Fatal(err)
	}
	d := &daemon{coord: store, now: now, log: slog.New(slog.DiscardHandler), ingress: ingress, ctx: ctx, nudge: make(chan struct{}, 1),
		reported: map[string]string{}, reportedAt: map[string]time.Time{}}

	d.reconcileIngress(ctx)
	<-slow.entered
	// The fast source's page is accepted and nudges the pass while the
	// slow page is still out.
	<-d.nudge
	var events []domain.AcceptedEvent
	if err = store.Transact(ctx, storage.Guard{Now: at}, func(tx storage.Tx) error {
		events, err = tx.Events(0, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event.Source != fastSource.Namespace {
		t.Fatalf("accepted %+v while the slow page was out, want the fast source's one Event", events)
	}
	// Another pass leaves the slow source to the worker still reading it.
	d.reconcileIngress(ctx)
	close(slow.release)
	d.wg.Wait()
	if n := slow.count(); n != 1 {
		t.Fatalf("slow source read %d times, want once", n)
	}
	if n := fast.count(); n != 1 {
		t.Fatalf("fast source read %d times, want once", n)
	}
}

// TestHungInjectHoldsUpOnlyItsOwnWork (#701): an executor whose inject for
// one Work hangs holds up neither the pass nor another Work's controls.
// The pass hands each running Work's intents to a worker of its own and
// returns; the other Work's inject is made while the first is still out; and a pass meanwhile doesn't dispatch the first Work's intent again.
func TestHungInjectHoldsUpOnlyItsOwnWork(t *testing.T) {
	ctx := context.Background()
	o := runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "inject")
	o.Capabilities = domain.Capabilities{Inject: true}
	f, executor := runtimetest.SchedulerFixture(t, runtimetest.Store(t, "direct"), o)
	driver := f.Driver.(*runtimetest.SchedulerDriver)
	s := driver.Scheduler
	executor.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &runtimetest.Handle{}, nil
	}
	for _, name := range []string{"hung", "fine"} {
		key := domain.CorrelationKey{Source: "fixture/account", ExternalID: name}
		runtimetest.RequireAccept(t, f, "first/"+name, &key)
		runtimetest.RequireRoute(t, f.Driver, domain.ID("first/"+name))
		if _, err := s.Start(ctx, domain.ID("work/"+name)); err != nil {
			t.Fatal(err)
		}
		runtimetest.RequireAccept(t, f, "more/"+name, &key)
		runtimetest.RequireRoute(t, f.Driver, domain.ID("more/"+name))
	}
	entered, release, fine := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
	var mu sync.Mutex
	hungCalls := 0
	executor.OnInject = func(ctx context.Context, _ domain.ID, e domain.Event) error {
		if !strings.HasSuffix(string(e.ID), "/hung") {
			fine <- struct{}{}
			return nil
		}
		mu.Lock()
		hungCalls++
		mu.Unlock()
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	intents, err := driver.Router.Intents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	intentFor := map[domain.ID]string{}
	for _, intent := range intents {
		intentFor[intent.WorkID] = intent.ID
	}
	if len(intentFor) != 2 {
		t.Fatalf("intents %+v, want one for each Work", intents)
	}
	d := &daemon{coord: f.Store, now: f.Clock.Now, log: slog.New(slog.DiscardHandler), ctx: ctx, nudge: make(chan struct{}, 1),
		reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	c := &components{scheduler: s, router: driver.Router}
	d.current.Store(c)

	d.dispatchControls(ctx, c)
	<-entered
	<-fine
	// Another pass leaves the hung Work to the worker still dispatching it.
	d.dispatchControls(ctx, c)
	close(release)
	d.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if hungCalls != 1 {
		t.Fatalf("hung Work's intent dispatched %d times, want once", hungCalls)
	}
	for work, intent := range intentFor {
		if state, err := s.ControlStatus(ctx, intent); err != nil || state.Status != "accepted" {
			t.Fatalf("%s intent %+v (%v), want accepted", work, state, err)
		}
	}
}

// TestLoopCheckSummarisesPasses: the loop check reports the latest
// passes' spread and each phase's, in the order a pass runs them.
func TestLoopCheckSummarisesPasses(t *testing.T) {
	p, err := newPassTimes(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Each phase takes as long as the fake clock says.
	var at time.Time
	p.nowFor = func() time.Time { return at }
	p.since = func(start time.Time) time.Duration { return at.Sub(start) }
	for i := 1; i <= 3; i++ {
		pass := p.begin()
		pass.phase("route", func() { at = at.Add(time.Duration(i) * time.Millisecond) })
		pass.phase("ingress", func() { at = at.Add(time.Microsecond) })
		pass.done(context.Background())
	}
	d := &daemon{passTimes: p}
	check := d.loopCheck(context.Background())
	if check.Status != runtime.CheckOK || check.Detail != "latest 3 of 3 passes: median 2ms, p95 3ms, longest 3ms" {
		t.Fatalf("check %+v", check)
	}
	want := []string{"route: median 2ms, p95 3ms, longest 3ms", "ingress: median 1µs, p95 1µs, longest 1µs"}
	if strings.Join(check.Items, "|") != strings.Join(want, "|") {
		t.Fatalf("items %q, want %q", check.Items, want)
	}
	if empty := (&daemon{}).loopCheck(context.Background()); empty.Detail != "no pass yet" {
		t.Fatalf("empty check %+v", empty)
	}
}
