package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// A finished attempt whose stop is unconfirmed (AttemptState.Excluded
// false) holds its Work and a slot until the host proves it stopped. The
// paths that end an attempt each ask the host for that proof once, but a
// single ask can miss: a launch failure the host doesn't stop for, or an
// observer whose epoch a recovery has since superseded. So the runtime
// follows every such attempt up itself until it is confirmed: each poll
// hands the host the attempts whose follow-up is due, at the attempt's
// current epoch, and moves the next one out with backoff. The schedule is
// durable, so a restart carries on where it was, and an epoch change
// starts it again.

// StopFollowupBase is the delay before an unconfirmed attempt's first
// follow-up, which leaves the host's own stop for that attempt time to
// finish, and the base of the backoff after it.
const StopFollowupBase = 30 * time.Second

// StopFollowupMax caps the delay between follow-ups.
const StopFollowupMax = 30 * time.Minute

// stopFollowupNamespace holds each unconfirmed attempt's follow-up, keyed
// like attemptNamespace. putAttemptState removes it once the attempt is
// excluded.
const stopFollowupNamespace = "core.scheduler.stop_followup"

// StopFollowup is the runtime's schedule for proving one attempt stopped.
type StopFollowup struct {
	ExecutionID domain.ID `json:"execution_id"`
	// Epoch is the attempt epoch the schedule is for; a newer one starts
	// it again.
	Epoch uint64 `json:"epoch"`
	// Attempts is how many follow-ups have been handed to the host.
	Attempts int       `json:"attempts"`
	DueAt    time.Time `json:"due_at"`
}

// StopDue is an attempt the host should stop and confirm (StopExecutor's
// Stop, then ConfirmStopped) at Epoch. Attempt counts the follow-ups so far,
// this one included.
type StopDue struct {
	ExecutionID domain.ID
	Epoch       uint64
	Attempt     int
}

// stopFollowupDelay is the delay after the n-th follow-up (0 before the
// first): StopFollowupBase doubled n times, at most StopFollowupMax.
func stopFollowupDelay(n int) time.Duration {
	d := StopFollowupBase
	for i := 0; i < n && d < StopFollowupMax; i++ {
		d *= 2
	}
	if d > StopFollowupMax {
		d = StopFollowupMax
	}
	return d
}

// StopFollowupStatus is the follow-up recorded for an attempt (ok false
// when there is none).
func (s *Scheduler) StopFollowupStatus(ctx context.Context, id domain.ID) (f StopFollowup, ok bool, err error) {
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		e := getJSON(tx, stopFollowupNamespace, identity(string(id)), &f)
		if errors.Is(e, storage.ErrNotFound) {
			return nil
		}
		ok = e == nil
		return e
	})
	return
}

// dueStops schedules a follow-up for every finished, unconfirmed attempt
// without one (or with one for an older epoch) and returns, at most limit,
// those that are due, moving each one's next follow-up out. A fair cursor
// shares the passes among more than limit attempts.
func (s *Scheduler) dueStops(ctx context.Context, limit int) (due []StopDue, err error) {
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		due = nil
		var cursor string
		if e := getJSON(tx, timerNamespace, "poll_stops", &cursor); e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		rows, e := tx.List(unexcludedNamespace, "")
		if e != nil {
			return e
		}
		ids := make([]domain.ID, 0, len(rows))
		for _, row := range rows {
			var id domain.ID
			if e = json.Unmarshal(row.Value, &id); e != nil {
				return e
			}
			ids = append(ids, id)
		}
		now := s.now()
		for _, id := range rotateIDs(ids, cursor, len(ids)) {
			if len(due) >= limit {
				break
			}
			d, e := s.followStopInTx(tx, id, now)
			if e != nil {
				return e
			}
			if d != nil {
				due = append(due, *d)
				cursor = string(id)
			}
		}
		if len(due) == 0 {
			return nil
		}
		return putJSON(tx, timerNamespace, "poll_stops", cursor)
	})
	return due, err
}

// followStopInTx keeps one unexcluded attempt's follow-up, returning it
// when it is due.
func (s *Scheduler) followStopInTx(tx storage.Tx, id domain.ID, now time.Time) (*StopDue, error) {
	key := identity(string(id))
	state, err := attemptState(tx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, dropStopFollowupInTx(tx, key)
	}
	if err != nil {
		return nil, err
	}
	x, err := tx.Execution(id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, dropStopFollowupInTx(tx, key)
	}
	if err != nil {
		return nil, err
	}
	if state.Excluded || !x.State.Terminal() {
		// A running or parked attempt is its observer's and recovery's;
		// only a finished one is followed up.
		return nil, dropStopFollowupInTx(tx, key)
	}
	var f StopFollowup
	err = getJSON(tx, stopFollowupNamespace, key, &f)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if errors.Is(err, storage.ErrNotFound) || f.Epoch != state.Epoch {
		f = StopFollowup{ExecutionID: id, Epoch: state.Epoch, DueAt: now.Add(stopFollowupDelay(0))}
		return nil, putJSON(tx, stopFollowupNamespace, key, f)
	}
	if now.Before(f.DueAt) {
		return nil, nil
	}
	f.Attempts++
	f.DueAt = now.Add(stopFollowupDelay(f.Attempts))
	return &StopDue{ExecutionID: id, Epoch: f.Epoch, Attempt: f.Attempts}, putJSON(tx, stopFollowupNamespace, key, f)
}

// dropStopFollowupInTx removes an attempt's follow-up, writing only when
// there is one, so a pass over running attempts stays a read.
func dropStopFollowupInTx(tx storage.Tx, key string) error {
	if _, err := tx.Get(stopFollowupNamespace, key); errors.Is(err, storage.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return tx.Delete(stopFollowupNamespace, key)
}
