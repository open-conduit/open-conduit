package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Job run parity (05-02) on the production router, scheduler and timers on
// SQLite with a fake clock: one deadline model (a binding's timeout_seconds
// replaces the runtime default for every executor), an executor retry hint
// honoured on standing job Work (v1's "busy" retried in ten minutes) while
// the budget retry stays off, and run-ended Events for `job:` waits.

func (e *scEnv) run(id domain.ID) runtime.Run {
	e.t.Helper()
	var r runtime.Run
	e.read(func(tx storage.Tx) error { var err error; r, err = runtime.RunInTx(tx, id); return err })
	return r
}

func (e *scEnv) publishRuns() []domain.Event {
	e.t.Helper()
	var out []domain.Event
	e.read(func(tx storage.Tx) error {
		ended, err := sc.EndedRunsInTx(tx, e.clock.Now(), 50)
		if err != nil {
			return err
		}
		for _, x := range ended {
			if x.Event.ID != "" {
				if _, _, err := runtime.AcceptDerivedInTx(tx, e.clock.Now(), runtime.CausalLimits{}, x.Scope, x.Parent, x.Event); err != nil {
					return err
				}
				out = append(out, x.Event)
			}
			if err := sc.MarkEndedInTx(tx, x.Work, x.Attempt); err != nil {
				return err
			}
		}
		return nil
	})
	return out
}

func TestJobDeadlineRetryHintAndRunEvents(t *testing.T) {
	start := utc(2026, 10, 21, 5, 17)
	e := &scEnv{t: t, clock: &ghClock{t: start}, path: filepath.Join(t.TempDir(), "conduit.db"), enabled: true, x: &jobExecutor{}, attempts: map[domain.ID]runtime.Attempt{},
		entry:    scEntry(`[{"name":"refresh"}]`),
		bindings: []domain.Binding{{Name: "job", Executor: "fixture", TimeoutSeconds: 90}}}
	e.open()
	e.putScope()
	e.register()

	// The binding's timeout replaces the runtime default (24h here).
	e.manual("refresh", "r1")
	e.pass()
	a := e.attempts[domain.ID(sc.WorkID(scNS, "refresh"))]
	if r := e.run(a.Lease.ExecutionID); !r.Deadline.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("deadline %v; want the binding's 90s", r.Deadline)
	}
	e.finish("refresh")

	// "busy": a failed run with a retry hint arms a retry on standing Work
	// (ten minutes: the hint's reset plus core's two-minute margin).
	reset := start.Add(8 * time.Minute)
	e.x.mu.Lock()
	e.x.outcome, e.x.hint = domain.OutcomeFailed, &reset
	e.x.mu.Unlock()
	e.manual("refresh", "r2")
	e.pass()
	e.finish("refresh")
	e.x.mu.Lock()
	e.x.outcome, e.x.hint = "", nil
	e.x.mu.Unlock()
	status, err := e.sched.RetryStatus(context.Background(), domain.ID(sc.WorkID(scNS, "refresh")))
	if err != nil || !status.Blocked || status.Exhausted || !status.EligibleAt.Equal(start.Add(10*time.Minute)) {
		t.Fatalf("retry status %+v %v", status, err)
	}
	// A trigger during the backoff waits for the retry (v1 RetryAfter).
	e.at(start.Add(5 * time.Minute))
	e.manual("refresh", "r3")
	e.route()
	if _, err := e.sched.Start(context.Background(), domain.ID(sc.WorkID(scNS, "refresh"))); !errors.Is(err, runtime.ErrNotEligible) {
		t.Fatalf("started during the busy backoff: %v", err)
	}
	e.at(start.Add(10 * time.Minute))
	e.pass()
	runs := e.runsOf("refresh")
	// The retry takes the whole pending batch (one pending run); the job's
	// trigger policy keeps the earliest request as its trigger.
	if len(runs) != 3 || runs[2].Trigger.ID != "manual-refresh-r3" || len(runs[2].Inputs) != 2 {
		t.Fatalf("busy retry %+v", runs)
	}
	e.finish("refresh")

	// Without a hint a failure still arms nothing (v1 had no job retry).
	e.x.mu.Lock()
	e.x.outcome = domain.OutcomeFailed
	e.x.mu.Unlock()
	e.manual("refresh", "r4")
	e.pass()
	e.finish("refresh")
	if status, err := e.sched.RetryStatus(context.Background(), domain.ID(sc.WorkID(scNS, "refresh"))); err != nil || status.Blocked || status.TimerID != "" {
		t.Fatalf("hintless failure armed a retry: %+v %v", status, err)
	}

	// Run-ended Events: one per finished run, typed by its outcome, derived
	// from the run's trigger on the job's key; publishing again adds none.
	ended := e.publishRuns()
	var types []string
	for _, ev := range ended {
		types = append(types, ev.Type)
		if ev.CorrelationKey == nil || ev.CorrelationKey.Source != scNS || ev.CorrelationKey.ExternalID != sc.JobKey("refresh") || ev.CausationID == "" {
			t.Fatalf("run-ended event %+v", ev)
		}
	}
	want := []string{sc.TypeRunCompleted, sc.TypeRunFailed, sc.TypeRunCompleted, sc.TypeRunFailed}
	if len(types) != len(want) {
		t.Fatalf("run-ended types %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("run-ended types %v; want %v", types, want)
		}
	}
	if again := e.publishRuns(); len(again) != 0 {
		t.Fatalf("published twice: %v", again)
	}
	// They are never job input: the schedule route does not match them.
	e.pass()
	if n := len(e.runsOf("refresh")); n != 4 {
		t.Fatalf("a run-ended event started a run: %d", n)
	}
}
