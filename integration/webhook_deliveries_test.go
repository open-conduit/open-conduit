package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/webhook"
	"github.com/open-conduit/open-conduit/storage"
)

// TestWebhookDeliveriesThroughRuntime runs the webhook package's ingress
// and route through the real runtime ingress and router over SQLite: a
// sender's retry is the first delivery again, a delivery with the same
// correlation value is the first one's Work's input, and each other
// delivery opens Work of its own.
func TestWebhookDeliveriesThroughRuntime(t *testing.T) {
	settings, err := webhook.ParseSettings(webhook.FixtureConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var e *incidentEnv
	adapter := &webhook.Ingress{Namespace: webhook.FixtureSource, Settings: settings, Now: func() time.Time { return e.clock.Now() }}
	e = newIncidentEnv(t, webhook.FixtureScope, webhook.FixtureEntry, webhook.Name, webhook.FixtureConfig(), webhook.Route{}, webhook.FixtureSource, webhook.FixtureSource, adapter)
	hook := http.StripPrefix("/"+webhook.FixtureSource, webhook.WebhookHandler(webhook.FixtureSource, settings, func(context.Context) (string, error) { return "s", nil }, e.push, e.clock.Now))
	post := func(name string, header http.Header, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/"+webhook.FixtureSource+"/"+name+"/"+webhook.Token("s", webhook.FixtureSource, name, 0), strings.NewReader(body))
		req.Header = header
		w := httptest.NewRecorder()
		hook.ServeHTTP(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s %s: %d", name, body, w.Code)
		}
	}
	id := func(v string) http.Header {
		return http.Header{"Content-Type": {"application/json"}, "X-Delivery-Id": {v}}
	}
	post("deploys", id("d-1"), `{"service":"checkout","status":"failed"}`)
	// A retry, even one whose body differs, is the first delivery again.
	post("deploys", id("d-1"), `{"service":"billing","status":"failed"}`)
	post("deploys", id("d-2"), `{"service":"checkout","status":"failed again"}`)
	post("deploys", id("d-3"), `{"service":"billing"}`)
	post("forms", nil, `name=a`)
	post("forms", nil, `name=a`)
	e.route()
	deliveries := e.events(webhook.TypeDelivery)
	if len(deliveries) != 5 {
		t.Fatalf("%d deliveries", len(deliveries))
	}
	works := e.works()
	if len(works) != 4 {
		t.Fatalf("works: %+v", works)
	}
	checkout := webhook.FixtureSource + "/" + webhook.Thread("deploys", "", "checkout", "")
	if w := works[checkout]; w.Kind != webhook.WorkKind || w.Display == nil || w.Display.Title != "A delivery to deploys: checkout" {
		t.Fatalf("checkout Work: %+v", w)
	}
	if deliveries[0].RoutedWorkID != deliveries[1].RoutedWorkID || string(deliveries[0].RoutedWorkID) != checkout || deliveries[2].RoutedWorkID == deliveries[0].RoutedWorkID {
		t.Fatalf("routed: %s %s %s", deliveries[0].RoutedWorkID, deliveries[1].RoutedWorkID, deliveries[2].RoutedWorkID)
	}
}

// TestWebhookFanOutThroughRuntime: a hook that fans out to triage and audit
// gives one POST an Event for each, and the real router sends each to the
// route that takes its target (first match, no broadcast), so each binding
// has Work of its own. A retry is every target's first delivery again, and
// a later delivery of the ticket is each target's own Work's input.
func TestWebhookFanOutThroughRuntime(t *testing.T) {
	cfg := json.RawMessage(`{"scope":"` + webhook.FixtureScope + `","webhook_secret":"hook","hooks":[` +
		`{"name":"tickets","delivery_key":"header:X-Delivery-Id","correlation_key":"body:/ticket","fan_out":["triage","audit"]}]}`)
	settings, err := webhook.ParseSettings(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var e *incidentEnv
	adapter := &webhook.Ingress{Namespace: webhook.FixtureSource, Settings: settings, Now: func() time.Time { return e.clock.Now() }}
	e = newIncidentEnv(t, webhook.FixtureScope, webhook.FixtureEntry, webhook.Name, cfg, webhook.Route{}, webhook.FixtureSource, webhook.FixtureSource, adapter)
	e.read(func(tx storage.Tx) error {
		s, err := tx.Scope(webhook.FixtureScope)
		if err != nil {
			return err
		}
		s.Bindings = []domain.Binding{{Name: "triage", Executor: "fixture"}, {Name: "audit", Executor: "fixture"}}
		s.Routes = []domain.Route{
			{Name: "triage", Handler: webhook.Name, Binding: "triage", ActivePolicy: "queue", Config: domain.Payload{Data: json.RawMessage(`{"targets":["triage"]}`)}},
			{Name: "audit", Handler: webhook.Name, Binding: "audit", ActivePolicy: "queue", Config: domain.Payload{Data: json.RawMessage(`{"targets":["audit"]}`)}},
		}
		return tx.PutScope(s, s.Revision)
	})
	hook := http.StripPrefix("/"+webhook.FixtureSource, webhook.WebhookHandler(webhook.FixtureSource, settings, func(context.Context) (string, error) { return "s", nil }, e.push, e.clock.Now))
	send := func(id, body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/"+webhook.FixtureSource+"/tickets/"+webhook.Token("s", webhook.FixtureSource, "tickets", 0), strings.NewReader(body))
		req.Header = http.Header{"Content-Type": {"application/json"}, "X-Delivery-Id": {id}}
		w := httptest.NewRecorder()
		hook.ServeHTTP(w, req)
		return w.Code
	}
	post := func(id, body string) {
		t.Helper()
		if code := send(id, body); code != http.StatusAccepted {
			t.Fatalf("%s: %d", id, code)
		}
	}
	post("d-1", `{"ticket":"T-1"}`)
	post("d-1", `{"ticket":"T-1"}`)
	post("d-2", `{"ticket":"T-1","note":"more"}`)
	// Each < encodes as six bytes in each target's Event. Just under the
	// bound, the real ingress accepts the delivery in one page; just over,
	// the sender is told it is too large rather than to retry.
	markup := func(kib int) string { return `{"ticket":"T-2","html":"` + strings.Repeat("<", kib<<10) + `"}` }
	if code := send("d-big", markup(590)); code != http.StatusAccepted {
		t.Fatalf("a delivery under the bound: %d", code)
	}
	if code := send("d-bigger", markup(610)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a delivery over the bound: %d", code)
	}
	e.route()
	deliveries := e.events(webhook.TypeDelivery)
	if len(deliveries) != 6 {
		t.Fatalf("%d deliveries", len(deliveries))
	}
	if works := e.works(); len(works) != 4 {
		t.Fatalf("works: %+v", works)
	}
	for _, target := range []string{"triage", "audit"} {
		want := domain.ID(webhook.FixtureSource + "/" + webhook.Thread("tickets", target, "T-1", ""))
		n := 0
		for _, a := range deliveries {
			rec, err := e.router.Decision(context.Background(), a.Event.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a.RoutedWorkID != want {
				continue
			}
			n++
			if rec.Route != target || rec.Binding != target {
				t.Fatalf("%s delivery routed by %s to %s", target, rec.Route, rec.Binding)
			}
		}
		if n != 2 {
			t.Fatalf("%s: %d deliveries on %s", target, n, want)
		}
	}
}
