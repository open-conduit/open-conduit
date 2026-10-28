package tracker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func scopeWith(t *testing.T, typ, cfg string) domain.Scope {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"extensions": []map[string]any{{"type": typ, "config": json.RawMessage(cfg)}}})
	return domain.Scope{ID: "eng", Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}
}

func event(t *testing.T, typ, schema string, payload any, key string) domain.AcceptedEvent {
	t.Helper()
	data, _ := json.Marshal(payload)
	return domain.AcceptedEvent{Event: domain.Event{ID: "e1", Type: typ, Source: "linear/eng", DeliveryID: "d",
		CorrelationKey: &domain.CorrelationKey{Source: "linear/eng", ExternalID: key}, Data: domain.Payload{Schema: schema, Data: data}}}
}

func TestRouteAdmissionMatchAndInput(t *testing.T) {
	r := Route{Type: "linear"}
	scope := scopeWith(t, "linear", `{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"labels":{"require":["agent"]}}`)
	tt := TypesOf("linear")
	open := IssuePayload{Key: "ENG-1", State: "open", Labels: []string{"agent"}, Attrs: map[string][]string{"team": {"ENG"}, "priority": {"High"}}}
	match := func(e domain.AcceptedEvent, w *domain.Work, route domain.Route) extension.RouteDecision {
		d, err := r.Match(context.Background(), extension.RouteInput{Event: e, Scope: scope, Work: w, Route: route})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := match(event(t, tt.IssueOpen, tt.SchemaIssue, open, "issue/ENG-1"), nil, domain.Route{}); !d.Match || d.Kind != "linear.issue" {
		t.Fatalf("admitted issue: %+v", d)
	}
	for name, p := range map[string]IssuePayload{
		"waiting":       {Key: "ENG-1", State: "open", Waiting: true, Labels: []string{"agent"}},
		"skip":          {Key: "ENG-1", State: "open", Labels: []string{"agent", "backlog"}},
		"not required":  {Key: "ENG-1", State: "open"},
		"claimed":       {Key: "ENG-1", State: "open", Labels: []string{"agent", "agent-in-progress"}},
		"closed":        {Key: "ENG-1", State: "closed", Labels: []string{"agent"}},
		"other scope's": {Key: "ENG-1", State: "open", Labels: []string{"agent"}},
	} {
		e := event(t, tt.IssueOpen, tt.SchemaIssue, p, "issue/ENG-1")
		if name == "other scope's" {
			e.Event.Source = "linear/ops"
		}
		if d := match(e, nil, domain.Route{}); d.Match {
			t.Errorf("%s issue admitted", name)
		}
	}
	reopened := IssuePayload{Key: "ENG-1", State: "open", Labels: []string{"agent", "agent-in-progress"}, Generation: "reopen-1"}
	if d := match(event(t, tt.IssueOpen, tt.SchemaIssue, reopened, "issue/ENG-1/generation/reopen-1"), nil, domain.Route{}); !d.Match {
		t.Fatal("a claimed issue's reopened generation is admitted")
	}
	// Multi-scope routing: the route's match selects issues by attribute.
	high := domain.Route{Config: domain.Payload{Data: json.RawMessage(`{"match":{"priority":["high","urgent"],"labels":["AGENT"]}}`)}}
	low := domain.Route{Config: domain.Payload{Data: json.RawMessage(`{"match":{"priority":["low"]}}`)}}
	if d := match(event(t, tt.IssueOpen, tt.SchemaIssue, open, "issue/ENG-1"), nil, high); !d.Match {
		t.Fatal("matching route")
	}
	if d := match(event(t, tt.IssueOpen, tt.SchemaIssue, open, "issue/ENG-1"), nil, low); d.Match {
		t.Fatal("non-matching route claimed")
	}
	w := &domain.Work{ID: "w1", CorrelationKeys: []domain.CorrelationKey{{Source: "linear/eng", ExternalID: "issue/ENG-1"}}}
	if d := match(event(t, tt.IssueOpen, tt.SchemaIssue, open, "issue/ENG-1"), w, domain.Route{}); d.Match {
		t.Fatal("open-state updates are never input")
	}
	if d := match(event(t, tt.IssueClosed, tt.SchemaIssue, IssuePayload{Key: "ENG-1", State: "closed"}, "issue/ENG-1"), w, domain.Route{}); !d.Match {
		t.Fatal("a close is input")
	}
	human := CommentPayload{Issue: "ENG-1", ID: "c1", Body: "hi"}
	if d := match(event(t, tt.CommentHuman, tt.SchemaComment, human, "issue/ENG-1"), w, domain.Route{}); !d.Match {
		t.Fatal("a human comment is input")
	}
	edited := human
	edited.Edited = true
	if d := match(event(t, tt.CommentHuman, tt.SchemaComment, edited, "issue/ENG-1"), w, domain.Route{}); d.Match {
		t.Fatal("an edit is not input")
	}
	own := CommentPayload{Issue: "ENG-1", ID: "c2", Agent: &Agent{Work: "w1"}}
	other := CommentPayload{Issue: "ENG-1", ID: "c3", Agent: &Agent{Work: "w9"}}
	legacyOwn := CommentPayload{Issue: "ENG-1", ID: "c4", Agent: &Agent{From: "eng-1"}}
	if d := match(event(t, tt.CommentAgent, tt.SchemaComment, own, "issue/ENG-1"), w, domain.Route{}); d.Match {
		t.Fatal("own note handed back")
	}
	if d := match(event(t, tt.CommentAgent, tt.SchemaComment, legacyOwn, "issue/ENG-1"), w, domain.Route{}); d.Match {
		t.Fatal("own Open Supervisor note handed back")
	}
	if d := match(event(t, tt.CommentAgent, tt.SchemaComment, other, "issue/ENG-1"), w, domain.Route{}); !d.Match {
		t.Fatal("another Work's note is input")
	}
	if d := match(event(t, tt.CommentBot, tt.SchemaComment, human, "issue/ENG-1"), w, domain.Route{}); d.Match {
		t.Fatal("bot comments are never input")
	}
	// A manual claim overrides labels but only an operator makes one.
	claim := event(t, tt.ClaimRequested, "", nil, "issue/ENG-1")
	if d := match(claim, nil, domain.Route{}); d.Match {
		t.Fatal("a claim not from an operator")
	}
	claim.Event.Source = ManualSource
	if d := match(claim, nil, low); !d.Match || d.Kind != "linear.issue" {
		t.Fatalf("manual claim: %+v", d)
	}
}

func TestGenerationAndKeys(t *testing.T) {
	t1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	reopens := []time.Time{t2, t1}
	if g, p, _ := generation("ENG-1", reopens, t1.Add(-time.Second)); g != "" || p != "" {
		t.Fatal(g, p)
	}
	if g, p, since := generation("ENG-1", reopens, t1.Add(time.Hour)); g != generationOf(t1) || p != "issue/ENG-1" || !since.Equal(t1) {
		t.Fatal(g, p)
	}
	if g, p, _ := generation("ENG-1", reopens, t2.Add(time.Hour)); g != generationOf(t2) || p != IssueKey("ENG-1", generationOf(t1)) {
		t.Fatal(g, p)
	}
	if k, g, ok := ParseIssueKey(IssueKey("ENG-12", generationOf(t1))); !ok || k != "ENG-12" || g != generationOf(t1) {
		t.Fatal(k, g, ok)
	}
	for _, bad := range []string{"issue/12", "issue/ENG-1/generation/x", "pr/ENG-1"} {
		if _, _, ok := ParseIssueKey(bad); ok {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestConfigAndRouteValidation(t *testing.T) {
	for _, bad := range []string{`{"match":{"Bad Name":["x"]}}`, `{"match":{"team":[]}}`, `{"match":{"team":[" "]}}`, `{"other":1}`} {
		if ValidateRoute(json.RawMessage(bad)) == nil {
			t.Errorf("route %s accepted", bad)
		}
	}
	c := Common{Namespace: "linear/eng", Scope: "eng", ManageWebhook: true}
	if _, err := c.Settings("linear", nil, true); err == nil {
		t.Error("manage_webhook without a secret")
	}
	c = Common{Namespace: "linear/eng", Scope: "eng", Transitions: map[string]string{"review": " In Review "}, ClosedHorizonHours: 24}
	s, err := c.Settings("linear", nil, true)
	if err != nil || s.Transitions["review"] != "In Review" || s.ClosedHorizon != 24*time.Hour || s.Labels.Claimed != "agent-in-progress" {
		t.Fatal(s, err)
	}
}

func TestVerifyHMAC(t *testing.T) {
	body := []byte(`{"a":1}`)
	sig := SignHMAC("k", body)
	if !VerifyHMAC("k", body, sig) || !VerifyHMAC("k", body, "sha256="+sig) || VerifyHMAC("", body, sig) || VerifyHMAC("k", body, "zz") || VerifyHMAC("j", body, sig) {
		t.Fatal("hmac")
	}
}

func TestSamplesTraceAgainstTheScope(t *testing.T) {
	r := Route{Type: "linear"}
	scope := scopeWith(t, "linear", `{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"labels":{"require":["agent"]}}`)
	route := domain.Route{Config: domain.Payload{Data: json.RawMessage(`{"match":{"priority":["high"]}}`)}}
	samples := r.Samples(scope, route)
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
	}
	d, err := r.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: samples[0].Event}, Scope: scope, Route: route})
	if err != nil || !d.Match || d.Kind != "linear.issue" {
		t.Fatalf("the new-issue sample must start Work: %+v, %v", d, err)
	}
}

