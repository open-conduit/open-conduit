package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

func ingressFixture() conformance.IngressFixture {
	now := conformance.NewClock().Now()
	o := extension.Observation{ID: "item", Version: "v2", Event: domain.Event{ID: "change", Type: "fixture.changed.v1", Source: "fixture/account/resource", DeliveryID: "item/v2", OccurredAt: now, ReceivedAt: now}}
	one := o.Event
	one.ID = "post"
	one.DeliveryID = "post"
	one.Type = "fixture.post.v1"
	i := &reference.Ingress{
		Declaration: extension.IngressSpec{Families: []extension.EventFamily{{Type: o.Event.Type, ReDerivable: true}, {Type: one.Type, Loss: "Arbitrary POST body has no readable history; an unaccepted delivery is lost."}}},
		PushResult:  extension.PushResult{Observations: []extension.Observation{o}, Events: []domain.Event{one}},
		Pages:       map[string]extension.Snapshot{"": {ScanID: "scan", NextCursor: "second", Checkpoint: "page-1"}, "second": {ScanID: "scan", Complete: true, Observations: []extension.Observation{o}, Checkpoint: "page-2"}},
	}
	return conformance.IngressFixture{Ingress: i, Source: extension.Source{Namespace: o.Event.Source, ScopeID: "scope"}, Delivery: extension.Delivery{ID: "webhook", ReceivedAt: now}, Canonical: o, OneShotType: one.Type}
}
func TestReferenceIngress(t *testing.T) { conformance.RunIngress(t, ingressFixture) }
func TestObservationDiffAndPushOverlap(t *testing.T) {
	f := ingressFixture()
	current := f.Canonical
	current.Version = "v1"
	s := &reference.ObservationSet{Current: map[string]extension.Observation{"item": current, "missing": {ID: "missing", Version: "old"}}}
	changed, absent, err := s.Page(extension.Snapshot{ScanID: "scan", NextCursor: "second", Observations: []extension.Observation{f.Canonical}})
	if err != nil || len(changed) != 0 || len(absent) != 0 || len(s.Current) != 2 {
		t.Fatalf("partial scan changed authoritative state: %v", err)
	}
	if _, _, err := s.Page(extension.Snapshot{ScanID: "other", Complete: true}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatal("mixed generations accepted")
	}
	changed, absent, err = s.Page(extension.Snapshot{ScanID: "scan", Complete: true})
	if err != nil || len(changed) != 1 || len(absent) != 1 || absent[0] != "missing" {
		t.Fatalf("complete scan diff incorrect: %v %v %v", changed, absent, err)
	}
	store := reference.New()
	err = store.Transact(context.Background(), storage.Guard{Now: conformance.NewClock().Now()}, func(tx storage.Tx) error {
		pushed := f.Canonical.Event
		if _, added, err := tx.AcceptEvent(pushed); err != nil || !added {
			return fmt.Errorf("push: %v", err)
		}
		reconciled := changed[0]
		reconciled.ID = "reconcile-internal-id"
		if _, added, err := tx.AcceptEvent(reconciled); err != nil || added {
			return fmt.Errorf("push/reconcile overlap duplicated: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	changed, absent, err = s.Page(extension.Snapshot{ScanID: "next", Complete: true, Observations: []extension.Observation{f.Canonical}})
	if err != nil || len(changed) != 0 || len(absent) != 0 {
		t.Fatal("unchanged snapshot re-emitted")
	}
	s = &reference.ObservationSet{Current: map[string]extension.Observation{"item": current}}
	_, absent, err = s.Page(extension.Snapshot{ScanID: "partial"})
	if err != nil || len(absent) != 0 || len(s.Current) != 1 {
		t.Fatal("truncated scan deleted state")
	}
}
func toolbeltFixture() conformance.ToolbeltFixture {
	ctx := context.Background()
	clock := conformance.NewClock()
	store := reference.New()
	var lease domain.LeaseRef
	err := store.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "work", Kind: "fixture.objective", ScopeID: "scope", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{{Source: "fixture", ExternalID: "item"}}}, 0); err != nil {
			return err
		}
		e := domain.Event{ID: "trigger", Type: "fixture.changed.v1", Source: "fixture", DeliveryID: "trigger", OccurredAt: clock.Now(), ReceivedAt: clock.Now()}
		if _, _, err := tx.AcceptEvent(e); err != nil {
			return err
		}
		if err := tx.PutExecution(domain.Execution{ID: "execution", WorkID: "work", ExecutorBinding: "shell", Attempt: 1, State: domain.ExecutionPending, TriggerID: "trigger"}, 0); err != nil {
			return err
		}
		var err error
		lease, err = tx.Claim("execution", "fixture", time.Hour)
		return err
	})
	if err != nil {
		panic(err)
	}
	belt := reference.NewToolbelt(store, clock.Now)
	if err := belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "fixture", EventTypes: []string{"fixture.changed.v1"}, Operations: []extension.Operation{{Name: "fixture.echo", Handler: func(_ context.Context, _ extension.Authority, p domain.Payload) (domain.Payload, error) {
		return p, nil
	}}}}); err != nil {
		panic(err)
	}
	a := extension.Authority{ScopeID: "scope", WorkID: "work", ExecutionID: "execution", TriggerID: "trigger", Lease: lease, ExpiresAt: clock.Now().Add(time.Minute), Operations: []string{extension.Emit, extension.CreateWork, extension.Wait, extension.DeclareOutcome, extension.ReadContext, "fixture.echo"}, EventTypes: []string{"fixture.changed.v1"}}
	token, err := belt.Issue(ctx, a)
	if err != nil {
		panic(err)
	}
	return conformance.ToolbeltFixture{Belt: belt, Issuer: belt, Registrar: belt, Token: token, Authority: a, Advance: clock.Advance, LoseLease: func() error {
		return store.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error { return tx.Release(lease) })
	}}
}
func TestReferenceToolbelt(t *testing.T) { conformance.RunToolbelt(t, toolbeltFixture) }
func TestReferenceExecutor(t *testing.T) {
	for _, kind := range []domain.OutcomeKind{domain.OutcomeCompleted, domain.OutcomeFailed, domain.OutcomeWaiting, domain.OutcomeHandoff} {
		t.Run(string(kind), func(t *testing.T) {
			conformance.RunExecutor(t, func() extension.Executor {
				return &reference.Executor{Result: extension.Result{Outcome: domain.Outcome{Kind: kind}}}
			}, extension.RunRequest{Work: domain.Work{ID: "work"}, Execution: domain.Execution{ID: "execution", WorkID: "work"}})
		})
	}
}

