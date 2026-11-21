package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// A package task either keeps its declared interval, or hands its waiting
// back: its result says when it next has something to do and what would
// bring that forward. The host keeps that durably, so a restart neither
// resets the time nor loses what happened while it was stopped.

// taskNamespace holds each sleeping task's taskSleep, by package/task.
const taskNamespace = "core.host.tasks"

// taskPageSize bounds one read of a named family's Events.
const taskPageSize = 200

// taskSleep is a task's durable wait. After and WorkSince/WorkDigest are what
// the task had already seen when its last run started, so anything that
// commits during the run, or while the daemon is stopped, still wakes it.
type taskSleep struct {
	NextDue    time.Time `json:"next_due"`
	Families   []string  `json:"families,omitempty"`
	Work       bool      `json:"work,omitempty"`
	After      uint64    `json:"after,omitempty"`
	WorkSince  time.Time `json:"work_since,omitzero"`
	WorkDigest string    `json:"work_digest,omitempty"`
}

// taskBaseline is what a run has seen, read before it starts.
type taskBaseline struct {
	sequence uint64
	since    time.Time
	digest   string
}

// taskClock is the sleeping tasks, read through to storage on write.
type taskClock struct {
	mu     sync.Mutex
	loaded bool
	sleeps map[string]taskSleep
	// woken tasks run on the next pass; wakes counts them, so a wake that
	// arrives during a run isn't overwritten by that run's result.
	woken map[string]bool
	wakes map[string]uint64
	// watching tasks have named a wake, so their runs read a baseline first.
	watching map[string]bool
	// generation is the package process each sleep was asked for in. A new
	// process has lost whatever the old one held in memory.
	generation map[string]uint64
}

func (c *taskClock) init() {
	if c.sleeps == nil {
		c.sleeps, c.woken, c.wakes, c.watching, c.generation = map[string]taskSleep{}, map[string]bool{}, map[string]uint64{}, map[string]bool{}, map[string]uint64{}
	}
}

