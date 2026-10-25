package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/internal/storage/sqlite"
	"github.com/alsiola/open-conduit/storage"
)

// A fixture calendar policy: on an even minute it publishes a substituted
// occurrence, on an odd minute it skips; it always rearms one minute later.
// spec {"bad": true} returns an occurrence from another source.
func fixtureCalendar(spec []byte, fired domain.Timer, now time.Time) (*domain.Event, *domain.Timer, error) {
	var s struct{ Bad, Past bool }
	if err := json.Unmarshal(spec, &s); err != nil {
		return nil, nil, err
	}
	minute := fired.FireAt.Minute()
	next := domain.Timer{ID: fired.ID, FireAt: fired.FireAt.Add(time.Minute), Event: calendarTemplate(fired.FireAt.Add(time.Minute))}
	if s.Past {
		next.FireAt = now
	}
	if minute%2 == 1 {
		return nil, &next, nil
	}
	occ := domain.Event{ID: domain.ID("sub-" + strconv.Itoa(minute)), Type: "fixture.calendar", Source: fired.Event.Source, DeliveryID: "sub/" + strconv.Itoa(minute), OccurredAt: fired.FireAt}
	if s.Bad {
		occ.Source = "fixture.elsewhere"
	}
	return &occ, &next, nil
}

func calendarTemplate(at time.Time) domain.Event {
	m := strconv.Itoa(at.Minute())
	return domain.Event{ID: domain.ID("tpl-" + m), Type: "fixture.calendar", Source: "fixture.calendar", DeliveryID: "tpl/" + m}
}

func calendarEvents(t *testing.T, db storage.Store, now time.Time) []string {
	t.Helper()
	var out []string
	if err := db.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if a.Event.Type == "fixture.calendar" {
				out = append(out, string(a.Event.ID))
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTimerRecurrenceContract (05-01): a registered calendar policy decides,
// inside the firing transaction, which occurrence a firing publishes (or
// none) and the replacement; the template itself is not accepted when the
// policy substitutes or skips; replay after restart returns the original
// result; an invalid answer or an unregistered kind is refused without
// publishing anything.
func TestTimerRecurrenceContract(t *testing.T) {
	ctx := context.Background()
	db, _, clock, path := causalFixture(t, runtime.CausalLimits{})
	open := func(store storage.Store, recur bool) *runtime.Timers {
		o := runtime.TimerOptions{}
		if recur {
			o.Recurrences = map[string]runtime.RecurrenceFunc{"fixture.calendar": fixtureCalendar}
		}
		timers, err := runtime.NewTimers(store, clock.Now, o)
		if err != nil {
			t.Fatal(err)
		}
		return timers
	}
	timers := open(db, true)
	start := clock.Now().Truncate(time.Hour)
	clock.Set(start)
	timer := domain.Timer{ID: "calendar", FireAt: start, Event: calendarTemplate(start)}
	for _, bad := range []struct {
		kind, spec string
		timer      domain.Timer
	}{
		{"unregistered", `{}`, timer},
		{"fixture.calendar", `not json`, timer},
		{"fixture.calendar", `{}`, domain.Timer{ID: "calendar", FireAt: start, Event: func() domain.Event { e := calendarTemplate(start); e.CausationID = "root"; return e }()}},
	} {
		if err := timers.ArmRecurrence(ctx, "scope_a", bad.timer, 0, bad.kind, []byte(bad.spec)); !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("arm %s %s: %v", bad.kind, bad.spec, err)
		}
	}
	if err := timers.ArmRecurrence(ctx, "scope_a", timer, 0, "fixture.calendar", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	armed := timerRead(t, db, clock.Now(), "calendar")
	first, err := timers.Fire(ctx, "calendar", armed.Revision, nil)
	if err != nil || !first.Fired || first.Event.Event.ID != "sub-0" {
		t.Fatalf("substituted firing %+v %v", first, err)
	}
	if got := calendarEvents(t, db, clock.Now()); len(got) != 1 || got[0] != "sub-0" {
		t.Fatalf("the template was accepted too: %v", got)
	}
	if replay, err := timers.Fire(ctx, "calendar", armed.Revision, nil); err != nil || replay.Event.Event.ID != "sub-0" {
		t.Fatalf("replay %+v %v", replay, err)
	}
	clock.Set(start.Add(time.Minute))
	second := timerRead(t, db, clock.Now(), "calendar")
	if !second.FireAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("rearm %v", second.FireAt)
	}
	skipped, err := timers.Fire(ctx, "calendar", second.Revision, nil)
	if err != nil || !skipped.Skipped || skipped.Fired || skipped.Event.Event.ID != "" {
		t.Fatalf("skip %+v %v", skipped, err)
	}
	if got := calendarEvents(t, db, clock.Now()); len(got) != 1 {
		t.Fatalf("a skipped firing published %v", got)
	}
	// Restart: replay keeps the skip; the recurrence continues.
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	timers = open(db, true)
	if replay, err := timers.Fire(ctx, "calendar", second.Revision, nil); err != nil || !replay.Skipped {
		t.Fatalf("restart replay %+v %v", replay, err)
	}
	// Without the kind registered the timer is refused, not fired, and Due
	// reports it without starving others.
	clock.Set(start.Add(2 * time.Minute))
	unregistered := open(db, false)
	due, err := unregistered.Due(ctx, 10)
	if err != nil || len(due) != 1 || due[0].Diagnostic != "invalid_timer" {
		t.Fatalf("unregistered %+v %v", due, err)
	}
	due, err = timers.Due(ctx, 10)
	if err != nil || len(due) != 1 || !due[0].Fired || due[0].Event.Event.ID != "sub-2" {
		t.Fatalf("due %+v %v", due, err)
	}
	// An invalid policy answer (foreign source, or a replacement not after
	// now) publishes nothing and leaves the timer for diagnosis.
	for _, spec := range []string{`{"bad":true}`, `{"past":true}`} {
		next := timerRead(t, db, clock.Now(), "calendar")
		if err := timers.ArmRecurrence(ctx, "scope_a", domain.Timer{ID: "calendar", FireAt: clock.Now(), Event: calendarTemplate(clock.Now().Add(time.Hour))}, next.Revision, "fixture.calendar", []byte(spec)); err != nil {
			t.Fatal(err)
		}
		bad := timerRead(t, db, clock.Now(), "calendar")
		if _, err := timers.Fire(ctx, "calendar", bad.Revision, nil); !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("%s: %v", spec, err)
		}
		if after := timerRead(t, db, clock.Now(), "calendar"); after.Revision != bad.Revision {
			t.Fatalf("%s: timer changed", spec)
		}
	}
}
