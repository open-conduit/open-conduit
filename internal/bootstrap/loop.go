package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

const passLimit = 100

// loop is the host driver. Every pass is idempotent and restart-safe: all
// progress is durable in coordination storage, so a crash at any point only
// repeats work that the runtime deduplicates or fences.
func (d *daemon) loop(ctx context.Context) error {
	ticker := time.NewTicker(d.o.PollInterval)
	defer ticker.Stop()
	var lastSweep time.Time
	// Events from before their scope was recorded get it, so a scope's
	// events are read from storage's index; a failure only leaves readers
	// resolving the rest one by one, as before.
	if n, err := d.current.Load().router.BackfillEventScopes(ctx); err != nil && ctx.Err() == nil {
		d.report("event_scope_backfill", "", errorCode(err))
	} else if n > 0 {
		d.log.Info("event scopes backfilled", "events", n)
	}
	// Attempts from before they recorded their executor get it, so Work is
	// selected by its executor from storage's index; a failure only leaves
	// those attempts out of an executor's Work.
	if n, err := d.current.Load().router.BackfillExecutors(ctx); err != nil && ctx.Err() == nil {
		d.report("executor_backfill", "", errorCode(err))
	} else if n > 0 {
		d.log.Info("attempt executors backfilled", "attempts", n)
	}
	// Open Work from before sources gave displays gets one from its
	// history, once per store; a failure only leaves it without.
	if n, err := d.current.Load().router.BackfillDisplays(ctx); err != nil && ctx.Err() == nil {
		d.report("display_backfill", "", errorCode(err))
	} else if n > 0 {
		d.log.Info("work displays backfilled from history", "work", n)
	}
	for {
		c := d.current.Load()
		d.routePending(ctx, c)
		poll, err := c.scheduler.PollClaims(ctx, c.timers, passLimit)
		if err != nil && ctx.Err() == nil {
			d.report("poll", "", errorCode(err))
		}
		for _, a := range poll.Recovered {
			d.attend(c, a, "")
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
		if err = c.scheduler.SettleBudgets(ctx); err != nil && ctx.Err() == nil {
			d.report("budgets", "", errorCode(err))
		}
		d.startReady(ctx)
		d.dispatchControls(ctx, c)
		if err = c.scheduler.ReconcileSubscriptions(ctx, passLimit); err != nil && ctx.Err() == nil {
			d.report("subscriptions", "", errorCode(err))
		}
		if _, err = d.ingress.ReconcileDue(ctx, passLimit); err != nil && ctx.Err() == nil {
			d.report("ingress", "", errorCode(err))
		}
		d.reconfigureOnURLChange(ctx)
		d.runPackageTasks(ctx)
		d.reconcileContainers(ctx)
		d.reconcileResources(c)
		d.reconcileEndpoints(ctx)
		d.sweepResources(c)
		d.retentionPass(ctx, c, &lastSweep)
		d.scheduleBackup(ctx)
		d.passed()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-d.nudge:
		}
		if p := removed(d.from); p != "" {
			d.log.Warn("stopping: the install this daemon runs from is gone", "path", p)
			return fmt.Errorf("%w (%s)", ErrInstallRemoved, p)
		}
	}
}

// retentionPass runs the retention sweep and artifact collection once
// RetentionInterval has passed since the last sweep. Both take the artifact
// store's exclusive gate, which a running backup holds shared for its whole
// artifact copy, and a waiting writer would also block every artifact read
// and write behind it. So while a backup runs the pass is skipped, leaving
// lastSweep where it was, and the first pass after the backup runs it. It
// reports whether it ran.
func (d *daemon) retentionPass(ctx context.Context, c *components, lastSweep *time.Time) bool {
	if d.now().Sub(*lastSweep) < d.o.RetentionInterval || d.backupRunning() {
		return false
	}
	if _, err := c.scheduler.SweepRetention(ctx, passLimit); err != nil && ctx.Err() == nil {
		d.report("retention", "", errorCode(err))
	} else {
		*lastSweep = d.now()
	}
	d.collectArtifacts(ctx)
	d.pruneReported()
	if c.toolbelt != nil {
		if _, err := c.toolbelt.SweepToolbelt(ctx, toolbeltGrace, passLimit); err != nil && ctx.Err() == nil {
			d.report("toolbelt_sweep", "", errorCode(err))
		}
	}
	return true
}

