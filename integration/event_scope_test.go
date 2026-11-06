package integration_test

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// scanningScopes is the negative control for the state.events bound: a
// host store that answers ScopeEvents by reading the whole log, as the host
// did before scopes were recorded (#536).
type scanningScopes struct{ storage.Store }
type scanningScopesTx struct{ storage.Tx }

func (s scanningScopes) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return s.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(scanningScopesTx{tx}) })
}
func (t scanningScopesTx) ScopeEvents(scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	all, err := t.Events(after, 0)
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if slices.Contains(scopes, a.ScopeID) && (limit <= 0 || len(out) < limit) {
			out = append(out, a)
		}
	}
	return out, err
}

// TestStateEventsFollowTheExtensionsScopes: the runtime records each
// Event's scope as it accepts it, and the host answers state.events from
// storage's scope index. An extension reading its scopes' events reads
// those and the few whose scope isn't recorded yet, not the 2,500 events of
// another scope; a host store that scans the log gets the same answer but
// fails the bound. BackfillEventScopes then records the rest, once (#541).
func TestStateEventsFollowTheExtensionsScopes(t *testing.T) {
	e := newExtEnv(t)
	ctx := context.Background()
	g := storage.Guard{Now: e.clock.Now()}
	event := func(id string, work domain.ID) domain.Event {
		return domain.Event{ID: domain.ID(id), Type: "other.thing", Source: "other", DeliveryID: id, WorkID: work, OccurredAt: e.clock.Now(), ReceivedAt: e.clock.Now()}
	}
	var want []string
	if err := e.store.Transact(ctx, g, func(tx storage.Tx) error {
		if err := tx.PutWork(domain.Work{ID: "work/thread", Kind: "notes.thread", ScopeID: extensionScope, State: domain.WorkReady}, 0); err != nil {
			return err
		}
		for i := range 2500 {
			if _, _, err := runtime.AcceptInTx(tx, "other-scope", event("foreign-"+strconv.Itoa(i), "")); err != nil {
				return err
			}
			// Some of the extension's own, scattered through the log.
			if i%500 == 0 {
				id := "mine-" + strconv.Itoa(i)
				a, _, err := runtime.AcceptInTx(tx, extensionScope, event(id, ""))
				if err != nil {
					return err
				}
				if a.ScopeID != extensionScope {
					t.Errorf("AcceptInTx recorded scope %q, want %q", a.ScopeID, extensionScope)
				}
				want = append(want, id)
			}
			// Events from before scopes were recorded: storage holds
			// them without one, and they resolve through their Work.
			if i%1000 == 1 {
				id := "legacy-" + strconv.Itoa(i)
				if _, _, err := tx.AcceptEvent(event(id, "work/thread")); err != nil {
					return err
				}
				want = append(want, id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bound := len(want)

	read := 0
	events := func(store storage.Store) ([]string, int) {
		t.Helper()
		deps := e.deps()
		deps.Store = store
		svc, err := extproc.NewService(notes.Manifest(notes.Options{Release: 2}), deps)
		if err != nil {
			t.Fatal(err)
		}
		read = 0
		var ids []string
		if err := sdk.NewStore(pipeHost(t, svc)).Transact(ctx, g, func(tx storage.Tx) error {
			list, err := tx.Events(0, 0)
			for _, a := range list {
				ids = append(ids, string(a.Event.ID))
				if a.ScopeID != extensionScope {
					t.Errorf("state.events gave %s scope %q, want %q", a.Event.ID, a.ScopeID, extensionScope)
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids, read
	}
	if ids, n := events(countingStore{e.store, &read}); !reflect.DeepEqual(ids, want) || n > bound {
		t.Fatalf("state.events: %v, %d events read on the host, want %v within %d", ids, n, want, bound)
	}
	ids, n := events(scanningScopes{countingStore{e.store, &read}})
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("negative control answered %v", ids)
	}
	if n <= bound {
		t.Fatalf("negative control: scanning the log read %d events on the host; the bound catches nothing", n)
	}

	router, err := runtime.NewRouter(e.store, e.clock.Now, runtime.RouterOptions{Generation: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := router.BackfillEventScopes(ctx); err != nil || n != 3 {
		t.Fatalf("backfill recorded %d scopes (%v), want the 3 legacy events", n, err)
	}
	if err := e.store.Transact(ctx, g, func(tx storage.Tx) error {
		left, err := tx.ScopeEvents([]domain.ID{""}, 0, 0)
		if err == nil && len(left) != 0 {
			t.Errorf("after the backfill %d events have no scope", len(left))
		}
		a, err := tx.Event("legacy-1")
		if err == nil && a.ScopeID != extensionScope {
			t.Errorf("backfilled scope %q, want %q", a.ScopeID, extensionScope)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := router.BackfillEventScopes(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill recorded %d scopes (%v), want none", n, err)
	}
	if ids, n := events(countingStore{e.store, &read}); !reflect.DeepEqual(ids, want) || n > bound {
		t.Fatalf("state.events after the backfill: %v, %d events read, want %v within %d", ids, n, want, bound)
	}
}
