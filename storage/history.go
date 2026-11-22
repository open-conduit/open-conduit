package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-conduit/open-conduit/domain"
)

// EventHistory reads Events across the live log and an archive, for the
// reads that show history to an operator or an extension. The runtime's own
// decisions read the Tx alone: a tombstone keeps everything they decide with,
// and no write transaction ever waits on an archive. The zero value has no
// archive and reads the live log.
//
// Each read takes the Tx's view first and the archive after it. An Event is
// in the archive before it is retired, so whatever this view sees retired is
// already there. A record in both (a pass that stopped between copying and
// retiring) is answered once, from the live log, and a record past this
// view's latest sequence is left out, so a page never shows more than the
// view it was read in.
type EventHistory struct {
	Archive EventArchive
}

// Event is Tx.Event with a tombstone's whole record when the archive holds
// it. Without one it answers with the tombstone, which is marked Retired. A
// failed archive read is an error, never a payload silently missing.
func (h EventHistory) Event(ctx context.Context, tx Tx, id domain.ID) (domain.AcceptedEvent, error) {
	a, err := tx.Event(id)
	if err != nil {
		return domain.AcceptedEvent{}, err
	}
	return h.whole(ctx, a)
}

// EventByDelivery is Tx.EventByDelivery with a tombstone's whole record, as
// Event.
func (h EventHistory) EventByDelivery(ctx context.Context, tx Tx, source, deliveryID string) (domain.AcceptedEvent, error) {
	a, err := tx.EventByDelivery(source, deliveryID)
	if err != nil {
		return domain.AcceptedEvent{}, err
	}
	return h.whole(ctx, a)
}

// whole swaps a tombstone for its archived record. The tombstone's recorded
// scope stands, since authority is decided on it.
func (h EventHistory) whole(ctx context.Context, a domain.AcceptedEvent) (domain.AcceptedEvent, error) {
	if !a.Retired || h.Archive == nil {
		return a, nil
	}
	held, err := h.Archive.Event(ctx, a.Event.ID)
	if errors.Is(err, ErrNotFound) {
		return a, nil
	}
	if err != nil {
		return domain.AcceptedEvent{}, err
	}
	if held.Sequence != a.Sequence {
		return domain.AcceptedEvent{}, fmt.Errorf("storage: archived event %s has sequence %d, tombstone has %d: %w", a.Event.ID, held.Sequence, a.Sequence, ErrConflict)
	}
	if a.ScopeID != "" {
		held.ScopeID = a.ScopeID
	}
	return held, nil
}

// Events is Tx.Events across the live log and the archive.
func (h EventHistory) Events(ctx context.Context, tx Tx, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	return h.merged(tx, after, limit, tx.Events, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
		return h.Archive.Events(ctx, after, limit)
	})
}

// WorkEvents is Tx.WorkEvents across the live log and the archive.
func (h EventHistory) WorkEvents(ctx context.Context, tx Tx, work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	return h.merged(tx, after, limit, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
		return tx.WorkEvents(work, after, limit)
	}, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
		return h.Archive.WorkEvents(ctx, work, after, limit)
	})
}

// ScopeEvents is Tx.ScopeEvents across the live log and the archive.
func (h EventHistory) ScopeEvents(ctx context.Context, tx Tx, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	return h.merged(tx, after, limit, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
		return tx.ScopeEvents(scopes, after, limit)
	}, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
		return h.Archive.ScopeEvents(ctx, scopes, after, limit)
	})
}

// TypeEvents is Tx.TypeEvents across the live log and the archive.
func (h EventHistory) TypeEvents(ctx context.Context, tx Tx, eventType string, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	return h.merged(tx, after, limit, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
		return tx.TypeEvents(eventType, scopes, after, limit)
	}, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
		return h.Archive.TypeEvents(ctx, eventType, scopes, after, limit)
	})
}

type eventPage func(after uint64, limit int) ([]domain.AcceptedEvent, error)

// merged reads up to limit from each side and keeps the first limit in
// sequence order: each side's page holds its own first limit, so the merge
// holds the first limit of both.
func (h EventHistory) merged(tx Tx, after uint64, limit int, live, archived eventPage) ([]domain.AcceptedEvent, error) {
	current, err := live(after, limit)
	if err != nil || h.Archive == nil {
		return current, err
	}
	bound, err := tx.LatestSequence()
	if err != nil {
		return nil, err
	}
	old, err := archived(after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.AcceptedEvent, 0, len(current)+len(old))
	i, j := 0, 0
	for (i < len(current) || j < len(old)) && (limit <= 0 || len(out) < limit) {
		switch {
		case j < len(old) && old[j].Sequence > bound:
			j = len(old)
		case j == len(old) || i < len(current) && current[i].Sequence < old[j].Sequence:
			out = append(out, current[i])
			i++
		case i == len(current) || old[j].Sequence < current[i].Sequence:
			out = append(out, old[j])
			j++
		default:
			out = append(out, current[i])
			i, j = i+1, j+1
		}
	}
	return out, nil
}
