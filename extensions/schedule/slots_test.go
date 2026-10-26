package schedule_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
)

func latestName(j sc.Job, now time.Time) string {
	s, _ := sc.Latest(j, now)
	return s.Name
}

// TestCronSlot ports v1 engine/jobs_test.go TestCronSlot onto Latest.
func TestCronSlot(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	at := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, london) }
	job := sc.Job{Name: "j", Cron: "0 9,13,18 * * *", TimeZone: "Europe/London"}
	for i, c := range []struct {
		job  sc.Job
		now  time.Time
		want string
	}{
		{job, at(2026, 10, 21, 9, 0), "2026-10-21 09:00 Europe/London"},
		{job, at(2026, 10, 21, 12, 59), "2026-10-21 09:00 Europe/London"},
		{job, at(2026, 10, 21, 17, 0), "2026-10-21 13:00 Europe/London"},
		// Six hours after the last slot, it waits for the next.
		{job, at(2026, 10, 22, 3, 0), ""},
		{job, at(2026, 10, 21, 23, 30), "2026-10-21 18:00 Europe/London"},
		// In summer time 09:00 London is 08:00 UTC.
		{job, time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC), "2026-07-01 09:00 Europe/London"},
		{job, time.Date(2026, 7, 1, 7, 59, 0, 0, time.UTC), ""},
		{job, time.Date(2026, 7, 1, 17, 0, 0, 0, time.UTC), "2026-07-01 18:00 Europe/London"},
		{sc.Job{Name: "j", Cron: "0 9 * * FRI"}, time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC), "2026-10-23 09:00 UTC"},
		{sc.Job{Name: "j", Cron: "0 9 * * FRI"}, time.Date(2026, 10, 22, 9, 5, 0, 0, time.UTC), ""},
	} {
		if got := latestName(c.job, c.now); got != c.want {
			t.Errorf("case %d: %q; want %q", i, got, c.want)
		}
	}
}

// TestCronSlotClockChanges ports v1 TestCronSlotClockChanges.
func TestCronSlotClockChanges(t *testing.T) {
	job := sc.Job{Name: "j", Cron: "30 1 * * *", TimeZone: "Europe/London"}
	// The clocks skip 01:00-02:00 on 29 March 2026: no 01:30 that night.
	if got := latestName(job, time.Date(2026, 3, 29, 2, 0, 0, 0, time.UTC)); got != "" {
		t.Errorf("spring: %q", got)
	}
	if got := latestName(job, time.Date(2026, 3, 30, 0, 31, 0, 0, time.UTC)); got != "2026-03-30 01:30 Europe/London" {
		t.Errorf("spring, the night after: %q", got)
	}
	// They repeat 01:00-02:00 on 25 October 2026: both 01:30s are one name.
	first, _ := sc.Latest(job, time.Date(2026, 10, 25, 0, 31, 0, 0, time.UTC))  // 01:31 BST
	second, _ := sc.Latest(job, time.Date(2026, 10, 25, 1, 31, 0, 0, time.UTC)) // 01:31 GMT
	if first.Name != "2026-10-25 01:30 Europe/London" || second.Name != first.Name || first.At.Equal(second.At) {
		t.Errorf("autumn: %+v then %+v", first, second)
	}
	// Next skips the repeated instant whose name just ran.
	n, ok := sc.Next(job, first.At, first.Name)
	if !ok || !n.At.Equal(time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC)) {
		t.Errorf("after the BST 01:30: %+v", n)
	}
	// A schedule within the hour runs at repeated times whose name differs
	// from the one just run, as v1's single LastSlot did.
	half := sc.Job{Name: "j", Cron: "*/30 * * * *", TimeZone: "Europe/London"}
	bst130 := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
	n, _ = sc.Next(half, bst130, "2026-10-25 01:30 Europe/London")
	if n.Name != "2026-10-25 01:00 Europe/London" || !n.At.Equal(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("*/30 after 01:30 BST: %+v", n)
	}
}

// The six-hour boundary is exclusive, exactly as v1's algorithm (not its
// README prose): now-slot < 6h runs, == 6h waits for the next slot.
func TestCatchUpBoundary(t *testing.T) {
	slot := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	for _, j := range []sc.Job{{Name: "c", Cron: "0 9 * * *"}, {Name: "a", At: []string{"09:00"}}} {
		if got := latestName(j, slot.Add(6*time.Hour-time.Second)); got == "" {
			t.Errorf("%s: just inside six hours: no slot", j.Name)
		}
		if got := latestName(j, slot.Add(6*time.Hour)); got != "" {
			t.Errorf("%s: exactly six hours: %q", j.Name, got)
		}
		if got := latestName(j, slot.Add(6*time.Hour+time.Minute)); got != "" {
			t.Errorf("%s: just outside: %q", j.Name, got)
		}
	}
}

