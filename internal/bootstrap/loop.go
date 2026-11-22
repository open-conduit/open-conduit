package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

const passLimit = 100

// loop is the host driver. Every pass is idempotent and restart-safe: all
// progress is durable in coordination storage, so a crash at any point only
// repeats work that the runtime deduplicates or fences.
func (d *daemon) loop(ctx context.Context) error {
	//duty:host_loop
	ticker := time.NewTicker(d.o.PollInterval)
	defer ticker.Stop()
	if d.passTimes == nil {
		d.passTimes, _ = newPassTimes(nil)
	}
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
		// The pass keeps the phases that only read and write storage. Those
		// that wait on another process (a provider's page, an executor's
		// inject or interrupt, a package's task, a container runtime) only
		// hand their work to workers of their own here (workers.go).
		pass := d.passTimes.begin()
		pass.phase("route", func() { d.routePending(ctx, c) })
		pass.phase("poll", func() { d.pollPass(ctx, c) })
		pass.phase("admission", func() { d.admit(ctx, c) })
		pass.phase("start", func() { d.startReady(ctx) })
		pass.phase("controls", func() { d.dispatchControls(ctx, c) })
		pass.phase("subscriptions", func() {
			if err := c.scheduler.ReconcileSubscriptions(ctx, passLimit); ctx.Err() == nil {
				d.reportSubscriptions(err)
			}
		})
		pass.phase("ingress", func() { d.reconcileIngress(ctx) })
		pass.phase("packages", func() {
			d.reconfigureOnURLChange(ctx)
			d.runPackageTasks(ctx)
			d.reconcileContainers(ctx)
			d.reconcileResources(c)
			d.drainPackages(ctx)
			d.reapValidation()
		})
		pass.phase("endpoints", func() {
			d.reconcileEndpoints(ctx)
			d.sweepResources(c)
			d.sweepExecutorState(ctx)
		})
		pass.phase("retention", func() { d.retentionPass(ctx, c) })
		pass.phase("backup", func() { d.scheduleBackup(ctx) })
		pass.done(ctx)
		d.passDone()
		d.recordStartup(ctx)
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

// pollPass runs Scheduler.PollClaims: recovered attempts go to their
// holders, unconfirmed stops to their follow-ups, and Events from fired
// timers are routed in the same pass. Budgets are settled after it.
func (d *daemon) pollPass(ctx context.Context, c *components) {
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
	for _, due := range poll.Stops {
		d.followUpStop(ctx, c, due)
	}
	if len(poll.Timers) > 0 {
		d.routePending(ctx, c)
	}
	if err = c.scheduler.SettleBudgets(ctx); err != nil && ctx.Err() == nil {
		d.report("budgets", "", errorCode(err))
	}
}

// retentionPass runs the retention sweep and artifact collection once
// RetentionInterval has passed since the last sweep, by its durable due time
// (duties.go), so a daemon restarted more often still sweeps. It runs during a
// backup too: the backup's retention hold defers the sweep's deletes
// (runtime.HoldRetention), and collection never deletes content an attempt
// references. It reports whether it ran.
func (d *daemon) retentionPass(ctx context.Context, c *components) bool {
	if !d.dutyDue(dutyRetention) {
		return false
	}
	d.retaining.Store(true)
	defer d.retaining.Store(false)
	if _, err := c.scheduler.SweepRetention(ctx, passLimit); err != nil && ctx.Err() == nil {
		d.report("retention", "", errorCode(err))
	} else {
		d.dutyRan(ctx, dutyRetention, d.now().Add(d.o.RetentionInterval))
	}
	d.archiveEvents(ctx, c)
	d.compactDatabase(ctx)
	d.collectArtifacts(ctx, c)
	d.pruneReported()
	if c.toolbelt != nil {
		if _, err := c.toolbelt.SweepToolbelt(ctx, toolbeltGrace, passLimit); err != nil && ctx.Err() == nil {
			d.report("toolbelt_sweep", "", errorCode(err))
		}
	}
	if c.server != nil {
		if _, err := c.server.SweepOperators(ctx, toolbeltGrace, passLimit); err != nil && ctx.Err() == nil {
			d.report("operator_sweep", "", errorCode(err))
		}
	}
	return true
}

// archivePages bounds how many pages of the live log one retention pass
// looks at, so a long backlog moves over several passes and never holds up
// the host loop.
const archivePages = 10

// archiveEvents moves finished history to the event archive when
// settings.archive_finished_days turns it on, and leaves a trail of what
// moved and what stayed.
func (d *daemon) archiveEvents(ctx context.Context, c *components) {
	if d.events == nil || d.configs == nil {
		return
	}
	if s, _ := d.configs.Current(); s != nil {
		d.runArchivePass(ctx, c.scheduler, s.Settings)
	}
}

// eventArchiver is the runtime's archive pass.
type eventArchiver interface {
	ArchiveEvents(context.Context, runtime.ArchivePolicy, int) (runtime.ArchivePass, error)
}

// archivePolicy is what the settings let leave the live log at now, and
// whether they turn archiving on.
func archivePolicy(s config.Settings, now time.Time) (runtime.ArchivePolicy, bool) {
	if s.ArchiveFinishedDays == 0 {
		return runtime.ArchivePolicy{}, false
	}
	unmatched := s.ArchiveUnmatchedDays
	if unmatched == 0 {
		unmatched = s.ArchiveFinishedDays
	}
	return runtime.ArchivePolicy{FinishedBefore: now.Add(-keptFinished(s.ArchiveFinishedDays)), UnmatchedBefore: now.Add(-keptFinished(unmatched))}, true
}

func (d *daemon) runArchivePass(ctx context.Context, archiver eventArchiver, s config.Settings) {
	policy, on := archivePolicy(s, d.now())
	if !on {
		return
	}
	var scanned, archived int
	kept := map[string]int{}
	for range archivePages {
		pass, err := archiver.ArchiveEvents(ctx, policy, passLimit)
		if err != nil {
			if ctx.Err() == nil {
				d.report("event_archive", "", errorCode(err))
			}
			break
		}
		d.clear("event_archive", "")
		scanned += pass.Scanned
		archived += pass.Archived
		for why, n := range pass.Kept {
			kept[why] += n
		}
		if pass.Wrapped {
			break
		}
	}
	if scanned == 0 {
		return
	}
	d.log.Info("event archive pass", "scanned", scanned, "archived", archived, "kept", kept)
	d.capture(ctx, "events.archived", map[string]any{"scanned": scanned, "archived": archived, "kept": kept})
}

// How the database's free space is given back. SQLite reuses free pages
// before growing, so a margin stays free rather than being returned and
// allocated again, and one pass moves a bounded amount so it never holds
// writes up for long.
const (
	compactKeepFree  = 16 << 20
	compactPassLimit = 64 << 20
	rebuildMinFree   = 64 << 20
	rebuildFreeShare = 4 // at least a quarter of the file
	compactKeepShare = 20
)

// reclaimBytes is how much of a database's free space one pass returns.
func reclaimBytes(s storage.Space) int64 {
	if !s.Incremental {
		return 0
	}
	return min(max(s.FreeBytes-max(compactKeepFree, s.TotalBytes/compactKeepShare), 0), compactPassLimit)
}

// rebuildDue says a database that can't return space while running holds
// enough free space to justify the one rewrite that lets it.
func rebuildDue(s storage.Space) bool {
	return !s.Incremental && s.FreeBytes >= rebuildMinFree && s.FreeBytes*rebuildFreeShare >= s.TotalBytes
}

// compactDatabase returns free space to the disk once archiving is on.
func (d *daemon) compactDatabase(ctx context.Context) {
	if d.compactor == nil || d.configs == nil {
		return
	}
	if s, _ := d.configs.Current(); s == nil || s.Settings.ArchiveFinishedDays == 0 {
		return
	}
	d.runCompaction(ctx, d.compactor)
}

func (d *daemon) runCompaction(ctx context.Context, c storage.Compactor) {
	space, err := c.Space(ctx)
	if err == nil {
		var returned int64
		if want := reclaimBytes(space); want > 0 {
			if returned, err = c.Reclaim(ctx, want); err == nil && returned > 0 {
				d.log.Info("database space returned", "bytes", returned, "file_bytes", space.TotalBytes-returned)
				d.capture(ctx, "database.compacted", map[string]any{"kind": "incremental", "bytes": returned, "file_bytes": space.TotalBytes - returned})
			}
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			d.report("database_compaction", "", errorCode(err))
		}
		return
	}
	d.clear("database_compaction", "")
}

// rebuildAtStart makes a database from before incremental compaction able to
// return space, when archiving has left enough free to be worth it. It runs
// before the daemon serves, because it holds up every write until it is done.
// Free pages are reused meanwhile, so a failure (a disk without room for the
// copy, say) costs nothing but the space: it is logged, and the start goes on.
func (d *daemon) rebuildAtStart(ctx context.Context, c storage.Compactor, s config.Settings) {
	if c == nil || s.ArchiveFinishedDays == 0 {
		return
	}
	space, err := c.Space(ctx)
	if err == nil && rebuildDue(space) {
		d.log.Info("rebuilding the database to return free space; writes wait until it finishes", "file_bytes", space.TotalBytes, "free_bytes", space.FreeBytes)
		if err = c.Rebuild(ctx); err == nil {
			after, spaceErr := c.Space(ctx)
			if spaceErr != nil {
				err = spaceErr
			} else {
				d.log.Info("database rebuilt", "file_bytes", after.TotalBytes)
				d.capture(ctx, "database.compacted", map[string]any{"kind": "rebuild", "bytes": space.TotalBytes - after.TotalBytes, "file_bytes": after.TotalBytes})
			}
		}
	}
	if err != nil && ctx.Err() == nil {
		d.log.Warn("database not rebuilt: free space stays in the file and is reused", "error", err)
	}
}

// eventArchiveCheck is doctor's view of the archive pass, from the record the
// live database keeps. It says what the last full pass over the log left
// live and why, which is how a Wait pinning old Events shows up.
func (d *daemon) eventArchiveCheck(ctx context.Context) runtime.Check {
	const name = "event_archive"
	if d.configs == nil || d.coord == nil {
		return runtime.Check{Name: name, Status: runtime.CheckInfo, Code: "off", Detail: "the event archive isn't in use"}
	}
	s, _ := d.configs.Current()
	if s == nil || s.Settings.ArchiveFinishedDays == 0 {
		return runtime.Check{Name: name, Status: runtime.CheckInfo, Code: "off", Detail: "settings.archive_finished_days is unset, so Events stay in the live database"}
	}
	var r runtime.ArchiveRecord
	if err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		var err error
		r, err = runtime.ArchiveRecordInTx(tx)
		return err
	}); err != nil {
		return runtime.Check{Name: name, Status: runtime.CheckWarn, Code: errorCode(err), Detail: "can't read the archive record"}
	}
	if code := d.reportedCode("event_archive", ""); code != "" {
		return runtime.Check{Name: name, Status: runtime.CheckWarn, Code: code, Detail: fmt.Sprintf("the last archive pass failed; %d Events archived so far", r.Archived)}
	}
	items := make([]string, 0, len(r.LastLap))
	for why, n := range r.LastLap {
		items = append(items, fmt.Sprintf("%s: %d", why, n))
	}
	slices.Sort(items)
	return runtime.Check{Name: name, Status: runtime.CheckOK, Code: "archiving", Detail: fmt.Sprintf("%d Events archived; the last full pass left these live", r.Archived), Items: items}
}

