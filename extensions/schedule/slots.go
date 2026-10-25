package schedule

import (
	"fmt"
	"sort"
	"time"
)

// Slot is one timed occurrence: its instant and its wall-clock name. The
// name is v1's slot identity ("2026-10-25 01:30 Europe/London" for cron,
// "2026-10-25 05:17" for at, which is UTC): a repeated wall-clock time on the
// night the clocks go back has two instants and one name. The instant gives
// each occurrence its stable delivery identity.
type Slot struct {
	At   time.Time
	Name string
}

func cronName(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02 15:04") + " " + loc.String()
}

func atName(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") }

// Label names a timed run for its record, as v1 did.
func (j Job) Label(s Slot) string {
	switch {
	case j.Cron != "":
		return "cron " + s.Name
	case len(j.At) > 0:
		return "at " + s.Name + " UTC"
	case j.EveryMinutes > 0:
		return fmt.Sprintf("every %dm", j.EveryMinutes)
	}
	return s.Name
}

// Latest is the latest of a timed job's slots at or before now and strictly
// within six hours of it (v1 dueSlot/cronSlot): a slot missed by six hours or
// more waits for the next. A skipped wall-clock time on the night the clocks
// go forward has no slot; a repeated one has two instants with one name.
func Latest(j Job, now time.Time) (Slot, bool) {
	if j.Cron != "" {
		sched, err := j.Schedule()
		if err != nil {
			return Slot{}, false
		}
		loc, err := j.Location()
		if err != nil {
			return Slot{}, false
		}
		// The cron library only steps forward, so step from six hours ago.
		var latest time.Time
		for t := sched.Next(now.Add(-CatchUp).In(loc)); !t.IsZero() && !t.After(now); t = sched.Next(t) {
			latest = t
		}
		if latest.IsZero() {
			return Slot{}, false
		}
		return Slot{At: latest.UTC(), Name: cronName(latest, loc)}, true
	}
	var cands []time.Time
	now = now.UTC()
	for _, hm := range j.At {
		hh, mm, ok := parseHM(hm)
		if !ok {
			continue
		}
		for back := 0; back <= 1; back++ {
			t := time.Date(now.Year(), now.Month(), now.Day()-back, hh, mm, 0, 0, time.UTC)
			if !t.After(now) && now.Sub(t) < CatchUp {
				cands = append(cands, t)
			}
		}
	}
	if len(cands) == 0 {
		return Slot{}, false
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].After(cands[b]) })
	return Slot{At: cands[0], Name: atName(cands[0])}, true
}

// Next is a timed job's first slot strictly after after whose name differs
// from last, the slot that ran last. Skipping the same name makes the one
// repeated wall-clock time of a daily schedule run once, exactly as v1's
// single LastSlot did (a schedule within the hour, like */30, still runs at
// the repeated times whose name differs from the one just run).
func Next(j Job, after time.Time, last string) (Slot, bool) {
	if j.Cron != "" {
		sched, err := j.Schedule()
		if err != nil {
			return Slot{}, false
		}
		loc, err := j.Location()
		if err != nil {
			return Slot{}, false
		}
		t := sched.Next(after.In(loc))
		for i := 0; i < 16 && !t.IsZero() && cronName(t, loc) == last; i++ {
			t = sched.Next(t)
		}
		if t.IsZero() || cronName(t, loc) == last {
			return Slot{}, false
		}
		return Slot{At: t.UTC(), Name: cronName(t, loc)}, true
	}
	var best time.Time
	after = after.UTC()
	for _, hm := range j.At {
		hh, mm, ok := parseHM(hm)
		if !ok {
			continue
		}
		for day := 0; day <= 2; day++ {
			t := time.Date(after.Year(), after.Month(), after.Day()+day, hh, mm, 0, 0, time.UTC)
			if t.After(after) && atName(t) != last && (best.IsZero() || t.Before(best)) {
				best = t
			}
		}
	}
	if best.IsZero() {
		return Slot{}, false
	}
	return Slot{At: best, Name: atName(best)}, true
}
