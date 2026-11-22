package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// archivedHistory accepts a root and a child in scope "history" and one Event
// in scope "foreign", then copies the root and the foreign Event to an
// archive and retires them.
func archivedHistory(t *testing.T, h *managementHarness) storage.EventHistory {
	t.Helper()
	ctx := context.Background()
	archive, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(h.state, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { archive.Close() })
	accept := func(tx storage.Tx, id, scope, cause string) (domain.AcceptedEvent, error) {
		now := h.clock.Now()
		a, _, e := tx.AcceptEvent(domain.Event{ID: domain.ID(id), Type: "test.history", Source: "test", DeliveryID: id, CausationID: domain.ID(cause), OccurredAt: now, ReceivedAt: now,
			Data: domain.Payload{Schema: "test.history.v1", Data: json.RawMessage(`{"secret":"` + id + `"}`)}})
		if e != nil {
			return a, e
		}
		if e = tx.MarkRouted(a.Event.ID, "", "unmatched"); e != nil {
			return a, e
		}
		if e = tx.SetEventScope(a.Event.ID, domain.ID(scope)); e != nil {
			return a, e
		}
		return tx.Event(a.Event.ID)
	}
	err = h.store.Transact(ctx, storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
		for _, s := range []domain.ID{"history", "foreign"} {
			if e := tx.PutScope(domain.Scope{ID: s, Name: string(s)}, 0); e != nil {
				return e
			}
		}
		root, e := accept(tx, "history-root", "history", "")
		if e != nil {
			return e
		}
		if _, e = accept(tx, "history-child", "history", "history-root"); e != nil {
			return e
		}
		foreign, e := accept(tx, "history-foreign", "foreign", "")
		if e != nil {
			return e
		}
		if e = archive.PutEvents(ctx, []domain.AcceptedEvent{root, foreign}); e != nil {
			return e
		}
		if e = tx.RetireEvent(root.Event.ID); e != nil {
			return e
		}
		return tx.RetireEvent(foreign.Event.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	return storage.EventHistory{Archive: archive}
}

// The management API reads an archived Event as it read the live one: the
// list, the single read and the causal chain all carry its payload.
func TestManagementReadsEventsAcrossTheArchive(t *testing.T) {
	h := newManagementHarness(t)
	history := archivedHistory(t, h)
	server, err := management.NewServer(management.Options{Store: h.store, History: history, Now: h.clock.Now, Control: &runtime.Control{}, Toolbelt: h.belt, Config: h.configs, Version: "test", InstanceID: "instance-1"})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := management.Listen(h.lock, filepath.Join(h.state, "history.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: server}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })
	token, err := client.ReadTokenFile(filepath.Join(h.state, "private", "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New("unix://"+filepath.Join(h.state, "history.sock"), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page, err := c.Events(ctx, url.Values{"scope_id": {"history"}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range page.Items {
		ids = append(ids, e.ID)
	}
	if !reflect.DeepEqual(ids, []string{"history-root", "history-child"}) {
		t.Fatalf("listed %v", ids)
	}
	chain, err := c.EventChain(ctx, "history-child")
	if err != nil || len(chain) != 2 || chain[1].ID != "history-root" {
		t.Fatalf("chain: %+v %v", chain, err)
	}
	raw, _ := json.Marshal(chain[1])
	if !strings.Contains(string(raw), "history-root") || !strings.Contains(string(raw), "secret") {
		t.Fatalf("the archived cause lost its payload: %s", raw)
	}
}

// An extension reads archived Events through the same state reads, limited
// to its scopes: an archived Event of another scope stays not found.
func TestExtensionStateReadsAcrossTheArchiveInScope(t *testing.T) {
	h := newManagementHarness(t)
	history := archivedHistory(t, h)
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"}}
	s, err := extproc.NewService(m, extproc.Deps{Store: h.store, History: history, Now: h.clock.Now, Scopes: func() []domain.ID { return []domain.ID{"history"} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	call := func(method string, p protocol.StateParams) (any, error) {
		raw, _ := json.Marshal(p)
		return s.Handle(ctx, method, raw)
	}
	out, err := call("state.events", protocol.StateParams{})
	if err != nil {
		t.Fatal(err)
	}
	list := out.([]protocol.AcceptedEvent)
	if len(list) != 2 || list[0].Event.ID != "history-root" || list[0].Retired || len(list[0].Event.Data.Data) == 0 || list[1].Event.ID != "history-child" {
		t.Fatalf("state.events: %+v", list)
	}
	out, err = call("state.event", protocol.StateParams{ID: "history-root"})
	if err != nil || out.(protocol.AcceptedEvent).Retired {
		t.Fatalf("state.event: %+v %v", out, err)
	}
	out, err = call("state.delivery", protocol.StateParams{Source: "test", DeliveryID: "history-root"})
	if err != nil || out.(protocol.AcceptedEvent).Retired {
		t.Fatalf("state.delivery: %+v %v", out, err)
	}
	if _, err = call("state.event", protocol.StateParams{ID: "history-foreign"}); err == nil {
		t.Fatal("an archived Event of another scope was readable")
	}
	if _, err = call("state.delivery", protocol.StateParams{Source: "test", DeliveryID: "history-foreign"}); err == nil {
		t.Fatal("an archived delivery of another scope was readable")
	}
}
