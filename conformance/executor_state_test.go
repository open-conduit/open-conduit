package conformance_test

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func stateFixture(e extension.Executor) conformance.StateSweepFixture {
	req := func(id domain.ID) extension.RunRequest {
		return extension.RunRequest{Work: domain.Work{ID: "work"}, Execution: domain.Execution{ID: id, WorkID: "work"}}
	}
	return conformance.StateSweepFixture{Executor: e, Known: req("known"), Unknown: req("unknown")}
}

func completed() extension.Result {
	return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}
}

func TestReferenceStateSweeper(t *testing.T) {
	conformance.RunStateSweeper(t, func() conformance.StateSweepFixture {
		return stateFixture(&reference.StateExecutor{Result: completed()})
	})
}

// keepingSweeper asks about its state and keeps it all: the negative
// control for state that outlives every record of it.
type keepingSweeper struct{ reference.StateExecutor }

func (e *keepingSweeper) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	_, err := extension.KnownExecutions(ctx, known, e.Held())
	return extension.StateSweepReport{}, err
}

// claimingSweeper reports the unknown execution removed and keeps it.
type claimingSweeper struct{ reference.StateExecutor }

func (e *claimingSweeper) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	got, err := extension.KnownExecutions(ctx, known, e.Held())
	var report extension.StateSweepReport
	for _, id := range e.Held() {
		if !got[id] {
			report.Removed = append(report.Removed, id)
		}
	}
	return report, err
}

// greedySweeper releases everything, known or not.
type greedySweeper struct{ reference.StateExecutor }

func (e *greedySweeper) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	var report extension.StateSweepReport
	if _, err := extension.KnownExecutions(ctx, known, e.Held()); err != nil {
		return report, err
	}
	for _, id := range e.Held() {
		_ = e.Forget(ctx, id)
		report.Removed = append(report.Removed, id)
	}
	return report, nil
}

// openSweeper releases its state when known fails (fails open).
type openSweeper struct{ reference.StateExecutor }

func (e *openSweeper) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	report, err := e.StateExecutor.SweepState(ctx, known)
	if err != nil {
		for _, id := range e.Held() {
			_ = e.Forget(ctx, id)
		}
	}
	return report, err
}

func TestStateSweeperNegativeControls(t *testing.T) {
	for name, e := range map[string]extension.Executor{
		"keeps unknown state":         &keepingSweeper{reference.StateExecutor{Result: completed()}},
		"claims a removal it skipped": &claimingSweeper{reference.StateExecutor{Result: completed()}},
		"removes known state":         &greedySweeper{reference.StateExecutor{Result: completed()}},
		"fails open":                  &openSweeper{reference.StateExecutor{Result: completed()}},
		"does not sweep":              &reference.Executor{Result: completed()},
	} {
		if err := conformance.CheckStateSweeper(stateFixture(e)); err == nil {
			t.Errorf("an executor that %s passed", name)
		}
	}
}
