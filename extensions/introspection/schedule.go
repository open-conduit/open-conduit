package introspection

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// The daily sweep and triage are each scope's standing Work, started by the
// extension's own calendar timers: durable core timers whose recurrence is
// this package's RecurrenceFunc (registered by the host under
// RecurrenceKind). The calendar rules are v1's, as the schedule extension
// ports them for jobs: a new timer waits for its next slot; an edited timing
// runs its latest slot strictly within six hours unless that slot ran last;
// a firing publishes the latest slot strictly within six hours, so a missed
// slot is caught up once and one missed by six hours or more is skipped.

// RecurrenceKind names Recurrence in the runtime's timer options.
const RecurrenceKind = "introspection.v1"

// SchemaDue is the payload schema of a sweep or triage slot Event.
const SchemaDue = "introspection.due.v1"

// CatchUp is how late a missed slot may still run (v1: strictly less than
// six hours).
const CatchUp = 6 * time.Hour

// Runs are the scheduled runs, in timer order.
var Runs = []string{WorkSweep, WorkTriage}

// Due is a slot Event's payload.
type Due struct {
	Run string `json:"run"`
	// Slot is the occurrence's wall-clock name ("2026-10-26 08:51 UTC") and
	// At its instant.
	Slot string    `json:"slot"`
	At   time.Time `json:"at"`
	// Spec is the timing hash the timer was armed with; Last the name of the
	// slot that ran before this one. Both are timer bookkeeping.
	Spec string `json:"spec,omitempty"`
	Last string `json:"last,omitempty"`
	// Epoch is when the schedule's timer was first armed. It names the
	// standing Work the slots go to (RunKey), so a schedule removed and added
	// again starts a new standing Work instead of addressing a retired one.
	Epoch string `json:"epoch"`
}

// DuePayload decodes a slot Event's payload.
func DuePayload(e domain.Event) (Due, bool) {
	var d Due
	if e.Data.Schema != SchemaDue || json.Unmarshal(e.Data.Data, &d) != nil || d.Run == "" {
		return d, false
	}
	return d, true
}

// TimerID is a scope's sweep or triage timer.
func TimerID(scope domain.ID, run string) domain.ID {
	return domain.ID(SourceNamespace(scope) + "/" + run)
}

func dueType(run string) string {
	if run == WorkSweep {
		return TypeSweepDue
	}
	return TypeTriageDue
}

// spec is the opaque recurrence spec stored with a timer: its timing and the
// schedule's epoch.
type spec struct {
	Scope    domain.ID `json:"scope"`
	Run      string    `json:"run"`
	Cron     string    `json:"cron"`
	TimeZone string    `json:"time_zone,omitempty"`
	Hash     string    `json:"hash"`
	Epoch    string    `json:"epoch"`
}

func newSpec(scope domain.ID, run string, s Schedule, epoch string) spec {
	return spec{Scope: scope, Run: run, Cron: s.Cron, TimeZone: s.TimeZone, Hash: digest(string(scope), run, s.Cron, s.TimeZone)[:16], Epoch: epoch}
}

// epochOf names a new schedule's epoch.
func epochOf(now time.Time) string { return now.UTC().Format("20060102T150405Z") }

type slot struct {
	At   time.Time
	Name string
}

func (sp spec) calendar() (cron.Schedule, *time.Location, error) {
	sched, err := cron.ParseStandard(sp.Cron)
	if err != nil {
		return nil, nil, err
	}
	loc, err := Schedule{TimeZone: sp.TimeZone}.Location()
	return sched, loc, err
}

func slotName(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02 15:04") + " " + loc.String()
}

// latest is the latest slot at or before now and strictly within CatchUp.
func (sp spec) latest(now time.Time) (slot, bool) {
	sched, loc, err := sp.calendar()
	if err != nil {
		return slot{}, false
	}
	var latest time.Time
	for t := sched.Next(now.Add(-CatchUp).In(loc)); !t.IsZero() && !t.After(now); t = sched.Next(t) {
		latest = t
	}
	if latest.IsZero() || now.Sub(latest) >= CatchUp {
		return slot{}, false
	}
	return slot{At: latest.UTC(), Name: slotName(latest, loc)}, true
}

