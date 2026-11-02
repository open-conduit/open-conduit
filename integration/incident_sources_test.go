package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/packages/pagerduty"
	"github.com/open-conduit/open-conduit/packages/pagerduty/pagerdutytest"
	"github.com/open-conduit/open-conduit/packages/sentry"
	"github.com/open-conduit/open-conduit/packages/sentry/sentrytest"
	"github.com/open-conduit/open-conduit/storage"
)

// The incident source packages' ingress through the real runtime ingress
// and router over SQLite, as the daemon runs a package's sources: every
// Event they push or reconcile must pass the runtime's envelope and
// canonical-identity checks, which the in-memory SDK host doesn't apply.

type incidentEnv struct {
	t       *testing.T
	clock   *ghClock
	store   *sqlite.Store
	ingress *runtime.Ingress
	router  *runtime.Router
	source  string
}

func newIncidentEnv(t *testing.T, scope, entry, extType string, cfg json.RawMessage, handler extension.RouteHandler, ns, source string, adapter extension.Ingress) *incidentEnv {
	t.Helper()
	e := &incidentEnv{t: t, clock: &ghClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}, source: source}
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e.store = store
	if e.ingress, err = runtime.NewIngress(store, e.clock.Now, runtime.IngressOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.router, err = runtime.NewRouter(store, e.clock.Now, runtime.RouterOptions{Generation: "1", Handlers: map[string]extension.RouteHandler{extType: handler},
		Bindings: map[string]runtime.RouteBinding{"fixture": {}}}); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": entry, "type": extType, "connection": extType, "config": cfg, "secret_names": []string{}},
	}})
	e.read(func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: domain.ID(scope), Name: scope, Bindings: []domain.Binding{{Name: "triage", Executor: "fixture"}},
			Routes: []domain.Route{{Name: "alerts", Handler: extType, Binding: "triage", ActivePolicy: "queue"}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	})
	if err := e.ingress.Register(context.Background(), source, extension.Source{Namespace: ns, ScopeID: domain.ID(scope)}, adapter, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *incidentEnv) read(fn func(storage.Tx) error) {
	e.t.Helper()
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *incidentEnv) push(ctx context.Context, d extension.Delivery) error {
	_, err := e.ingress.Push(ctx, e.source, d)
	return err
}

func (e *incidentEnv) deliver(h http.Handler, header http.Header, body []byte) int {
	req := httptest.NewRequest(http.MethodPost, "/"+e.source, strings.NewReader(string(body)))
	req.Header = header
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func (e *incidentEnv) reconcile() runtime.ReconcileResult {
	e.t.Helper()
	var last runtime.ReconcileResult
	for range 50 {
		res, err := e.ingress.Reconcile(context.Background(), e.source)
		if err != nil {
			e.t.Fatalf("reconcile: %v (%s)", err, res.ErrorCode)
		}
		last.Accepted = append(last.Accepted, res.Accepted...)
		if res.Complete {
			return last
		}
	}
	e.t.Fatal("reconcile did not complete")
	return last
}

func (e *incidentEnv) route() {
	e.t.Helper()
	for {
		page, err := e.ingress.Pending(context.Background(), 0, 200)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(page.Items) == 0 {
			return
		}
		for _, p := range page.Items {
			if p.Diagnostic != "" {
				e.t.Fatalf("pending diagnostic %s for %s", p.Diagnostic, p.Event.Event.ID)
			}
			if err := e.router.Route(context.Background(), p.Event.Event.ID); err != nil {
				e.t.Fatalf("route %s: %v", p.Event.Event.Type, err)
			}
		}
	}
}

func (e *incidentEnv) events(prefix string) []domain.AcceptedEvent {
	var out []domain.AcceptedEvent
	e.read(func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if strings.HasPrefix(a.Event.Type, prefix) {
				out = append(out, a)
			}
		}
		return err
	})
	return out
}

func (e *incidentEnv) works() map[string]domain.Work {
	out := map[string]domain.Work{}
	e.read(func(tx storage.Tx) error {
		all, err := tx.ListWorks()
		for _, w := range all {
			out[string(w.ID)] = w
		}
		return err
	})
	return out
}