func TestAtSlots(t *testing.T) {
	j := sc.Job{Name: "a", At: []string{"05:17", "9:00"}}
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	if l, _ := sc.Latest(j, now); l.Name != "2026-10-21 09:00" || j.Label(l) != "at 2026-10-21 09:00 UTC" {
		t.Errorf("latest %+v", l)
	}
	// Across midnight: 05:17 yesterday is more than six hours ago at 02:00.
	if l, ok := sc.Latest(j, time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)); ok {
		t.Errorf("latest after midnight %+v", l)
	}
	if n, _ := sc.Next(j, now, ""); n.Name != "2026-10-22 05:17" {
		t.Errorf("next %+v", n)
	}
	if n, _ := sc.Next(j, time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC), "2026-10-21 05:17"); n.Name != "2026-10-21 09:00" {
		t.Errorf("next skipping last %+v", n)
	}
}

// fire drives the recurrence the way the core firing transaction does.
func fire(t *testing.T, spec []byte, timer domain.Timer, now time.Time) (*domain.Event, *domain.Timer) {
	t.Helper()
	occ, next, err := sc.Recurrence(spec, timer, now)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil && (!next.FireAt.After(now) || next.ID != timer.ID || next.Event.ID == timer.Event.ID) {
		t.Fatalf("invalid replacement %+v after %v", next, now)
	}
	return occ, next
}

func slotOf(t *testing.T, e *domain.Event) string {
	t.Helper()
	if e == nil {
		return ""
	}
	p, ok := sc.Payload(*e)
	if !ok {
		t.Fatalf("payload %+v", e)
	}
	return p.Slot
}

func armed(t *testing.T, j sc.Job, now time.Time) ([]byte, domain.Timer) {
	t.Helper()
	timer, spec, changed := sc.Desired("schedule/demo", j, now, nil)
	if !changed || timer == nil {
		t.Fatal("no timer")
	}
	return spec, *timer
}

func TestRecurrenceCatchUp(t *testing.T) {
	j := sc.Job{Name: "report", Cron: "0 9,13,18 * * *"}
	// First start at 08:00 waits for the 09:00 slot (it does not run a slot
	// someone may already have run).
	spec, timer := armed(t, j, time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC))
	if !timer.FireAt.Equal(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("first fire %v", timer.FireAt)
	}
	// On time.
	occ, next := fire(t, spec, timer, timer.FireAt)
	if slotOf(t, occ) != "2026-10-21 09:00 UTC" || occ.ID != timer.Event.ID || !next.FireAt.Equal(time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("on time: %v %v", slotOf(t, occ), next.FireAt)
	}
	// Down 12:00 → 20:00: 13:00 and 18:00 missed; only the latest within six
	// hours (18:00) runs, once, and the timer moves past both.
	occ, next = fire(t, spec, *next, time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC))
	if slotOf(t, occ) != "2026-10-21 18:00 UTC" || !next.FireAt.Equal(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("missed slots: %v %v", slotOf(t, occ), next.FireAt)
	}
	p, _ := sc.Payload(next.Event)
	if p.Last != "2026-10-21 18:00 UTC" {
		t.Fatalf("last %q", p.Last)
	}
	// Fired six hours after 09:00 (15:00): 09:00 is skipped and 13:00, the
	// latest slot within six hours, runs instead.
	occ, _ = fire(t, spec, *next, next.FireAt.Add(6*time.Hour))
	if slotOf(t, occ) != "2026-10-22 13:00 UTC" {
		t.Fatalf("latest within six hours: %v", slotOf(t, occ))
	}
	// A daily slot: exactly six hours late is skipped, just inside runs.
	dspec, daily := armed(t, sc.Job{Name: "daily", Cron: "0 9 * * *"}, time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC))
	occ, dnext := fire(t, dspec, daily, daily.FireAt.Add(6*time.Hour))
	if occ != nil || !dnext.FireAt.Equal(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("exactly six hours: %v %v", slotOf(t, occ), dnext.FireAt)
	}
	occ, _ = fire(t, dspec, daily, daily.FireAt.Add(6*time.Hour-time.Second))
	if slotOf(t, occ) != "2026-10-21 09:00 UTC" {
		t.Fatalf("just inside: %v", slotOf(t, occ))
	}
	// Days down: everything older than six hours is skipped.
	occ, next3 := fire(t, spec, *next, time.Date(2026, 10, 25, 7, 0, 0, 0, time.UTC))
	if occ != nil || !next3.FireAt.Equal(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("days down: %v %v", slotOf(t, occ), next3.FireAt)
	}
}

