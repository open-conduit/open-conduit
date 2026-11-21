package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
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
	// swept is each executor's last sweep, which doctor reports.
	swept map[string]executorSweepRecord
}

// executorSweepRecord is one executor's last sweep: when it ran, what it
// released and kept, and how it failed.
type executorSweepRecord struct {
	at     time.Time
	report extension.StateSweepReport
	err    error
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
		s.next, s.done, s.swept = map[string]time.Time{}, map[string]bool{}, map[string]executorSweepRecord{}
	}
	var due []string
	for name, e := range executors {
		//duty:executor_state_sweep
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
			report, err := d.sweepExecutor(ctx, name, executors[name].(extension.StateSweeper))
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			if !errors.Is(err, extension.ErrUnsupported) {
				s.swept[name] = executorSweepRecord{at: d.now(), report: report, err: err}
			}
			if err == nil || errors.Is(err, extension.ErrUnsupported) {
				s.done[name] = true
			} else {
				s.next[name] = d.now().Add(d.o.SweepInterval)
			}
			s.mu.Unlock()
		}
	})
}

// sweepExecutor sweeps one executor against the coordination state and
// logs each execution whose state it released and each thing it kept.
func (d *daemon) sweepExecutor(ctx context.Context, name string, x extension.StateSweeper) (extension.StateSweepReport, error) {
	report, err := x.SweepState(ctx, func(ctx context.Context, ids []domain.ID) (map[domain.ID]bool, error) {
		if len(ids) > extension.MaxKnownBatch {
			return nil, extension.ErrInvalid
		}
		return knownIn(ctx, d.coord, d.now(), ids)
	})
	if errors.Is(err, extension.ErrUnsupported) {
		return report, err
	}
	for _, id := range report.Removed {
		d.log.Info("released executor state of an execution the coordination state doesn't know", "executor", name, "execution", string(id))
	}
	for _, k := range report.Kept {
		d.log.Info("executor kept state the coordination state doesn't know", "executor", name, "execution", string(k.Execution), "ref", k.Ref, "kept", k.Kept)
	}
	if err != nil {
		if ctx.Err() == nil {
			d.report("executor_sweep", domain.ID(name), errorCode(err), err.Error())
		}
		return report, err
	}
	d.clear("executor_sweep", domain.ID(name))
	return report, nil
}

// maxSweepItems bounds the items the executor_state check lists; the log
// has every one.
const maxSweepItems = 100

// executorStateCheck is doctor's view of each executor's last state sweep:
// the executions whose state it released, what it kept and why, and a
// sweep that failed.
func (d *daemon) executorStateCheck(context.Context) runtime.Check {
	s := &d.executorState
	s.mu.Lock()
	defer s.mu.Unlock()
	check := runtime.Check{Name: "executor_state", Status: runtime.CheckOK, Code: "ok", Detail: "no executor state swept yet"}
	if len(s.swept) == 0 {
		return check
	}
	names := make([]string, 0, len(s.swept))
	for name := range s.swept {
		names = append(names, name)
	}
	sort.Strings(names)
	var details []string
	items := 0
	add := func(item string) {
		if items < maxSweepItems {
			check.Items = append(check.Items, item)
		}
		items++
	}
	for _, name := range names {
		r := s.swept[name]
		detail := fmt.Sprintf("%s swept at %s: released %d execution(s), kept %d", name, r.at.UTC().Format(time.RFC3339), len(r.report.Removed), len(r.report.Kept))
		if r.err != nil {
			detail += " (failed: " + errorCode(r.err) + ")"
			check.Status, check.Code = runtime.CheckWarn, "sweep_failed"
		}
		details = append(details, detail)
		for _, id := range r.report.Removed {
			add(name + " released " + string(id))
		}
		for _, k := range r.report.Kept {
			item := name + " kept " + k.Ref + ": " + k.Kept
			if k.Execution != "" {
				item = name + " kept " + string(k.Execution) + " (" + k.Ref + "): " + k.Kept
			}
			add(item)
			if check.Status == runtime.CheckOK {
				check.Status, check.Code = runtime.CheckInfo, "state_kept"
			}
		}
	}
	if items > maxSweepItems {
		check.Items = append(check.Items, fmt.Sprintf("and %d more in the daemon log", items-maxSweepItems))
	}
	check.Detail = strings.Join(details, "; ")
	return check
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
