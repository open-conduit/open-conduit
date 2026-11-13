package webhook_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/webhook"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

const scope = webhook.FixtureScope

func harness(t *testing.T) *sdktest.Host {
	t.Helper()
	h := sdktest.New(t, webhook.Build(webhook.Options{}), sdktest.WithSecrets(scope, map[string]string{webhook.FixtureSecret: webhook.FixtureValue}))
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{PublicURL: "https://hooks.example.test", Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: webhook.Name,
		Name: webhook.FixtureEntry, Config: webhook.FixtureConfig(), Secrets: []string{webhook.FixtureSecret}}}}); err != nil {
		t.Fatal(err)
	}
	return h
}

func post(t *testing.T, h *sdktest.Host, path string, header http.Header, body string) int {
	t.Helper()
	var out protocol.WebhookHandleResult
	if err := h.Call(context.Background(), "webhook.handle", protocol.WebhookHandleParams{Path: path, Method: http.MethodPost, Header: header, Body: []byte(body), ReceivedAt: sdktest.Start}, &out); err != nil {
		t.Fatal(err)
	}
	return out.Status
}

func url(hook string) string {
	return "/" + webhook.FixtureSource + "/" + hook + "/" + webhook.Token(webhook.FixtureValue, webhook.FixtureSource, hook)
}

