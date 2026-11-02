package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// `job:<name>` waits (05-02) over the production toolbelt, scheduler and
// subscriptions on SQLite: a finished job run is announced as a run-ended
// Event on the job's key (derived from its trigger, after a per-job
// checkpoint), and the GitHub wait translation makes that type a FromNow
// dependency, so v1's TestJobRunEndsAWait holds with no condition kind in
// core.

const jobNS = "schedule/eng"

func (e *toolsEnv) withJobs(names ...string) {
	e.jobs = func(_ context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
		for _, n := range names {
			if scope == ghScope && n == name {
				return domain.CorrelationKey{Source: jobNS, ExternalID: sc.JobKey(name)}, sc.RunTypes, true
			}
		}
		return domain.CorrelationKey{}, nil, false
	}
	e.compose()
}

// endJobRun records one finished run of a job's standing Work (the job
// path itself is driven end to end by the schedule and daemon tests), then
// publishes run-ended Events exactly as bootstrap's pass does.
func (e *toolsEnv) endJobRun(job string, attempt uint32, state domain.ExecutionState) {
	e.t.Helper()
	key := domain.CorrelationKey{Source: jobNS, ExternalID: sc.JobKey(job)}
	work := domain.ID(sc.WorkID(jobNS, job))
	trigger := domain.Event{ID: domain.ID("slot-" + job + "-" + itoa(int(attempt))), Type: sc.TypeSlot, Source: jobNS, DeliveryID: "slot/" + job + "/" + itoa(int(attempt)),
		OccurredAt: e.clock.Now(), ReceivedAt: e.clock.Now(), CorrelationKey: &key}
	e.read(func(tx storage.Tx) error {
		if _, _, err := runtime.AcceptInTx(tx, ghScope, trigger); err != nil {
			return err
		}
		if err := tx.MarkRouted(trigger.ID, "", "unmatched"); err != nil {
			return err
		}
		if _, err := tx.Work(work); err != nil {
			w := domain.Work{ID: work, Kind: sc.WorkKind, ScopeID: ghScope, State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{key}}
			if err = tx.PutWork(w, 0); err != nil {
				return err
			}
		}
		id := domain.ID(string(work) + "/attempt/" + itoa(int(attempt)))
		if err := tx.PutExecution(domain.Execution{ID: id, WorkID: work, Attempt: attempt, State: domain.ExecutionPending, ExecutorBinding: "job", TriggerID: trigger.ID}, 0); err != nil {
			return err
		}
		if _, err := tx.Claim(id, "test", time.Hour); err != nil {
			return err
		}
		x, err := tx.Execution(id)
		if err != nil {
			return err
		}
		now := e.clock.Now()
		x.State, x.FinishedAt, x.Outcome = state, &now, &domain.Outcome{Kind: domain.OutcomeKind(state)}
		return tx.PutExecution(x, x.Revision)
	})
	e.publishJobRuns()
}

func (e *toolsEnv) publishJobRuns() {
	e.t.Helper()
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
			}
			if err := sc.MarkEndedInTx(tx, x.Work, x.Attempt); err != nil {
				return err
			}
		}
		return nil
	})
	e.route()
	if err := e.scheduler.ReconcileSubscriptions(context.Background(), 50); err != nil {
		e.t.Fatal(err)
	}
	e.route()
}

// TestJobRunEndsAWait ports v1 TestJobRunEndsAWait: a held issue waiting
// for job:backup starts when the job's next run ends; an agent's
// `github.wait job:backup` is woken by the next run, not an earlier one, and
// is told how it ended; an unknown job is refused.
func TestJobRunEndsAWait(t *testing.T) {
	e := newToolsEnv(t)
	e.withJobs("backup")

	// A run that ended before anyone waited never meets a wait (FromNow).
	e.endJobRun("backup", 1, domain.ExecutionCompleted)
	until := e.clock.Now().Add(30 * 24 * time.Hour).UTC().Format("2006-01-02T15:04Z")
	n, hw := e.heldClaim("Check the backup", "<!-- waitFor: job:backup; until "+until+" -->")
	if why := e.runHold(hw.ID); why != "" {
		t.Fatalf("released before the job ran: %q", why)
	}
	if got := e.work(hw.ID); got.State != domain.WorkWaiting {
		t.Fatalf("held Work: %v", got.State)
	}
	e.endJobRun("backup", 2, domain.ExecutionCompleted)
	if got := e.work(hw.ID); got.State != domain.WorkReady {
		t.Fatalf("the job's run did not wake the hold: %v", got.State)
	}
	if why := e.runHold(hw.ID); !strings.Contains(why, "This issue was held, waiting for the next run of the backup job to end. That has happened: a run of the backup job ended: completed.") {
		t.Fatalf("release: %q", why)
	}
	_ = n

	_, w := e.claim("Agent", "", "bug")
	a := e.start(w.ID)
	_, err := e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{"job:nightly"}, "reason": "x"})
	refused(t, err, "no_job")
	out, err := e.call(a, gh.OpWait, "wait-backup", map[string]any{"conditions": []string{"job:backup"}, "reason": "Waiting for the backup to finish"})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait: %v", err)
	}
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("agent Work after wait: %v", got.State)
	}
	e.clock.Add(time.Minute)
	e.endJobRun("backup", 3, domain.ExecutionFailed)
	if got := e.work(w.ID); got.State != domain.WorkReady {
		t.Fatalf("the failed run did not wake the agent: %v", got.State)
	}
	var said string
	e.read(func(tx storage.Tx) error {
		wt, err := tx.Wait(out.Wait.ID)
		if err != nil {
			return err
		}
		wake, err := tx.Event(wt.WakeEventID)
		said, _ = gh.RenderEvent(wake.Event)
		return err
	})
	if !strings.Contains(said, "You were waiting for the next run of the backup job to end. That has happened: a run of the backup job ended: failed.") {
		t.Fatalf("agent told: %q", said)
	}
	// Publishing again is a no-op: one Event per run.
	before := e.countType("schedule.run.")
	e.publishJobRuns()
	if got := e.countType("schedule.run."); got != before || got != 3 {
		t.Fatalf("run-ended events: %d then %d", before, got)
	}
}
