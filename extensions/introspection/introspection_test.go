package introspection

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func scope(id domain.ID, enabled bool, types ...string) domain.Scope {
	var exts []map[string]any
	for _, t := range types {
		exts = append(exts, map[string]any{"name": "x", "type": t, "config": map[string]any{}})
	}
	meta, _ := json.Marshal(map[string]any{"enabled": enabled, "extensions": exts})
	return domain.Scope{ID: id, Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}
}

func TestConfigValidation(t *testing.T) {
	for _, ok := range []string{``, `{}`} {
		if err := ValidateExtension(json.RawMessage(ok), nil); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
		if err := ValidateRoute(json.RawMessage(ok)); err != nil {
			t.Errorf("route %q: %v", ok, err)
		}
	}
	for _, bad := range []string{`{"x":1}`, `[]`, `{} {}`} {
		if ValidateExtension(json.RawMessage(bad), nil) == nil || ValidateRoute(json.RawMessage(bad)) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if ValidateExtension(json.RawMessage(`{}`), []string{"token"}) == nil {
		t.Error("secrets accepted")
	}
	for _, ok := range []string{
		`{"notes":["team.notes","other.notes"],"telemetry":true}`,
		`{"sweep":{"cron":"41 8 * * *","rules":["CLAUDE.md"],"guide":"docs/rules.md","land":"pr","remote":"upstream"}}`,
		`{"triage":{"cron":"51 8 * * *","time_zone":"Europe/London","file_issues":true}}`,
		`{"host_watch":{}}`,
		`{"host_watch":{"interval_seconds":30,"load":2,"memory":80,"disk":95,"disks":["~/work"]}}`,
	} {
		if err := ValidateExtension(json.RawMessage(ok), nil); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{"notes":["core.causal"]}`, `{"notes":[""]}`, `{"notes":["a b"]}`,
		`{"sweep":{}}`, `{"sweep":{"cron":"@every 1m"}}`, `{"sweep":{"cron":"61 8 * * *"}}`,
		`{"triage":{"cron":"* * * * *","time_zone":"Mars/Base"}}`, `{"triage":{"cron":"* * * * *","land":"merge"}}`,
		`{"triage":{"cron":"* * * * *","rules":["CLAUDE.md"]}}`, `{"sweep":{"cron":"* * * * *","file_issues":true}}`,
		`{"sweep":{"cron":"* * * * *","rules":[""]}}`,
		`{"host_watch":{"interval_seconds":1}}`, `{"host_watch":{"memory":101}}`, `{"host_watch":{"disks":[""]}}`,
	} {
		if ValidateExtension(json.RawMessage(bad), nil) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, ok := range []string{`{"work":"report"}`, `{"work":"sweep"}`, `{"work":"triage"}`, `{"work":"fix"}`} {
		if err := ValidateRoute(json.RawMessage(ok)); err != nil {
			t.Errorf("route %s: %v", ok, err)
		}
	}
	if ValidateRoute(json.RawMessage(`{"work":"jobs"}`)) == nil {
		t.Error("unknown work accepted")
	}
	c, _ := ParseConfig(json.RawMessage(`{}`))
	if ns := c.NoteNamespaces(); len(ns) != 1 || ns[0] != NotesNamespace {
		t.Fatalf("default notes: %v", ns)
	}
	c, _ = ParseConfig(json.RawMessage(`{"notes":["team.notes","introspection.notes"]}`))
	if ns := c.NoteNamespaces(); len(ns) != 2 || ns[0] != NotesNamespace || ns[1] != "team.notes" {
		t.Fatalf("named notes: %v", ns)
	}
	if l, m, d := (HostWatch{}).Thresholds(); l != 1.5 || m != 90 || d != 90 || (HostWatch{}).Interval() != time.Minute {
		t.Fatal("host watch defaults")
	}
}

func TestEnabled(t *testing.T) {
	if !Enabled(scope("eng", true, "github", Type)) || Enabled(scope("eng", false, Type)) || Enabled(scope("eng", true, "github")) || Enabled(domain.Scope{ID: "eng"}) {
		t.Fatal("Enabled")
	}
}

func TestRoute(t *testing.T) {
	key := ReportKey("eng", "abc")
	if id, ok := ParseReportKey("eng", key); !ok || id != "abc" {
		t.Fatalf("parse: %q %v", id, ok)
	}
	if _, ok := ParseReportKey("ops", key); ok {
		t.Fatal("another scope's key parsed")
	}
	report := domain.AcceptedEvent{Event: domain.Event{Type: TypeReported, CorrelationKey: &key}}
	match := func(s domain.Scope, e domain.AcceptedEvent, w *domain.Work) extension.RouteDecision {
		d, err := Route{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: s, Work: w, Route: domain.Route{Config: domain.Payload{Data: []byte(`{}`)}}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := match(scope("eng", true, Type), report, nil); !d.Match || d.Kind != WorkKind || d.CreateID != WorkID("eng", "abc") {
		t.Fatalf("new report: %+v", d)
	}
	if d := match(scope("eng", true, Type), report, &domain.Work{Kind: WorkKind}); !d.Match || d.CreateID != "" {
		t.Fatalf("input of its Work: %+v", d)
	}
	for name, d := range map[string]extension.RouteDecision{
		"no entry":     match(scope("eng", true), report, nil),
		"disabled":     match(scope("eng", false, Type), report, nil),
		"other scope":  match(scope("ops", true, Type), report, nil),
		"other kind":   match(scope("eng", true, Type), report, &domain.Work{Kind: "github.issue"}),
		"other family": match(scope("eng", true, Type), domain.AcceptedEvent{Event: domain.Event{Type: "github.comment", CorrelationKey: &key}}, nil),
		"no key":       match(scope("eng", true, Type), domain.AcceptedEvent{Event: domain.Event{Type: TypeReported}}, nil),
	} {
		if d.Match {
			t.Errorf("%s matched", name)
		}
	}
}

func TestRenderEvent(t *testing.T) {
	data, _ := json.Marshal(Report{ID: "abc", Kind: "runtime", Title: "os call failed", Body: "503", Work: "w", Execution: "x", Trigger: "t"})
	text, ok := RenderEvent(domain.Event{ID: "emit/1", Type: TypeReported, Data: domain.Payload{Schema: SchemaReport, Data: data}})
	if !ok || !strings.Contains(text, "os call failed") || !strings.Contains(text, OpResolve) || !strings.Contains(text, "execution x") {
		t.Fatalf("render: %q %v", text, ok)
	}
	data, _ = json.Marshal(Report{ID: "h", Kind: "env", Title: "host: disk / is at 95%", Body: "full", Origin: OriginHostWatch})
	if text, ok := RenderEvent(domain.Event{ID: "introspection-1", Type: TypeReported, Data: domain.Payload{Schema: SchemaReport, Data: data}}); !ok || !strings.Contains(text, "extension's host-watch") {
		t.Fatalf("origin render: %q", text)
	}
	due, _ := json.Marshal(Due{Run: WorkTriage, Slot: "2026-10-26 08:51 UTC", Epoch: "e"})
	if text, ok := RenderEvent(domain.Event{Type: TypeTriageDue, Data: domain.Payload{Schema: SchemaDue, Data: due}}); !ok || !strings.Contains(text, OpEvidence) || !strings.Contains(text, "08:51") {
		t.Fatalf("due render: %q", text)
	}
	fix, _ := json.Marshal(Fix{ID: "f", Title: "Skills call reply.sh", Body: "edit", Reports: []string{"r1"}, Work: "w", Execution: "x"})
	if text, ok := RenderEvent(domain.Event{ID: "emit/f", Type: TypeFixRequested, CausationID: "emit/r1", Data: domain.Payload{Schema: SchemaFix, Data: fix}}); !ok || !strings.Contains(text, "caused by report Event emit/r1") || !strings.Contains(text, OpResolve) {
		t.Fatalf("fix render: %q", text)
	}
	if _, ok := RenderEvent(domain.Event{Type: "github.comment"}); ok {
		t.Fatal("rendered another family")
	}
}

func TestRegistration(t *testing.T) {
	tools, err := NewTools(ToolsOptions{Store: nil})
	if err == nil || tools != nil {
		t.Fatal("tools without a store")
	}
	r := (&Tools{}).Registration()
	if r.Name != Type || len(r.EventTypes) != 2 || r.EventTypes[0] != TypeReported || r.EventTypes[1] != TypeFixRequested || len(r.Operations) != len(Operations()) {
		t.Fatalf("registration: %+v", r)
	}
	for _, op := range r.Operations {
		if !strings.HasPrefix(op.Name, Type+".") {
			t.Errorf("operation %s is outside the extension's namespace", op.Name)
		}
	}
}

func TestSamplesTraceAgainstTheScope(t *testing.T) {
	s := scope("eng", true, Type)
	samples := Route{}.Samples(s, domain.Route{})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, x := range samples {
		if x.ID == "" || x.Title == "" || x.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", x)
		}
	}
	d, err := Route{}.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: samples[0].Event}, Scope: s, Route: domain.Route{Config: domain.Payload{Data: []byte(`{}`)}}})
	if err != nil || !d.Match || d.Kind != WorkKind {
		t.Fatalf("the report sample must start Work: %+v, %v", d, err)
	}
	if got := (Route{}).Samples(scope("eng", true), domain.Route{}); len(got) != 0 {
		t.Fatal("samples offered without an entry")
	}
}
