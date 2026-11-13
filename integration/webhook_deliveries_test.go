package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/webhook"
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
		req := httptest.NewRequest(http.MethodPost, "/"+webhook.FixtureSource+"/"+name+"/"+webhook.Token("s", webhook.FixtureSource, name), strings.NewReader(body))
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
	checkout := webhook.FixtureSource + "/" + webhook.Thread("deploys", "checkout", "")
	if w := works[checkout]; w.Kind != webhook.WorkKind || w.Display == nil || w.Display.Title != "A delivery to deploys: checkout" {
		t.Fatalf("checkout Work: %+v", w)
	}
	if deliveries[0].RoutedWorkID != deliveries[1].RoutedWorkID || string(deliveries[0].RoutedWorkID) != checkout || deliveries[2].RoutedWorkID == deliveries[0].RoutedWorkID {
		t.Fatalf("routed: %s %s %s", deliveries[0].RoutedWorkID, deliveries[1].RoutedWorkID, deliveries[2].RoutedWorkID)
	}
}
