package notes

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json (07-03): a fixture
// command that writes the note service file for the run.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: []string{"notes-extension", "certify-fixture"}}
}

// FixtureScope is the scope the certification fixture configures.
const FixtureScope = "notes-scope"

// NewFixture writes a note service file into dir and returns the release-2
// certification fixture over it: an ingress case whose canonical
// observation comes from the in-process reference ingress, a route case,
// and two lookup operation cases.
func NewFixture(dir string) (protocol.Fixture, error) {
	at := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	service := filepath.Join(dir, "service.json")
	raw, _ := json.Marshal(ServiceFile{Notes: []Note{{ID: "a", Rev: "r1", Text: "hello world", At: at}, {ID: "b", Rev: "r1", Text: "second note", At: at}}})
	if err := os.WriteFile(service, raw, 0o600); err != nil {
		return protocol.Fixture{}, err
	}
	source := extension.Source{Namespace: Name + "/" + FixtureScope, ScopeID: FixtureScope}
	body, _ := json.Marshal(map[string]any{"note": Note{ID: "a", Rev: "r1", Text: "hello world", At: at}})
	deleted, _ := json.Marshal(map[string]any{"deleted": map[string]any{"id": "b", "at": at}})
	reference := &Ingress{Service: service, Release: 2}
	d := extension.Delivery{ID: "d1", ReceivedAt: at, Body: body}
	res, err := reference.Push(context.Background(), source, d)
	if err != nil {
		return protocol.Fixture{}, err
	}
	oneShot := protocol.FromDelivery(extension.Delivery{ID: "d2", ReceivedAt: at, Body: deleted})
	cfg, _ := json.Marshal(map[string]string{"service": service})
	ev := res.Observations[0].Event
	ev.ID = "certify-note"
	route := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 1}, Scope: domain.Scope{ID: FixtureScope, Name: FixtureScope}, Route: domain.Route{Name: "notes", Handler: Name, Binding: "agent"}}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: Name, Name: "notes", Config: cfg}},
		Ingress: &protocol.IngressCase{Source: Name + "/" + FixtureScope, Delivery: protocol.FromDelivery(d),
			Canonical:   protocol.FromPushResult(res).Observations[0],
			OneShotType: Deleted, OneShot: &oneShot},
		Routes: []protocol.RouteCase{{Handler: Name, Input: protocol.FromRouteInput(route), Want: protocol.RouteDecision{Match: true, CreateID: "notes/a", Kind: Kind, Display: protocol.FromDisplay(Display("a"))}}},
		Operations: []protocol.OperationCase{
			{Operation: "notes.lookup", Input: protocol.Payload{Schema: "notes.lookup.request.v1", Data: json.RawMessage(`{"id":"missing"}`)}, Error: "unknown_note"},
			{Operation: "notes.lookup", Input: protocol.Payload{Schema: "notes.lookup.request.v1", Data: json.RawMessage(`{}`)}, Error: "bad_request"},
		},
		Resources: []protocol.ResourceCase{
			{Type: Scratch, Config: json.RawMessage(`{"mode":"fresh"}`), ModeDefault: extension.ResourceFresh},
			{Type: Scratch, Config: json.RawMessage(`{}`), Mode: extension.ResourceWork},
			{Type: Scratch, Config: json.RawMessage(`{"cache":true}`), SandboxRefused: true},
		},
	}, nil
}

// ServeFixture runs the fixture command in the current directory: it prints
// the fixture as one JSON line and waits for stdin to close.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	f, err := NewFixture(dir)
	if err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}