// replacingExecutor relaunches on recovery, persisting new metadata; the
// shared case must reject it while permitting identical re-acknowledgement.
type replacingExecutor struct{ reference.Executor }

func (e *replacingExecutor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if state.Schema == "" {
		return nil, extension.ErrNotAdoptable
	}
	if err := r.PersistRecovery(domain.Payload{Schema: "fixture.adoption.v1", Data: []byte(`{"handle":"replacement"}`)}); err != nil {
		return nil, err
	}
	return e.Executor.Recover(ctx, r, state)
}

type reacknowledgingExecutor struct{ reference.Executor }

func (e *reacknowledgingExecutor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if state.Schema == "" {
		return nil, extension.ErrNotAdoptable
	}
	if err := r.PersistRecovery(state); err != nil {
		return nil, err
	}
	return e.Executor.Recover(ctx, r, state)
}

func TestExecutorRecoveryReplacement(t *testing.T) {
	r := extension.RunRequest{Work: domain.Work{ID: "work"}, Execution: domain.Execution{ID: "execution", WorkID: "work"}}
	result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}
	if err := conformance.CheckExecutor(&replacingExecutor{reference.Executor{Result: result}}, r); err == nil || !strings.Contains(err.Error(), "replacement") {
		t.Fatalf("replacement recovery accepted: %v", err)
	}
	if err := conformance.CheckExecutor(&reacknowledgingExecutor{reference.Executor{Result: result}}, r); err != nil {
		t.Fatalf("identical re-acknowledgement rejected: %v", err)
	}
}

func TestBindingRejection(t *testing.T) {
	e := &reference.Executor{}
	for _, requires := range []domain.Capabilities{{Inject: true}, {Interrupt: true}, {Resume: true}, {Workspace: true}, {Toolbelt: extension.ToolbeltMCP}} {
		if err := extension.ValidateBinding(domain.Binding{Requires: requires}, e); !errors.Is(err, extension.ErrUnsupported) {
			t.Fatalf("unsupported binding accepted: %v", err)
		}
	}
	if err := extension.ValidateBinding(domain.Binding{Requires: domain.Capabilities{Toolbelt: "magic"}}, e); !errors.Is(err, extension.ErrInvalid) {
		t.Fatal("invalid transport accepted")
	}
	if err := extension.ValidateResult(extension.Result{}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatal("unstructured outcome accepted")
	}
	if err := extension.ValidateBinding(domain.Binding{}, &lyingExecutor{}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatal("advertised capability without implementation accepted")
	}
}

type lyingExecutor struct{ reference.Executor }

func (lyingExecutor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Inject: true, Toolbelt: extension.ToolbeltCLI}
}
func TestReferenceObserver(t *testing.T) {
	o := &reference.Observer{Secrets: []string{"secret-value"}}
	conformance.RunObserver(t, o, o.Records)
}

// Negative controls run the actual shared cases and assert their diagnostics.
type unsafeBelt struct{ extension.Toolbelt }

func (b unsafeBelt) Call(ctx context.Context, token string, c extension.ToolCall) (extension.ToolResult, error) {
	c.ScopeID = ""
	c.WorkID = ""
	c.ExecutionID = ""
	return b.Toolbelt.Call(ctx, token, c)
}

type unsafeObserver struct{ records []observability.Record }