// toolbeltGrace is how long a revoked or expired token, a finished
// execution's replay record and an expired operator credential are kept
// before the retention pass deletes them.
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

// admit runs the admission pass (Scheduler.Admit): open Work beyond its
// scope's admission_limit is held, and held Work is let in as places free.
func (d *daemon) admit(ctx context.Context, c *components) {
	r, err := c.scheduler.Admit(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.report("admission", "", errorCode(err))
		}
		return
	}
	d.clear("admission", "")
	if len(r.Held) > 0 || len(r.Released) > 0 {
		d.log.Info("admission", "held", len(r.Held), "released", len(r.Released))
	}
}

func (d *daemon) startReady(ctx context.Context) {
	var ready, idle []domain.ID
	timed := map[string]bool{}
	if d.o.DryRun {
		timed = d.timedKinds()
	}
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		ready, idle = nil, nil
		// A drain starts nothing. Input stays queued for whichever daemon
		// next runs on this state.
		if _, draining, err := runtime.DrainInTx(tx); err != nil || draining {
			return err
		}
		works, err := tx.LiveWorks()
		for _, w := range works {
			if w.State != domain.WorkReady {
				continue
			}
			// A dry run starts no scheduled job; its triggers wait.
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
			d.report("start", id, errorCode(err), heldBy(err))
			continue
		}
		d.clear("start", id)
		d.attend(c, a, id)
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
			// Attendance that ended with the shutdown leaves the attempt
			// running for the next daemon; releaseAll releases its lease
			// once every attendance has ended. One that had already
			// finished is refused by the lease fence, harmlessly.
			if d.ctx.Err() != nil {
				d.leftForNext = append(d.leftForNext, a.Lease)
			}
			d.mu.Unlock()
			d.poke()
		}()
		//duty:lease_renewal
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
		cause := heldBy(err)
		if cause == "" {
			cause = runtime.FailureCause(err)
		}
		d.report("start", work, code, cause)
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
	result, err := c.scheduler.Await(ctx, a)
	// The handle answers once. Whether it has (with a result or an
	// executor error) decides how a stop collects the stopped turn: with
	// the result already in hand, or by waiting for it after the stop
	// when the hold ended before any answer (#647).
	stopped := &stoppedTurn{attempt: a}
	if err == nil {
		stopped.result = &result
	} else {
		stopped.answered = ctx.Err() == nil || !errors.Is(err, ctx.Err())
	}
	if err == nil {
		err = c.scheduler.Complete(ctx, a.Lease, result)
		for i := 0; i < 5 && errors.Is(err, storage.ErrConflict); i++ {
			// Completion cannot overtake accepted unrouted input (such as
			// the execution's own emission): route it, then apply the
			// result again. The handle gives its result only once, so it
			// is the same result, never a second wait.
			d.routePending(ctx, d.current.Load())
			err = c.scheduler.Complete(ctx, a.Lease, result)
		}
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
	d.stopAttempt(d.ctx, c, a.Lease.ExecutionID, a.Lease.Epoch, stopped)
}

