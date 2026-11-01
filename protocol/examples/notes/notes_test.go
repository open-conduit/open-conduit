package notes_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// The notes handlers, unit-tested against sdktest's in-memory host.

func changed(scope, id, text string, release int) domain.Event {
	key := domain.CorrelationKey{Source: notes.Name, ExternalID: id}
	return domain.Event{ID: domain.ID("notes/" + id), Type: notes.Changed, Source: notes.Name + "/" + scope, DeliveryID: id + "/1", CorrelationKey: &key, Data: notes.Payload(release, notes.Note{ID: id, Text: text})}
}

func TestLookupReadsTheIndex(t *testing.T) {
	ctx := context.Background()
	h := sdktest.New(t, notes.Build(notes.Options{Release: 2}, nil), sdktest.WithScopes(domain.Scope{ID: "s1"}),
		sdktest.WithKV(notes.IndexNS, "a", []byte(`{"text":"hello"}`)))
	got, err := h.Operation(ctx, "notes.lookup", domain.Payload{Data: []byte(`{"id":"a"}`)})
	if err != nil || string(got.Output.Data) != `{"text":"hello"}` || got.Output.Schema != "notes.lookup.v1" {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	_, err = h.Operation(ctx, "notes.lookup", domain.Payload{Data: []byte(`{"id":"missing"}`)})
	var oe *extension.OperationError
	if !errors.As(err, &oe) || oe.Code != "unknown_note" {
		t.Fatalf("missing note: %v", err)
	}
}

func TestIndexTaskDerivesEventsWithRecords(t *testing.T) {
	ctx := context.Background()
	clock := conformance.NewClock()
	h := sdktest.New(t, notes.Build(notes.Options{Release: 2}, nil), sdktest.WithClock(clock),
		sdktest.WithEvents("s1", changed("s1", "a", "first", 1), changed("s1", "b", "second", 2)))
	clock.Advance(time.Minute)
	if err := h.RunTask(ctx, "notes-index"); err != nil {
		t.Fatal(err)
	}
	index := h.KV(notes.IndexNS)
	if string(index["a"]) != `{"text":"first"}` || string(index["b"]) != `{"text":"second"}` {
		t.Fatalf("index %q", index)
	}
	events := h.Events()
	if len(events) != 4 {
		t.Fatalf("%d events", len(events))
	}
	derived := events[2].Event
	if derived.Type != notes.Indexed || derived.CausationID != "notes/a" || !derived.ReceivedAt.Equal(clock.Now()) {
		t.Fatalf("derived %+v", derived)
	}
	// A second pass derives nothing new: event.accept is idempotent.
	if err := h.RunTask(ctx, "notes-index"); err != nil || len(h.Events()) != 4 {
		t.Fatalf("second pass: %d events, %v", len(h.Events()), err)
	}
}

func TestMigrateRewritesTheIndex(t *testing.T) {
	ctx := context.Background()
	h := sdktest.New(t, notes.Build(notes.Options{Release: 2}, nil), sdktest.WithKVVersion(1),
		sdktest.WithKV(notes.IndexNS, "a", []byte("plain")))
	res, err := h.Configure(ctx, protocol.ConfigureParams{})
	if err != nil || res.KVVersion != 2 || h.KVVersion() != 2 {
		t.Fatalf("configure: %+v %v", res, err)
	}
	if got := string(h.KV(notes.IndexNS)["a"]); got != `{"text":"plain"}` {
		t.Fatalf("migrated %q", got)
	}
	if len(h.Commits()) != 1 || len(h.Commits()[0].Checks) != 1 {
		t.Fatalf("commits %+v", h.Commits())
	}
}

func TestAnnotateEmitsAndCommitsRecords(t *testing.T) {
	ctx := context.Background()
	h := sdktest.New(t, notes.Build(notes.Options{Release: 2}, nil), sdktest.WithEvents("s1", changed("s1", "a", "first", 2)),
		sdktest.WithAuthority(extension.Authority{ScopeID: "s1", TriggerID: "notes/a"}))
	got, err := h.Operation(ctx, "notes.annotate", domain.Payload{Data: []byte(`{"note":"remember"}`)})
	if err != nil || got.Event == nil || !got.Event.Added || got.Event.Depth != 1 || got.Event.Root != "notes/a" {
		t.Fatalf("annotate: %+v %v", got, err)
	}
	id := string(got.Event.Event.Event.ID)
	if string(h.KV(notes.Name + ".annotations")[id]) != "remember" {
		t.Fatalf("records %q", h.KV(notes.Name+".annotations"))
	}
	var out struct {
		Event string `json:"event"`
		Depth int    `json:"depth"`
	}
	if json.Unmarshal(got.Output.Data, &out) != nil || out.Event != id || out.Depth != 1 {
		t.Fatalf("output %s", got.Output.Data)
	}
}

func TestRouteMatchesChangedNotes(t *testing.T) {
	ctx := context.Background()
	h := sdktest.New(t, notes.Build(notes.Options{Release: 2}, nil))
	d, err := h.Route(ctx, "notes", extension.RouteInput{Event: domain.AcceptedEvent{Event: changed("s1", "a", "first", 2)}})
	if err != nil || !d.Match || d.CreateID != "notes/a" || d.Kind != notes.Kind {
		t.Fatalf("route: %+v %v", d, err)
	}
	d, err = h.Route(ctx, "notes", extension.RouteInput{Event: domain.AcceptedEvent{Event: domain.Event{Type: notes.Deleted}}})
	if err != nil || d.Match {
		t.Fatalf("deleted note matched: %+v %v", d, err)
	}
}

func TestWebhookDeliversThroughIngress(t *testing.T) {
	ctx := context.Background()
	h := sdktest.New(t, notes.Build(notes.Options{Release: 2}, nil))
	cfg, _ := json.Marshal(map[string]string{"service": "notes.json"})
	res, err := h.Configure(ctx, protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "s1", Type: "notes", Config: cfg}}})
	if err != nil || len(res.WebhookPaths) != 1 {
		t.Fatalf("configure: %+v %v", res, err)
	}
	body, _ := json.Marshal(map[string]any{"note": notes.Note{ID: "a", Rev: "r1", Text: "pushed"}})
	var out protocol.WebhookHandleResult
	req := protocol.WebhookHandleParams{Path: res.WebhookPaths[0], Method: http.MethodPost, Body: body,
		Header: map[string][]string{"X-Notes-Token": {notes.WebhookToken}, "X-Notes-Delivery": {"d1"}}}
	if err := h.Call(ctx, "webhook.handle", req, &out); err != nil || out.Status != http.StatusAccepted {
		t.Fatalf("webhook: %+v %v", out, err)
	}
	events := h.Events()
	if len(events) != 1 || events[0].Event.Type != notes.Changed || notes.Text(events[0].Event.Data) != "pushed" {
		t.Fatalf("events %+v", events)
	}
}