func (o *unsafeObserver) Observe(_ context.Context, r observability.Record) error {
	o.records = append(o.records, r)
	return nil
}
func TestWeakenedExtensionContractsAreRejected(t *testing.T) {
	f := ingressFixture()
	i := f.Ingress.(*reference.Ingress)
	p := i.Pages["second"]
	p.Observations = append([]extension.Observation(nil), p.Observations...)
	p.Observations[0].Event.DeliveryID = "different"
	i.Pages["second"] = p
	if err := conformance.CheckIngress(f); err == nil || !strings.Contains(err.Error(), "canonical identity differs") {
		t.Fatalf("identity drift undetected: %v", err)
	}
	f = ingressFixture()
	i = f.Ingress.(*reference.Ingress)
	i.Declaration.Families[1].Loss = ""
	if err := conformance.CheckIngress(f); err == nil || !strings.Contains(err.Error(), "loss limitation") {
		t.Fatalf("silent loss undetected: %v", err)
	}
	for _, c := range conformance.ToolbeltCases() {
		if c.Name == "cross_scope_work_execution" {
			f := toolbeltFixture()
			f.Belt = unsafeBelt{f.Belt}
			if err := c.Check(f); err == nil || !strings.Contains(err.Error(), "cross-target authority") {
				t.Fatalf("weak authority undetected: %v", err)
			}
		}
	}
	o := &unsafeObserver{}
	if err := conformance.CheckObserver(o, func() []observability.Record { return o.records }); err == nil || !strings.Contains(err.Error(), "correlation accepted") {
		t.Fatalf("missing telemetry undetected: %v", err)
	}
}

func injectFixture(e *reference.InjectExecutor, finish func()) conformance.InjectFixture {
	return conformance.InjectFixture{Executor: e, Request: extension.RunRequest{Work: domain.Work{ID: "work"}, Execution: domain.Execution{ID: "execution", WorkID: "work"}},
		Finish: func() error { e.Consume(); finish(); return nil }}
}

func TestReferenceInjectExecutor(t *testing.T) {
	conformance.RunInjectExecutor(t, func() conformance.InjectFixture {
		e := &reference.InjectExecutor{}
		return injectFixture(e, e.Finish)
	})
}

// Weakened inject executors must fail the shared case: one claiming an Event
// it was never given, one still accepting input after its result, one
// accepting input for an execution it does not hold, and one whose recovery
// loses the proof.
type overclaimingInject struct{ *reference.InjectExecutor }

type lateAcceptingInject struct{ *reference.InjectExecutor }

func (e lateAcceptingInject) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	if ev.ID == "inject-conformance-late" {
		return nil
	}
	return e.InjectExecutor.Inject(ctx, id, ev)
}

type promiscuousInject struct{ *reference.InjectExecutor }

func (e promiscuousInject) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	if id == "inject-conformance-unknown" {
		return nil
	}
	return e.InjectExecutor.Inject(ctx, id, ev)
}

type forgetfulInject struct{ *reference.InjectExecutor }
type proofless struct{ extension.RunHandle }

func (h proofless) Wait(ctx context.Context) (extension.Result, error) {
	r, err := h.RunHandle.Wait(ctx)
	r.ConsumedInputs = nil
	return r, err
}
func (e forgetfulInject) Recover(ctx context.Context, r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
	h, err := e.InjectExecutor.Recover(ctx, r, p)
	if err != nil {
		return nil, err
	}
	return proofless{h}, nil
}

func TestInjectExecutorNegativeControls(t *testing.T) {
	base := extension.RunRequest{Work: domain.Work{ID: "work"}, Execution: domain.Execution{ID: "execution", WorkID: "work"}}
	over := &reference.InjectExecutor{}
	cases := map[string]conformance.InjectFixture{
		"overclaim": {Executor: overclaimingInject{over}, Request: base, Finish: func() error {
			over.Consume()
			_ = over.Inject(context.Background(), "execution", domain.Event{ID: "never-given-by-core"})
			over.Consume()
			over.Finish()
			return nil
		}},
	}
	late := &reference.InjectExecutor{}
	cases["late"] = conformance.InjectFixture{Executor: lateAcceptingInject{late}, Request: base, Finish: func() error { late.Consume(); late.Finish(); return nil }}
	prom := &reference.InjectExecutor{}
	cases["unknown"] = conformance.InjectFixture{Executor: promiscuousInject{prom}, Request: base, Finish: func() error { prom.Consume(); prom.Finish(); return nil }}
	forget := &reference.InjectExecutor{}
	cases["recovery"] = conformance.InjectFixture{Executor: forgetfulInject{forget}, Request: base, Finish: func() error { forget.Consume(); forget.Finish(); return nil }}
	lazy := &reference.InjectExecutor{}
	cases["unconsumed"] = conformance.InjectFixture{Executor: lazy, Request: base, Finish: func() error { lazy.Finish(); return nil }}
	for name, f := range cases {
		if err := conformance.CheckInjectExecutor(f); err == nil {
			t.Errorf("%s: weakened inject executor passed", name)
		}
	}
	if err := conformance.CheckInjectExecutor(conformance.InjectFixture{Executor: &reference.Executor{}, Request: base}); err == nil {
		t.Error("an executor without inject passed")
	}
}