// followUpStop stops and confirms a finished attempt whose stop the
// runtime's follow-up found unconfirmed (SchedulerPoll.Stops), off the
// loop: a follow-up comes back exactly when stopping is failing (a wedged
// container runtime, say), which must not hold routing, timers and starts.
// It is skipped while a holder of this daemon still has the attempt at that
// epoch, whose own end stops it, or while an earlier follow-up for it is
// still stopping. The stop is bounded by stopFollowupTimeout. A failure is
// reported as a stop diagnostic, and the runtime hands the attempt over
// again after its backoff.
func (d *daemon) followUpStop(ctx context.Context, c *components, due runtime.StopDue) {
	key := attemptKey{due.ExecutionID, due.Epoch}
	d.mu.Lock()
	_, held := d.inflight[key]
	if held || d.stopping[key] {
		d.mu.Unlock()
		return
	}
	if d.stopping == nil {
		d.stopping = map[attemptKey]bool{}
	}
	d.stopping[key] = true
	d.mu.Unlock()
	d.goSafe("stop", func() {
		defer func() {
			d.mu.Lock()
			delete(d.stopping, key)
			d.mu.Unlock()
		}()
		stopCtx, cancel := context.WithTimeout(ctx, stopFollowupTimeout)
		defer cancel()
		d.confirmStopped(stopCtx, c, due.ExecutionID, due.Epoch)
	})
}