func payloads(t *testing.T, h *sdktest.Host) []webhook.Payload {
	t.Helper()
	var out []webhook.Payload
	for _, a := range h.Events() {
		var p webhook.Payload
		if err := json.Unmarshal(a.Event.Data.Data, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestDeliveriesAreDeduplicatedAndThreaded(t *testing.T) {
	h := harness(t)
	plain := http.Header{"Content-Type": {"application/json"}}
	with := func(id string) http.Header {
		return http.Header{"Content-Type": {"application/json"}, "X-Delivery-Id": {id}}
	}
	for _, c := range []struct {
		name, path string
		header     http.Header
		body       string
		want       int
	}{
		{"first", url("deploys"), with("d-1"), `{"service":"checkout","status":"failed"}`, http.StatusAccepted},
		{"retry", url("deploys"), with("d-1"), `{"service":"checkout","status":"failed"}`, http.StatusAccepted},
		{"same thread", url("deploys"), with("d-2"), `{"service":"checkout","status":"failed again"}`, http.StatusAccepted},
		{"another thread", url("deploys"), with("d-3"), `{"service":"billing"}`, http.StatusAccepted},
		{"no thread", url("deploys"), with("d-4"), `{"status":"unknown"}`, http.StatusAccepted},
		{"no delivery key", url("deploys"), plain, `{"service":"checkout"}`, http.StatusBadRequest},
		{"a key too long", url("deploys"), with(strings.Repeat("x", webhook.MaxKey+1)), `{}`, http.StatusBadRequest},
		{"wrong token", "/" + webhook.FixtureSource + "/deploys/" + strings.Repeat("0", 32), with("d-5"), `{}`, http.StatusNotFound},
		{"unknown hook", "/" + webhook.FixtureSource + "/nope/" + webhook.Token(webhook.FixtureValue, webhook.FixtureSource, "nope"), with("d-6"), `{}`, http.StatusNotFound},
		{"the mount itself", "/" + webhook.FixtureSource, with("d-7"), `{}`, http.StatusNotFound},
		{"too large", url("forms"), plain, strings.Repeat("x", webhook.MaxBody+1), http.StatusRequestEntityTooLarge},
		{"a form", url("forms"), http.Header{"Content-Type": {"text/plain"}}, "hello", http.StatusAccepted},
		{"the same form again", url("forms"), http.Header{"Content-Type": {"text/plain"}}, "hello", http.StatusAccepted},
	} {
		if got := post(t, h, c.path, c.header, c.body); got != c.want {
			t.Fatalf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	events := h.Events()
	// The retry is the first delivery again; a form has no delivery key, so
	// each POST is its own delivery.
	if len(events) != 6 {
		t.Fatalf("%d events", len(events))
	}
	threads := map[string]int{}
	for _, a := range events {
		threads[a.Event.CorrelationKey.ExternalID]++
	}
	if len(threads) != 5 || threads[webhook.Thread("deploys", "checkout", "")] != 2 {
		t.Fatalf("threads: %v", threads)
	}
	ps := payloads(t, h)
	if ps[0].Delivery != "d-1" || ps[0].Correlation != "checkout" || string(ps[0].Body) != `{"service":"checkout","status":"failed"}` || ps[5].Text != "hello" || ps[5].Body != nil {
		t.Fatalf("payloads: %+v", ps)
	}
}

func TestBodyKeys(t *testing.T) {
	body := []byte(`{"ticket":{"id":42,"tags":["a","b"],"a/b":"slash","t~":"tilde","open":true,"none":null}}`)
	for ptr, want := range map[string]string{
		"/ticket/id": "42", "/ticket/tags/1": "b", "/ticket/a~1b": "slash", "/ticket/t~0": "tilde", "/ticket/open": "true",
		"/ticket/none": "", "/ticket": "", "/ticket/tags/2": "", "/ticket/tags/01": "", "/missing": "",
	} {
		k, err := webhook.ParseKey("body:" + ptr)
		if err != nil {
			t.Fatal(err)
		}
		got, err := k.Value(nil, body)
		if got != want || (want != "") != (err == nil) {
			t.Fatalf("%s: %q %v", ptr, got, err)
		}
	}
	for _, bad := range []string{"id", "header:", "header:a b", "body:id", "body:/a~2"} {
		if _, err := webhook.ParseKey(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestSettings(t *testing.T) {
	for name, cfg := range map[string]string{
		"no secret":      `{"scope":"ops","hooks":[{"name":"a"}]}`,
		"no hooks":       `{"scope":"ops","webhook_secret":"hook","hooks":[]}`,
		"bad name":       `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"A"}]}`,
		"twice":          `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a"},{"name":"a"}]}`,
		"bad key":        `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a","delivery_key":"query:id"}]}`,
		"unknown field":  `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a","per_delivery":true}]}`,
		"secret unnamed": `{"scope":"ops","webhook_secret":"other","hooks":[{"name":"a"}]}`,
	} {
		if _, err := webhook.ParseSettings(json.RawMessage(cfg), []string{"hook"}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestRouteOpensWorkPerThread(t *testing.T) {
	h := harness(t)
	if got := post(t, h, url("deploys"), http.Header{"X-Delivery-Id": {"d-1"}}, `{"service":"checkout"}`); got != http.StatusAccepted {
		t.Fatal(got)
	}
	ev := h.Events()[0]
	s := domain.Scope{ID: scope, Name: scope}
	r := domain.Route{Name: "deliveries", Handler: webhook.Name, Binding: "triage"}
	d, err := h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: ev, Scope: s, Route: r})
	want := domain.ID(webhook.FixtureSource + "/" + webhook.Thread("deploys", "checkout", ""))
	if err != nil || !d.Match || d.Kind != webhook.WorkKind || d.CreateID != want || d.Display.Title != "A delivery to deploys: checkout" {
		t.Fatalf("decision %+v %v", d, err)
	}
	if err := extension.ValidateDisplay(d.Display); err != nil {
		t.Fatal(err)
	}
	other := domain.Work{ID: "x", ScopeID: scope, Kind: "manual.work"}
	if d, _ := h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: ev, Scope: s, Route: r, Work: &other}); d.Match {
		t.Fatalf("joined other Work: %+v", d)
	}
	// Another scope's route takes nothing from this scope's webhooks.
	if d, _ := h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: ev, Scope: domain.Scope{ID: "elsewhere"}, Route: r}); d.Match {
		t.Fatalf("another scope matched: %+v", d)
	}
}

func TestRenderAndChecks(t *testing.T) {
	h := harness(t)
	big := `{"service":"checkout","log":"` + strings.Repeat("x", 9000) + `"}`
	if got := post(t, h, url("deploys"), http.Header{"X-Delivery-Id": {"d-1"}, "Content-Type": {"application/json"}}, big); got != http.StatusAccepted {
		t.Fatal(got)
	}
	text, ok := webhook.RenderEvent(h.Events()[0].Event)
	id := string(h.Events()[0].Event.ID)
	if !ok || !strings.HasPrefix(text, "### A delivery to the deploys webhook\nEvent: "+id+"\nDelivery: d-1\nCorrelation: checkout\nContent type: application/json\n\n{") ||
		!strings.Contains(text, `Call webhook.delivery.read with {"event": "`+id+`"} for the whole body.`) || len(text) > 9000 {
		t.Fatalf("render: %q", text)
	}
	plain, err := h.Check(context.Background(), webhook.Name)
	if err != nil || !strings.Contains(plain.Detail, "hook deploys: POST https://hooks.example.test/webhook/ops/inbox/deploys/<token>") {
		t.Fatalf("check: %+v %v", plain, err)
	}
	deep, err := h.Check(context.Background(), webhook.Name+"-urls")
	if err != nil || !strings.Contains(deep.Detail, url("forms")) {
		t.Fatalf("deep check: %+v %v", deep, err)
	}
}

func TestReadReturnsTheWholeBodyOfTheWorksOwnDelivery(t *testing.T) {
	big := `{"service":"checkout","log":"` + strings.Repeat("x", 9000) + `"}`
	h0 := harness(t)
	if got := post(t, h0, url("deploys"), http.Header{"X-Delivery-Id": {"d-1"}}, big); got != http.StatusAccepted {
		t.Fatal(got)
	}
	ev := h0.Events()[0].Event
	key := *ev.CorrelationKey
	own := domain.Work{ID: "mine", ScopeID: scope, Kind: webhook.WorkKind, State: domain.WorkRunning, CorrelationKeys: []domain.CorrelationKey{key}}
	other := domain.Work{ID: "theirs", ScopeID: scope, Kind: webhook.WorkKind, State: domain.WorkRunning}
	// The runtime lists a Work's routed input among its Events; the SDK host
	// lists the Events that name it.
	ev.WorkID = own.ID
	h := sdktest.New(t, webhook.Build(webhook.Options{}), sdktest.WithWorks(own, other), sdktest.WithEvents(scope, ev))
	res, err := h.OperationAs(context.Background(), extension.Authority{ScopeID: scope, WorkID: "mine"}, webhook.OpRead, domain.Payload{Data: json.RawMessage(`{"event":"` + string(ev.ID) + `"}`)})
	var out webhook.ReadOutput
	if err != nil || json.Unmarshal(res.Output.Data, &out) != nil || string(out.Body) != big || out.Event != string(ev.ID) || out.Hook != "deploys" {
		t.Fatalf("read: %s %v", res.Output.Data, err)
	}
	if _, err := h.OperationAs(context.Background(), extension.Authority{ScopeID: scope, WorkID: "mine"}, webhook.OpRead, domain.Payload{Data: json.RawMessage(`{"event":"nope"}`)}); err == nil {
		t.Fatal("read an unknown Event")
	}
	// Another Work can't read it, and a Work the scope doesn't hold is refused.
	if _, err := h.OperationAs(context.Background(), extension.Authority{ScopeID: scope, WorkID: "theirs"}, webhook.OpRead, domain.Payload{Data: json.RawMessage(`{"event":"` + string(ev.ID) + `"}`)}); err == nil {
		t.Fatal("another Work read the delivery")
	}
	if _, err := h.OperationAs(context.Background(), extension.Authority{ScopeID: scope, WorkID: "nope"}, webhook.OpRead, domain.Payload{Data: json.RawMessage(`{"event":"` + string(ev.ID) + `"}`)}); err == nil {
		t.Fatal("unknown Work read a delivery")
	}
}