func TestCheckPasses(t *testing.T) {
	h := sdktest.New(t, notes.Build(notes.Options{Release: 2}, nil))
	if r, err := h.Check(context.Background(), "notes"); err != nil || !r.OK || r.Detail != "ok" {
		t.Fatalf("check: %+v %v", r, err)
	}
}

// The fixture profile is declared once (sdk.New and its registrations);
// the manifest derived from them is the hand-written one.
func TestFixtureManifestIsDerived(t *testing.T) {
	want, _ := json.Marshal(notes.Manifest(notes.Options{Fixture: true}))
	got, _ := json.Marshal(notes.Build(notes.Options{Fixture: true}, nil).Manifest)
	if string(got) != string(want) {
		t.Fatalf("derived manifest\n%s\nhand-written\n%s", got, want)
	}
}

func TestFixtureConnectionTakesTypedInput(t *testing.T) {
	ctx := context.Background()
	h := sdktest.New(t, notes.Build(notes.Options{Fixture: true}, nil), sdktest.WithScopes(domain.Scope{ID: "s1"}),
		sdktest.WithAuthority(extension.Authority{ScopeID: "s1", Connection: "conn"}),
		sdktest.WithConnectionSecrets("s1", "conn", map[string]string{"token": "t0ken"}))
	got, err := sdktest.Call[notes.ConnectionResult](ctx, h, "fixture.connection", notes.ConnectionRequest{Names: []string{"token", "other"}})
	if err != nil || got.Connection != "conn" || len(got.Resolved) != 1 || got.Resolved[0] != "token" {
		t.Fatalf("connection: %+v %v", got, err)
	}
	_, err = sdktest.Call[notes.ConnectionResult](ctx, h, "fixture.connection", map[string]any{"name": "token"})
	var oe *extension.OperationError
	if !errors.As(err, &oe) || oe.Code != "bad_request" || oe.Message != `fixture.connection: unknown field "name"` {
		t.Fatalf("unknown field: %v", err)
	}
}