// stopFollowupTimeout bounds one follow-up's stop; one that takes longer
// is reported and tried again at the runtime's next follow-up.
const stopFollowupTimeout = 2 * time.Minute

// heldBy is the cause a refused start reports when an unconfirmed stop
// holds its Work: the attempt holding it.
func heldBy(err error) string {
	var held *runtime.ResourceHeldError
	if errors.As(err, &held) {
		return "held by " + string(held.ExecutionID)
	}
	return ""
}

// confirmStopped proves an attempt is not running before releasing its
// resource hold. An epoch of zero uses the attempt's current epoch. A
// superseded epoch is refused by the executor and confirms nothing; the
// runtime's follow-up (followUpStop) asks again at the current one.
func (d *daemon) confirmStopped(ctx context.Context, c *components, id domain.ID, epoch uint64) {
	d.stopAttempt(ctx, c, id, epoch, nil)
}

// collectStoppedTimeout bounds the wait for a stopped turn's result once
// its process is verified stopped; the executor has nothing left to wait
// for by then, so it is only a guard against one that hangs.
const collectStoppedTimeout = 30 * time.Second

// stoppedTurn is what an observer knows of its attempt's turn when it
// goes to stop it: the attempt, and the handle's one answer if it gave
// one (result, or answered with an executor error).
type stoppedTurn struct {
	attempt  runtime.Attempt
	result   *extension.Result
	answered bool
}

