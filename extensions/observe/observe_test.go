package observe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func TestParseConfig(t *testing.T) {
	for raw, want := range map[string]string{
		`{"sources":[{"name":"slack-help","reporters":["watcher","poller"]}]}`: "",
		`{}`: "name at least one source",
		`{"sources":[{"name":"Slack Help","reporters":["w"]}]}`:                       "not a source name",
		`{"sources":[{"name":"a","reporters":["w"]},{"name":"a","reporters":["x"]}]}`: "declared twice",
		`{"sources":[{"name":"a","reporters":[]}]}`:                                   "at least one binding",
		`{"sources":[{"name":"a","reporters":["w"]}],"extra":1}`:                      "unknown field",
	} {
		_, err := ParseConfig(json.RawMessage(raw))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", raw, err, want)
		}
	}
	if ValidateExtension(json.RawMessage(`{"sources":[{"name":"a","reporters":["w"]}]}`), []string{"token"}) == nil {
		t.Error("an entry with secrets validated")
	}
	if ValidateRoute(json.RawMessage(`{}`)) == nil || ValidateRoute(json.RawMessage(`{"source":"slack-help"}`)) != nil {
		t.Error("route validation")
	}
}

func scope() domain.Scope {
	meta := `{"extensions":[{"type":"observe","config":{"sources":[{"name":"slack-help","reporters":["watcher"]},{"name":"forms","reporters":["watcher","poller"]}]}},{"type":"observe","config":{"sources":[{"name":"slack-help","reporters":["poller"]}]}}]}`
	return domain.Scope{ID: "help", Config: domain.Payload{Data: []byte(meta)}}
}

func TestReportsFor(t *testing.T) {
	s := scope()
	if got := ReportsFor(s, "watcher"); strings.Join(got, ",") != "forms,slack-help" {
		t.Errorf("watcher: %v", got)
	}
	if got := ReportsFor(s, "poller"); strings.Join(got, ",") != "forms,slack-help" {
		t.Errorf("poller, across entries: %v", got)
	}
	if got := ReportsFor(s, "responder"); len(got) != 0 {
		t.Errorf("responder: %v", got)
	}
}

func itemEvent(sc domain.ID, source, item, thread, text string) domain.AcceptedEvent {
	key := Key(sc, source, thread)
	data, _ := json.Marshal(Item{Source: source, Item: item, Thread: thread, Text: text})
	return domain.AcceptedEvent{Event: domain.Event{Type: TypeRecorded, CorrelationKey: &key, Data: domain.Payload{Schema: SchemaItem, Data: data}}}
}

func TestRoute(t *testing.T) {
	ctx := context.Background()
	route := domain.Route{Name: "help", Handler: Type, Config: domain.Payload{Data: []byte(`{"source":"slack-help"}`)}}
	in := func(e domain.AcceptedEvent, w *domain.Work) extension.RouteInput {
		return extension.RouteInput{Event: e, Scope: scope(), Work: w, Route: route}
	}
	d, _ := Route{}.Match(ctx, in(itemEvent("help", "slack-help", "1.001", "1.001", "VPN broken\nmore detail"), nil))
	if !d.Match || d.Kind != WorkKind || d.Display == nil || d.Display.Title != "VPN broken" {
		t.Fatalf("new thread: %+v", d)
	}
	thread := &domain.Work{Kind: WorkKind}
	if d, _ := (Route{}).Match(ctx, in(itemEvent("help", "slack-help", "1.002", "1.001", "and again"), thread)); !d.Match || d.Kind != "" {
		t.Fatalf("follow-up: %+v", d)
	}
	for name, c := range map[string]struct {
		e domain.AcceptedEvent
		w *domain.Work
	}{
		"another source":      {itemEvent("help", "forms", "1", "1", "x"), nil},
		"another scope's key": {itemEvent("ops", "slack-help", "1", "1", "x"), nil},
		"other Work's key":    {itemEvent("help", "slack-help", "1", "1", "x"), &domain.Work{Kind: "slack.conversation"}},
		"another family":      {domain.AcceptedEvent{Event: domain.Event{Type: "slack.message.human"}}, nil},
	} {
		if d, _ := (Route{}).Match(ctx, in(c.e, c.w)); d.Match {
			t.Errorf("%s matched: %+v", name, d)
		}
	}
	samples := Route{}.Samples(ctx, extension.SampleInput{Scope: scope(), Route: route, Now: time.Now()})
	if len(samples) != 1 {
		t.Fatalf("samples: %v", samples)
	}
	if d, _ := (Route{}).Match(ctx, in(domain.AcceptedEvent{Event: samples[0].Event}, nil)); !d.Match {
		t.Errorf("the sample does not match its own route: %+v", d)
	}
	undeclared := domain.Route{Config: domain.Payload{Data: []byte(`{"source":"nowhere"}`)}}
	if s := (Route{}).Samples(ctx, extension.SampleInput{Scope: scope(), Route: undeclared}); len(s) != 0 {
		t.Errorf("samples for an undeclared source: %v", s)
	}
}

func TestGuidanceFits(t *testing.T) {
	if n := len(strings.TrimSpace(recordGuidance)); n == 0 || n > extension.MaxGuidance {
		t.Fatalf("guidance is %d bytes", n)
	}
}
