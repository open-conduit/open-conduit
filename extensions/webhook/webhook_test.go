package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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
	return handle(t, h, path, header, body).Status
}

func handle(t *testing.T, h *sdktest.Host, path string, header http.Header, body string) protocol.WebhookHandleResult {
	t.Helper()
	var out protocol.WebhookHandleResult
	if err := h.Call(context.Background(), "webhook.handle", protocol.WebhookHandleParams{Path: path, Method: http.MethodPost, Header: header, Body: []byte(body), ReceivedAt: sdktest.Start}, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// receipt posts a delivery that must be accepted and returns its 202's body.
func receipt(t *testing.T, h *sdktest.Host, path string, header http.Header, body string) webhook.Receipt {
	t.Helper()
	out := handle(t, h, path, header, body)
	var r webhook.Receipt
	if out.Status != http.StatusAccepted || http.Header(out.Header).Get("Content-Type") != "application/json" || json.Unmarshal(out.Body, &r) != nil {
		t.Fatalf("%s: %d %v %q", path, out.Status, out.Header, out.Body)
	}
	return r
}

func url(hook string) string {
	return "/" + webhook.FixtureSource + "/" + hook + "/" + webhook.Token(webhook.FixtureValue, webhook.FixtureSource, hook, 0)
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
		{"unknown hook", "/" + webhook.FixtureSource + "/nope/" + webhook.Token(webhook.FixtureValue, webhook.FixtureSource, "nope", 0), with("d-6"), `{}`, http.StatusNotFound},
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
	if len(threads) != 5 || threads[webhook.Thread("deploys", "", "checkout", "")] != 2 {
		t.Fatalf("threads: %v", threads)
	}
	ps := payloads(t, h)
	if ps[0].Delivery != "d-1" || ps[0].Correlation != "checkout" || string(ps[0].Body) != `{"service":"checkout","status":"failed"}` || ps[5].Text != "hello" || ps[5].Body != nil {
		t.Fatalf("payloads: %+v", ps)
	}
}

// Bumping one hook's version revokes that hook's URL alone: the old URL
// is refused, the new one (which doctor --check-secrets prints) is served,
// and the entry's other hook keeps its URL.
func TestVersionRevokesOneHooksURL(t *testing.T) {
	// Version 0 signs only the namespace and hook, so URLs issued before
	// versions existed still work.
	mac := hmac.New(sha256.New, []byte(webhook.FixtureValue))
	mac.Write([]byte(webhook.FixtureSource + "\x00deploys"))
	if webhook.Token(webhook.FixtureValue, webhook.FixtureSource, "deploys", 0) != hex.EncodeToString(mac.Sum(nil))[:32] {
		t.Fatal("version 0 changed the token")
	}
	h := harness(t)
	with := func(id string) http.Header { return http.Header{"X-Delivery-Id": {id}} }
	old := url("deploys")
	if got := post(t, h, old, with("d-1"), `{}`); got != http.StatusAccepted {
		t.Fatalf("before the bump: %d", got)
	}
	cfg := strings.Replace(string(webhook.FixtureConfig()), `"name":"deploys",`, `"name":"deploys","version":1,`, 1)
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{PublicURL: "https://hooks.example.test", Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: webhook.Name,
		Name: webhook.FixtureEntry, Config: json.RawMessage(cfg), Secrets: []string{webhook.FixtureSecret}}}}); err != nil {
		t.Fatal(err)
	}
	bumped := "/" + webhook.FixtureSource + "/deploys/" + webhook.Token(webhook.FixtureValue, webhook.FixtureSource, "deploys", 1)
	if bumped == old {
		t.Fatal("the bump kept the token")
	}
	for _, c := range []struct {
		name, path string
		want       int
	}{
		{"the old URL", old, http.StatusNotFound},
		{"the new URL", bumped, http.StatusAccepted},
		{"the other hook", url("forms"), http.StatusAccepted},
	} {
		if got := post(t, h, c.path, with("d-2"), `{}`); got != c.want {
			t.Fatalf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	deep, err := h.Check(context.Background(), webhook.Name+"-urls")
	if err != nil || !strings.Contains(deep.Detail, bumped) || strings.Contains(deep.Detail, old) || !strings.Contains(deep.Detail, url("forms")) {
		t.Fatalf("doctor: %+v %v", deep, err)
	}
}

// event is the ID of a receipt's only Event.
func event(t *testing.T, r webhook.Receipt) domain.ID {
	t.Helper()
	if len(r.Events) != 1 || r.Events[0].Target != "" || r.Events[0].Duplicate != r.Duplicate {
		t.Fatalf("receipt %+v", r)
	}
	return r.Events[0].ID
}

// The 202 names the delivery's Event, and a retry of the same delivery key
// names the first delivery's Event as a duplicate.
func TestReceiptNamesTheEvent(t *testing.T) {
	h := harness(t)
	with := func(id string) http.Header {
		return http.Header{"Content-Type": {"application/json"}, "X-Delivery-Id": {id}}
	}
	first := receipt(t, h, url("deploys"), with("d-1"), `{"service":"checkout"}`)
	retry := receipt(t, h, url("deploys"), with("d-1"), `{"service":"checkout"}`)
	other := receipt(t, h, url("deploys"), with("d-2"), `{"service":"checkout"}`)
	events := h.Events()
	if len(events) != 2 || event(t, first) != events[0].Event.ID || event(t, other) != events[1].Event.ID {
		t.Fatalf("receipts %+v %+v, events %d", first, other, len(events))
	}
	if first.Duplicate || !retry.Duplicate || event(t, retry) != events[0].Event.ID || other.Duplicate {
		t.Fatalf("receipts: %+v %+v %+v", first, retry, other)
	}
}

// A hook that fans out answers with each target's Event, in target order,
// and a retry is a duplicate for every target.
func TestReceiptNamesEachTargetsEvent(t *testing.T) {
	h := fanOut(t)
	with := http.Header{"X-Delivery-Id": {"d-1"}}
	first := receipt(t, h, url("tickets"), with, `{"ticket":"T-1"}`)
	retry := receipt(t, h, url("tickets"), with, `{"ticket":"T-1"}`)
	events := h.Events()
	want := webhook.Receipt{Events: []webhook.ReceiptEvent{{ID: events[0].Event.ID, Target: "triage"}, {ID: events[1].Event.ID, Target: "audit"}}}
	if len(events) != 2 || !reflect.DeepEqual(first, want) {
		t.Fatalf("receipt %+v, want %+v", first, want)
	}
	want.Duplicate, want.Events[0].Duplicate, want.Events[1].Duplicate = true, true, true
	if !reflect.DeepEqual(retry, want) {
		t.Fatalf("retry %+v, want %+v", retry, want)
	}
}

func TestReceiptOf(t *testing.T) {
	delivery := func(id domain.ID, target string) domain.AcceptedEvent {
		data, _ := json.Marshal(webhook.Payload{Hook: "h", Target: target})
		return domain.AcceptedEvent{Event: domain.Event{ID: id, Type: webhook.TypeDelivery, Data: domain.Payload{Schema: webhook.SchemaDelivery, Data: data}}}
	}
	two := []domain.AcceptedEvent{delivery("a", "x"), delivery("b", "y")}
	// Only a delivery every one of whose Events was accepted before is a
	// duplicate.
	r, err := webhook.ReceiptOf(two, []bool{false, true}, nil)
	if err != nil || r.Duplicate || !r.Events[0].Duplicate || r.Events[1].Duplicate || r.Events[1].Target != "y" {
		t.Fatalf("%+v %v", r, err)
	}
	// An answer that names nothing, or whose flags don't match its Events,
	// isn't a 202.
	for name, c := range map[string]struct {
		accepted []domain.AcceptedEvent
		added    []bool
	}{
		"no events":     {nil, nil},
		"missing flags": {two, []bool{true}},
		"another type":  {[]domain.AcceptedEvent{{Event: domain.Event{ID: "c", Type: "other"}}}, []bool{true}},
	} {
		if _, err := webhook.ReceiptOf(c.accepted, c.added, nil); err == nil {
			t.Fatalf("%s gave a receipt", name)
		}
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
		"negative":       `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a","version":-1}]}`,
		"bad target":     `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a","fan_out":["Triage"]}]}`,
		"target twice":   `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a","fan_out":["x","x"]}]}`,
		"many targets":   `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a","fan_out":["a","b","c","d","e"]}]}`,
	} {
		if _, err := webhook.ParseSettings(json.RawMessage(cfg), []string{"hook"}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := webhook.ParseRoute(json.RawMessage(`{"targets":["Audit"]}`)); err == nil {
		t.Fatal("a bad route target parsed")
	}
}

// fanOut is a harness whose tickets hook fans each delivery out to triage
// and audit.
func fanOut(t *testing.T) *sdktest.Host {
	t.Helper()
	h := sdktest.New(t, webhook.Build(webhook.Options{}), sdktest.WithSecrets(scope, map[string]string{webhook.FixtureSecret: webhook.FixtureValue}))
	cfg := json.RawMessage(`{"scope":"` + scope + `","webhook_secret":"` + webhook.FixtureSecret + `","hooks":[` +
		`{"name":"tickets","delivery_key":"header:X-Delivery-Id","correlation_key":"body:/ticket","fan_out":["triage","audit"]}]}`)
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: webhook.Name,
		Name: webhook.FixtureEntry, Config: cfg, Secrets: []string{webhook.FixtureSecret}}}}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestFanOutAcceptsAnEventForEachTarget(t *testing.T) {
	h := fanOut(t)
	with := func(id string) http.Header { return http.Header{"X-Delivery-Id": {id}} }
	for _, c := range []struct{ id, body string }{
		{"d-1", `{"ticket":"T-1"}`},
		{"d-1", `{"ticket":"T-1"}`}, // a retry: every target's Event again
		{"d-2", `{"ticket":"T-1"}`},
	} {
		if got := post(t, h, url("tickets"), with(c.id), c.body); got != http.StatusAccepted {
			t.Fatalf("%s: %d", c.id, got)
		}
	}
	events := h.Events()
	if len(events) != 4 {
		t.Fatalf("%d events", len(events))
	}
	ids, deliveries := map[domain.ID]bool{}, map[string]bool{}
	threads := map[string]int{}
	for _, a := range events {
		ids[a.Event.ID], deliveries[a.Event.DeliveryID] = true, true
		threads[a.Event.CorrelationKey.ExternalID]++
	}
	if len(ids) != 4 || len(deliveries) != 4 || !strings.HasPrefix(events[0].Event.DeliveryID, "tickets/triage/k-") || !strings.HasPrefix(events[1].Event.DeliveryID, "tickets/audit/k-") {
		t.Fatalf("ids %v deliveries %v", ids, deliveries)
	}
	// Each target threads on its own: the same ticket is one thread per
	// target.
	if len(threads) != 2 || threads[webhook.Thread("tickets", "triage", "T-1", "")] != 2 || threads[webhook.Thread("tickets", "audit", "T-1", "")] != 2 {
		t.Fatalf("threads %v", threads)
	}
	ps := payloads(t, h)
	if ps[0].Target != "triage" || ps[1].Target != "audit" || string(ps[1].Body) != `{"ticket":"T-1"}` {
		t.Fatalf("payloads %+v", ps)
	}
	text, ok := webhook.RenderEvent(events[1].Event)
	if !ok || !strings.Contains(text, "\nTarget: audit\nDelivery: d-1\n") {
		t.Fatalf("render %q", text)
	}
}

func TestFanOutRefusesABodyItsEventsCannotCarry(t *testing.T) {
	h := sdktest.New(t, webhook.Build(webhook.Options{}), sdktest.WithSecrets(scope, map[string]string{webhook.FixtureSecret: webhook.FixtureValue}))
	cfg := json.RawMessage(`{"scope":"` + scope + `","webhook_secret":"` + webhook.FixtureSecret + `","hooks":[` +
		`{"name":"wide","fan_out":["a","b","c","d"]},{"name":"narrow"}]}`)
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: webhook.Name,
		Name: webhook.FixtureEntry, Config: cfg, Secrets: []string{webhook.FixtureSecret}}}}); err != nil {
		t.Fatal(err)
	}
	// JSON escapes each < as six bytes, in text and in a JSON body alike.
	markup := strings.Repeat("<", 400<<10)
	text := http.Header{"Content-Type": {"text/html"}}
	if got := post(t, h, url("wide"), text, markup); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("escape-heavy text to 4 targets: %d", got)
	}
	if got := post(t, h, url("wide"), nil, `{"html":"`+markup+`"}`); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("escape-heavy JSON to 4 targets: %d", got)
	}
	// A plain 1 MiB body fits 4 targets, and a hook without fan-out takes
	// the most escape-heavy body it can be sent.
	if got := post(t, h, url("wide"), text, strings.Repeat("x", webhook.MaxBody)); got != http.StatusAccepted {
		t.Fatalf("plain body to 4 targets: %d", got)
	}
	if got := post(t, h, url("narrow"), text, strings.Repeat("<", webhook.MaxBody)); got != http.StatusAccepted {
		t.Fatalf("escape-heavy body without fan-out: %d", got)
	}
	if n := len(h.Events()); n != 5 {
		t.Fatalf("%d events", n)
	}
}

