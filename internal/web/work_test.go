package web

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

var t0 = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

func at0(d time.Duration) string { return t0.Add(d).Format(time.RFC3339Nano) }

// turnRun is a run of attempt n in state, started at start and, unless
// zero, finished at end.
func turnRun(n uint32, state string, start, end time.Duration) client.Run {
	r := client.Run{}
	r.ID, r.WorkID, r.Attempt, r.State = "work/w/attempt/"+itoa(int(n)), "w", n, state
	r.CreatedAt, r.StartedAt = at0(start), at0(start)
	if end != 0 {
		r.FinishedAt, r.UpdatedAt = at0(end), at0(end)
	}
	return r
}

func TestDisposition(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	failed := turnRun(1, "failed", time.Hour, 2*time.Hour)
	failed.Outcome = &client.Outcome{Kind: "failed", Summary: "workspace setup failed"}
	exhausted := failed
	exhausted.Retry = &client.RetryStatus{FailureCount: 3, Exhausted: true}
	retrying := failed
	retrying.Retry = &client.RetryStatus{FailureCount: 1, EligibleAt: at0(3 * time.Hour)}
	handoff := turnRun(1, "handoff", time.Hour, 2*time.Hour)
	handoff.Outcome = &client.Outcome{Kind: "handoff", Summary: "Opened PR #12; waiting on checks"}
	zero, one := 0, 1
	wait := &client.Wait{ID: "wait/1", State: "active", Match: client.Match{Types: []string{"github.check_suite.completed", "github.pull_request.merged"}}, Description: "checks on PR #137", CreatedAt: at0(4 * time.Hour)}
	ask := []client.Action{{Provider: "acme", ID: "answer", Label: "Answer the question", Awaited: true}}
	panel := []client.Panel{{Provider: "acme", ID: "ask", Title: "The agent asks", Sections: []client.PanelSection{{Text: "May I refund?"}, {Actions: ask}}}}
	// A standing action a person may take at any time, not one the Work
	// waits for (#518).
	message := client.Action{Provider: "acme", ID: "message", Label: "Send a message", Prompt: "your message"}
	thread := []client.Panel{{Provider: "acme", ID: "thread", Title: "Conversation", Sections: []client.PanelSection{{Text: "Sent the emails."}, {Actions: []client.Action{message}}}}}
	timer := &client.Wait{State: "active", TimeoutTimerID: "timer/1", Description: "Check Gmail for trade offers", CreatedAt: at0(4 * time.Hour)}
	cases := []struct {
		name    string
		work    client.Work
		runs    []client.Run
		wait    *client.Wait
		group   string
		kind    string
		label   string
		detail  string
		waiting string
	}{
		{name: "completed", work: client.Work{State: "completed"}, runs: []client.Run{turnRun(1, "completed", time.Hour, 2*time.Hour)}, group: "closed", kind: "completed", label: "Completed"},
		{name: "held beats a wait", work: client.Work{State: "ready", Hold: &client.Hold{Reason: "busy", At: at0(5 * time.Hour)}}, wait: wait, group: "held", kind: "held", detail: "busy"},
		{name: "a waiting Work held for a refused wake", work: client.Work{State: "waiting", Hold: &client.Hold{Reason: "Its wait is blocked: depth_limit", At: at0(5 * time.Hour)}}, runs: []client.Run{turnRun(1, "parked", time.Hour, 2*time.Hour)}, wait: wait, group: "held", kind: "held", detail: "Its wait is blocked: depth_limit"},
		{name: "an offered action is a decision", work: client.Work{State: "waiting", Actions: ask, Panels: panel}, wait: wait, group: "attention", kind: "decision", label: "The agent asks", detail: "May I refund?"},
		{name: "an action with no panel", work: client.Work{State: "waiting", Actions: ask}, group: "attention", kind: "decision", label: "Answer the question"},
		{name: "a standing action is not a decision", work: client.Work{State: "waiting", Actions: []client.Action{message}, Panels: thread}, wait: timer, group: "waiting", kind: "waiting", detail: "Check Gmail for trade offers", waiting: "time"},
		{name: "an awaited action beside a standing one", work: client.Work{State: "waiting", Actions: append([]client.Action{message}, ask...), Panels: append(thread, panel...)}, wait: wait, group: "attention", kind: "decision", label: "The agent asks", detail: "May I refund?"},
		{name: "running", work: client.Work{State: "running"}, runs: []client.Run{turnRun(1, "running", time.Hour, 0)}, group: "running", kind: "running", detail: "Turn 1"},
		{name: "retries exhausted is a repair", work: client.Work{State: "ready"}, runs: []client.Run{exhausted}, group: "attention", kind: "repair", detail: "workspace setup failed"},
		{name: "retries left is queued", work: client.Work{State: "ready"}, runs: []client.Run{retrying}, group: "queued", kind: "queued", detail: "Retrying after turn 1 failed"},
		{name: "an operator-cancelled turn is a repair", work: client.Work{State: "ready"}, runs: []client.Run{turnRun(1, "cancelled", time.Hour, 2*time.Hour)}, group: "attention", kind: "repair", label: "Turn cancelled"},
		{name: "waiting says what for", work: client.Work{State: "waiting"}, runs: []client.Run{turnRun(1, "parked", time.Hour, 2*time.Hour)}, wait: wait, group: "waiting", kind: "waiting", detail: "checks on PR #137", waiting: "github"},
		{name: "a wait with progress", work: client.Work{State: "waiting"}, wait: &client.Wait{State: "active", Description: "checks on PR #137", Progress: "3 of 7 checks finished"}, group: "waiting", kind: "waiting", detail: "checks on PR #137 · 3 of 7 checks finished", waiting: "events"},
		{name: "a wait on a timer alone", work: client.Work{State: "waiting"}, wait: &client.Wait{State: "active", TimeoutTimerID: "timer/1", Description: "until Mon 09:00"}, group: "waiting", kind: "waiting", detail: "until Mon 09:00", waiting: "time"},
		{name: "a wait across sources", work: client.Work{State: "waiting"}, wait: &client.Wait{State: "active", Match: client.Match{Types: []string{"github.x", "slack.y"}}}, group: "waiting", kind: "waiting", waiting: "events"},
		{name: "queued", work: client.Work{State: "ready"}, group: "queued", kind: "queued", detail: "Ready for the next turn"},
		{name: "ready with queued input is queued", work: client.Work{State: "ready", Inputs: &one}, runs: []client.Run{handoff}, group: "queued", kind: "queued", detail: "Ready for the next turn"},
		{name: "ready with no queued input waits for its next event", work: client.Work{State: "ready", Inputs: &zero}, runs: []client.Run{handoff}, group: "waiting", kind: "waiting", detail: "Opened PR #12; waiting on checks", waiting: "events"},
		{name: "idle with no summary", work: client.Work{State: "ready", Inputs: &zero}, group: "waiting", kind: "waiting", detail: "Waiting for its next event", waiting: "events"},
		{name: "a retry is queued with no input", work: client.Work{State: "ready", Inputs: &zero}, runs: []client.Run{retrying}, group: "queued", kind: "queued", detail: "Retrying after turn 1 failed"},
		{name: "held back by a limit says so", work: client.Work{State: "ready", Inputs: &one, Annotations: []client.Annotation{{Source: "runtime", Name: "waiting", Value: "limit reached: agent turns (settings.concurrency) 5/5 running"}}}, group: "queued", kind: "queued", detail: "Waiting for a slot: agent turns (settings.concurrency) 5/5 running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.work.ID, c.work.CreatedAt, c.work.UpdatedAt = "w", at0(0), at0(6*time.Hour)
			d := newWork(c.work, c.runs, c.wait, now).Disp
			if d.Group != c.group || d.Kind != c.kind {
				t.Fatalf("got %s/%s, want %s/%s", d.Group, d.Kind, c.group, c.kind)
			}
			if c.label != "" && d.Label != c.label {
				t.Errorf("label %q, want %q", d.Label, c.label)
			}
			if c.detail != "" && d.Detail != c.detail {
				t.Errorf("detail %q, want %q", d.Detail, c.detail)
			}
			if d.Waiting != c.waiting {
				t.Errorf("waiting on %q, want %q", d.Waiting, c.waiting)
			}
		})
	}
}

