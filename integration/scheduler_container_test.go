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

// pinningContainers stands in for the host's container binding: its
// container is pinned through the scheduler's pin before Launch returns.
type pinningContainers struct {
	pin func(extension.ContainerPin) error
}

func (b pinningContainers) Launch(_ context.Context, _ extension.SandboxNeeds, _ extension.SandboxLaunch) (extension.SandboxPlan, error) {
	c := extension.ContainerPin{Digest: "sha256:plan", Summary: "fake container", Image: "img@sha256:1", Daemon: "daemon-1", ID: "c1"}
	return extension.SandboxPlan{Argv: []string{"open-conduit", "container", "run", "c1"}, Digest: c.Digest, Signals: extension.SandboxTerm}, b.pin(c)
}
func (pinningContainers) Adopt(context.Context, bool) (extension.ContainerExit, error) {
	return extension.ContainerExit{}, nil
}

// TestProductionSchedulerContainerHook: core hands the host's container
// broker to the executor, records the container's pin (plan, image,
// daemon) with the open segment, carries it into a recovered segment,
// passes the result through the host's Settle, and a binding failure fails
// the attempt before any executor call.
func TestProductionSchedulerContainerHook(t *testing.T) {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if r.Context.Container != nil {
			if _, err := r.Context.Container.Launch(ctx, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"x"}}); err != nil {
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
	contained, broken := seedSchedulerWork(t, f, "contained"), seedSchedulerWork(t, f, "broken")
	var recovering []runtime.AttemptState
	hook := func(_ context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
		if state.LaunchState == "recovering" {
			recovering = append(recovering, state)
		}
		if state.WorkID == broken {
			return runtime.ContainerBinding{}, errors.New("docker missing")
		}
		settle := func(r extension.Result) extension.Result {
			r.Outcome = domain.Outcome{Kind: domain.OutcomeFailed, Summary: "interrupted"}
			return r
		}
		return runtime.ContainerBinding{Broker: pinningContainers{pin}, Resources: resources, Settle: settle}, nil
	}
	o := schedulerDefaultOptions(f, executor)
	o.Container = hook
	s := newTestScheduler(t, f, o)
	a, err := s.Start(context.Background(), contained)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.InspectAttempt(context.Background(), a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if pin := state.ContainerPin(); pin == nil || pin.ID != "c1" || pin.Image != "img@sha256:1" || pin.Daemon != "daemon-1" || pin.Digest != "sha256:plan" {
		t.Fatalf("segments %+v, want the container pinned to the open segment", state.Segments)
	}
	second := newTestScheduler(t, f, o)
	f.Clock.Advance(time.Minute)
	recovered, err := second.Recover(context.Background(), a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovering) != 1 || recovering[0].ContainerPin() == nil || recovering[0].ContainerPin().ID != "c1" {
		t.Fatalf("the container binding saw %+v while recovering, want the pin", recovering)
	}
	if err = second.Observe(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	done, err := s.InspectAttempt(context.Background(), a.Lease.ExecutionID)
	if err != nil || done.Result == nil || done.Result.Outcome.Summary != "interrupted" {
		t.Fatalf("result %+v %v, want the host's Settle applied", done.Result, err)
	}
	before := len(executor.runs)
	b, err := s.Start(context.Background(), broken)
	if !errors.Is(err, extension.ErrSandboxUnavailable) || len(executor.runs) != before {
		t.Fatalf("broken container: %v (runs %d -> %d)", err, before, len(executor.runs))
	}
	failed, err := s.InspectAttempt(context.Background(), b.Lease.ExecutionID)
	if err != nil || failed.Result == nil || failed.Result.Outcome.Summary != "container unavailable" || !failed.Excluded {
		t.Fatalf("broken container attempt: %+v %v", failed, err)
	}
}