// toolbeltGrace is how long a revoked or expired token, and a finished
// execution's replay record, are kept before the retention pass deletes them.
const toolbeltGrace = 24 * time.Hour

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
				var re *runtime.RouteError
				if errors.As(err, &re) && re.DeadLettered {
					// Routing gave up: the Event is routed as failed, in
					// its Work's history, and holds nothing any more.
					d.log.Warn("event routed as failed", "event", string(re.EventID), "route", re.Route, "code", re.Code, "attempts", re.Attempts, "error", re.Err)
					d.clear("route", p.Event.Event.ID)
					continue
				}
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

func (d *daemon) startReady(ctx context.Context) {
	var ready, idle []domain.ID
	timed := map[string]bool{}
	if d.o.DryRun {
		timed = d.timedKinds()
	}
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		works, err := tx.LiveWorks()
		for _, w := range works {
			if w.State != domain.WorkReady {
				continue
			}
			// A dry run starts no scheduled job (v1); its triggers wait.
			if d.o.DryRun && timed[w.Kind] {
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
			} else {
				idle = append(idle, w.ID)
			}
		}
		return err
	})
	if err != nil {
		d.report("start", "", errorCode(err))
		return
	}
	// Idle Work is not waiting for a slot: drop any held-back marker its
	// last start left, so capacity stops counting it.
	for _, id := range idle {
		d.clear("start", id)
	}
	for _, id := range ready {
		if ctx.Err() != nil {
			return
		}
		// Load the generation immediately before each claim, so an attempt
		// claimed after a reload returned pins the new generation.
		c := d.current.Load()
		a, err := c.scheduler.Claim(ctx, id)
		if err != nil {
			d.report("start", id, errorCode(err))
			continue
		}
		d.clear("start", id)
		d.attend(c, a, id)
	}
}

// dispatchControls delivers committed inject/interrupt intents for running Work.
func (d *daemon) dispatchControls(ctx context.Context, c *components) {
	var running []domain.ID
	_ = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		works, err := tx.LiveWorks()
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

// attend holds a claimed attempt from its claim to its end, off the
// host's pass: it renews the lease from the claim (Scheduler.Hold), so a
// launch that takes many lease durations keeps it, launches the attempt
// (a start's Run or Resume, a recovery's adoption or relaunch) and then
// observes it. work is the Work a start claimed for ("" for a recovery),
// which a failed launch is reported against.
//
// A lost lease ends the hold's context, which stops a launch at once.
// Losing authority to an end (cancel, deadline, a failed observation)
// leads to a trusted physical stop and ConfirmStopped. A lease that merely
// lapsed, with the attempt still running, is left for Poll's recovery to
// adopt or, when its launch had started nothing, to relaunch, as is one a
// newer epoch already holds; daemon shutdown likewise only stops, leaving
// the detached process for the next daemon to adopt.
func (d *daemon) attend(c *components, a runtime.Attempt, work domain.ID) {
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
		ticker := time.NewTicker(d.o.LeaseDuration / 3)
		defer ticker.Stop()
		ctx, lost, stop := c.scheduler.Hold(d.ctx, a.Lease, ticker.C, func(err error) {
			d.report("renew", a.Lease.ExecutionID, errorCode(err))
		})
		defer stop()
		if !a.Launched() {
			launched, err := c.scheduler.Launch(ctx, a)
			if err != nil {
				d.launchFailed(c, a, work, lost, err)
				return
			}
			a = launched
		}
		d.observe(ctx, c, a, lost)
	}()
}

// launchFailed handles an attempt whose launch returned an error. A lost
// lease leaves it to recovery: with nothing pinned it is relaunched, with
// a process pinned that process is adopted. Shutdown leaves it for the next
// daemon. Otherwise the failure is reported, and a launch failure after the
// claim leaves an uncertain hold until the executor proves the attempt is
// not running.
func (d *daemon) launchFailed(c *components, a runtime.Attempt, work domain.ID, lost <-chan struct{}, err error) {
	select {
	case <-lost:
		d.log.Warn("lease lost while launching; leaving the attempt for recovery", "execution", string(a.Lease.ExecutionID), "epoch", a.Lease.Epoch)
		return
	default:
	}
	if d.ctx.Err() != nil {
		return
	}
	if work != "" {
		code := errorCode(err)
		d.report("start", work, code)
		if code == "executor_failed" || code == "invalid_state" {
			d.confirmStopped(d.ctx, c, a.Lease.ExecutionID, a.Lease.Epoch)
		}
		return
	}
	code := runtime.RecoveryDiagnostic(err)
	if code == "" {
		code = errorCode(err)
	}
	d.report("poll", a.Lease.ExecutionID, code)
	if code == "executor_recovery_failed" {
		d.confirmStopped(d.ctx, c, a.Lease.ExecutionID, 0)
	}
}

