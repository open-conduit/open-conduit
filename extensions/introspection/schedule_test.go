package introspection

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/extensions/introspection/host"
	"github.com/alsiola/open-conduit/observability"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Each route takes its own family on its own key prefix; standing runs are
// one Work per schedule epoch, and fixes one Work each.
func TestRouteWorks(t *testing.T) {
	s := scope("eng", true, Type)
	route := func(work string) domain.Route {
		return domain.Route{Config: domain.Payload{Data: []byte(`{"work":"` + work + `"}`)}}
	}
	ev := func(typ string, k domain.CorrelationKey) domain.AcceptedEvent {
		return domain.AcceptedEvent{Event: domain.Event{Type: typ, CorrelationKey: &k}}
	}
	match := func(r domain.Route, e domain.AcceptedEvent, w *domain.Work) extension.RouteDecision {
		d, err := Route{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: s, Work: w, Route: r})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	sweep := ev(TypeSweepDue, RunKey("eng", WorkSweep, "E1"))
	triage := ev(TypeTriageDue, RunKey("eng", WorkTriage, "E1"))
	fix := ev(TypeFixRequested, FixKey("eng", "f1"))
	if d := match(route("sweep"), sweep, nil); !d.Match || d.Kind != WorkKindSweep || d.CreateID != "introspection/eng/sweep/E1" {
		t.Fatalf("sweep: %+v", d)
	}
	if d := match(route("triage"), triage, nil); !d.Match || d.Kind != WorkKindTriage || d.CreateID != "introspection/eng/triage/E1" {
		t.Fatalf("triage: %+v", d)
	}
	if d := match(route("triage"), triage, &domain.Work{Kind: WorkKindTriage}); !d.Match || d.CreateID != "" {
		t.Fatalf("standing input: %+v", d)
	}
	if d := match(route("fix"), fix, nil); !d.Match || d.Kind != WorkKindFix || d.CreateID != FixWorkID("eng", "f1") {
		t.Fatalf("fix: %+v", d)
	}
	for name, d := range map[string]extension.RouteDecision{
		"report route, sweep slot": match(route("report"), sweep, nil),
		"sweep route, triage slot": match(route("sweep"), triage, nil),
		"fix route, report":        match(route("fix"), ev(TypeReported, ReportKey("eng", "r")), nil),
		"triage on a fix key":      match(route("triage"), ev(TypeTriageDue, FixKey("eng", "f")), nil),
		"bare run key":             match(route("sweep"), ev(TypeSweepDue, domain.CorrelationKey{Source: SourceNamespace("eng"), ExternalID: "sweep"}), nil),
		"other scope's namespace":  match(route("fix"), ev(TypeFixRequested, FixKey("ops", "f")), nil),
		"fix as sweep input":       match(route("fix"), fix, &domain.Work{Kind: WorkKindSweep}),
	} {
		if d.Match {
			t.Errorf("%s matched", name)
		}
	}
}

func newTimer(t *testing.T, s Schedule, now time.Time, current *domain.Timer) (*domain.Timer, spec, bool) {
	t.Helper()
	timer, raw, changed := desired("eng", WorkTriage, s, now, current)
	var sp spec
	if err := json.Unmarshal(raw, &sp); err != nil {
		t.Fatal(err)
	}
	return timer, sp, changed
}

