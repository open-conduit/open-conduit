package bootstrap

import (
	"context"
	"errors"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	sc "github.com/alsiola/open-conduit/extensions/schedule"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
)

const passLimit = 100

// loop is the host driver. Every pass is idempotent and restart-safe: all
// progress is durable in coordination storage, so a crash at any point only
// repeats work that the runtime deduplicates or fences.
func (d *daemon) loop(ctx context.Context) error {
	ticker := time.NewTicker(d.o.PollInterval)
	defer ticker.Stop()
	var lastSweep time.Time
	for {
		c := d.current.Load()
		d.routePending(ctx, c)
		poll, err := c.scheduler.Poll(ctx, c.timers, passLimit)
		if err != nil && ctx.Err() == nil {
			d.report("poll", "", errorCode(err))
		}
		for _, a := range poll.Recovered {
			d.observe(c, a)
		}
		for _, diag := range poll.Diagnostics {
			d.report("poll", diag.ID, diag.Code)
			if diag.Code == "executor_recovery_failed" {
				d.confirmStopped(ctx, c, diag.ID, 0)
			}
		}
		if len(poll.Timers) > 0 {
			d.routePending(ctx, c)
		}
		d.startReady(ctx, c)
		d.dispatchControls(ctx, c)
		if err = c.scheduler.ReconcileSubscriptions(ctx, passLimit); err != nil && ctx.Err() == nil {
			d.report("subscriptions", "", errorCode(err))
		}
		if _, err = d.ingress.ReconcileDue(ctx, passLimit); err != nil && ctx.Err() == nil {
			d.report("ingress", "", errorCode(err))
		}
		d.translateSchedule(ctx)
		d.publishJobRuns(ctx)
		if snapshot, _ := d.configs.Current(); snapshot != nil {
			d.ensureSchedules(ctx, c, snapshot, false)
			d.ensureIntrospection(ctx, c, snapshot, false)
		}
		d.hostWatch(ctx)
		d.projectGitHub(ctx)
		d.projectTrackers(ctx)
		d.runSlackSockets(ctx)
		d.projectSlack(ctx)
		d.manageHooks(ctx)
		d.reconcileResources(c)
		d.maintainClaudeCode(ctx)
		if d.now().Sub(lastSweep) >= d.o.RetentionInterval {
			if _, err = c.scheduler.SweepRetention(ctx, passLimit); err != nil && ctx.Err() == nil {
				d.report("retention", "", errorCode(err))
			} else {
				lastSweep = d.now()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-d.nudge:
		}
	}
}

// routePending routes every accepted but unrouted Event in acceptance order,
// including Events accepted by a timer fire or an execution's emission.
func (d *daemon) routePending(ctx context.Context, c *components) {
	var after uint64
	for ctx.Err() == nil {
		page, err := d.ingress.Pending(ctx, after, 200)
		if err != nil {
			d.report("route", "", errorCode(err))
			return
		}
		for _, p := range page.Items {
			if p.Diagnostic != "" {
				d.report("route", p.Event.Event.ID, p.Diagnostic)
				continue
			}
			if err = c.router.Route(ctx, p.Event.Event.ID); err != nil {
				d.report("route", p.Event.Event.ID, errorCode(err))
			} else {
				d.clear("route", p.Event.Event.ID)
			}
		}
		if !page.More {
			return
		}
		after = page.NextSequence
	}
}

func (d *daemon) startReady(ctx context.Context, c *components) {
	var ready []domain.ID
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		for _, w := range works {
			if w.State != domain.WorkReady {
				continue
			}
			// A dry run starts no scheduled job (v1); its triggers wait.
			if d.o.DryRun && w.Kind == sc.WorkKind {
				continue
			}
			// Idle Work (handed back with no input yet) waits quietly for
			// its next event instead of reporting invalid_state every pass.
			inputs, e := runtime.PendingInputsInTx(tx, w.ID)
			if e != nil {
				return e
			}
			if len(inputs) > 0 {
				ready = append(ready, w.ID)
			}
		}
		return err
	})
	if err != nil {
		d.report("start", "", errorCode(err))
		return
	}
	for _, id := range ready {
		if ctx.Err() != nil {
			return
		}
		// Load the generation immediately before each claim, so an attempt
		// claimed after a reload returned pins the new generation.
		c = d.current.Load()
		a, err := c.scheduler.Start(ctx, id)
		if err != nil {
			code := errorCode(err)
			d.report("start", id, code)
			// A launch failure after claim leaves an uncertain hold until the
			// executor proves the attempt is not running.
			if a.Lease.ExecutionID != "" && (code == "executor_failed" || code == "invalid_state") {
				d.confirmStopped(ctx, c, a.Lease.ExecutionID, a.Lease.Epoch)
			}
			continue
		}
		d.clear("start", id)
		d.observe(c, a)
	}
}

