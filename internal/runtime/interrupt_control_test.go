package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// interruptFixture starts an attempt on Work whose route injects, with the
// binding advertising caps, and queues one injected input behind it: the
// assist thread whose turn sits in a long tool call while a correction
// waits.
func interruptFixture(t *testing.T, caps domain.Capabilities, fallback string) (f interruptRig) {
	t.Helper()
	o := routingOptions(routingHandlerFunc(routingDecision), "inject")
	o.Capabilities = caps
	fx, e := schedulerFixture(t, "reopen", o)
	work := seedSchedulerWork(t, fx, "operator-interrupt")
	options := schedulerDefaultOptions(fx, e)
	if fallback != "" {
		options.ControlErrorFallback = map[string]string{"fixture": fallback}
	}
	s := newTestScheduler(t, fx, options)
	control, err := runtime.NewControl(fx.Store, fx.Clock.Now, fx.Driver.(*schedulerDriver).Router, s, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "operator-interrupt"}
	requireAccept(t, fx, "correction", &key)
	requireRoute(t, fx.Driver, "correction")
	return interruptRig{f: fx, e: e, s: s, control: control, work: work, attempt: a}
}

type interruptRig struct {
	f       conformance.RuntimeFixture
	e       *schedulerExecutor
	s       *runtime.Scheduler
	control *runtime.Control
	work    domain.ID
	attempt runtime.Attempt
}

func (r interruptRig) read(t *testing.T, fn func(storage.Tx) error) {
	t.Helper()
	routingRead(t, r.f, fn)
}

func (r interruptRig) revision(t *testing.T) domain.Revision {
	t.Helper()
	var rev domain.Revision
	r.read(t, func(tx storage.Tx) error {
		w, err := tx.Work(r.work)
		rev = w.Revision
		return err
	})
	return rev
}