// v1's calendar: a new schedule waits for its next slot; a firing publishes
// the latest slot strictly within six hours once and rearms; a slot missed
// by six hours or more is skipped; an edited timing catches up its latest
// slot unless it ran; the epoch survives edits.
func TestRecurrenceCalendar(t *testing.T) {
	daily := Schedule{Cron: "51 8 * * *"}
	now := at("2026-10-26T09:30:00Z")
	timer, sp, changed := newTimer(t, daily, now, nil)
	if !changed || timer == nil || !timer.FireAt.Equal(at("2026-10-27T08:51:00Z")) || sp.Epoch != "20261026T093000Z" {
		t.Fatalf("first arm: %+v %+v", timer, sp)
	}
	d, _ := DuePayload(timer.Event)
	if d.Last != "2026-10-26 08:51 UTC" || d.Epoch != sp.Epoch || timer.Event.CorrelationKey.ExternalID != "triage/"+sp.Epoch || timer.ID != TimerID("eng", WorkTriage) {
		t.Fatalf("first arm skips today's slot: %+v %+v", d, timer.Event)
	}
	raw, _ := json.Marshal(sp)

	// Fired on time: publishes the slot and rearms for tomorrow.
	occ, next, err := Recurrence(raw, *timer, at("2026-10-27T08:51:00Z"))
	if err != nil || occ == nil || next == nil {
		t.Fatalf("fire: %v %v %v", occ, next, err)
	}
	if p, _ := DuePayload(*occ); p.Slot != "2026-10-27 08:51 UTC" || occ.Type != TypeTriageDue || !next.FireAt.Equal(at("2026-10-28T08:51:00Z")) {
		t.Fatalf("occurrence %+v next %+v", p, next.FireAt)
	}
	// A replay of the same firing yields the same identity.
	again, _, _ := Recurrence(raw, *timer, at("2026-10-27T08:52:00Z"))
	if again == nil || again.ID != occ.ID || again.DeliveryID != occ.DeliveryID {
		t.Fatal("slot identity is not the instant")
	}
	// Five hours late: caught up once.
	if occ, _, _ := Recurrence(raw, *next, at("2026-10-28T13:50:00Z")); occ == nil {
		t.Fatal("a slot five hours late was not caught up")
	}
	// Exactly six hours late: skipped, rearmed for the next day.
	occ, n2, _ := Recurrence(raw, *next, at("2026-10-28T14:51:00Z"))
	if occ != nil || n2 == nil || !n2.FireAt.Equal(at("2026-10-29T08:51:00Z")) {
		t.Fatalf("six hours late: %v %v", occ, n2)
	}

	// An unchanged timing is left alone; an edit an hour past the new time
	// catches up that slot once, keeping the epoch.
	if _, _, changed := newTimer(t, daily, now, timer); changed {
		t.Fatal("unchanged schedule rearmed")
	}
	edited := Schedule{Cron: "30 8 * * *"}
	re, sp2, changed := newTimer(t, edited, now, timer)
	if !changed || re == nil || !re.FireAt.Equal(at("2026-10-26T08:30:00Z")) || sp2.Epoch != sp.Epoch {
		t.Fatalf("edit catch-up: %+v %+v", re, sp2)
	}
	// Once that slot ran, a further edit to the same time waits.
	ran := *re
	p, _ := DuePayload(ran.Event)
	p.Last, p.Spec = "2026-10-26 08:30 UTC", "old"
	ran.Event.Data.Data, _ = json.Marshal(p)
	if w, _, _ := newTimer(t, edited, now, &ran); w == nil || !w.FireAt.Equal(at("2026-10-27T08:30:00Z")) {
		t.Fatalf("ran slot repeated: %+v", w)
	}
}

// DST: a skipped wall-clock time has no slot; a repeated one runs once.
func TestRecurrenceDST(t *testing.T) {
	london := Schedule{Cron: "30 1 * * *", TimeZone: "Europe/London"}
	// 2026-10-25 01:30 happens twice in London (BST then GMT).
	timer, sp, _ := newTimer(t, london, at("2026-10-24T12:00:00Z"), nil)
	raw, _ := json.Marshal(sp)
	occ, next, _ := Recurrence(raw, *timer, timer.FireAt)
	if occ == nil || next == nil {
		t.Fatal("first 01:30 did not run")
	}
	if strings.HasPrefix(next.FireAt.Format(time.RFC3339), "2026-10-25") {
		t.Fatalf("repeated 01:30 runs twice: next %s", next.FireAt)
	}
	// 2027-03-28 01:30 does not exist in London.
	spring, sp2, _ := newTimer(t, london, at("2027-03-27T12:00:00Z"), nil)
	_ = sp2
	if strings.HasPrefix(spring.FireAt.UTC().Format(time.RFC3339), "2027-03-28") {
		t.Fatalf("a skipped wall-clock time has a slot: %s", spring.FireAt)
	}
}