func TestRoutesSelectByTarget(t *testing.T) {
	h := fanOut(t)
	if got := post(t, h, url("tickets"), http.Header{"X-Delivery-Id": {"d-1"}}, `{"ticket":"T-1"}`); got != http.StatusAccepted {
		t.Fatal(got)
	}
	triage, audit := h.Events()[0], h.Events()[1]
	s := domain.Scope{ID: scope, Name: scope}
	only := func(targets string) domain.Route {
		return domain.Route{Name: "r", Handler: webhook.Name, Binding: "b", Config: domain.Payload{Data: json.RawMessage(`{"targets":` + targets + `}`)}}
	}
	d, err := h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: audit, Scope: s, Route: only(`["audit"]`)})
	want := domain.ID(webhook.FixtureSource + "/" + webhook.Thread("tickets", "audit", "T-1", ""))
	if err != nil || !d.Match || d.CreateID != want || d.Display.Title != "A delivery to tickets for audit: T-1" {
		t.Fatalf("audit decision %+v %v", d, err)
	}
	if d, _ := h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: triage, Scope: s, Route: only(`["audit"]`)}); d.Match || d.Reason != "a target this route doesn't take" {
		t.Fatalf("audit route took triage: %+v", d)
	}
	// A route without targets takes every target, each as Work of its own.
	all := domain.Route{Name: "r", Handler: webhook.Name, Binding: "b"}
	if d, _ := h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: triage, Scope: s, Route: all}); !d.Match || d.CreateID == want {
		t.Fatalf("untargeted route: %+v", d)
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
	want := domain.ID(webhook.FixtureSource + "/" + webhook.Thread("deploys", "", "checkout", ""))
	if err != nil || !d.Match || d.Kind != webhook.WorkKind || d.CreateID != want || d.Display.Title != "A delivery to deploys: checkout" {
		t.Fatalf("decision %+v %v", d, err)
	}
	if err := extension.ValidateDisplay(d.Display); err != nil {
		t.Fatal(err)
	}
	// Once the thread's Work has finished, its next delivery opens the
	// thread's next generation, and that one's the next.
	done := domain.Work{ID: want, ScopeID: scope, Kind: webhook.WorkKind, State: domain.WorkCompleted}
	d, err = h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: ev, Scope: s, Route: r, Work: &done})
	if err != nil || !d.Match || !d.Successor || d.CreateID != want+"/g2" || d.Kind != webhook.WorkKind || len(d.AttachKeys) != 1 ||
		d.AttachKeys[0] != (domain.CorrelationKey{Source: ev.Event.Source, ExternalID: ev.Event.CorrelationKey.ExternalID + "/g2"}) {
		t.Fatalf("successor decision %+v %v", d, err)
	}
	if got := webhook.NextGeneration(want + "/g2"); got != want+"/g3" {
		t.Fatalf("third generation %s", got)
	}
	running := domain.Work{ID: want, ScopeID: scope, Kind: webhook.WorkKind, State: domain.WorkRunning}
	if d, _ := h.Route(context.Background(), webhook.Name, extension.RouteInput{Event: ev, Scope: s, Route: r, Work: &running}); !d.Match || d.Successor {
		t.Fatalf("live thread: %+v", d)
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

// signed is a harness whose hooks also check the sender's own signature,
// each in the way a known sender signs, with the clock at sdktest.Start.
func signed(t *testing.T) *sdktest.Host {
	t.Helper()
	h := sdktest.New(t, webhook.Build(webhook.Options{Now: func() time.Time { return sdktest.Start }}),
		sdktest.WithSecrets(scope, map[string]string{webhook.FixtureSecret: webhook.FixtureValue, "sender": "sender-secret"}))
	cfg := json.RawMessage(`{"scope":"` + scope + `","webhook_secret":"` + webhook.FixtureSecret + `","hooks":[` +
		`{"name":"github","signature":{"secret":"sender","header":"X-Hub-Signature-256","prefix":"sha256="}},` +
		`{"name":"shopify","signature":{"secret":"sender","header":"X-Shopify-Hmac-Sha256","encoding":"base64"}},` +
		`{"name":"slack","signature":{"secret":"sender","header":"X-Slack-Signature","prefix":"v0=","signed":"v0:{timestamp}:{body}","timestamp":"header:X-Slack-Request-Timestamp"}},` +
		`{"name":"stripe","signature":{"secret":"sender","header":"Stripe-Signature","prefix":"v1=","signed":"{timestamp}.{body}","timestamp":"signature:t=","tolerance_seconds":60}},` +
		`{"name":"linear","signature":{"secret":"sender","header":"Linear-Signature","timestamp":"body:/webhookTimestamp","tolerance_seconds":600}}]}`)
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: webhook.Name,
		Name: webhook.FixtureEntry, Config: cfg, Secrets: []string{webhook.FixtureSecret, "sender"}}}}); err != nil {
		t.Fatal(err)
	}
	return h
}

