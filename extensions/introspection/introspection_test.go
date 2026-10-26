package introspection

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
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
		d, err := Route{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: s, Work: w})
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
	if r.Name != Type || len(r.EventTypes) != 1 || r.EventTypes[0] != TypeReported || len(r.Operations) != len(Operations()) {
		t.Fatalf("registration: %+v", r)
	}
	for _, op := range r.Operations {
		if !strings.HasPrefix(op.Name, Type+".") {
			t.Errorf("operation %s is outside the extension's namespace", op.Name)
		}
	}
}
