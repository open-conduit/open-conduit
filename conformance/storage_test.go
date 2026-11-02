package conformance_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

func TestReferenceStorage(t *testing.T) {
	conformance.RunStorage(t, func() storage.Store { return reference.New() })
}

// brokenAtomic acknowledges callback errors but commits their writes anyway.
type brokenAtomic struct{ storage.Store }

func (b brokenAtomic) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	var callbackErr error
	err := b.Store.Transact(ctx, g, func(tx storage.Tx) error { callbackErr = fn(tx); return nil })
	if err != nil {
		return err
	}
	return callbackErr
}

type brokenDedup struct{ storage.Store }
type brokenDedupTx struct{ storage.Tx }

func (b brokenDedup) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenDedupTx{tx}) })
}
func (b brokenDedupTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	// Incorrectly includes the internal event ID in the external delivery identity.
	e.DeliveryID += ":" + string(e.ID)
	return b.Tx.AcceptEvent(e)
}

// brokenStepBack renews the way backends did before a stepped-back clock
// was tolerated: it refuses a renewal whose new expiry is earlier than the
// stored one.
type brokenStepBack struct{ storage.Store }
type brokenStepBackTx struct {
	storage.Tx
	now time.Time
}

func (b brokenStepBack) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenStepBackTx{tx, g.Now}) })
}
func (b brokenStepBackTx) Renew(ref domain.LeaseRef, ttl time.Duration) error {
	if x, err := b.Execution(ref.ExecutionID); err == nil && b.now.Add(ttl).Before(x.Lease.ExpiresAt) {
		return storage.ErrInvalid
	}
	return b.Tx.Renew(ref, ttl)
}

// brokenActor stores Events without who caused them, as a store mapping
// Events to columns might if it missed the field.
type brokenActor struct{ storage.Store }
type brokenActorTx struct{ storage.Tx }

func (b brokenActor) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenActorTx{tx}) })
}
func (b brokenActorTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	e.Actor = ""
	return b.Tx.AcceptEvent(e)
}

func TestWeakenedBackendsAreRejected(t *testing.T) {
	cases := []struct {
		name, diagnostic string
		factory          func() storage.Store
	}{
		{"event_identity_not_correlation", "duplicate delivery", func() storage.Store { return brokenDedup{reference.New()} }},
		{"event_actor_round_trip", "event actor lost", func() storage.Store { return brokenActor{reference.New()} }},
		{"atomic_route_work_kv_timer", "event leaked", func() storage.Store { return brokenAtomic{reference.New()} }},
		{"timer_fire_rearm_rollback", "rollback was not atomic", func() storage.Store { return brokenAtomic{reference.New()} }},
		{"lease_renew_survives_clock_step_back", "renewal after clock step back", func() storage.Store { return brokenStepBack{reference.New()} }},
	}
	for _, negative := range cases {
		t.Run(negative.name, func(t *testing.T) {
			for _, c := range conformance.StorageCases() {
				if c.Name == negative.name {
					err := c.Check(negative.factory())
					if err == nil || !strings.Contains(err.Error(), negative.diagnostic) {
						t.Fatalf("wanted conformance rejection containing %q, got %v", negative.diagnostic, err)
					}
					t.Logf("weakened backend rejected: %v", err)
					return
				}
			}
			t.Fatal("negative control did not select a real conformance case")
		})
	}
}