func mac(secret, msg string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func hexMAC(secret, msg string) string { return hex.EncodeToString(mac(secret, msg)) }

// A hook with a signature check takes a delivery its sender signed and
// refuses, with 401 and before acceptance, one that is unsigned, signed
// with another secret, tampered with or signed too long ago, whatever
// scheme the sender signs with. The URL token is still checked first.
func TestSignatureIsCheckedBeforeAcceptance(t *testing.T) {
	h := signed(t)
	const body = `{"action":"opened"}`
	now := strconv.FormatInt(sdktest.Start.Unix(), 10)
	stale := strconv.FormatInt(sdktest.Start.Add(-301*time.Second).Unix(), 10)
	ahead := strconv.FormatInt(sdktest.Start.Add(301*time.Second).Unix(), 10)
	slack := func(ts, signedTS, secret string) http.Header {
		return http.Header{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {"v0=" + hexMAC(secret, "v0:"+signedTS+":"+body)}}
	}
	linear := func(at time.Time) string { return `{"webhookTimestamp":` + strconv.FormatInt(at.UnixMilli(), 10) + `}` }
	accepted := 0
	for _, c := range []struct {
		name, hook string
		header     http.Header
		body       string
		want       int
	}{
		{"github signed", "github", http.Header{"X-Hub-Signature-256": {"sha256=" + hexMAC("sender-secret", body)}}, body, http.StatusAccepted},
		{"github upper-case hex", "github", http.Header{"X-Hub-Signature-256": {"sha256=" + strings.ToUpper(hexMAC("sender-secret", body))}}, body, http.StatusAccepted},
		{"github unsigned", "github", nil, body, http.StatusUnauthorized},
		{"github other secret", "github", http.Header{"X-Hub-Signature-256": {"sha256=" + hexMAC("webhook-secret", body)}}, body, http.StatusUnauthorized},
		{"github tampered body", "github", http.Header{"X-Hub-Signature-256": {"sha256=" + hexMAC("sender-secret", body)}}, `{"action":"closed"}`, http.StatusUnauthorized},
		{"github without prefix", "github", http.Header{"X-Hub-Signature-256": {hexMAC("sender-secret", body)}}, body, http.StatusUnauthorized},
		{"shopify base64", "shopify", http.Header{"X-Shopify-Hmac-Sha256": {base64.StdEncoding.EncodeToString(mac("sender-secret", body))}}, body, http.StatusAccepted},
		{"shopify hex where base64 is due", "shopify", http.Header{"X-Shopify-Hmac-Sha256": {hexMAC("sender-secret", body)}}, body, http.StatusUnauthorized},
		{"slack fresh", "slack", slack(now, now, "sender-secret"), body, http.StatusAccepted},
		{"slack stale", "slack", slack(stale, stale, "sender-secret"), body, http.StatusUnauthorized},
		{"slack ahead", "slack", slack(ahead, ahead, "sender-secret"), body, http.StatusUnauthorized},
		{"slack replayed under a fresh timestamp", "slack", slack(now, stale, "sender-secret"), body, http.StatusUnauthorized},
		{"slack no timestamp", "slack", http.Header{"X-Slack-Signature": {"v0=" + hexMAC("sender-secret", "v0::"+body)}}, body, http.StatusUnauthorized},
		{"stripe rolling secret", "stripe", http.Header{"Stripe-Signature": {"t=" + now + ",v1=" + hexMAC("old-secret", now+"."+body) + ",v1=" + hexMAC("sender-secret", now+"."+body) + ",v0=ignored"}}, body, http.StatusAccepted},
		{"stripe beyond its tolerance", "stripe", http.Header{"Stripe-Signature": {"t=" + strconv.FormatInt(sdktest.Start.Add(-61*time.Second).Unix(), 10) + ",v1=" + hexMAC("sender-secret", strconv.FormatInt(sdktest.Start.Add(-61*time.Second).Unix(), 10)+"."+body)}}, body, http.StatusUnauthorized},
		{"linear body timestamp in ms", "linear", http.Header{"Linear-Signature": {hexMAC("sender-secret", linear(sdktest.Start.Add(-9*time.Minute)))}}, linear(sdktest.Start.Add(-9 * time.Minute)), http.StatusAccepted},
		{"linear stale", "linear", http.Header{"Linear-Signature": {hexMAC("sender-secret", linear(sdktest.Start.Add(-11*time.Minute)))}}, linear(sdktest.Start.Add(-11 * time.Minute)), http.StatusUnauthorized},
	} {
		if got := post(t, h, url(c.hook), c.header, c.body); got != c.want {
			t.Fatalf("%s: %d, want %d", c.name, got, c.want)
		}
		if c.want == http.StatusAccepted {
			accepted++
		}
		if n := len(h.Events()); n != accepted {
			t.Fatalf("%s: %d events, want %d", c.name, n, accepted)
		}
	}
	// A wrong URL token is still a 404, signature or not.
	wrong := "/" + webhook.FixtureSource + "/github/" + strings.Repeat("0", 32)
	if got := post(t, h, wrong, http.Header{"X-Hub-Signature-256": {"sha256=" + hexMAC("sender-secret", body)}}, body); got != http.StatusNotFound {
		t.Fatalf("wrong token: %d", got)
	}
}

// A hook whose signature secret can't be resolved refuses with 503, so the
// sender retries, rather than taking the delivery unchecked.
func TestSignatureSecretUnavailable(t *testing.T) {
	h := sdktest.New(t, webhook.Build(webhook.Options{}), sdktest.WithSecrets(scope, map[string]string{webhook.FixtureSecret: webhook.FixtureValue}))
	cfg := json.RawMessage(`{"scope":"` + scope + `","webhook_secret":"` + webhook.FixtureSecret + `","hooks":[{"name":"github","signature":{"secret":"sender","header":"X-Hub-Signature-256","prefix":"sha256="}}]}`)
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: webhook.Name,
		Name: webhook.FixtureEntry, Config: cfg, Secrets: []string{webhook.FixtureSecret, "sender"}}}}); err != nil {
		t.Fatal(err)
	}
	if got := post(t, h, url("github"), http.Header{"X-Hub-Signature-256": {"sha256=" + hexMAC("", "{}")}}, `{}`); got != http.StatusServiceUnavailable || len(h.Events()) != 0 {
		t.Fatalf("%d, %d events", got, len(h.Events()))
	}
}

