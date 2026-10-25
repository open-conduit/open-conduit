package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/internal/runtime"
)

type opaqueBroker struct{ owner runtime.AttemptState }

func (opaqueBroker) Acquire(context.Context, extension.ResourceRequest) (extension.Resource, error) {
	return extension.Resource{}, nil
}
func (opaqueBroker) Lookup(context.Context, string) (extension.Resource, error) {
	return extension.Resource{}, extension.ErrResourceNotFound
}

// TestProductionSchedulerResourceHook: core hands the host-bound broker to the
// executor without inspecting it, gives none when the host binds none, and a
// binding failure fails the attempt before any executor call.
func TestProductionSchedulerResourceHook(t *testing.T) {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	got := make(chan extension.ResourceBroker, 4)
	executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		got <- r.Context.Resources
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	bound, none, broken := seedSchedulerWork(t, f, "bound"), seedSchedulerWork(t, f, "none"), seedSchedulerWork(t, f, "broken")
	o := schedulerDefaultOptions(f, executor)
	o.Resources = func(_ context.Context, state runtime.AttemptState) (extension.ResourceBroker, error) {
		switch state.WorkID {
		case bound:
			return opaqueBroker{state}, nil
		case broken:
			return nil, errors.New("provider missing")
		}
		return nil, nil
	}
	s := newTestScheduler(t, f, o)
	for _, id := range []domain.ID{bound, none} {
		a, err := s.Start(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Observe(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if b, ok := (<-got).(opaqueBroker); !ok || b.owner.WorkID != bound || b.owner.ExecutionID == "" {
		t.Fatalf("bound attempt got broker %#v", b)
	}
	if b := <-got; b != nil {
		t.Fatalf("attempt without a resource got %#v", b)
	}
	before := len(executor.runs)
	a, err := s.Start(context.Background(), broken)
	if !errors.Is(err, runtime.ErrResourceUnavailable) || len(executor.runs) != before {
		t.Fatalf("broken binding: %v (runs %d -> %d)", err, before, len(executor.runs))
	}
	state, err := s.InspectAttempt(context.Background(), a.Lease.ExecutionID)
	if err != nil || state.Result == nil || state.Result.Outcome.Kind != domain.OutcomeFailed || !state.Excluded {
		t.Fatalf("broken binding attempt: %+v %v", state, err)
	}
}