// next is the first slot strictly after after whose name differs from last
// (a repeated wall-clock time on the night the clocks go back runs once).
func (sp spec) next(after time.Time, last string) (slot, bool) {
	sched, loc, err := sp.calendar()
	if err != nil {
		return slot{}, false
	}
	t := sched.Next(after.In(loc))
	for i := 0; i < 16 && !t.IsZero() && slotName(t, loc) == last; i++ {
		t = sched.Next(t)
	}
	if t.IsZero() || slotName(t, loc) == last {
		return slot{}, false
	}
	return slot{At: t.UTC(), Name: slotName(t, loc)}, true
}

// event is the slot Event of one occurrence. Its identity is the instant, so
// a replayed firing or a repeated catch-up is deduplicated by acceptance.
func (sp spec) event(s slot, last string) domain.Event {
	instant := s.At.UTC().Format(time.RFC3339)
	data, _ := json.Marshal(Due{Run: sp.Run, Slot: s.Name, At: s.At.UTC(), Spec: sp.Hash, Last: last, Epoch: sp.Epoch})
	key := RunKey(sp.Scope, sp.Run, sp.Epoch)
	return domain.Event{ID: domain.ID("introspection-" + digest(string(sp.Scope), sp.Run, instant)[:40]), Type: dueType(sp.Run), Source: SourceNamespace(sp.Scope),
		DeliveryID: "slot/" + sp.Run + "/" + instant, OccurredAt: s.At.UTC(), ReceivedAt: s.At.UTC(), CorrelationKey: &key,
		Data: domain.Payload{Schema: SchemaDue, Data: data}}
}

func (sp spec) timer(s slot, last string) domain.Timer {
	return domain.Timer{ID: TimerID(sp.Scope, sp.Run), FireAt: s.At.UTC(), Event: sp.event(s, last)}
}

// Recurrence is the RecurrenceFunc of RecurrenceKind. It runs inside the core
// firing transaction and is pure: it publishes the latest slot strictly
// within CatchUp unless that slot ran last, then rearms for the next slot.
func Recurrence(raw []byte, fired domain.Timer, now time.Time) (*domain.Event, *domain.Timer, error) {
	var sp spec
	if err := json.Unmarshal(raw, &sp); err != nil || sp.Scope == "" || sp.Run == "" || sp.Epoch == "" {
		return nil, nil, errors.New("introspection: invalid recurrence spec")
	}
	if _, _, err := sp.calendar(); err != nil {
		return nil, nil, errors.New("introspection: invalid recurrence calendar")
	}
	p, _ := DuePayload(fired.Event)
	last := p.Last
	var occurrence *domain.Event
	if l, ok := sp.latest(now); ok && l.Name != last {
		e := sp.event(l, last)
		occurrence, last = &e, l.Name
	}
	n, ok := sp.next(now, last)
	if !ok {
		return occurrence, nil, nil
	}
	t := sp.timer(n, last)
	return occurrence, &t, nil
}

// desired is the timer a run should have at now given its current one (nil
// when none); changed is false when the current one already matches.
func desired(scope domain.ID, run string, s Schedule, now time.Time, current *domain.Timer) (*domain.Timer, []byte, bool) {
	epoch := epochOf(now)
	var last, hash string
	if current != nil {
		p, _ := DuePayload(current.Event)
		if p.Epoch != "" {
			epoch = p.Epoch
		}
		last, hash = p.Last, p.Spec
	}
	sp := newSpec(scope, run, s, epoch)
	raw, _ := json.Marshal(sp)
	if current != nil && hash == sp.Hash {
		return current, raw, false
	}
	if current == nil {
		if l, ok := sp.latest(now); ok {
			last = l.Name
		}
	} else if l, ok := sp.latest(now); ok && l.Name != last {
		t := sp.timer(l, last)
		return &t, raw, true
	}
	n, ok := sp.next(now, last)
	if !ok {
		return nil, raw, current != nil
	}
	t := sp.timer(n, last)
	return &t, raw, true
}