func TestSignatureSettings(t *testing.T) {
	hook := func(sig string) string {
		return `{"scope":"ops","webhook_secret":"hook","hooks":[{"name":"a","signature":` + sig + `}]}`
	}
	for name, sig := range map[string]string{
		"no secret":              `{"header":"X-Sig"}`,
		"secret unnamed":         `{"secret":"other","header":"X-Sig"}`,
		"no header":              `{"secret":"sender"}`,
		"bad algorithm":          `{"secret":"sender","header":"X-Sig","algorithm":"md5"}`,
		"bad encoding":           `{"secret":"sender","header":"X-Sig","encoding":"base32"}`,
		"prefix with a comma":    `{"secret":"sender","header":"X-Sig","prefix":"a,b"}`,
		"no body":                `{"secret":"sender","header":"X-Sig","signed":"{timestamp}","timestamp":"header:X-Ts"}`,
		"body twice":             `{"secret":"sender","header":"X-Sig","signed":"{body}{body}"}`,
		"unknown placeholder":    `{"secret":"sender","header":"X-Sig","signed":"{id}.{body}"}`,
		"unclosed":               `{"secret":"sender","header":"X-Sig","signed":"{body"}`,
		"timestamp unset":        `{"secret":"sender","header":"X-Sig","signed":"{timestamp}.{body}"}`,
		"timestamp unsigned":     `{"secret":"sender","header":"X-Sig","timestamp":"header:X-Ts"}`,
		"tolerance alone":        `{"secret":"sender","header":"X-Sig","tolerance_seconds":60}`,
		"tolerance over a day":   `{"secret":"sender","header":"X-Sig","signed":"{timestamp}.{body}","timestamp":"header:X-Ts","tolerance_seconds":86401}`,
		"bad timestamp key":      `{"secret":"sender","header":"X-Sig","signed":"{timestamp}.{body}","timestamp":"query:t"}`,
		"timestamp as signature": `{"secret":"sender","header":"X-Sig","prefix":"v1=","signed":"{timestamp}.{body}","timestamp":"signature:v1="}`,
		"unknown field":          `{"secret":"sender","header":"X-Sig","scheme":"github"}`,
	} {
		if _, err := webhook.ParseSettings(json.RawMessage(hook(sig)), []string{"hook", "sender"}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := webhook.ParseSettings(json.RawMessage(hook(`{"secret":"sender","header":"X-Sig","algorithm":"hmac-sha1","signed":"{header:X-Id}.{timestamp}.{body}","timestamp":"header:X-Ts"}`)), []string{"hook", "sender"}); err != nil {
		t.Fatal(err)
	}
}
