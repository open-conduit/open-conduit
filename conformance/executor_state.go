package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// StateSweepFixture drives an executor that sweeps its private state
// (extension.StateSweeper). Known and Unknown start attempts of two
// different executions that finish on their own; the sweeps say the host
// holds Known and not Unknown.
type StateSweepFixture struct {
	Executor       extension.Executor
	Known, Unknown extension.RunRequest
}

// CheckStateSweeper verifies, for an executor that sweeps its state:
//   - a sweep whose known call fails fails, and releases nothing: both
//     attempts can still be recovered;
//   - a sweep asks known about the unknown execution, in batches of at most
//     extension.MaxKnownBatch, and reports it removed, never the known one;
//   - after it, the known attempt still recovers with its result and the
//     unknown one is not adoptable (its state is gone);
//   - a second sweep removes nothing more.
func CheckStateSweeper(f StateSweepFixture) error {
	x, ok := f.Executor.(extension.StateSweeper)
	if !ok {
		return fmt.Errorf("executor does not sweep its state")
	}
	ctx := context.Background()
	known, unknown := f.Known.Execution.ID, f.Unknown.Execution.ID
	if known == "" || unknown == "" || known == unknown {
		return fmt.Errorf("fixture needs two different executions")
	}
	type attempt struct {
		r      extension.RunRequest
		state  domain.Payload
		result extension.Result
	}
	finish := func(r extension.RunRequest) (attempt, error) {
		a := attempt{r: r}
		a.r.PersistRecovery = func(s domain.Payload) error { a.state = s; return nil }
		h, err := f.Executor.Run(ctx, a.r)
		if err != nil {
			return a, err
		}
		a.result, err = h.Wait(ctx)
		return a, err
	}
	k, err := finish(f.Known)
	if err != nil {
		return fmt.Errorf("known attempt: %w", err)
	}
	u, err := finish(f.Unknown)
	if err != nil {
		return fmt.Errorf("unknown attempt: %w", err)
	}
	adopt := func(a attempt) error {
		r := a.r
		r.PersistRecovery = func(domain.Payload) error { return nil }
		h, err := f.Executor.Recover(ctx, r, a.state)
		if err != nil {
			return err
		}
		got, err := h.Wait(ctx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, a.result) {
			return fmt.Errorf("recovery changed the result")
		}
		return nil
	}

	failed := errors.New("known failed")
	if _, err := x.SweepState(ctx, func(context.Context, []domain.ID) (map[domain.ID]bool, error) { return nil, failed }); !errors.Is(err, failed) {
		return fmt.Errorf("a sweep whose known call failed did not fail with it: %v", err)
	}
	for _, a := range []attempt{k, u} {
		if err := adopt(a); err != nil {
			return fmt.Errorf("a sweep whose known call failed released %s: %v", a.r.Execution.ID, err)
		}
	}

	var asked []domain.ID
	oracle := func(_ context.Context, ids []domain.ID) (map[domain.ID]bool, error) {
		if len(ids) > extension.MaxKnownBatch {
			return nil, fmt.Errorf("asked about %d executions at once", len(ids))
		}
		asked = append(asked, ids...)
		return map[domain.ID]bool{known: true}, nil
	}
	report, err := x.SweepState(ctx, oracle)
	if err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	if !slices.Contains(asked, unknown) {
		return fmt.Errorf("the sweep never asked about %s", unknown)
	}
	if !slices.Contains(report.Removed, unknown) {
		return fmt.Errorf("the sweep did not report %s removed: %+v", unknown, report)
	}
	if slices.Contains(report.Removed, known) {
		return fmt.Errorf("the sweep removed %s, which the host holds", known)
	}
	if err := adopt(k); err != nil {
		return fmt.Errorf("the known attempt no longer recovers after a sweep: %v", err)
	}
	if err := adopt(u); !errors.Is(err, extension.ErrNotAdoptable) {
		return fmt.Errorf("the unknown attempt's state outlived the sweep: recover returned %v", err)
	}
	again, err := x.SweepState(ctx, oracle)
	if err != nil {
		return fmt.Errorf("second sweep: %w", err)
	}
	if len(again.Removed) > 0 {
		return fmt.Errorf("a second sweep removed %v", again.Removed)
	}
	return nil
}

// RunStateSweeper runs CheckStateSweeper on a fresh fixture.
func RunStateSweeper(t *testing.T, factory func() StateSweepFixture) {
	t.Helper()
	if err := CheckStateSweeper(factory()); err != nil {
		t.Fatal(err)
	}
}