func TestTimesAndSegments(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	parked := turnRun(1, "completed", time.Hour, 3*time.Hour)
	// Parked from 1h20m to 2h40m: two segments of 20m.
	parked.Segments = []client.RunSegment{{StartedAt: at0(time.Hour), EndedAt: at0(80 * time.Minute), Parked: true}, {StartedAt: at0(160 * time.Minute), EndedAt: at0(3 * time.Hour)}}
	parked.ActiveMS = (40 * time.Minute).Milliseconds()
	running := turnRun(2, "running", 4*time.Hour, 0)
	v := newWork(client.Work{ID: "w", State: "running", CreatedAt: at0(0), UpdatedAt: at0(4 * time.Hour)}, []client.Run{running, parked}, nil, now)
	if v.Turns[0].Number != 1 || len(v.Turns[0].Segments) != 2 {
		t.Fatalf("turns not ordered with segments: %+v", v.Turns)
	}
	if got := v.Active(now); got != 40*time.Minute+time.Hour {
		t.Fatalf("active %s", got)
	}
	if got := v.Elapsed(now); got != 5*time.Hour {
		t.Fatalf("elapsed %s", got)
	}
	closed := newWork(client.Work{ID: "w", State: "completed", CreatedAt: at0(0), UpdatedAt: at0(3 * time.Hour)}, []client.Run{parked}, nil, now)
	if got := closed.Elapsed(now); got != 3*time.Hour {
		t.Fatalf("closed elapsed runs to its close: %s", got)
	}
	// A run timed from before the Work was created (clock skew) never shows
	// the Work executing for longer than it has existed.
	early := turnRun(1, "running", -10*time.Second, 0)
	young := newWork(client.Work{ID: "w", State: "running", CreatedAt: at0(0), UpdatedAt: at0(0)}, []client.Run{early}, nil, t0.Add(3*time.Minute))
	if got := young.Active(t0.Add(3 * time.Minute)); got != 3*time.Minute {
		t.Fatalf("active %s, want capped at the 3m lifetime", got)
	}
}