// stopAttempt is confirmStopped for an attempt this daemon observed.
// Between the verified stop and the confirmation it records what the
// stopped turn wrote, so a run ended while its turn ran keeps that turn's
// transcript (#647): the result the handle already gave, kept again in
// case Complete's own record failed, or, when the hold ended before the
// handle answered, the result it gives now the process has stopped
// (Scheduler.CollectStopped). A handle that answered with an error is not
// waited on again.
func (d *daemon) stopAttempt(ctx context.Context, c *components, id domain.ID, epoch uint64, turn *stoppedTurn) {
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
	s, ok := d.executorMap()[state.Executor].(extension.StopExecutor)
	if !ok {
		d.report("stop", id, "executor_cannot_prove_stop")
		return
	}
	// The executor's stop reaches its container as a TERM the host sent.
	d.containerSignalled(ctx, id, "stop")
	if err = s.Stop(ctx, id, epoch); err != nil {
		d.report("stop", id, "stop_unverified", runtime.ExecutorCause(err))
		return
	}
	// The executor's wrapper is stopped; its container must be too.
	if err = d.stopContainer(ctx, state); err != nil {
		d.report("stop", id, "container_stop_unverified")
		return
	}
	if turn != nil && turn.attempt.Lease.Epoch == epoch {
		switch {
		case turn.result != nil:
			_, err = c.scheduler.RecordStopped(ctx, turn.attempt.Lease, *turn.result)
		case !turn.answered:
			collect, cancel := context.WithTimeout(ctx, collectStoppedTimeout)
			err = c.scheduler.CollectStopped(collect, turn.attempt)
			cancel()
		}
		if err != nil {
			d.log.Warn("the stopped turn's result was not recorded", "execution", string(id), "epoch", epoch, "error", errorCode(err))
		}
	}
	if err = c.scheduler.ConfirmStopped(ctx, id, epoch); err != nil && !errors.Is(err, storage.ErrConflict) {
		d.report("stop", id, errorCode(err))
		return
	}
	d.clear("stop", id)
}

