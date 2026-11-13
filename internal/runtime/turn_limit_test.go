package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// turnLimitRig runs one attempt of Work on a binding advertising caps, with
// a 30-second turn limit.
type turnLimitRig struct {
	interruptRig
	timers *runtime.Timers
}

func turnLimitFixture(t *testing.T, caps domain.Capabilities, name string) turnLimitRig {
	t.Helper()
	o := routingOptions(routingHandlerFunc(routingDecision), "queue")
	o.Capabilities = caps
	fx, e := schedulerFixture(t, "reopen", o)
	work := seedSchedulerWork(t, fx, name)
	options := schedulerDefaultOptions(fx, e)
	options.Timeout = 30 * time.Second
	s := newTestScheduler(t, fx, options)
	timers, err := runtime.NewTimers(fx.Store, fx.Clock.Now, runtime.TimerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	return turnLimitRig{interruptRig: interruptRig{f: fx, e: e, s: s, work: work, attempt: a}, timers: timers}
}

// expire passes the turn's deadline and lets the host's pass deliver it, as
// the daemon does: the deadline's Timer fires and Poll settles its Event.
func (r turnLimitRig) expire(t *testing.T) {
	t.Helper()
	r.f.Clock.Advance(30 * time.Second)
	poll, err := r.s.Poll(context.Background(), r.timers, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(poll.Diagnostics) > 0 {
		t.Fatalf("deadline pass: %+v", poll.Diagnostics)
	}
}

// TestProductionTurnLimitEndsAResumableTurn: when a turn passes its deadline
// on an executor pinned with interrupt and resume, the deadline ends the
// turn through the interrupt path rather than failing the attempt (#564).
// The attempt is cancelled with the core.execution.timeout Event as its
// interrupt, the executor is interrupted, no failure is counted and no
// backoff waited, and the next attempt starts at once with the deadline
// Event as its input, so the executor can tell its last turn was cut off.
func TestProductionTurnLimitEndsAResumableTurn(t *testing.T) {
	ctx := context.Background()
	r := turnLimitFixture(t, domain.Capabilities{Interrupt: true, Resume: true}, "turn-limit")
	r.expire(t)
	execution := r.attempt.Lease.ExecutionID
	var deadline domain.ID
	r.read(t, func(tx storage.Tx) error {
		run, err := runtime.RunInTx(tx, execution)
		if err != nil {
			return err
		}
		if run.Execution.State != domain.ExecutionCancelled || run.Interrupt == nil || run.Interrupt.Type != domain.TypeExecutionTimeout || run.InterruptEventID != run.Interrupt.ID {
			t.Fatalf("run at its time limit: %s interrupted by %+v, want cancelled by its deadline", run.Execution.State, run.Interrupt)
		}
		if run.Retry != nil {
			t.Fatalf("a turn that reached its limit counted as a failure: %+v", run.Retry)
		}
		deadline = run.InterruptEventID
		if a, err := tx.Event(deadline); err != nil || a.Disposition != runtime.RoutedTurnLimit || a.RoutedWorkID != r.work {
			t.Fatalf("deadline Event: %+v (%v), want routed to the Work as %s", a, err, runtime.RoutedTurnLimit)
		}
		w, err := tx.Work(r.work)
		if err != nil {
			return err
		}
		if w.State != domain.WorkReady {
			t.Fatalf("work after its turn's limit: %s, want ready", w.State)
		}
		pending, err := runtime.PendingInputsInTx(tx, r.work)
		if err != nil {
			return err
		}
		if len(pending) != 1 || pending[0].EventID != deadline {
			t.Fatalf("queued input after the limit: %+v, want the deadline Event", pending)
		}
		return nil
	})
	if _, err := r.s.RetryStatus(ctx, r.work); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("retry state after a turn limit: %v, want none", err)
	}
	if _, _, _, interrupts := r.e.Counts(); interrupts != 1 {
		t.Fatalf("executor interrupts = %d, want 1", interrupts)
	}
	if err := r.s.Complete(ctx, r.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("an attempt past its limit kept authority: %v", err)
	}
	if _, err := r.s.Start(ctx, r.work); !errors.Is(err, runtime.ErrResourceHeld) {
		t.Fatalf("next attempt before the old process stopped: %v, want it held", err)
	}
	if err := r.s.ConfirmStopped(ctx, execution, r.attempt.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	next, err := r.s.Start(ctx, r.work)
	if err != nil {
		t.Fatalf("next attempt after the limit: %v", err)
	}
	if next.Request.Execution.Attempt != 2 || next.Request.Trigger.ID != deadline || next.Request.Trigger.Type != domain.TypeExecutionTimeout {
		t.Fatalf("next attempt %d triggered by %s %s, want attempt 2 carrying on from the deadline", next.Request.Execution.Attempt, next.Request.Trigger.Type, next.Request.Trigger.ID)
	}
	var p domain.ExecutionTimeout
	if err = json.Unmarshal(next.Request.Trigger.Data.Data, &p); err != nil || p.TimeoutSeconds != 30 || p.ExecutionID != execution {
		t.Fatalf("deadline payload: %+v (%v)", p, err)
	}
}

// TestProductionTurnLimitFailsAnAttemptThatCannotResume is the negative
// control: an executor without both interrupt and resume (a shell command)
// keeps today's behaviour, and the deadline fails the attempt under retry
// policy.
func TestProductionTurnLimitFailsAnAttemptThatCannotResume(t *testing.T) {
	for name, caps := range map[string]domain.Capabilities{
		"neither":        {},
		"interrupt only": {Interrupt: true},
		"resume only":    {Resume: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := turnLimitFixture(t, caps, "turn-limit-fails")
			r.expire(t)
			r.read(t, func(tx storage.Tx) error {
				run, err := runtime.RunInTx(tx, r.attempt.Lease.ExecutionID)
				if err != nil {
					return err
				}
				if run.Execution.State != domain.ExecutionFailed || run.Interrupt != nil || run.Execution.Outcome == nil || run.Execution.Outcome.Summary != "execution timed out" {
					t.Fatalf("run past its limit: %s %+v, want failed: execution timed out", run.Execution.State, run.Execution.Outcome)
				}
				pending, err := runtime.PendingInputsInTx(tx, r.work)
				if err != nil || len(pending) != 0 {
					t.Fatalf("a failed attempt queued input: %+v (%v)", pending, err)
				}
				return nil
			})
			retry, err := r.s.RetryStatus(context.Background(), r.work)
			if err != nil || !retry.Blocked || retry.FailureCount != 1 {
				t.Fatalf("retry after the limit: %+v (%v), want one failure in backoff", retry, err)
			}
			if _, _, _, interrupts := r.e.Counts(); interrupts != 0 {
				t.Fatalf("executor interrupts = %d, want none", interrupts)
			}
		})
	}
}
