package integration_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// failingArchive fails every read, as a missing or damaged archive would.
type failingArchive struct{ storage.EventArchive }

var errArchiveDown = errors.New("archive down")

func (failingArchive) Event(context.Context, domain.ID) (domain.AcceptedEvent, error) {
	return domain.AcceptedEvent{}, errArchiveDown
}

func (failingArchive) Events(context.Context, uint64, int) ([]domain.AcceptedEvent, error) {
	return nil, errArchiveDown
}

// historyFixture holds a log of five Events: 2 and 3 retired after copying,
// 4 copied but still live (a pass that stopped before retiring it), and an
// archived record past the log's latest sequence.
func historyFixture(t *testing.T) (storage.Store, storage.EventArchive) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	store := reference.New()
	archive, err := sqlite.ArchiveBackend.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	var copied []domain.AcceptedEvent
	err = store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if e := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); e != nil {
			return e
		}
		for i, id := range []domain.ID{"e1", "e2", "e3", "e4", "e5"} {
			at := now.AddDate(0, i, 0)
			a, _, e := tx.AcceptEvent(domain.Event{ID: id, Type: "test.history", Source: "test", DeliveryID: string(id), OccurredAt: at, ReceivedAt: at,
				Data: domain.Payload{Schema: "test.history.v1", Data: []byte(`{"id":"` + string(id) + `"}`)}})
			if e != nil {
				return e
			}
			if e = tx.MarkRouted(id, "", "unmatched"); e != nil {
				return e
			}
			if e = tx.SetEventScope(id, "scope"); e != nil {
				return e
			}
			if a, e = tx.Event(a.Event.ID); e != nil {
				return e
			}
			if id == "e2" || id == "e3" || id == "e4" {
				copied = append(copied, a)
			}
		}
		beyond := copied[0]
		beyond.Event.ID, beyond.Event.DeliveryID, beyond.Sequence = "beyond", "beyond", 99
		if e := archive.PutEvents(ctx, append(copied, beyond)); e != nil {
			return e
		}
		if e := tx.RetireEvent("e2"); e != nil {
			return e
		}
		return tx.RetireEvent("e3")
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, archive
}

func historyIDs(events []domain.AcceptedEvent) []domain.ID {
	out := []domain.ID{}
	for _, e := range events {
		out = append(out, e.Event.ID)
	}
	return out
}

func TestEventHistoryReadsAcrossTheArchive(t *testing.T) {
	ctx := context.Background()
	store, archive := historyFixture(t)
	h := storage.EventHistory{Archive: archive}
	all := []domain.ID{"e1", "e2", "e3", "e4", "e5"}
	err := storage.View(ctx, store, time.Now(), func(tx storage.Tx) error {
		for name, read := range map[string]func(after uint64, limit int) ([]domain.AcceptedEvent, error){
			"events": func(after uint64, limit int) ([]domain.AcceptedEvent, error) { return h.Events(ctx, tx, after, limit) },
			"scope": func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
				return h.ScopeEvents(ctx, tx, []domain.ID{"scope"}, after, limit)
			},
			"type": func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
				return h.TypeEvents(ctx, tx, "test.history", nil, after, limit)
			},
		} {
			got, e := read(0, 0)
			if e != nil {
				return e
			}
			if ids := historyIDs(got); !reflect.DeepEqual(ids, all) {
				t.Fatalf("%s: got %v, want %v", name, ids, all)
			}
			for _, a := range got {
				if a.Retired || len(a.Event.Data.Data) == 0 {
					t.Fatalf("%s: %s came back without its payload", name, a.Event.ID)
				}
			}
			// Paging one at a time visits each once, across both sides.
			var paged []domain.ID
			for after := uint64(0); ; {
				page, e := read(after, 1)
				if e != nil {
					return e
				}
				if len(page) == 0 {
					break
				}
				paged, after = append(paged, page[0].Event.ID), page[0].Sequence
			}
			if !reflect.DeepEqual(paged, all) {
				t.Fatalf("%s paged: got %v, want %v", name, paged, all)
			}
		}
		for _, read := range []func() (domain.AcceptedEvent, error){
			func() (domain.AcceptedEvent, error) { return h.Event(ctx, tx, "e2") },
			func() (domain.AcceptedEvent, error) { return h.EventByDelivery(ctx, tx, "test", "e2") },
		} {
			a, e := read()
			if e != nil {
				return e
			}
			if a.Retired || string(a.Event.Data.Data) != `{"id":"e2"}` {
				t.Fatalf("whole record: %+v", a)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventHistoryWithoutAnArchiveReadsTheLiveLog(t *testing.T) {
	ctx := context.Background()
	store, _ := historyFixture(t)
	h := storage.EventHistory{}
	err := storage.View(ctx, store, time.Now(), func(tx storage.Tx) error {
		got, e := h.Events(ctx, tx, 0, 0)
		if e != nil {
			return e
		}
		if ids := historyIDs(got); !reflect.DeepEqual(ids, []domain.ID{"e1", "e4", "e5"}) {
			t.Fatalf("live log: %v", ids)
		}
		a, e := h.Event(ctx, tx, "e2")
		if e != nil {
			return e
		}
		if !a.Retired {
			t.Fatalf("want the tombstone, got %+v", a)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A failed archive read is an error, never a page with a silent gap or a
// record that looks as if it had no payload.
func TestEventHistoryFailsClosedWhenTheArchiveFails(t *testing.T) {
	ctx := context.Background()
	store, archive := historyFixture(t)
	h := storage.EventHistory{Archive: failingArchive{archive}}
	err := storage.View(ctx, store, time.Now(), func(tx storage.Tx) error {
		if _, e := h.Events(ctx, tx, 0, 0); !errors.Is(e, errArchiveDown) {
			t.Fatalf("list: %v", e)
		}
		if _, e := h.Event(ctx, tx, "e2"); !errors.Is(e, errArchiveDown) {
			t.Fatalf("event: %v", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