// loadTaskSleeps reads the stored waits once. Until it succeeds every task
// runs at its interval, which errs towards running.
func (d *daemon) loadTaskSleeps(ctx context.Context) {
	c := &d.external.clock
	c.mu.Lock()
	loaded := c.loaded
	c.mu.Unlock()
	if loaded || d.coord == nil {
		return
	}
	stored := map[string]taskSleep{}
	err := storage.View(ctx, d.coord, d.now(), func(tx storage.Tx) error {
		clear(stored)
		rows, e := tx.List(taskNamespace, "")
		if e != nil {
			return e
		}
		for _, row := range rows {
			var r taskSleep
			if json.Unmarshal(row.Value, &r) == nil {
				stored[row.Key] = r
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			d.report("task_schedule", "", errorCode(err))
		}
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	latest := d.now().Add(protocol.MaxTaskSleep)
	for key, r := range stored {
		// A clock set back must not stretch a wait past the bound.
		if r.NextDue.After(latest) {
			r.NextDue = latest
		}
		c.sleeps[key] = r
		c.watching[key] = r.Work || len(r.Families) > 0
	}
	c.loaded = true
}

// wakeTask runs a task on the next pass, if it is sleeping.
func (d *daemon) wakeTask(key string) {
	c := &d.external.clock
	c.mu.Lock()
	c.init()
	c.woken[key] = true
	c.wakes[key]++
	c.mu.Unlock()
	d.poke()
}

// wakePackageTasks runs every sleeping task on the next pass: the
// configuration they planned on has changed.
func (d *daemon) wakePackageTasks() {
	c := &d.external.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return // the first pass runs on the configuration just applied
	}
	for key := range c.sleeps {
		c.woken[key] = true
		c.wakes[key]++
	}
}

// wakeSleepingTasks marks each sleeping task whose named Events or Work have
// changed. It only reads; a task it wakes rewrites its wait when it runs, so
// a crash before then finds the same change again.
func (d *daemon) wakeSleepingTasks(ctx context.Context, packages []*installed) {
	c := &d.external.clock
	type watch struct {
		key   string
		sleep taskSleep
		m     protocol.Manifest
	}
	var watches []watch
	c.mu.Lock()
	for _, x := range packages {
		for _, t := range x.m.Provides.Tasks {
			key := x.Name() + "/" + t.Name
			r, ok := c.sleeps[key]
			if !ok {
				continue
			}
			if g, seen := c.generation[key]; seen && g != x.generation() {
				c.init()
				c.woken[key] = true
				continue
			}
			if !c.woken[key] && (r.Work || len(r.Families) > 0) {
				watches = append(watches, watch{key, r, x.m})
			}
		}
	}
	c.mu.Unlock()
	if len(watches) == 0 {
		return
	}
	woken := []string{}
	scanned := map[string]uint64{}
	err := storage.View(ctx, d.coord, d.now(), func(tx storage.Tx) error {
		woken = woken[:0]
		clear(scanned)
		latest, e := tx.LatestSequence()
		if e != nil {
			return e
		}
		for _, w := range watches {
			scopes := d.packageScopes(w.m)
			hit := false
			if latest > w.sleep.After && len(w.sleep.Families) > 0 {
				if hit, e = familyAccepted(tx, w.sleep, scopes); e != nil {
					return e
				}
				if !hit {
					scanned[w.key] = latest
				}
			}
			if !hit && w.sleep.Work {
				digest, e := workDigest(tx, w.m, scopes, w.sleep.WorkSince)
				if e != nil {
					return e
				}
				hit = digest != w.sleep.WorkDigest
			}
			if hit {
				woken = append(woken, w.key)
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			d.report("task_schedule", "", errorCode(err))
		}
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range woken {
		c.woken[key] = true
	}
	// Events of the family in other scopes aren't read again.
	for key, seq := range scanned {
		if r, ok := c.sleeps[key]; ok && seq > r.After {
			r.After = seq
			c.sleeps[key] = r
		}
	}
}

// familyAccepted says whether an Event of a named family was accepted after
// the sleep's cursor in a scope the package may read.
func familyAccepted(tx storage.Tx, r taskSleep, scopes []domain.ID) (bool, error) {
	allowed := make(map[domain.ID]bool, len(scopes))
	for _, id := range scopes {
		allowed[id] = true
	}
	read := append([]domain.ID{""}, scopes...)
	for _, family := range r.Families {
		for after := r.After; ; {
			events, err := tx.TypeEvents(family, read, after, taskPageSize)
			if err != nil {
				return false, err
			}
			for _, a := range events {
				if scope, err := runtime.EventScopeInTx(tx, a); err == nil && allowed[scope] {
					return true, nil
				}
			}
			if len(events) < taskPageSize {
				break
			}
			after = events[len(events)-1].Sequence
		}
	}
	return false, nil
}

// workDigest identifies the state of the Work the package may read that
// changed since the time given, and of its live attempts: writing an attempt
// doesn't move its Work's UpdatedAt, and one leaving the live set changes the
// digest too.
func workDigest(tx storage.Tx, m protocol.Manifest, scopes []domain.ID, since time.Time) (string, error) {
	allowed := make(map[domain.ID]bool, len(scopes))
	for _, id := range scopes {
		allowed[id] = true
	}
	own := make(map[string]bool, len(m.Provides.WorkKinds))
	for _, k := range m.Provides.WorkKinds {
		own[k.Kind] = true
	}
	var lines []string
	seen := map[domain.ID]bool{}
	add := func(works []domain.Work) {
		for _, w := range works {
			if !seen[w.ID] {
				seen[w.ID] = true
				lines = append(lines, fmt.Sprintf("w %s %d", w.ID, w.Revision))
			}
		}
	}
	within := slices.Clone(scopes)
	if within == nil {
		within = []domain.ID{}
	}
	works, err := tx.QueryWorks(storage.WorkQuery{Within: within, UpdatedSince: since})
	if err != nil {
		return "", err
	}
	add(works)
	for kind := range own {
		if works, err = tx.QueryWorks(storage.WorkQuery{Kind: kind, UpdatedSince: since}); err != nil {
			return "", err
		}
		add(works)
	}
	live, err := tx.LiveExecutions()
	if err != nil {
		return "", err
	}
	readable := map[domain.ID]bool{}
	for _, x := range live {
		can, known := readable[x.WorkID]
		if !known {
			w, err := tx.Work(x.WorkID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return "", err
			}
			can = allowed[w.ScopeID] || own[w.Kind]
			readable[x.WorkID] = can
		}
		if can {
			lines = append(lines, fmt.Sprintf("x %s %s", x.ID, x.State))
		}
	}
	slices.Sort(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// taskBaselineFor reads what a run is about to see.
func (d *daemon) taskBaselineFor(ctx context.Context, x *installed) (taskBaseline, error) {
	var b taskBaseline
	now := d.now()
	b.since = now.Add(-storage.ChangeLag)
	err := storage.View(ctx, d.coord, now, func(tx storage.Tx) error {
		var e error
		if b.sequence, e = tx.LatestSequence(); e != nil {
			return e
		}
		b.digest, e = workDigest(tx, x.m, d.packageScopes(x.m), b.since)
		return e
	})
	return b, err
}

// runTask starts a package task, keyed package/task, on its own goroutine
// once it is due; one task never overlaps itself. Its error is reported as
// an extension_task diagnostic, and returns it to its interval.
func (d *daemon) runTask(ctx context.Context, x *installed, key string, t task) {
	c := &d.external.clock
	d.external.mu.Lock()
	if d.external.tasks == nil {
		d.external.tasks, d.external.running = map[string]time.Time{}, map[string]bool{}
	}
	c.mu.Lock()
	c.init()
	r, sleeping := c.sleeps[key]
	//duty:package_task
	due := d.now().Sub(d.external.tasks[key]) >= t.Pace
	if sleeping {
		//duty:package_task
		due = c.woken[key] || !d.now().Before(r.NextDue)
	}
	due = due && !d.external.running[key]
	watching := c.watching[key]
	wakes := c.wakes[key]
	if due {
		d.external.running[key], d.external.tasks[key] = true, d.now()
		delete(c.woken, key)
	}
	c.mu.Unlock()
	d.external.mu.Unlock()
	if !due {
		return
	}
	d.goSafe("extension_task", func() {
		defer func() {
			d.external.mu.Lock()
			d.external.running[key] = false
			d.external.mu.Unlock()
		}()
		var base *taskBaseline
		if watching && d.coord != nil {
			if b, err := d.taskBaselineFor(ctx, x); err == nil {
				base = &b
			}
		}
		generation := x.generation()
		cx, cancel := context.WithTimeout(ctx, min(max(t.Pace, time.Minute), 10*time.Minute))
		defer cancel()
		res, err := t.Run(cx)
		if err == nil {
			err = res.Validate()
		}
		if err != nil {
			res = protocol.TaskRunResult{}
		}
		if ctx.Err() != nil {
			return // stopping: the stored wait stands
		}
		if err != nil {
			d.report("extension_task", domain.ID(key), errorCode(err))
		} else {
			d.clear("extension_task", domain.ID(key))
		}
		d.settleTask(ctx, key, res, base, wakes, generation)
	})
}

// settleTask records what a finished run said about the next one. A result
// that names a wake but ran without a baseline runs once more straight away,
// with one, so nothing committed during this run is missed.
func (d *daemon) settleTask(ctx context.Context, key string, res protocol.TaskRunResult, base *taskBaseline, wakes, generation uint64) {
	c := &d.external.clock
	c.mu.Lock()
	_, had := c.sleeps[key]
	if !res.Sleeps() {
		delete(c.sleeps, key)
		delete(c.watching, key)
		delete(c.generation, key)
		c.mu.Unlock()
		if had {
			d.storeTaskSleep(ctx, key, nil)
		}
		return
	}
	now := d.now()
	// Never sooner than a second, so a task can't spin the loop by answering
	// with a time already past, and never later than the bound.
	next := now.Add(protocol.MaxTaskSleep)
	if res.NextRunAt != nil && res.NextRunAt.Before(next) {
		next = *res.NextRunAt
	}
	if earliest := now.Add(time.Second); next.Before(earliest) {
		next = earliest
	}
	r := taskSleep{NextDue: next.UTC()}
	if res.Wake != nil {
		r.Families, r.Work = slices.Clone(res.Wake.Families), res.Wake.Work
		slices.Sort(r.Families)
		if base == nil {
			r.NextDue = now
			c.watching[key] = true
		} else {
			r.After, r.WorkSince, r.WorkDigest = base.sequence, base.since, base.digest
			c.watching[key] = true
		}
	} else {
		delete(c.watching, key)
	}
	if c.wakes[key] != wakes {
		c.woken[key] = true
	}
	c.sleeps[key] = r
	c.generation[key] = generation
	c.mu.Unlock()
	d.storeTaskSleep(ctx, key, &r)
}

// storeTaskSleep writes a task's wait, or removes it. A failed write is
// reported and keeps the wait in memory: at worst the task runs early after a
// restart.
func (d *daemon) storeTaskSleep(ctx context.Context, key string, r *taskSleep) {
	if d.coord == nil {
		return
	}
	var raw []byte
	var err error
	if r != nil {
		raw, err = json.Marshal(r)
	}
	if err == nil {
		err = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
			if r == nil {
				return tx.Delete(taskNamespace, key)
			}
			return tx.Put(taskNamespace, key, raw)
		})
	}
	if err != nil && ctx.Err() == nil {
		d.report("task_schedule", "", errorCode(err))
		return
	}
	d.clear("task_schedule", "")
}
