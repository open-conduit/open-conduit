package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	archiveNamespace = "core.archive"
	archiveCursorKey = "cursor"
)

// Why the archive pass left an Event live, beside RetirementGuard's reasons.
const (
	ArchiveKeptRecent          = "within_horizon"
	ArchiveKeptScopeUnrecorded = "scope_unrecorded"
	ArchiveKeptChanged         = "changed_since_copied"
	ArchiveKeptConflict        = "archive_conflict"
)

// ArchivePolicy is what is old enough to leave the live log: Events of Work
// that finished by FinishedBefore, and Events that opened no Work and were
// received by UnmatchedBefore.
type ArchivePolicy struct {
	FinishedBefore, UnmatchedBefore time.Time
}

// ArchivePass is what one pass did. Kept counts the Events it left live, by
// reason; Wrapped says it reached the end of the live log.
type ArchivePass struct {
	Scanned, Archived int
	Kept              map[string]int
	Wrapped           bool
}

// ArchiveEvents moves up to limit Events' worth of finished history to the
// archive. It reads a page from a rotating cursor, so an Event kept today is
// looked at again and the cost of a pass doesn't follow the log's size. Each
// record is durable in the archive before the transaction that retires it,
// which rebuilds the guard and decides again, so a crash at any point leaves
// the Event live or archived and the next pass completes it.
func (s *Scheduler) ArchiveEvents(ctx context.Context, policy ArchivePolicy, limit int) (ArchivePass, error) {
	if limit < 1 || limit > 200 || s.options.EventArchive == nil || policy.FinishedBefore.IsZero() || policy.UnmatchedBefore.IsZero() {
		return ArchivePass{}, storage.ErrInvalid
	}
	var page, candidates []domain.AcceptedEvent
	var kept map[string]int
	if err := s.transact(ctx, nil, func(tx storage.Tx) error {
		page, candidates, kept = nil, nil, map[string]int{}
		var cursor uint64
		if er := getJSON(tx, archiveNamespace, archiveCursorKey, &cursor); er != nil && !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		var er error
		if page, er = tx.Events(cursor, limit); er != nil {
			return er
		}
		guard, er := NewRetirementGuardInTx(tx)
		if er != nil {
			return er
		}
		for _, a := range page {
			why, er := archiveBlockedInTx(tx, guard, policy, a)
			if er != nil {
				return er
			}
			if why != "" {
				kept[why]++
				continue
			}
			candidates = append(candidates, a)
		}
		return nil
	}); err != nil {
		return ArchivePass{}, err
	}
	copied, err := s.copyToArchive(ctx, candidates, kept)
	if err != nil {
		return ArchivePass{}, err
	}
	pass := ArchivePass{Scanned: len(page), Wrapped: len(page) < limit}
	var retired int
	var late map[string]int
	if err = s.transact(ctx, nil, func(tx storage.Tx) error {
		retired, late = 0, map[string]int{}
		guard, er := NewRetirementGuardInTx(tx)
		if er != nil {
			return er
		}
		for _, held := range copied {
			current, er := tx.Event(held.Event.ID)
			if er != nil {
				return er
			}
			if current.Retired {
				continue
			}
			if !storage.SameAcceptedEvent(current, held) {
				late[ArchiveKeptChanged]++
				continue
			}
			why, er := archiveBlockedInTx(tx, guard, policy, current)
			if er != nil {
				return er
			}
			if why != "" {
				late[why]++
				continue
			}
			if er = tx.RetireEvent(current.Event.ID); er != nil {
				return er
			}
			retired++
		}
		var next uint64
		if !pass.Wrapped {
			next = page[len(page)-1].Sequence
		}
		return putJSON(tx, archiveNamespace, archiveCursorKey, next)
	}); err != nil {
		return ArchivePass{}, err
	}
	for why, n := range late {
		kept[why] += n
	}
	pass.Archived, pass.Kept = retired, kept
	return pass, nil
}

// copyToArchive stores candidates durably and returns those the archive
// holds. A record that differs from the archive's copy stays live and is
// counted, so one conflict doesn't stop the rest of the page moving.
func (s *Scheduler) copyToArchive(ctx context.Context, candidates []domain.AcceptedEvent, kept map[string]int) ([]domain.AcceptedEvent, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	err := s.options.EventArchive.PutEvents(ctx, candidates)
	if err == nil {
		return candidates, nil
	}
	if !errors.Is(err, storage.ErrConflict) {
		return nil, err
	}
	copied := make([]domain.AcceptedEvent, 0, len(candidates))
	for _, a := range candidates {
		switch err = s.options.EventArchive.PutEvents(ctx, []domain.AcceptedEvent{a}); {
		case err == nil:
			copied = append(copied, a)
		case errors.Is(err, storage.ErrConflict):
			kept[ArchiveKeptConflict]++
		default:
			return nil, err
		}
	}
	return copied, nil
}

// archiveBlockedInTx is why a must stay live under policy, empty when it may
// go. Only an Event whose scope is recorded goes: recording it later would
// change the record the archive holds.
func archiveBlockedInTx(tx storage.Tx, guard *RetirementGuard, policy ArchivePolicy, a domain.AcceptedEvent) (string, error) {
	if why := guard.Blocked(a); why != "" {
		return why, nil
	}
	if a.ScopeID == "" {
		return ArchiveKeptScopeUnrecorded, nil
	}
	owners := make([]domain.ID, 0, 2)
	for _, id := range []domain.ID{a.Event.WorkID, a.RoutedWorkID} {
		if id != "" {
			owners = append(owners, id)
		}
	}
	if len(owners) == 0 {
		if a.Event.ReceivedAt.After(policy.UnmatchedBefore) {
			return ArchiveKeptRecent, nil
		}
		return "", nil
	}
	for _, id := range owners {
		w, err := tx.Work(id)
		if err != nil {
			return "", err
		}
		if !w.State.Terminal() {
			return RetireBlockedLiveWork, nil
		}
		if w.UpdatedAt.After(policy.FinishedBefore) {
			return ArchiveKeptRecent, nil
		}
	}
	return "", nil
}