func TestLifeline(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	mk := func(state string, runs ...client.Run) workView {
		return newWork(client.Work{ID: "w", State: state, CreatedAt: at0(0), UpdatedAt: at0(10 * time.Hour)}, runs, nil, now)
	}
	t.Run("single turn spans its share", func(t *testing.T) {
		l := lifelineOf(mk("waiting", turnRun(1, "completed", 0, 5*time.Hour)), now, 22)
		if len(l.Bars) != 1 || l.Bars[0].X != 4 || int(l.Bars[0].Width) != 496 {
			t.Fatalf("bars %+v", l.Bars)
		}
		if l.Mark != "waiting" || l.EndX != lifeWidth-4 {
			t.Fatalf("end %v mark %q", l.EndX, l.Mark)
		}
	})
	t.Run("many turns keep order and tone", func(t *testing.T) {
		l := lifelineOf(mk("running", turnRun(1, "failed", 0, time.Hour), turnRun(2, "completed", 2*time.Hour, 3*time.Hour), turnRun(3, "running", 9*time.Hour, 0)), now, 22)
		var tones []string
		for i, b := range l.Bars {
			tones = append(tones, b.Tone)
			if i > 0 && b.X <= l.Bars[i-1].X {
				t.Fatalf("bars out of order: %+v", l.Bars)
			}
		}
		if !slices.Equal(tones, []string{"fail", "ink", "run"}) {
			t.Fatalf("tones %v", tones)
		}
		if last := l.Bars[2]; last.X+last.Width < 995 {
			t.Fatalf("running bar should reach now: %+v", last)
		}
	})
	t.Run("zero-length turn has the minimum width", func(t *testing.T) {
		l := lifelineOf(mk("waiting", turnRun(1, "completed", time.Hour, time.Hour+time.Second)), now, 22)
		if l.Bars[0].Width != lifeMinBar {
			t.Fatalf("width %v", l.Bars[0].Width)
		}
	})
	t.Run("closed Work has no end mark", func(t *testing.T) {
		v := newWork(client.Work{ID: "w", State: "completed", CreatedAt: at0(0), UpdatedAt: at0(2 * time.Hour)}, []client.Run{turnRun(1, "completed", 0, 2*time.Hour)}, nil, now)
		l := lifelineOf(v, now, 22)
		if l.Mark != "" || l.Bars[0].Width < 990 {
			t.Fatalf("closed: %+v", l)
		}
	})
	t.Run("events outside the span are clamped", func(t *testing.T) {
		v := mk("waiting")
		v.Events = []client.Event{{OccurredAt: at0(-time.Hour), Source: "github"}, {OccurredAt: at0(20 * time.Hour), Source: "github-bot"}}
		l := lifelineOf(v, now, 22)
		if l.Ticks[0].X != 4 || l.Ticks[1].X != lifeWidth-4 || !l.Ticks[0].Person || l.Ticks[1].Person {
			t.Fatalf("ticks %+v", l.Ticks)
		}
	})
	svg := string(lifelineSVG(lifelineOf(mk("waiting", turnRun(1, "completed", 0, time.Hour)), now, 22)))
	if strings.Contains(svg, "style=") || !strings.Contains(svg, `class="mark mark-wait"`) {
		t.Fatalf("svg: %s", svg)
	}
}

