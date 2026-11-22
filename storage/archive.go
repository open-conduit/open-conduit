package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	"github.com/open-conduit/open-conduit/domain"
)

// EventArchiveBackend is one kind of event archive's lifecycle. A location
// names one archive in the backend's own terms; the host passes it through
// without interpreting it.
type EventArchiveBackend interface {
	// Open opens the archive at location, creating it when it is absent. A
	// location the backend cannot resolve the same way from any process (a
	// relative path) is ErrInvalid.
	Open(ctx context.Context, location string) (EventArchive, error)
}

// EventArchive holds the whole records of Events that have left the live
// log. An Event is copied here, and PutEvents has returned, before
// Tx.RetireEvent drops its payload: the tombstone keeps what the live system
// decides with and the archive keeps the fact.
type EventArchive interface {
	// PutEvents stores whole records durably before it returns. Each must
	// be routed, with an ID, a sequence and a ReceivedAt, and must not be a
	// tombstone (Retired), whose payload is already gone: otherwise it is
	// ErrInvalid and none of the batch is stored. A record already held is
	// nil, so a pass that crashed can run again, under a newer binary too:
	// records are the same by SameAcceptedEvent, never by how a backend
	// encodes them. A different record under a held ID or sequence is
	// ErrConflict and none of the batch is stored. Any other failure may
	// leave part of the batch stored, which running it again completes.
	PutEvents(ctx context.Context, events []domain.AcceptedEvent) error
	// Event reads one whole record, ErrNotFound when it isn't held.
	Event(ctx context.Context, id domain.ID) (domain.AcceptedEvent, error)
	// Events returns records in sequence order, exclusively after after,
	// however they are partitioned; zero limit = all.
	Events(ctx context.Context, after uint64, limit int) ([]domain.AcceptedEvent, error)
	// WorkEvents is Events restricted to one Work's Events: those naming it
	// (Event.WorkID) and those routed to it (RoutedWorkID).
	WorkEvents(ctx context.Context, work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error)
	// ScopeEvents is Events restricted to those whose recorded ScopeID is one
	// of scopes, an empty ID standing for "not recorded", as Tx.ScopeEvents.
	// No scopes is an empty page.
	ScopeEvents(ctx context.Context, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error)
	// TypeEvents is Events restricted to one type and, when scopes is not
	// nil, to those recorded in scopes, as Tx.TypeEvents.
	TypeEvents(ctx context.Context, eventType string, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error)
	// Close releases the archive. Calls after it are ErrInvalid.
	Close() error
}

// SameAcceptedEvent reports whether a and b are the same record: the same
// instants, whatever their zones, the same payload JSON, whatever its
// insignificant whitespace, and every other field equal.
func SameAcceptedEvent(a, b domain.AcceptedEvent) bool {
	if !a.Event.OccurredAt.Equal(b.Event.OccurredAt) || !a.Event.ReceivedAt.Equal(b.Event.ReceivedAt) || !samePayload(a.Event.Data.Data, b.Event.Data.Data) {
		return false
	}
	for _, r := range []*domain.AcceptedEvent{&a, &b} {
		r.Event.OccurredAt, r.Event.ReceivedAt, r.Event.Data.Data = r.Event.OccurredAt.UTC().Truncate(0), r.Event.ReceivedAt.UTC().Truncate(0), nil
	}
	a.Event.OccurredAt, a.Event.ReceivedAt = b.Event.OccurredAt, b.Event.ReceivedAt
	return reflect.DeepEqual(a, b)
}

func samePayload(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}