// TestRecurrenceDST steps the autumn and spring nights in London.
func TestRecurrenceDST(t *testing.T) {
	j := sc.Job{Name: "night", Cron: "30 1 * * *", TimeZone: "Europe/London"}
	spec, timer := armed(t, j, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	var runs []string
	now := timer.FireAt
	for i := 0; i < 3; i++ {
		occ, next := fire(t, spec, timer, now)
		runs = append(runs, slotOf(t, occ))
		timer, now = *next, next.FireAt
	}
	want := []string{"2026-10-25 01:30 Europe/London", "2026-10-26 01:30 Europe/London", "2026-10-27 01:30 Europe/London"}
	if strings.Join(runs, "|") != strings.Join(want, "|") {
		t.Fatalf("autumn runs %v", runs)
	}
	spec, timer = armed(t, j, time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC))
	if !timer.FireAt.Equal(time.Date(2026, 3, 30, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("spring: the skipped 01:30 has no slot; armed %v", timer.FireAt)
	}
	// Even a firing during the skipped hour publishes nothing extra.
	occ, _ := fire(t, spec, timer, timer.FireAt)
	if slotOf(t, occ) != "2026-03-30 01:30 Europe/London" {
		t.Fatalf("spring night after %v", slotOf(t, occ))
	}
}

func TestRecurrenceEvery(t *testing.T) {
	j := sc.Job{Name: "tick", EveryMinutes: 60}
	start := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	spec, timer := armed(t, j, start)
	if !timer.FireAt.Equal(start) {
		t.Fatalf("an interval job runs at once: %v", timer.FireAt)
	}
	occ, next := fire(t, spec, timer, start)
	if occ == nil || !next.FireAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("every: %v", next.FireAt)
	}
	// After an outage one run, then the cadence restarts from now.
	late := start.Add(5*time.Hour + 30*time.Minute)
	occ, next = fire(t, spec, *next, late)
	if occ == nil || !next.FireAt.Equal(late.Add(time.Hour)) {
		t.Fatalf("every after outage: %v", next.FireAt)
	}
	if p, _ := sc.Payload(*occ); p.Trigger != "every 60m" || p.Kind != sc.KindTimer {
		t.Fatalf("payload %+v", p)
	}
}

func TestDesiredEdits(t *testing.T) {
	ns := "schedule/demo"
	j := sc.Job{Name: "report", Cron: "0 9 * * *"}
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	cur, _, _ := sc.Desired(ns, j, now, nil)
	// New job at 10:00: today's 09:00 is treated as run; next is tomorrow.
	if !cur.FireAt.Equal(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("new %v", cur.FireAt)
	}
	if _, _, changed := sc.Desired(ns, j, now.Add(time.Hour), cur); changed {
		t.Fatal("unchanged timing rearmed")
	}
	// Editing other triggers does not rearm.
	if _, _, changed := sc.Desired(ns, sc.Job{Name: "report", Cron: "0 9 * * *", On: &sc.Trigger{Webhook: true}}, now, cur); changed {
		t.Fatal("trigger edit rearmed")
	}
	// Edited to 08:00 at 10:00: today's 08:00 is within six hours and is not
	// the slot that ran last, so it runs now (v1 kept LastSlot).
	edited := sc.Job{Name: "report", Cron: "0 8 * * *"}
	next, spec, changed := sc.Desired(ns, edited, now, cur)
	if !changed || !next.FireAt.Equal(time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("edit %v", next)
	}
	var sp sc.Spec
	if json.Unmarshal(spec, &sp) != nil || sp.Job.Cron != "0 8 * * *" {
		t.Fatalf("spec %s", spec)
	}
	// A cron that never fires leaves no timer.
	if gone, _, changed := sc.Desired(ns, sc.Job{Name: "report", Cron: "0 0 30 2 *"}, now, cur); gone != nil || !changed {
		t.Fatalf("never %v %v", gone, changed)
	}
}