// releaseAll releases, at shutdown and once every attendance has ended, the
// leases of the attempts left running, so the next daemon recovers them at
// once instead of when they would have expired.
func (d *daemon) releaseAll(ctx context.Context) {
	d.mu.Lock()
	refs := append([]domain.LeaseRef(nil), d.leftForNext...)
	d.leftForNext = nil
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
// reportSubscriptions reports a subscription pass. Each dependency a filter
// couldn't answer about is its own diagnostic, with its Wait as the subject
// and the namespace, dependency and Event as the cause, so the held Work can
// be found; one that answers again is cleared. Any other failure is the
// pass's own.
func (d *daemon) reportSubscriptions(err error) {
	held := map[domain.ID]bool{}
	var rest []error
	for _, e := range joined(err) {
		var f *runtime.DependencyFilterError
		if !errors.As(e, &f) {
			rest = append(rest, e)
			continue
		}
		code := "dependency_filter_unavailable"
		if !f.Held {
			code = "dependency_filter_passed_by"
		} else {
			held[f.WaitID] = true
		}
		d.report("subscription", f.WaitID, code, f.Error())
	}
	d.mu.Lock()
	var cleared []domain.ID
	for key, code := range d.reported {
		if id, ok := strings.CutPrefix(key, "subscription\x00"); ok && code == "dependency_filter_unavailable" && !held[domain.ID(id)] {
			cleared = append(cleared, domain.ID(id))
		}
	}
	d.mu.Unlock()
	for _, id := range cleared {
		d.report("subscription", id, "")
	}
	d.report("subscriptions", "", errorCode(errors.Join(rest...)))
}

// joined is err's parts when it is an errors.Join, else err alone.
func joined(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		if _, typed := err.(*runtime.DependencyFilterError); !typed {
			return j.Unwrap()
		}
	}
	return []error{err}
}

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
	case errors.Is(err, runtime.ErrDraining):
		return "draining"
	case errors.Is(err, runtime.ErrNotAdmitted):
		return "not_admitted"
	case errors.Is(err, runtime.ErrBudgetHeld):
		return "over_budget"
	case errors.Is(err, runtime.ErrContainerCapacity):
		return "container_capacity"
	case errors.Is(err, runtime.ErrExecutorCapacity):
		return "executor_capacity"
	case errors.Is(err, runtime.ErrResourceHeld):
		return "resource_held"
	case errors.Is(err, runtime.ErrNotEligible):
		return "not_eligible"
	case errors.Is(err, runtime.ErrSecretUnavailable):
		return "secret_unavailable"
	case errors.Is(err, runtime.ErrCausalRejected):
		return "causal_rejected"
	case errors.Is(err, runtime.ErrDependencyFilterUnavailable):
		return "dependency_filter_unavailable"
	case errors.Is(err, runtime.ErrDependencyArchiveUnavailable):
		return "dependency_archive_unavailable"
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

// collectArtifacts removes artifact content no attempt references once it
// is older than ArtifactGrace (Scheduler.CollectArtifacts).
func (d *daemon) collectArtifacts(ctx context.Context, c *components) {
	if d.artifacts == nil {
		return
	}
	if _, err := c.scheduler.CollectArtifacts(ctx, d.artifacts, d.now().Add(-d.o.ArtifactGrace)); err != nil && ctx.Err() == nil {
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
	holding := func(ids []domain.ID) (map[domain.ID]bool, error) { return scheduler.Holding(ctx, ids) }
	err := errors.Join(d.endpoints.Reconcile(holding), d.git.Reconcile(holding), d.served.Reconcile(holding))
	if err != nil && ctx.Err() == nil {
		d.report("endpoints", "", errorCode(err))
		return
	}
	d.clear("endpoints", "")
}
