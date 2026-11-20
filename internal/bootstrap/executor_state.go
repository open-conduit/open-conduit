package bootstrap

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// executorStateSweep is the host side of extension.StateSweeper: each
// running executor that sweeps is asked once, when the daemon starts or
// its package first runs, to release the private state of executions the
// coordination state doesn't know (those from after the bundle an
// in-place restore went back to, say). A sweep that fails is asked again
// after SweepInterval.
type executorStateSweep struct {
	mu       sync.Mutex
	sweeping bool
	// next is when each executor is next due: the zero time once swept.
	next map[string]time.Time
	done map[string]bool
}

// sweepExecutorState runs the due executors' sweeps in the background.
func (d *daemon) sweepExecutorState(ctx context.Context) {
	s := &d.executorState
	executors := d.executorMap()
	s.mu.Lock()
	if s.sweeping {
		s.mu.Unlock()
		return
	}
	if s.next == nil {
		s.next, s.done = map[string]time.Time{}, map[string]bool{}
	}
	var due []string
	for name, e := range executors {
		if _, ok := e.(extension.StateSweeper); !ok || s.done[name] || d.now().Before(s.next[name]) {
			continue
		}
		due = append(due, name)
	}
	if len(due) == 0 {
		s.mu.Unlock()
		return
	}
	s.sweeping = true
	s.mu.Unlock()
	sort.Strings(due)
	d.goSafe("executor_sweep", func() {
		defer func() {
			s.mu.Lock()
			s.sweeping = false
			s.mu.Unlock()
		}()
		for _, name := range due {
			err := d.sweepExecutor(ctx, name, executors[name].(extension.StateSweeper))
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			if err == nil {
				s.done[name] = true
			} else {
				s.next[name] = d.now().Add(d.o.SweepInterval)
			}
			s.mu.Unlock()
		}
	})
}

// sweepExecutor sweeps one executor against the coordination state and
// logs what it released and kept.
func (d *daemon) sweepExecutor(ctx context.Context, name string, x extension.StateSweeper) error {
	report, err := x.SweepState(ctx, func(ctx context.Context, ids []domain.ID) (map[domain.ID]bool, error) {
		if len(ids) > extension.MaxKnownBatch {
			return nil, extension.ErrInvalid
		}
		return knownIn(ctx, d.coord, d.now(), ids)
	})
	if errors.Is(err, extension.ErrUnsupported) {
		return nil
	}
	if len(report.Removed) > 0 {
		d.log.Info("released executor state of executions the coordination state doesn't know", "executor", name, "removed", len(report.Removed))
	}
	for _, k := range report.Kept {
		d.log.Info("executor kept state the coordination state doesn't know", "executor", name, "execution", string(k.Execution), "ref", k.Ref, "kept", k.Kept)
	}
	if err != nil {
		if ctx.Err() == nil {
			d.report("executor_sweep", domain.ID(name), errorCode(err), err.Error())
		}
		return err
	}
	d.clear("executor_sweep", domain.ID(name))
	return nil
}

// knownIn reports which of ids the coordination state holds.
func knownIn(ctx context.Context, store storage.Store, now time.Time, ids []domain.ID) (map[domain.ID]bool, error) {
	var known map[domain.ID]bool
	err := storage.View(ctx, store, now, func(tx storage.Tx) error {
		known = map[domain.ID]bool{}
		for _, id := range ids {
			_, err := tx.Execution(id)
			switch {
			case err == nil:
				known[id] = true
			case !errors.Is(err, storage.ErrNotFound):
				return err
			}
		}
		return nil
	})
	return known, err
}
