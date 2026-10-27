package human

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// auditWindow is how long after an ask is answered later answers are
// still looked for and recorded. Only answers racing the first can land
// after it: once the record is answered, the action refuses.
const auditWindow = time.Hour

// SyncResult counts what a pass changed.
type SyncResult struct {
	Answered, Expired, Withdrawn, Late int
}

// Sync settles ask records against what happened: the first accepted
// answer (lowest sequence, the one that woke the asker) is recorded as the
// answer and the rest as late; an ask past its deadline expires; an ask
// whose asker stopped waiting is withdrawn. It is idempotent and
// restart-safe: everything it reads is durable.
func Sync(ctx context.Context, store storage.Store, now time.Time) (SyncResult, error) {
	var res SyncResult
	var ids []string
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		rows, err := tx.List(Namespace, "ask/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var a Ask
			if json.Unmarshal(row.Value, &a) != nil {
				continue
			}
			if a.State == StateOpen || (a.State == StateAnswered && now.Sub(a.ClosedAt) < auditWindow) {
				ids = append(ids, a.ID)
			}
		}
		return nil
	}); err != nil {
		return res, err
	}
	for _, id := range ids {
		if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			return settle(tx, id, now, &res)
		}); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Settle is Sync for one ask (the renderer settles the ask an answer is
// for before saying whether it counted).
func Settle(ctx context.Context, store storage.Store, id string, now time.Time) (Ask, error) {
	var a Ask
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := settle(tx, id, now, &SyncResult{}); err != nil {
			return err
		}
		var err error
		a, err = getAsk(tx, id)
		return err
	})
	return a, err
}

func settle(tx storage.Tx, id string, now time.Time, res *SyncResult) error {
	a, err := getAsk(tx, id)
	if err != nil {
		return err
	}
	if a.State != StateOpen && a.State != StateAnswered {
		return nil
	}
	given, through, err := answers(tx, a, a.Checked)
	if err != nil {
		return err
	}
	a.Checked = through
	switch {
	case a.State == StateAnswered:
		a.Late = append(a.Late, given...)
		res.Late += len(given)
	case len(given) > 0:
		first := given[0]
		a.State, a.Answer, a.ClosedAt = StateAnswered, &first, now
		a.Late = append(a.Late, given[1:]...)
		res.Answered++
		res.Late += len(given) - 1
	case a.expired(now):
		a.State, a.ClosedAt = StateExpired, now
		res.Expired++
	default:
		gone, err := withdrawn(tx, a)
		if err != nil {
			return err
		}
		if gone {
			a.State, a.ClosedAt = StateWithdrawn, now
			res.Withdrawn++
		}
	}
	return putAsk(tx, a)
}

// withdrawn reports whether the asker no longer waits for the ask: its
// Work ended or is gone, its wait was woken without an answer, or it has
// asked something since.
func withdrawn(tx storage.Tx, a Ask) (bool, error) {
	w, err := tx.Work(a.Work)
	if errors.Is(err, storage.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if w.State.Terminal() {
		return true, nil
	}
	if latest, err := latestAsk(tx, a.Work); err == nil && latest.ID != a.ID {
		return true, nil
	}
	wait, ok, err := askWait(tx, a)
	return ok && wait.State != domain.WaitActive, err
}