func TestWorldReadsListsAndOffersOnlyForOpenWork(t *testing.T) {
	f, s, _ := setup(t)
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "waiting", Revision: "7"}
	f.work["wrk_2"] = client.Work{ID: "wrk_2", State: "ready", Revision: "2", Hold: &client.Hold{Reason: "busy"}}
	f.work["wrk_3"] = client.Work{ID: "wrk_3", State: "completed", Revision: "9"}
	f.work["wrk_4"] = client.Work{ID: "wrk_4", State: "waiting", Revision: "4", Hold: &client.Hold{Reason: "Its wait is blocked"}}
	w := s.world(context.Background())
	if len(w.Works) != 3 || len(w.Errs) != 0 {
		t.Fatalf("works %d errs %v", len(w.Works), w.Errs)
	}
	if !slices.Equal(f.calls, []string{"works+runs ready,running,waiting"}) {
		t.Fatalf("calls %v", f.calls)
	}
	if f.withCalls != 3 {
		t.Fatalf("offers asked for %d Work, want the 3 open ones", f.withCalls)
	}
	for _, v := range w.Works {
		if v.ID == "wrk_2" && v.Disp.Group != "held" {
			t.Fatalf("ready Work's hold not read: %+v", v.Disp)
		}
		if v.ID == "wrk_4" && v.Disp.Group != "held" {
			t.Fatalf("waiting Work's hold not read: %+v", v.Disp)
		}
		if v.ID == "wrk_1" && v.Disp.Kind != "decision" {
			t.Fatalf("an offered answer should make a decision: %+v", v.Disp)
		}
	}
}

// The world reads each open Work's hold and queued input without the
// extensions' annotations (#545), keeping the runtime's capacity wait; a
// daemon that refuses annotations=runtime is read in full.
func TestWorldReadsHoldsWithoutExtensionAnnotations(t *testing.T) {
	for _, old := range []bool{false, true} {
		f, s, _ := setup(t)
		f.old = old
		one := 1
		f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "ready", Revision: "2", Inputs: &one, Annotations: []client.Annotation{
			{Source: "runtime", Name: "waiting", Value: "limit reached: agent turns (settings.concurrency) 5/5 running"},
			{Source: "github", Name: "issue", Value: "o/r#1"},
		}}
		w := s.world(context.Background())
		if got := f.runtimeReads.Load(); got != 1 {
			t.Fatalf("old=%v: runtime reads %d, want 1", old, got)
		}
		i := slices.IndexFunc(w.Works, func(v workView) bool { return v.ID == "wrk_1" })
		if i < 0 || w.Works[i].Disp.Detail != "Waiting for a slot: agent turns (settings.concurrency) 5/5 running" {
			t.Fatalf("old=%v: capacity wait lost: %+v", old, w.Works)
		}
	}
}

func TestWorldOnADaemonWithoutListForms(t *testing.T) {
	f, s, _ := setup(t)
	f.old = true
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "waiting", Revision: "7"}
	f.work["wrk_3"] = client.Work{ID: "wrk_3", State: "completed", Revision: "9"}
	w := s.world(context.Background())
	if len(w.Errs) != 0 || len(w.Works) != 1 {
		t.Fatalf("works %d errs %v", len(w.Works), w.Errs)
	}
	for _, v := range w.Works {
		if v.ID == "wrk_1" && v.Disp.Kind != "decision" {
			t.Fatalf("offers on waiting Work not read one by one: %+v", v.Disp)
		}
	}
}

// A daemon from before the Work list's runs (#544) has each open Work's
// runs read by its ID, and closed Work not read at all.
func TestWorldOnADaemonWithoutListedRuns(t *testing.T) {
	f, s, _ := setup(t)
	f.noRuns = true
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "running", Revision: "7"}
	f.work["wrk_3"] = client.Work{ID: "wrk_3", State: "completed", Revision: "9"}
	live := turnRun(1, "running", 0, 0)
	live.ID, live.WorkID = "work/wrk_1/attempt/1", "wrk_1"
	f.runs = []client.Run{live}
	w := s.world(context.Background())
	if len(w.Errs) != 0 || len(w.Works) != 1 || len(w.Works[0].Turns) != 1 {
		t.Fatalf("works %+v errs %v", w.Works, w.Errs)
	}
	if !slices.Equal(f.runReads, []string{"wrk_1"}) {
		t.Fatalf("runs read for %v, want the open Work alone", f.runReads)
	}
}