func TestSentryIngressThroughRuntime(t *testing.T) {
	fake := sentrytest.New(sentry.FixtureOrg)
	t.Cleanup(fake.Close)
	sentry.SeedFixture(fake)
	settings, err := sentry.ParseSettings(sentry.FixtureEntry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var e *incidentEnv
	adapter := &sentry.Ingress{Settings: settings, Client: sentry.NewClient(fake.URL, sentry.FixtureOrg, func(context.Context) (string, error) { return sentry.FixtureToken, nil }, nil),
		Now: func() time.Time { return e.clock.Now() }}
	e = newIncidentEnv(t, sentry.FixtureScope, "errors", sentry.Name, sentry.FixtureEntry(), sentry.Route{}, settings.Namespace, "sentry/incidents/errors", adapter)
	hook := sentry.WebhookHandler(func(context.Context) (string, error) { return "client-secret", nil }, e.push, e.clock.Now)

	body := fake.IssueWebhook("created", "1001")
	for _, id := range []string{"r1", "r2"} { // Sentry redelivering
		if code := e.deliver(hook, sentrytest.Headers("client-secret", "issue", id, body), body); code != http.StatusOK {
			t.Fatalf("issue delivery: %d", code)
		}
	}
	if got := len(e.events("sentry.issue.")); got != 1 {
		t.Fatalf("issue events after push: %d", got)
	}
	// Reconcile agrees with the push and adds the other watched issues.
	e.reconcile()
	if got := len(e.events("sentry.issue.")); got != 3 {
		t.Fatalf("issue events after reconcile: %d", got)
	}
	// Sentry changing the culprit and level of an open issue is a new
	// version, not a conflict that stalls the source.
	i, _ := fake.Issue("1001")
	i.Culprit, i.Level = "app/session.ts in load", "fatal"
	fake.Put(i)
	e.clock.Add(time.Minute)
	if res := e.reconcile(); len(res.Accepted) != 1 {
		t.Fatalf("changed issue accepted %d", len(res.Accepted))
	}
	changed := fake.IssueWebhook("unresolved", "1001")
	if code := e.deliver(hook, sentrytest.Headers("client-secret", "issue", "r3", changed), changed); code != http.StatusOK {
		t.Fatalf("changed issue delivery: %d", code)
	}
	alert := fake.AlertWebhook("Page on checkout errors", "1001", "ev-1")
	if code := e.deliver(hook, sentrytest.Headers("client-secret", "event_alert", "r4", alert), alert); code != http.StatusOK {
		t.Fatalf("alert delivery: %d", code)
	}
	e.route()
	works := e.works()
	if len(works) != 3 || works["sentry/acme/1001"].Kind != sentry.WorkKind || works["sentry/acme/1003"].ID == "" {
		t.Fatalf("works: %+v", works)
	}
	// The alert went to the issue's Work, not a Work of its own.
	if a := e.events(sentry.TypeAlert); len(a) != 1 || a[0].RoutedWorkID != "sentry/acme/1001" {
		t.Fatalf("alert routed to %+v", a)
	}
}

func TestPagerDutyIngressThroughRuntime(t *testing.T) {
	fake := pagerdutytest.New()
	t.Cleanup(fake.Close)
	pagerduty.SeedFixture(fake)
	settings, err := pagerduty.ParseSettings(pagerduty.FixtureEntry(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var e *incidentEnv
	adapter := &pagerduty.Ingress{Settings: settings, Client: pagerduty.NewClient(fake.URL, "", func(context.Context) (string, error) { return pagerduty.FixtureToken, nil }, nil),
		Now: func() time.Time { return e.clock.Now() }}
	e = newIncidentEnv(t, pagerduty.FixtureScope, "oncall", pagerduty.Name, pagerduty.FixtureEntry(), pagerduty.Route{}, settings.Namespace, "pagerduty/incidents/oncall", adapter)
	hook := pagerduty.WebhookHandler(func(context.Context) (string, error) { return "signing-secret", nil }, e.push, e.clock.Now)
	deliver := func(id, typ string) {
		t.Helper()
		body := fake.Webhook(id, typ, "Q1")
		if code := e.deliver(hook, pagerdutytest.Headers("signing-secret", body), body); code != http.StatusOK {
			t.Fatalf("%s delivery: %d", typ, code)
		}
	}
	deliver("E1", "incident.triggered")
	deliver("E1", "incident.triggered")
	e.reconcile()
	// A responder retitling the incident is a new version, not a conflict.
	q1, _ := fake.Incident("Q1")
	q1.Title, q1.Urgency = "Checkout down", "high"
	fake.Put(q1)
	e.clock.Add(time.Minute)
	if res := e.reconcile(); len(res.Accepted) != 1 {
		t.Fatalf("retitled incident accepted %d", len(res.Accepted))
	}
	// Acknowledged, then triggered again: two more Events.
	q1.Status, q1.LastStatusChangeAt = "acknowledged", "2026-11-01T12:10:00Z"
	fake.Put(q1)
	deliver("E2", "incident.acknowledged")
	q1.Status, q1.LastStatusChangeAt = "triggered", "2026-11-01T12:40:00Z"
	fake.Put(q1)
	deliver("E3", "incident.triggered")
	e.route()
	works := e.works()
	if len(works) != 2 || works["pagerduty/Q1"].Kind != pagerduty.WorkKind || works["pagerduty/Q4"].ID == "" {
		t.Fatalf("works: %+v", works)
	}
	if got := len(e.events("pagerduty.incident.")); got != 6 {
		t.Fatalf("incident events: %d", got)
	}
	// Resolved while the webhook was missed: reconcile reports it absent,
	// and that reaches the incident's Work.
	q1.Status, q1.LastStatusChangeAt = "resolved", "2026-11-01T13:00:00Z"
	fake.Put(q1)
	e.clock.Add(time.Minute)
	e.reconcile()
	e.route()
	absent := e.events("core.ingress.absent")
	if len(absent) != 1 || absent[0].RoutedWorkID != "pagerduty/Q1" {
		t.Fatalf("absence: %+v", absent)
	}
}