// observe waits for a launched attempt's result under its hold (ctx ends
// when the lease is lost, and lost is closed then).
func (d *daemon) observe(ctx context.Context, c *components, a runtime.Attempt, lost <-chan struct{}) {
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
	if errors.Is(err, storage.ErrLeaseLost) && c.scheduler.Adoptable(d.ctx, a.Lease) {
		// The lease lapsed (the host slept or stalled past it) but the
		// attempt still runs and nothing has ended it: Poll's recovery
		// adopts the process, or already has, so it is left running.
		d.log.Warn("lease lost; leaving the running attempt for recovery to adopt", "execution", string(a.Lease.ExecutionID), "epoch", a.Lease.Epoch)
		return
	}
	d.report("observe", a.Lease.ExecutionID, errorCode(err), runtime.FailureCause(err))
	d.confirmStopped(d.ctx, c, a.Lease.ExecutionID, a.Lease.Epoch)
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
	s, ok := d.executors[state.Executor].(extension.StopExecutor)
	if !ok {
		d.report("stop", id, "executor_cannot_prove_stop")
		return
	}
	// The executor's stop reaches its container as a TERM the host sent.
	d.containerSignalled(ctx, id, "stop")
	if err = s.Stop(ctx, id, epoch); err != nil {
		d.report("stop", id, "stop_unverified")
		return
	}
	// The executor's wrapper is stopped; its container must be too.
	if err = d.stopContainer(ctx, state); err != nil {
		d.report("stop", id, "container_stop_unverified")
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
	var capacity *runtime.CapacityError
	var route *runtime.RouteError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &route):
		// A retry still in its backoff reports the failure it follows,
		// so each failure is logged once.
		return "route_" + route.Code
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.As(err, &capacity):
		return capacityCode(capacity)
	case errors.Is(err, runtime.ErrCapacity):
		return "capacity"
	case errors.Is(err, runtime.ErrBudgetHeld):
		return "over_budget"
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
	case errors.Is(err, runtime.ErrIngressNamespaceOwned):
		return "ingress_namespace_owned"
	case errors.Is(err, runtime.ErrIngressNameTaken):
		return "ingress_name_taken"
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

// collectArtifacts removes artifact content no execution references once it
// is older than ArtifactGrace: orphans an interrupted publication left, or
// content published but never attached. Executors attach what they publish
// in the same result, so the grace far exceeds the publish-to-attach window.
func (d *daemon) collectArtifacts(ctx context.Context) {
	if d.artifacts == nil {
		return
	}
	live := map[string]bool{}
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions("")
		if err != nil {
			return err
		}
		for _, x := range xs {
			for _, ref := range x.Artifacts {
				live[ref.ID] = true
			}
		}
		return nil
	})
	if err == nil {
		err = d.artifacts.Collect(ctx, d.now().Add(-d.o.ArtifactGrace), live)
	}
	if err != nil && ctx.Err() == nil {
		d.report("artifacts", "", errorCode(err))
		return
	}
	d.clear("artifacts", "")
}

// reconcileEndpoints stops serving the toolbelt socket of every execution
// that is terminal (or gone) and whose process is confirmed stopped, and
// removes its run directory, including those a previous daemon left behind.
// A parked execution keeps its run directory: its resume serves the socket
// again with a fresh token. So does one that timed out or was cancelled
// until its stop is confirmed: its token is revoked already, so a call the
// process makes meanwhile is refused by the host and recorded, rather than
// failing in the process for want of a token file.
func (d *daemon) reconcileEndpoints(ctx context.Context) {
	scheduler := d.current.Load().scheduler
	err := d.endpoints.Reconcile(func(ids []domain.ID) (map[domain.ID]bool, error) {
		return scheduler.Holding(ctx, ids)
	})
	if err != nil && ctx.Err() == nil {
		d.report("endpoints", "", errorCode(err))
		return
	}
	d.clear("endpoints", "")
}