// On a daemon without listed runs, each Work's runs are read by its ID,
// so however many runs the daemon holds, an open Work's live turn shows
// (#450).
func TestWorldReadsOpenWorksRunsPastTheCap(t *testing.T) {
	f, s, _ := setup(t)
	f.noRuns = true
	f.work["wrk_old"] = client.Work{ID: "wrk_old", State: "completed", Revision: "1"}
	f.work["wrk_live"] = client.Work{ID: "wrk_live", State: "running", Revision: "2"}
	for i := range fetchCap {
		r := turnRun(uint32(i+1), "completed", 0, time.Minute)
		r.WorkID = "wrk_old"
		f.runs = append(f.runs, r)
	}
	live := turnRun(1, "running", 0, 0)
	live.ID, live.WorkID = "work/wrk_live/attempt/1", "wrk_live"
	f.runs = append(f.runs, live)
	w := s.world(context.Background())
	if len(w.Errs) != 0 {
		t.Fatalf("errs %v", w.Errs)
	}
	for _, v := range w.Works {
		if v.ID != "wrk_live" {
			continue
		}
		if len(v.Turns) != 1 || v.Disp.Group == "queued" {
			t.Fatalf("live turn past the cap missing: turns %d disp %+v", len(v.Turns), v.Disp)
		}
		return
	}
	t.Fatal("no row for the running Work")
}

// A Work with fetchCap runs of its own still shows its live and parked
// turns: they are read by state too.
func TestWorldReadsAWorksLiveRunPastItsOwnCap(t *testing.T) {
	f, s, _ := setup(t)
	f.noRuns = true
	f.work["wrk_live"] = client.Work{ID: "wrk_live", State: "running", Revision: "2"}
	for i := range fetchCap {
		r := turnRun(uint32(i+1), "completed", 0, time.Minute)
		r.ID, r.WorkID = fmt.Sprintf("work/wrk_live/attempt/%d", i+1), "wrk_live"
		f.runs = append(f.runs, r)
	}
	parked := turnRun(fetchCap+1, "parked", 0, 0)
	parked.ID, parked.WorkID = "work/wrk_live/attempt/parked", "wrk_live"
	live := turnRun(fetchCap+2, "running", 0, 0)
	live.ID, live.WorkID = "work/wrk_live/attempt/live", "wrk_live"
	f.runs = append(f.runs, parked, live)
	w := s.world(context.Background())
	if len(w.Errs) != 0 || len(w.Works) != 1 {
		t.Fatalf("works %d errs %v", len(w.Works), w.Errs)
	}
	if v := w.Works[0]; len(v.Turns) != fetchCap+2 || v.Disp.Group != "running" {
		t.Fatalf("live turn past the Work's own cap missing: turns %d disp %+v", len(v.Turns), v.Disp)
	}
}

// The world is the open Work, its runs listed with it: however much Work
// has closed, a read is one list of the open Work and no runs read one
// Work at a time (#544).
func TestWorldReadsOnlyOpenWorkWithListedRuns(t *testing.T) {
	f, s, _ := setup(t)
	f.work = map[string]client.Work{"wrk_open": {ID: "wrk_open", State: "running", Revision: "5"}}
	for i := range 300 {
		id := fmt.Sprintf("wrk_done_%d", i)
		f.work[id] = client.Work{ID: id, State: "completed", Revision: "3"}
	}
	live := turnRun(1, "running", 0, 0)
	live.ID, live.WorkID = "work/wrk_open/attempt/1", "wrk_open"
	f.runs = []client.Run{live}
	w := s.world(context.Background())
	if len(w.Works) != 1 || len(w.Works[0].Turns) != 1 || w.Works[0].Disp.Group != "running" {
		t.Fatalf("works %+v", w.Works)
	}
	if len(f.runReads) != 0 || !slices.Equal(f.calls, []string{"works+runs ready,running,waiting"}) {
		t.Fatalf("runs read %v, calls %v", f.runReads, f.calls)
	}
}

// Without a display, Work is named by its key's ID: the row shows the
// source beside it.
func TestTitleWithoutDisplay(t *testing.T) {
	w := client.Work{ID: "w", State: "waiting", CorrelationKeys: []client.Key{{Source: "slack/T1", ExternalID: "channel/C1/thread/1.2"}}}
	if v := newWork(w, nil, nil, t0); v.Title != "channel/C1/thread/1.2" || v.Source != "slack" {
		t.Fatalf("title %q source %q", v.Title, v.Source)
	}
	w.Display = &client.Display{Title: "Can you look at the build?"}
	if v := newWork(w, nil, nil, t0); v.Title != "Can you look at the build?" {
		t.Fatalf("display title %q", v.Title)
	}
}