// An issue's Work says what it is: title, key, author and link.
func TestRouteDisplay(t *testing.T) {
	r := Route{Type: "linear"}
	scope := scopeWith(t, "linear", `{"namespace":"linear/eng","scope":"eng","teams":["ENG"]}`)
	tt := TypesOf("linear")
	p := IssuePayload{Key: "ENG-12", Title: "Example widget is slow", Author: "example-user", URL: "https://linear.example/eng/issue/ENG-12", State: "open", Attrs: map[string][]string{"team": {"ENG"}}}
	d, err := r.Match(context.Background(), extension.RouteInput{Event: event(t, tt.IssueOpen, tt.SchemaIssue, p, "issue/ENG-12"), Scope: scope})
	if err != nil || !d.Match || d.Display == nil {
		t.Fatalf("%+v %v", d, err)
	}
	want := domain.WorkDisplay{Title: "Example widget is slow", Ref: "ENG-12", Requester: "example-user", Links: []domain.Link{{Label: "Linear ENG-12", URL: p.URL}}}
	if got := *d.Display; got.Title != want.Title || got.Ref != want.Ref || got.Requester != want.Requester || len(got.Links) != 1 || got.Links[0] != want.Links[0] {
		t.Fatalf("display %+v", got)
	}
	if err := extension.ValidateDisplay(d.Display); err != nil {
		t.Fatal(err)
	}
}
