package integration_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
)

// Generic inject acknowledgement (B04, 04-02) through the production
// scheduler on reopened SQLite. Result.ConsumedInputs proves consumption on
// every outcome, including while the dispatch acknowledgement is still in
// flight; ErrNotAccepting settles an undelivered injection without retries;
// unproved input is presented again; old intents never retarget.

type injectAckFixture struct {
	t     *testing.T
	f     conformance.RuntimeFixture
	e     *schedulerExecutor
	s     *runtime.Scheduler
	work  domain.ID
	key   domain.CorrelationKey
	first runtime.Attempt
}

func newInjectAckFixture(t *testing.T, name string) *injectAckFixture {
	t.Helper()
	o := routingOptions(routingHandlerFunc(routingDecision), "inject")
	o.Capabilities = domain.Capabilities{Inject: true}
	f, e := schedulerFixture(t, "reopen", o)
	x := &injectAckFixture{t: t, f: f, e: e, s: f.Driver.(*schedulerDriver).scheduler, work: seedSchedulerWork(t, f, name), key: domain.CorrelationKey{Source: "fixture/account", ExternalID: name}}
	e.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{}, nil
	}
	a, err := x.s.Start(context.Background(), x.work)
	if err != nil {
		t.Fatal(err)
	}
	x.first = a
	return x
}

// inject accepts and routes one input and returns its intent ID.
func (x *injectAckFixture) inject(id string) string {
	x.t.Helper()
	requireAccept(x.t, x.f, id, &x.key)
	requireRoute(x.t, x.f.Driver, domain.ID(id))
	intents, err := x.f.Driver.(*schedulerDriver).router.Intents(context.Background(), x.work)
	if err != nil {
		x.t.Fatal(err)
	}
	for _, intent := range intents {
		if intent.EventID == domain.ID(id) {
			return intent.ID
		}
	}
	x.t.Fatalf("no inject intent for %s: %+v", id, intents)
	return ""
}

func (x *injectAckFixture) pending() []domain.ID {
	x.t.Helper()
	in, err := x.f.Driver.(*schedulerDriver).router.PendingInputs(context.Background(), x.work)
	if err != nil {
		x.t.Fatal(err)
	}
	out := []domain.ID{}
	for _, p := range in {
		out = append(out, p.EventID)
	}
	return out
}

func (x *injectAckFixture) status(intent string) string {
	x.t.Helper()
	st, err := x.s.ControlStatus(context.Background(), intent)
	if err != nil {
		x.t.Fatal(err)
	}
	return st.Status
}

func (x *injectAckFixture) workState() domain.WorkState {
	x.t.Helper()
	var w domain.Work
	if err := conformance.RuntimeRead(x.f, func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(x.work)
		return err
	}); err != nil {
		x.t.Fatal(err)
	}
	return w.State
}

// TestInjectProofWhileDispatchInFlight: the process echoes the input and the
// turn ends before DispatchControl commits its acknowledgement (03-06's
// remaining edge). The proof still consumes the input with the turn, the
// late acknowledgement keeps the intent consumed, and the Work completes
// without an extra attempt.
func TestInjectProofWhileDispatchInFlight(t *testing.T) {
	x := newInjectAckFixture(t, "inflight")
	intent := x.inject("echoed")
	x.e.onInject = func(ctx context.Context, _ domain.ID, ev domain.Event) error {
		// The finished result is recorded while Inject is still outside
		// storage, before the dispatch acknowledgement.
		return x.s.Complete(ctx, x.first.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, ConsumedInputs: []domain.ID{ev.ID}})
	}
	if err := x.s.DispatchControl(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if p := x.pending(); len(p) != 0 {
		t.Fatalf("consumed input still pending: %v", p)
	}
	if st := x.status(intent); st != "consumed" {
		t.Fatalf("intent status %s, want consumed", st)
	}
	if st := x.workState(); st != domain.WorkCompleted {
		t.Fatalf("work %s: a consumed injection started another attempt", st)
	}
	// Replay of the intent is a no-op.
	if err := x.s.DispatchControl(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if _, _, injects, _ := x.e.counts(); injects != 1 {
		t.Fatalf("consumed intent dispatched again (%d)", injects)
	}
}

// TestInjectProofOnWaitingResult: a turn that takes in an injected message
// and then waits records the proof with the wait. The registration scan does
// not wake on what it already took in; the next human message wakes it and
// is the only input of the next attempt.
func TestInjectProofOnWaitingResult(t *testing.T) {
	x := newInjectAckFixture(t, "waiting")
	intent := x.inject("taken")
	if err := x.s.DispatchControl(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	state, err := x.s.InspectAttempt(context.Background(), x.first.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	wait := domain.Wait{ID: "wait/waiting", Match: domain.EventMatch{Keys: []domain.CorrelationKey{x.key}}, AfterSequence: state.ObservedSequence}
	result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting}, ConsumedInputs: []domain.ID{"taken"}}
	if err = x.s.WaitWithResult(context.Background(), x.first.Lease, wait, false, result); err != nil {
		t.Fatal(err)
	}
	if st := x.workState(); st != domain.WorkWaiting {
		t.Fatalf("work %s: woken by the input it already took in", st)
	}
	if p := x.pending(); len(p) != 0 {
		t.Fatalf("pending %v", p)
	}
	requireAccept(t, x.f, "later", &x.key)
	requireRoute(t, x.f.Driver, "later")
	if st := x.workState(); st != domain.WorkReady {
		t.Fatalf("work %s after a new message", st)
	}
	next, err := x.s.Start(context.Background(), x.work)
	if err != nil {
		t.Fatal(err)
	}
	ids := []domain.ID{}
	for _, in := range next.Request.Inputs {
		ids = append(ids, in.ID)
	}
	if !reflect.DeepEqual(ids, []domain.ID{"later"}) {
		t.Fatalf("next attempt inputs %v, want only the new message", ids)
	}
}

// TestInjectProofOnFailedResult: a failed turn's proved injection is
// consumed like its claimed input; an unproved one stays for the retry.
func TestInjectProofOnFailedResult(t *testing.T) {
	x := newInjectAckFixture(t, "failed")
	taken, missed := x.inject("taken"), x.inject("missed")
	for _, intent := range []string{taken, missed} {
		if err := x.s.DispatchControl(context.Background(), intent); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.s.Fail(context.Background(), x.first.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "crashed"}, ConsumedInputs: []domain.ID{"taken"}}); err != nil {
		t.Fatal(err)
	}
	if p := x.pending(); !reflect.DeepEqual(p, []domain.ID{"missed"}) {
		t.Fatalf("pending %v, want only the unproved input", p)
	}
	if x.status(taken) != "consumed" || x.status(missed) != "accepted" {
		t.Fatalf("statuses %s %s", x.status(taken), x.status(missed))
	}
}

