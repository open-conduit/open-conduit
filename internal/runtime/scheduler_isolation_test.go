package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// TestProductionSchedulerIsolationRequired: when the host says the
// attempt's scope requires isolation, a launch neither a sandbox nor a
// container confines fails closed before the executor is called, a
// recovered segment that ran unconfined is not adopted and is left
// unconfirmed for the host to stop, and a confined launch or an executor
// that runs no process is not refused. Without the requirement the same
// launch runs (the control).
func TestProductionSchedulerIsolationRequired(t *testing.T) {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	open, required, confined, recovered := seedSchedulerWork(t, f, "open"), seedSchedulerWork(t, f, "required"), seedSchedulerWork(t, f, "confined"), seedSchedulerWork(t, f, "recovered")
	requiring := map[domain.ID]bool{required: true, confined: true}
	var asked []domain.ID
	isolation := func(_ context.Context, state runtime.AttemptState) (domain.ID, error) {
		asked = append(asked, state.WorkID)
		if requiring[state.WorkID] {
			return "eng", nil
		}
		return "", nil
	}
	o := schedulerDefaultOptions(f, executor)
	o.Isolation = isolation
	o.Sandbox = func(_ context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.SandboxPlan) error) (extension.SandboxBroker, extension.ResourceBroker, error) {
		if state.WorkID == confined {
			return pinningBroker{Pin: pin}, resources, nil
		}
		return nil, resources, nil
	}
	s := newTestScheduler(t, f, o)
	ctx := context.Background()

	if _, err := s.Start(ctx, open); err != nil {
		t.Fatalf("control: %v", err)
	}
	before := len(executor.Runs)
	a, err := s.Start(ctx, required)
	if !errors.Is(err, runtime.ErrIsolationRequired) || len(executor.Runs) != before {
		t.Fatalf("unconfined launch in a requiring scope: %v (runs %d -> %d)", err, before, len(executor.Runs))
	}
	failed, err := s.InspectAttempt(ctx, a.Lease.ExecutionID)
	if err != nil || failed.Result == nil || failed.Result.Outcome.Kind != domain.OutcomeFailed || failed.Result.Outcome.Summary != "scope eng requires a sandbox or container" || !failed.Excluded {
		t.Fatalf("refused attempt: %+v %v", failed, err)
	}
	asked = nil
	if _, err = s.Start(ctx, confined); err != nil || len(asked) != 0 {
		t.Fatalf("sandboxed launch: %v (asked %v)", err, asked)
	}

	// The attempt launched unconfined before the requirement; the host
	// that recovers it has the requirement.
	b, err := s.Start(ctx, recovered)
	if err != nil {
		t.Fatal(err)
	}
	requiring[recovered] = true
	second := newTestScheduler(t, f, o)
	f.Clock.Advance(time.Minute)
	recoveries := len(executor.Recoveries)
	if _, err = second.Recover(ctx, b.Lease.ExecutionID); !errors.Is(err, runtime.ErrIsolationRequired) || len(executor.Recoveries) != recoveries {
		t.Fatalf("unconfined recovery: %v (recoveries %d -> %d)", err, recoveries, len(executor.Recoveries))
	}
	stopped, err := second.InspectAttempt(ctx, b.Lease.ExecutionID)
	if err != nil || stopped.Result == nil || stopped.Result.Outcome.Kind != domain.OutcomeFailed || stopped.Excluded {
		t.Fatalf("refused recovery: %+v %v, want failed and left for the host to stop", stopped, err)
	}

	// An executor that runs no process isn't asked.
	executor.Caps.NoProcess = true
	asked = nil
	again := seedSchedulerWork(t, f, "again")
	requiring[again] = true
	if _, err = newTestScheduler(t, f, o).Start(ctx, again); err != nil || len(asked) != 0 {
		t.Fatalf("no-process executor: %v (asked %v)", err, asked)
	}
}