func TestRecurrenceRejectsBadSpecs(t *testing.T) {
	for _, raw := range []string{`{`, `{"scope":"eng","run":"triage","cron":"51 8 * * *"}`, `{"scope":"eng","run":"triage","cron":"bad","epoch":"e"}`} {
		if _, _, err := Recurrence([]byte(raw), domain.Timer{}, time.Now()); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}

// v1's host watch: a crossing alerts once, recovery needs 5% headroom.
func TestHostWatcher(t *testing.T) {
	var w HostWatcher
	h := HostWatch{Disk: 90}
	sample := func(free uint64) host.Sample {
		return host.Sample{CPUs: 2, Disks: []host.Disk{{Path: "/", Total: 100, Free: free}}}
	}
	if a := w.Update(sample(50), h); len(a) != 0 || len(w.Over()) != 0 {
		t.Fatalf("under: %+v", a)
	}
	a := w.Update(sample(5), h)
	if len(a) != 1 || !a[0].Over || a[0].Key() != "disk:/" || !strings.Contains(a[0].String(), "disk / is at 95%, over the 90% threshold") {
		t.Fatalf("over: %+v", a)
	}
	if over := w.Over(); len(over) != 1 || over[0] != "disk / 95%" {
		t.Fatalf("over list: %v", over)
	}
	if a := w.Update(sample(5), h); len(a) != 0 {
		t.Fatal("still over alerted again")
	}
	if a := w.Update(sample(12), h); len(a) != 0 {
		t.Fatal("88% recovered without headroom")
	}
	if a := w.Update(sample(20), h); len(a) != 1 || a[0].Over || !strings.Contains(a[0].String(), "back to 80%") {
		t.Fatalf("recovered: %+v", a)
	}
	e, r := HostReport("eng", HostAlert{Metric: "disk", Path: "/", Value: 95, Threshold: 90, Over: true}, at("2026-10-26T10:00:00Z"))
	again, _ := HostReport("eng", HostAlert{Metric: "disk", Path: "/", Value: 96, Threshold: 90, Over: true}, at("2026-10-26T10:00:00Z"))
	if e.ID != again.ID || e.CausationID != "" || e.Type != TypeReported || r.Origin != OriginHostWatch || r.Kind != "env" || e.CorrelationKey.Source != SourceNamespace("eng") {
		t.Fatalf("host report: %+v %+v", e, r)
	}
	if props := HostProps(sample(5)); props["disk_path"] != "/" || props["cpus"] != 2 {
		t.Fatalf("props: %+v", props)
	}
}

type captured struct{ names []string }

func (c *captured) Capture(_ context.Context, name string, _ time.Time, _ map[string]any) error {
	c.names = append(c.names, name)
	return nil
}

// The telemetry adapter maps only the extension's acceptance records, and
// only while an entry turns it on.
func TestTelemetryExporter(t *testing.T) {
	sink := &captured{}
	on := false
	x := &Exporter{Sink: sink, Enabled: func() bool { return on }}
	rec := func(name, typ string) observability.Record {
		return observability.Record{Name: name, Attributes: map[string]string{"event.type": typ}}
	}
	batch := []observability.Record{rec("event.accepted", TypeReported), rec("event.duplicate", TypeReported), rec("event.accepted", TypeFixRequested),
		rec("event.accepted", TypeSweepDue), rec("event.accepted", "github.comment"), rec("route.decided", TypeReported)}
	if err := x.Export(context.Background(), batch); err != nil || len(sink.names) != 0 {
		t.Fatalf("exported while off: %v", sink.names)
	}
	on = true
	if err := x.Export(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sink.names, ",") != EventReport+","+EventFix+","+EventRunDue {
		t.Fatalf("events: %v", sink.names)
	}
}