// TestInjectNotAcceptingSettlesWithoutRetry: ErrNotAccepting (an adopted
// segment without stdin, a finished process) is definitive. The intent stops
// dispatching, nothing is dropped and the input reaches attempt N+1.
func TestInjectNotAcceptingSettlesWithoutRetry(t *testing.T) {
	x := newInjectAckFixture(t, "refused")
	intent := x.inject("refused")
	x.e.onInject = func(context.Context, domain.ID, domain.Event) error { return extension.ErrNotAccepting }
	if err := x.s.DispatchControl(context.Background(), intent); err != nil {
		t.Fatalf("a definitive refusal is not a dispatch failure: %v", err)
	}
	if err := x.s.DispatchControl(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if _, _, injects, _ := x.e.counts(); injects != 1 {
		t.Fatalf("not-accepting intent retried (%d)", injects)
	}
	if st := x.status(intent); st != "not_accepting" {
		t.Fatalf("status %s", st)
	}
	// A proof naming it anyway grants nothing: it was never delivered.
	if err := x.s.Complete(context.Background(), x.first.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, ConsumedInputs: []domain.ID{"refused"}}); err != nil {
		t.Fatal(err)
	}
	if st := x.workState(); st != domain.WorkReady {
		t.Fatalf("work %s: retained input lost", st)
	}
	next, err := x.s.Start(context.Background(), x.work)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Request.Inputs) != 1 || next.Request.Inputs[0].ID != "refused" {
		t.Fatalf("N+1 inputs %+v", next.Request.Inputs)
	}
}

// TestInjectUndispatchedOrUnknownProofIgnored: an input whose intent was never
// dispatched, and an Event never injected, cannot be consumed by a proof; an
// old intent dispatched after completion goes stale instead of reaching the
// next attempt.
func TestInjectUndispatchedOrUnknownProofIgnored(t *testing.T) {
	x := newInjectAckFixture(t, "undispatched")
	intent := x.inject("queued")
	if err := x.s.Complete(context.Background(), x.first.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff}, ConsumedInputs: []domain.ID{"queued", "trigger/undispatched", "invented"}}); err != nil {
		t.Fatal(err)
	}
	if p := x.pending(); !reflect.DeepEqual(p, []domain.ID{"queued"}) {
		t.Fatalf("pending %v", p)
	}
	next, err := x.s.Start(context.Background(), x.work)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.s.DispatchControl(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if st := x.status(intent); st != "stale" {
		t.Fatalf("old intent %s", st)
	}
	x.e.mu.Lock()
	targets := append([]domain.ID(nil), x.e.injects...)
	x.e.mu.Unlock()
	if len(targets) != 0 || next.Lease.ExecutionID == x.first.Lease.ExecutionID {
		t.Fatalf("old intent retargeted: %v", targets)
	}
	if len(next.Request.Inputs) != 1 || next.Request.Inputs[0].ID != "queued" {
		t.Fatalf("N+1 inputs %+v", next.Request.Inputs)
	}
}