// dispatchControls delivers committed inject/interrupt intents for running Work.
func (d *daemon) dispatchControls(ctx context.Context, c *components) {
	var running []domain.ID
	_ = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		for _, w := range works {
			if w.State == domain.WorkRunning {
				running = append(running, w.ID)
			}
		}
		return err
	})
	for _, id := range running {
		intents, err := c.router.Intents(ctx, id)
		if err != nil {
			continue
		}
		for _, intent := range intents {
			if err = c.scheduler.DispatchControl(ctx, intent.ID); err != nil {
				d.report("control", domain.ID(intent.ID), errorCode(err))
			}
		}
	}
}

// observe waits for an attempt's result while renewing its lease. Losing
// authority (cancel, deadline, supersession) or an observation failure leads
// to a trusted physical stop and ConfirmStopped; daemon shutdown only stops
// observing, leaving the detached process for the next daemon to adopt.
func (d *daemon) observe(c *components, a runtime.Attempt) {
	key := attemptKey{a.Lease.ExecutionID, a.Lease.Epoch}
	d.mu.Lock()
	if _, ok := d.inflight[key]; ok {
		d.mu.Unlock()
		return
	}
	d.inflight[key] = a.Lease
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.inflight, key)
			d.mu.Unlock()
			d.poke()
		}()
		ctx, cancel := context.WithCancel(d.ctx)
		defer cancel()
		lost := make(chan struct{})
		go func() {
			interval := d.o.LeaseDuration / 3
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := c.scheduler.Renew(ctx, a.Lease); err != nil && ctx.Err() == nil {
						if errors.Is(err, storage.ErrLeaseLost) || errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrInvalid) || errors.Is(err, storage.ErrNotFound) {
							close(lost)
							cancel()
							return
						}
						d.report("renew", a.Lease.ExecutionID, errorCode(err))
					}
				}
			}
		}()
		err := c.scheduler.Observe(ctx, a)
		for i := 0; i < 5 && errors.Is(err, storage.ErrConflict); i++ {
			// Completion cannot overtake accepted unrouted input (such as the
			// execution's own emission): route it, then reapply the result.
			d.routePending(ctx, d.current.Load())
			err = c.scheduler.Observe(ctx, a)
		}
		if err != nil {
			select {
			case <-lost:
				err = storage.ErrLeaseLost
			default:
			}
		}
		if err == nil {
			d.clear("observe", a.Lease.ExecutionID)
			return
		}
		if d.ctx.Err() != nil {
			return // shutdown: keep the process for adoption
		}
		d.report("observe", a.Lease.ExecutionID, errorCode(err))
		d.confirmStopped(d.ctx, c, a.Lease.ExecutionID, a.Lease.Epoch)
	}()
}

// stopper is the trusted physical-stop evidence a host executor provides.
type stopper interface {
	Stop(context.Context, domain.ID, uint64) error
}

// confirmStopped proves an attempt is not running before releasing its
// resource hold. An epoch of zero uses the attempt's current epoch. A
// superseded epoch is refused by the executor and confirms nothing.
func (d *daemon) confirmStopped(ctx context.Context, c *components, id domain.ID, epoch uint64) {
	state, err := c.scheduler.InspectAttempt(ctx, id)
	if err != nil || state.Excluded {
		return
	}
	if epoch == 0 {
		epoch = state.Epoch
	}
	if epoch != state.Epoch {
		return
	}
	s, ok := d.executors[state.Executor].(stopper)
	if !ok {
		d.report("stop", id, "executor_cannot_prove_stop")
		return
	}
	if err = s.Stop(ctx, id, epoch); err != nil {
		d.report("stop", id, "stop_unverified")
		return
	}
	if err = c.scheduler.ConfirmStopped(ctx, id, epoch); err != nil && !errors.Is(err, storage.ErrConflict) {
		d.report("stop", id, errorCode(err))
		return
	}
	d.clear("stop", id)
}

func (d *daemon) releaseAll(ctx context.Context) {
	d.mu.Lock()
	refs := make([]domain.LeaseRef, 0, len(d.inflight))
	for _, ref := range d.inflight {
		refs = append(refs, ref)
	}
	d.mu.Unlock()
	c := d.current.Load()
	for _, ref := range refs {
		_ = c.scheduler.Release(ctx, ref)
	}
}

// errorCode maps runtime errors to safe stable diagnostic codes.
func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, runtime.ErrCapacity):
		return "capacity"
	case errors.Is(err, runtime.ErrNotEligible):
		return "not_eligible"
	case errors.Is(err, runtime.ErrSecretUnavailable):
		return "secret_unavailable"
	case errors.Is(err, runtime.ErrCausalRejected):
		return "causal_rejected"
	case errors.Is(err, extension.ErrNotAdoptable), errors.Is(err, runtime.ErrExecutorCall):
		return "executor_failed"
	case errors.Is(err, extension.ErrUnsupported):
		return "executor_unavailable"
	case errors.Is(err, extension.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, storage.ErrClaimed), errors.Is(err, storage.ErrLeaseLost):
		return "authority_unavailable"
	case errors.Is(err, storage.ErrConflict):
		return "conflict"
	case errors.Is(err, storage.ErrNotFound):
		return "not_found"
	case errors.Is(err, storage.ErrInvalid), errors.Is(err, extension.ErrInvalid):
		return "invalid_state"
	}
	return "internal"
}