// TestProductionOperatorInterruptKeepsTheWork: an operator's interrupt
// fences the running attempt through the interrupt dispatch, records who
// did it and links the Event to the attempt it ended, leaves the Work ready
// with the injected input still queued, and the next attempt takes it in.
// Replaying the request returns the same Event and interrupts nothing more.
func TestProductionOperatorInterruptKeepsTheWork(t *testing.T) {
	r := interruptFixture(t, domain.Capabilities{Inject: true, Interrupt: true}, "")
	ctx := runtime.WithActor(context.Background(), "operator:alex")
	rev := r.revision(t)
	event, execution, err := r.control.InterruptWork(ctx, r.work, rev, "read my messages", "interrupt-1")
	if err != nil {
		t.Fatal(err)
	}
	if execution != r.attempt.Lease.ExecutionID {
		t.Fatalf("interrupted %s, want the running attempt %s", execution, r.attempt.Lease.ExecutionID)
	}
	if _, _, _, interrupts := r.e.Counts(); interrupts != 1 {
		t.Fatalf("executor interrupts = %d, want 1", interrupts)
	}
	again, againExecution, err := r.control.InterruptWork(ctx, r.work, rev, "read my messages", "interrupt-1")
	if err != nil || again != event || againExecution != execution {
		t.Fatalf("replay: %s %s %v, want %s %s", again, againExecution, err, event, execution)
	}
	if _, _, _, interrupts := r.e.Counts(); interrupts != 1 {
		t.Fatalf("replay interrupted again: %d", interrupts)
	}
	r.read(t, func(tx storage.Tx) error {
		a, err := tx.Event(event)
		if err != nil {
			return err
		}
		if a.Event.Type != "core.manual.interrupt" || a.Event.Actor != "operator:alex" || a.Event.WorkID != r.work {
			t.Fatalf("interrupt event: %+v", a.Event)
		}
		w, err := tx.Work(r.work)
		if err != nil {
			return err
		}
		if w.State != domain.WorkReady {
			t.Fatalf("work after interrupt: %s, want ready", w.State)
		}
		run, err := runtime.RunInTx(tx, execution)
		if err != nil {
			return err
		}
		if run.Execution.State != domain.ExecutionCancelled || run.InterruptEventID != event {
			t.Fatalf("run after interrupt: %s interrupted by %q, want cancelled by %s", run.Execution.State, run.InterruptEventID, event)
		}
		pending, err := runtime.PendingInputsInTx(tx, r.work)
		if err != nil {
			return err
		}
		if len(pending) != 1 || pending[0].EventID != "correction" {
			t.Fatalf("queued input after interrupt: %+v, want the correction", pending)
		}
		return nil
	})
	if err = r.s.Complete(context.Background(), r.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("interrupted attempt kept authority: %v", err)
	}
	if err = r.s.ConfirmStopped(context.Background(), r.attempt.Lease.ExecutionID, r.attempt.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	next, err := r.s.Start(context.Background(), r.work)
	if err != nil {
		t.Fatal(err)
	}
	r.read(t, func(tx storage.Tx) error {
		run, err := runtime.RunInTx(tx, next.Lease.ExecutionID)
		if err != nil {
			return err
		}
		if run.Inputs != 1 {
			t.Fatalf("next attempt inputs = %d, want the correction", run.Inputs)
		}
		return nil
	})
}

// TestProductionOperatorInterruptFailsClosed: Work whose running attempt
// can't be interrupted is refused, not cancelled, and Work that isn't
// running has nothing to interrupt.
func TestProductionOperatorInterruptFailsClosed(t *testing.T) {
	r := interruptFixture(t, domain.Capabilities{Inject: true}, "")
	ctx := context.Background()
	if _, _, err := r.control.InterruptWork(ctx, r.work, r.revision(t), "", "interrupt-unsupported"); !errors.Is(err, runtime.ErrInterruptUnsupported) {
		t.Fatalf("interrupt without the capability: %v", err)
	}
	r.read(t, func(tx storage.Tx) error {
		x, err := tx.Execution(r.attempt.Lease.ExecutionID)
		if err != nil {
			return err
		}
		if x.State != domain.ExecutionRunning {
			t.Fatalf("refused interrupt changed the attempt: %s", x.State)
		}
		return nil
	})
	if _, _, _, interrupts := r.e.Counts(); interrupts != 0 {
		t.Fatalf("refused interrupt reached the executor: %d", interrupts)
	}

	idle := interruptFixture(t, domain.Capabilities{Inject: true, Interrupt: true}, "")
	if err := idle.s.Complete(ctx, idle.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := idle.control.InterruptWork(ctx, idle.work, idle.revision(t), "", "interrupt-idle"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("interrupt of Work with no running attempt: %v", err)
	}
	if _, _, err := idle.control.InterruptWork(ctx, idle.work, idle.revision(t)+1, "", "interrupt-stale"); !errors.Is(err, runtime.ErrRevision) {
		t.Fatalf("interrupt at a stale revision: %v", err)
	}
}

// TestProductionOperatorInterruptTakesNoFallback: a failed dispatch of an
// operator's interrupt never takes the binding's control_error_fallback,
// which is for routed input; the attempt stays fenced and the input queued.
func TestProductionOperatorInterruptTakesNoFallback(t *testing.T) {
	r := interruptFixture(t, domain.Capabilities{Inject: true, Interrupt: true}, "drop")
	r.e.OnInterrupt = func(context.Context, domain.ID) error { return errors.New("signal failed") }
	ctx := context.Background()
	if _, _, err := r.control.InterruptWork(ctx, r.work, r.revision(t), "", "interrupt-fails"); err != nil {
		t.Fatal(err)
	}
	intents, err := r.f.Driver.(*schedulerDriver).Router.Intents(ctx, r.work)
	if err != nil {
		t.Fatal(err)
	}
	var operator *runtime.ControlIntent
	for i := range intents {
		if intents[i].Operator {
			operator = &intents[i]
		}
	}
	if operator == nil {
		t.Fatalf("no operator intent in %+v", intents)
	}
	state, err := r.s.ControlStatus(ctx, operator.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "retryable" || state.EffectivePolicy != "interrupt" || !state.Fenced {
		t.Fatalf("failed operator interrupt: %+v, want retryable, fenced, interrupt", state)
	}
	r.read(t, func(tx storage.Tx) error {
		pending, err := runtime.PendingInputsInTx(tx, r.work)
		if err != nil {
			return err
		}
		if len(pending) != 1 {
			t.Fatalf("queued input after a failed interrupt: %+v", pending)
		}
		return nil
	})
}