// Armer is the runtime's trusted timer control (runtime.Timers).
type Armer interface {
	ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error
	Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error
}

// Entry is one scope's `introspection` entry in the current generation.
type Entry struct {
	Scope domain.ID
	// Enabled is the scope's effective enabled state: a disabled scope's
	// timers stay durable and untouched (B05), as the schedule extension's do.
	Enabled bool
	Config  Config
}

// EnsureResult counts one reconciliation pass.
type EnsureResult struct {
	Armed, Rearmed, Cancelled, Kept int
	// Codes are safe per-timer failure codes; the next pass retries.
	Codes []string
}

// Ensure reconciles every sweep and triage timer with the current
// generation: a new schedule gets its timer, an edited timing rearms, an
// unchanged one is left alone, and a timer whose schedule or entry was
// removed is cancelled while a run already going carries on. It is
// idempotent and restart-safe.
func Ensure(ctx context.Context, store storage.Store, now time.Time, armer Armer, entries []Entry) (EnsureResult, error) {
	var res EnsureResult
	current := map[domain.ID]domain.Timer{}
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		all, err := tx.ListTimers()
		for _, t := range all {
			if strings.HasPrefix(string(t.ID), Type+"/") && (t.Event.Type == TypeSweepDue || t.Event.Type == TypeTriageDue) {
				current[t.ID] = t
			}
		}
		return err
	})
	if err != nil {
		return res, err
	}
	fail := func(err error) { res.Codes = append(res.Codes, code(err)) }
	byScope := map[domain.ID]Entry{}
	handled := map[domain.ID]bool{}
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Scope < sorted[j].Scope })
	for _, e := range sorted {
		byScope[e.Scope] = e
		if !e.Enabled {
			continue
		}
		for _, run := range Runs {
			s := e.Config.Sweep
			if run == WorkTriage {
				s = e.Config.Triage
			}
			if s == nil {
				continue
			}
			id := TimerID(e.Scope, run)
			handled[id] = true
			var cur *domain.Timer
			if t, ok := current[id]; ok {
				cur = &t
			}
			want, raw, changed := desired(e.Scope, run, *s, now, cur)
			switch {
			case !changed:
				res.Kept++
			case want == nil:
				if err := armer.Cancel(ctx, id, cur.Revision); err != nil {
					fail(err)
					continue
				}
				res.Cancelled++
			default:
				var expected domain.Revision
				if cur != nil {
					expected = cur.Revision
				}
				if err := armer.ArmRecurrence(ctx, e.Scope, *want, expected, RecurrenceKind, raw); err != nil {
					fail(err)
					continue
				}
				if cur != nil {
					res.Rearmed++
				} else {
					res.Armed++
				}
			}
		}
	}
	ids := make([]domain.ID, 0, len(current))
	for id := range current {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if handled[id] {
			continue
		}
		t := current[id]
		if e, ok := byScope[timerScope(t)]; ok && !e.Enabled {
			res.Kept++
			continue
		}
		if err := armer.Cancel(ctx, id, t.Revision); err != nil {
			fail(err)
			continue
		}
		res.Cancelled++
	}
	return res, nil
}

// timerScope is the scope a slot timer belongs to (its Event's namespace).
func timerScope(t domain.Timer) domain.ID {
	return domain.ID(strings.TrimPrefix(t.Event.Source, Type+"/"))
}

func code(err error) string {
	switch {
	case errors.Is(err, storage.ErrConflict):
		return "conflict"
	case errors.Is(err, storage.ErrInvalid):
		return "invalid_timer"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	}
	return "timer_failed"
}
