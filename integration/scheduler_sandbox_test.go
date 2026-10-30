package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// pinningBroker stands in for the host's sandbox binding: its plan is
// pinned through the scheduler's pin before it is returned.
type pinningBroker struct {
	pin func(extension.SandboxPlan) error
}

func (b pinningBroker) Plan(_ context.Context, _ extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	p := extension.SandboxPlan{Argv: l.Argv, Digest: "sha256:pinned", Signals: extension.SandboxTerm, Summary: "fake sandbox"}
	return p, b.pin(p)
}
func (pinningBroker) Adopt(context.Context, int) error { return nil }

// TestProductionSchedulerSandboxHook: core hands the host's sandbox broker
// to the executor, records the plan's digest with the attempt's open
// segment, carries it into a recovered segment for the host to check, and a
// binding failure fails the attempt before any executor call.
func TestProductionSchedulerSandboxHook(t *testing.T) {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if r.Context.Sandbox != nil {
			if _, err := extension.WrapLaunch(ctx, r.Context.Sandbox, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"x"}}); err != nil {
				return nil, err
			}
		}
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	executor.onRecover = func(_ context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
		if err := r.PersistRecovery(state); err != nil {
			return nil, err
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	sandboxed, broken := seedSchedulerWork(t, f, "sandboxed"), seedSchedulerWork(t, f, "broken")
	var recovering []runtime.AttemptState
	hook := func(_ context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.SandboxPlan) error) (extension.SandboxBroker, extension.ResourceBroker, error) {
		if state.LaunchState == "recovering" {
			recovering = append(recovering, state)
		}
		if state.WorkID == broken {
			return nil, nil, errors.New("srt missing")
		}
		return pinningBroker{pin}, resources, nil
	}
	o := schedulerDefaultOptions(f, executor)
	o.Sandbox = hook
	s := newTestScheduler(t, f, o)
	a, err := s.Start(context.Background(), sandboxed)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.InspectAttempt(context.Background(), a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if pin := state.SandboxPin(); pin == nil || pin.Digest != "sha256:pinned" || pin.Summary != "fake sandbox" {
		t.Fatalf("segments %+v, want the plan pinned to the open segment", state.Segments)
	}
	// A restarted host recovers the attempt: the recovered segment carries
	// the pin, so the host can check the process against it.
	second := newTestScheduler(t, f, o)
	f.Clock.Advance(time.Minute)
	recovered, err := second.Recover(context.Background(), a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovering) != 1 || recovering[0].SandboxPin() == nil || recovering[0].SandboxPin().Digest != "sha256:pinned" {
		t.Fatalf("the sandbox binding saw %+v while recovering, want the pinned digest", recovering)
	}
	if err = second.Observe(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	before := len(executor.runs)
	b, err := s.Start(context.Background(), broken)
	if !errors.Is(err, extension.ErrSandboxUnavailable) || len(executor.runs) != before {
		t.Fatalf("broken sandbox: %v (runs %d -> %d)", err, before, len(executor.runs))
	}
	failed, err := s.InspectAttempt(context.Background(), b.Lease.ExecutionID)
	if err != nil || failed.Result == nil || failed.Result.Outcome.Kind != domain.OutcomeFailed || failed.Result.Outcome.Summary != "sandbox unavailable" || !failed.Excluded {
		t.Fatalf("broken sandbox attempt: %+v %v", failed, err)
	}
}
