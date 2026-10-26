package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Negative controls on the real backend (05-04): the shared storage suite
// must reject SQLite when a wrapper weakens one guarantee, so its passing
// on the unwrapped store is evidence and not a property of the suite.

// lostAtomicity acknowledges a callback's error but commits its writes.
type lostAtomicity struct{ storage.Store }

func (b lostAtomicity) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	var callbackErr error
	if err := b.Store.Transact(ctx, g, func(tx storage.Tx) error { callbackErr = fn(tx); return nil }); err != nil {
		return err
	}
	return callbackErr
}

// lostDedup folds the internal Event ID into the delivery identity, so a
// redelivery is accepted again.
type lostDedup struct{ storage.Store }
type lostDedupTx struct{ storage.Tx }

func (b lostDedup) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(lostDedupTx{tx}) })
}
func (b lostDedupTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	e.DeliveryID += ":" + string(e.ID)
	return b.Tx.AcceptEvent(e)
}

// lostFencing drops the lease guard, so a superseded worker can still write.
type lostFencing struct{ storage.Store }

func (b lostFencing) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	g.Lease = nil
	return b.Store.Transact(ctx, g, fn)
}

func TestWeakenedSQLiteIsRejected(t *testing.T) {
	cases := []struct {
		name   string
		weaken func(storage.Store) storage.Store
		cases  []string
	}{
		{"lost_atomicity", func(s storage.Store) storage.Store { return lostAtomicity{s} }, []string{"atomic_route_work_kv_timer", "timer_fire_rearm_rollback", "namespaced_kv_and_checkpoint_rollback", "wait_registration_atomic_with_park"}},
		{"lost_dedup", func(s storage.Store) storage.Store { return lostDedup{s} }, []string{"event_identity_not_correlation"}},
		{"lost_fencing", func(s storage.Store) storage.Store { return lostFencing{s} }, []string{"lease_expiry_recovery_and_fencing", "terminal_work_cancellation_fences_worker"}},
	}
	byName := map[string]conformance.StorageCase{}
	for _, c := range conformance.StorageCases() {
		byName[c.Name] = c
	}
	for _, negative := range cases {
		t.Run(negative.name, func(t *testing.T) {
			for _, name := range negative.cases {
				c, ok := byName[name]
				if !ok {
					t.Fatalf("no conformance case %q", name)
				}
				store := open(t, filepath.Join(t.TempDir(), "conduit.db"))
				if err := c.Check(negative.weaken(store)); err != nil {
					t.Logf("%s rejected weakened SQLite: %v", name, err)
				} else {
					t.Errorf("%s did not detect %s", name, negative.name)
				}
				// The same case passes on the unweakened store.
				if err := c.Check(open(t, filepath.Join(t.TempDir(), "conduit.db"))); err != nil {
					t.Fatalf("%s fails on plain SQLite: %v", name, err)
				}
			}
		})
	}
}
