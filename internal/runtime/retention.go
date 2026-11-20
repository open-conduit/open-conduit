package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

const retentionNamespace = "core.retention"

// retentionDoneNamespace marks a finished Work all of whose attempts have
// expired. A finished Work can't run again, so later sweeps skip it without
// reading it or its ledger entries, and the sweep's cost stays with the Work
// that still have content to release.
const retentionDoneNamespace = "core.retention.done"

// retentionCountedNamespace marks a finished Work, not yet old enough to
// expire whole, whose attempts beyond kept_executions have all expired. The
// value is that kept_executions: a lower setting makes the sweep read the
// Work again.
const retentionCountedNamespace = "core.retention.counted"

// retentionPendingNamespace lists the ledger entries whose content is not
// yet fully released, keyed like the ledger, so a sweep reads those and not
// every expiration ever made. retentionIndexKey in retentionIndexNamespace
// records that a ledger written before the list existed has been indexed.
const (
	retentionPendingNamespace = "core.retention.pending"
	retentionIndexNamespace   = "core.retention.index"
	retentionIndexKey         = "pending"
)

// retentionCursorNamespace keeps the sweep's cursors (retentionCursor), so
// a sweep reads the Work that can have something to expire, not every Work
// ever made.
const (
	retentionCursorNamespace = "core.retention.cursor"
	retentionCursorKey       = "works"
)

// retentionHeldNamespace lists the finished Work a sweep could not settle
// because an attempt's resource hold is uncertain, keyed like the other
// markers. The cursors pass it, and each sweep reads it from this list
// until it settles, so one stuck attempt costs one read a sweep, not a
// re-read of everything after it.
const retentionHeldNamespace = "core.retention.held"

// retentionCursor is where the sweep reads finished Work from. Changed is
// the change cursor (storage.NextChangeCursor) of Work that finished since
// the last sweeps, which a sweep expires by count; Aged is the last
// finished Work, in order of its last change (storage.WorkUpdated), that
// the sweep has expired whole once older than kept_finished_days (nil
// before any). Neither passes finished Work a sweep left unsettled. Kept is
// the kept_executions they were read with.
type retentionCursor struct {
	Changed time.Time
	Aged    *storage.WorkKey
	Kept    int
}

// retentionAgedPage is the most finished Work one read of the aged list
// takes. The first read takes one, and each after it twice as many, so a
// sweep with nothing aged reads one record.
const retentionAgedPage = 256

// retentionCandidates is the Work a sweep looks at, in ID order: the live
// Work and the finished Work changed since cur.Changed (changed), the
// finished Work last changed after cur.Aged and by finishedBefore (aged,
// in that order, at most limit of them), and the held Work (held, by ID).
// Each is read through the store's indexes or by ID, so the sweep's cost
// follows them, not every Work ever made.
func retentionCandidates(tx storage.Tx, cur retentionCursor, finishedBefore time.Time, limit int) (works, changed, aged []domain.Work, held map[domain.ID]bool, err error) {
	if changed, err = storage.LiveOrChanged(tx, storage.WorkQuery{}, cur.Changed); err != nil {
		return nil, nil, nil, nil, err
	}
	rows, err := tx.List(retentionHeldNamespace, "")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	held = make(map[domain.ID]bool, len(rows))
	var heldWorks []domain.Work
	for _, row := range rows {
		var id domain.ID
		if err = json.Unmarshal(row.Value, &id); err != nil {
			return nil, nil, nil, nil, err
		}
		w, err := tx.Work(id)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		held[id] = true
		heldWorks = append(heldWorks, w)
	}
	q := storage.WorkQuery{States: []domain.WorkState{domain.WorkCompleted, domain.WorkFailed, domain.WorkCancelled}, Order: storage.WorkUpdated, After: cur.Aged, Limit: 1}
	// At most limit aged Work a sweep: it can expire no more than that, and
	// the cursor carries on from the last one next sweep.
	for len(aged) < limit {
		q.Limit = min(q.Limit, limit-len(aged))
		page, err := tx.QueryWorks(q)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		for _, w := range page {
			if w.UpdatedAt.After(finishedBefore) {
				page = nil
				break
			}
			aged = append(aged, w)
		}
		if len(page) < q.Limit {
			break
		}
		k := q.Order.Key(page[len(page)-1])
		q.After, q.Limit = &k, min(2*q.Limit, retentionAgedPage)
	}
	seen := map[domain.ID]bool{}
	for _, w := range append(append(slices.Clone(changed), aged...), heldWorks...) {
		if !seen[w.ID] {
			seen[w.ID] = true
			works = append(works, w)
		}
	}
	sort.Slice(works, func(i, j int) bool { return works[i].ID < works[j].ID })
	return works, changed, aged, held, nil
}

// nextAgedCursor is the aged cursor after a sweep read aged after since:
// the last of them before the first it left unsettled. Finished Work does
// not change again, so the cursor needs no lag behind it.
func nextAgedCursor(since *storage.WorkKey, aged []domain.Work, unsettled map[domain.ID]bool) *storage.WorkKey {
	next := since
	for _, w := range aged {
		if unsettled[w.ID] {
			break
		}
		k := storage.WorkUpdated.Key(w)
		next = &k
	}
	return next
}

// hasKey says whether namespace holds key.
func hasKey(tx storage.Tx, namespace, key string) (bool, error) {
	_, err := tx.Get(namespace, key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// retentionHoldNamespace holds the retention holds, each keyed by its ID
// with its expiry as the value. While one is live the sweep releases no
// content (HoldRetention).
const retentionHoldNamespace = "core.retention.hold"

type retentionHold struct{ Until time.Time }

// HoldRetention commits, or renews, the retention hold id until until, in
// tx. While a hold is live, SweepRetention deletes no artifact content: it
// records expirations as ever and leaves their content pending until no
// hold is live. A backup takes one in its own transaction before it
// snapshots, renews it while it copies and releases it when done. That is
// enough under serializable transactions: a release whose pending list was
// read before the hold committed acts only on expirations committed before
// the hold, and so before the snapshot, which then records those attempts
// as expired and has no need of their content; one read after it sees the
// hold. A hold its holder never releases (it crashed) delays the sweep
// only until until. An empty ID, or a zero until, is ErrInvalid.
func HoldRetention(tx storage.Tx, id string, until time.Time) error {
	if id == "" || until.IsZero() {
		return storage.ErrInvalid
	}
	return putJSON(tx, retentionHoldNamespace, id, retentionHold{Until: until.UTC()})
}

// ReleaseRetention removes the retention hold id, in tx. An absent hold is
// nil.
func ReleaseRetention(tx storage.Tx, id string) error {
	if id == "" {
		return storage.ErrInvalid
	}
	return tx.Delete(retentionHoldNamespace, id)
}

// retentionHeld reports whether a retention hold is live at now, and
// removes the ones that lapsed.
func retentionHeld(tx storage.Tx, now time.Time) (bool, error) {
	rows, err := tx.List(retentionHoldNamespace, "")
	if err != nil {
		return false, err
	}
	held := false
	for _, row := range rows {
		var h retentionHold
		if err = json.Unmarshal(row.Value, &h); err != nil {
			return false, err
		}
		if now.Before(h.Until) {
			held = true
			continue
		}
		if err = tx.Delete(retentionHoldNamespace, row.Key); err != nil {
			return false, err
		}
	}
	return held, nil
}

// indexRetentionPending adds every unreleased ledger entry to the pending
// list, once per store.
func indexRetentionPending(tx storage.Tx) error {
	if _, err := tx.Get(retentionIndexNamespace, retentionIndexKey); err == nil || !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	rows, err := tx.List(retentionNamespace, "")
	if err != nil {
		return err
	}
	for _, row := range rows {
		var r retentionRecord
		if err = json.Unmarshal(row.Value, &r); err != nil {
			return err
		}
		if !r.ArtifactsReleased || !r.Forgotten {
			if err = putJSON(tx, retentionPendingNamespace, row.Key, true); err != nil {
				return err
			}
		}
	}
	return putJSON(tx, retentionIndexNamespace, retentionIndexKey, true)
}

// pendingRetention lists the ledger entries whose content is not yet fully
// released: from the pending list once it is indexed, else from the whole
// ledger.
func pendingRetention(tx storage.Tx) ([]retentionRecord, error) {
	ns := retentionPendingNamespace
	if _, err := tx.Get(retentionIndexNamespace, retentionIndexKey); errors.Is(err, storage.ErrNotFound) {
		ns = retentionNamespace
	} else if err != nil {
		return nil, err
	}
	rows, err := tx.List(ns, "")
	if err != nil {
		return nil, err
	}
	var pending []retentionRecord
	for _, row := range rows {
		var r retentionRecord
		if err = getJSON(tx, retentionNamespace, row.Key, &r); errors.Is(err, storage.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		if !r.ArtifactsReleased || !r.Forgotten {
			pending = append(pending, r)
		}
	}
	return pending, nil
}

// DefaultKeptExecutions is the B05 kept_executions default (per Work).
const DefaultKeptExecutions = 20

// DefaultKeptFinished is the kept_finished_days default: a finished Work's
// attempts keep their content for 14 days after the Work last changed.
const DefaultKeptFinished = 14 * 24 * time.Hour

// Retention reasons say which rule expired an attempt.
const (
	// RetentionCount: newer attempts of the same Work pushed it past kept_executions.
	RetentionCount = "kept_executions"
	// RetentionFinished: its Work finished longer ago than kept_finished_days.
	RetentionFinished = "kept_finished_days"
)

// retentionRecord is the durable expiration ledger entry. It is committed
// before any content is deleted, so a crash between phases is resumed rather
// than leaving references to silently missing content.
type retentionRecord struct {
	ExecutionID, WorkID           domain.ID
	Generation, Executor          string
	Artifacts                     []domain.ArtifactRef
	Kept                          int
	Reason                        string `json:",omitempty"`
	ExpiredAt                     time.Time
	ArtifactsReleased, Forgotten  bool
	ArtifactFailures, ForgetError int
}

// RetentionStatus reports whether an execution's retained content expired.
type RetentionStatus struct {
	Expired   bool
	ExpiredAt time.Time
	// Reason is RetentionCount or RetentionFinished; records written before
	// age-based expiry have none and were expired by count.
	Reason            string `json:",omitempty"`
	ArtifactsReleased bool
	Forgotten         bool
}

// RetentionResult summarizes one sweep.
type RetentionResult struct {
	Expired  []domain.ID
	Released []domain.ID
	Pending  int
	// Held is set when a retention hold was live, so the sweep released no
	// content (HoldRetention).
	Held bool
}

func retentionKey(id domain.ID) string { return identity(string(id)) }

// RetentionInTx reads the expiration ledger for one execution.
func RetentionInTx(tx storage.Tx, id domain.ID) (RetentionStatus, error) {
	var r retentionRecord
	err := getJSON(tx, retentionNamespace, retentionKey(id), &r)
	if errors.Is(err, storage.ErrNotFound) {
		return RetentionStatus{}, nil
	}
	if err != nil {
		return RetentionStatus{}, err
	}
	return RetentionStatus{Expired: true, ExpiredAt: r.ExpiredAt, Reason: r.Reason, ArtifactsReleased: r.ArtifactsReleased, Forgotten: r.Forgotten}, nil
}

// SweepRetention applies the single retention policy shared by every
// executor: per Work, the newest KeptExecutions attempts keep their artifact
// content and executor-private state; older terminal attempts whose resource
// exclusion is confirmed expire. Once a Work has finished and not changed for
// KeptFinished, all its attempts expire the same way. Coordination history
// (records, outcomes, usage and artifact metadata) is never deleted. Expiration is committed
// first; content deletion and executor Forget follow outside the transaction
// and are retried by later sweeps until both are released.
func (s *Scheduler) SweepRetention(ctx context.Context, limit int) (result RetentionResult, err error) {
	if limit < 1 || limit > 1000 {
		return result, storage.ErrInvalid
	}
	kept := s.options.KeptExecutions
	now := s.now()
	finishedBefore := now.Add(-s.options.KeptFinished)
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		result.Expired = nil
		if e := indexRetentionPending(tx); e != nil {
			return e
		}
		var cur retentionCursor
		if e := getJSON(tx, retentionCursorNamespace, retentionCursorKey, &cur); e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		if kept < cur.Kept {
			// A lower kept_executions has more to expire on finished Work
			// already counted: read it all again, once.
			cur.Changed = time.Time{}
		}
		works, changed, aged, held, e := retentionCandidates(tx, cur, finishedBefore, limit)
		if e != nil {
			return e
		}
		// unsettled is the finished Work this sweep's limit left work on;
		// the cursors stay before it, so the next sweep reads it again.
		// Finished Work an uncertain hold leaves goes on the held list
		// instead, which the cursors pass.
		unsettled := map[domain.ID]bool{}
		hold := func(w domain.Work, on bool) error {
			switch {
			case on && !held[w.ID]:
				return putJSON(tx, retentionHeldNamespace, retentionKey(w.ID), w.ID)
			case !on && held[w.ID]:
				return tx.Delete(retentionHeldNamespace, retentionKey(w.ID))
			}
			return nil
		}
		for i, w := range works {
			work := w.ID
			if len(result.Expired) >= limit {
				for _, rest := range works[i:] {
					unsettled[rest.ID] = true
				}
				break
			}
			if done, e := hasKey(tx, retentionDoneNamespace, retentionKey(work)); e != nil {
				return e
			} else if done {
				if e := hold(w, false); e != nil {
					return e
				}
				continue
			}
			finishedAge := w.State.Terminal() && !w.UpdatedAt.After(finishedBefore)
			var n int
			if e := getJSON(tx, retentionCountedNamespace, retentionKey(work), &n); e == nil && w.State.Terminal() && !finishedAge && n <= kept {
				if e := hold(w, false); e != nil {
					return e
				}
				continue // Nothing more to expire until it reaches kept_finished_days.
			} else if e != nil && !errors.Is(e, storage.ErrNotFound) {
				return e
			}
			attempts, e := tx.ListExecutions(work)
			if e != nil {
				return e
			}
			if len(attempts) == 0 {
				if e := hold(w, false); e != nil {
					return e
				}
				continue
			}
			sort.Slice(attempts, func(i, j int) bool { return attempts[i].Attempt > attempts[j].Attempt })
			// A Work whose newest attempt is still live has not finished.
			finished := attempts[0].State.Terminal() && finishedAge
			// allExpired stays true while every attempt has a ledger entry,
			// countSettled while every attempt beyond kept has one.
			allExpired, countSettled := finished, true
			for i, x := range attempts {
				if i < kept && !finished {
					allExpired = false
					continue
				}
				if !x.State.Terminal() || len(result.Expired) >= limit {
					allExpired, countSettled = false, false
					continue
				}
				reason := RetentionCount
				if i < kept {
					reason = RetentionFinished
				}
				if _, e := tx.Get(retentionNamespace, retentionKey(x.ID)); e == nil {
					continue
				} else if !errors.Is(e, storage.ErrNotFound) {
					return e
				}
				state, e := attemptState(tx, x.ID)
				if e != nil && !errors.Is(e, storage.ErrNotFound) {
					return e
				}
				if e == nil && !state.Excluded {
					allExpired, countSettled = false, false
					continue // Uncertain resource hold: never release private state.
				}
				record := retentionRecord{ExecutionID: x.ID, WorkID: x.WorkID, Generation: state.Generation, Executor: state.Executor, Artifacts: append([]domain.ArtifactRef(nil), x.AllArtifacts()...), Kept: kept, Reason: reason, ExpiredAt: now}
				if e = putJSON(tx, retentionNamespace, retentionKey(x.ID), record); e != nil {
					return e
				}
				if e = putJSON(tx, retentionPendingNamespace, retentionKey(x.ID), true); e != nil {
					return e
				}
				result.Expired = append(result.Expired, x.ID)
			}
			counted := w.State.Terminal() && !finished && countSettled
			switch {
			case allExpired:
				if e := tx.Put(retentionDoneNamespace, retentionKey(work), []byte("{}")); e != nil {
					return e
				}
				if e := tx.Delete(retentionCountedNamespace, retentionKey(work)); e != nil {
					return e
				}
			case counted:
				// A finished Work gets no new attempts, so until it is old
				// enough to expire whole, later sweeps need not read it.
				if e := putJSON(tx, retentionCountedNamespace, retentionKey(work), kept); e != nil {
					return e
				}
			}
			if !w.State.Terminal() {
				continue // Live Work is read every sweep.
			}
			left := !allExpired && !counted
			if left && len(result.Expired) >= limit {
				// The limit stopped it: the cursors wait for it.
				unsettled[work] = true
				continue
			}
			// What is left is held by an uncertain resource: the held list
			// reads it again until it settles.
			if e := hold(w, left); e != nil {
				return e
			}
		}
		cur.Changed = storage.NextChangeCursor(cur.Changed, now, changed, func(w domain.Work) bool { return unsettled[w.ID] })
		cur.Aged = nextAgedCursor(cur.Aged, aged, unsettled)
		cur.Kept = kept
		return putJSON(tx, retentionCursorNamespace, retentionCursorKey, cur)
	})
	if err != nil {
		return RetentionResult{}, err
	}
	// The holds are read in the transaction that reads what to release
	// (HoldRetention says why that is enough).
	var pending []retentionRecord
	if err = s.transact(ctx, nil, func(tx storage.Tx) (e error) {
		if result.Held, e = retentionHeld(tx, s.now()); e != nil {
			return e
		}
		pending, e = pendingRetention(tx)
		return e
	}); err != nil {
		return result, err
	}
	for _, r := range pending {
		if len(result.Released) >= limit {
			result.Pending++
			continue
		}
		artifactsDone := r.ArtifactsReleased
		// While a hold is live, content waits; private state does not, as
		// a backup copies none of it.
		if !artifactsDone && !result.Held {
			artifactsDone = true
			for _, ref := range r.Artifacts {
				if s.options.Artifacts == nil {
					break
				}
				if e := s.options.Artifacts.Delete(ctx, ref); e != nil && !errors.Is(e, storage.ErrNotFound) {
					artifactsDone = false
				}
			}
		}
		forgotten := r.Forgotten
		if !forgotten {
			forgotten = true
			if executor, e := s.executorFor(AttemptState{Generation: r.Generation, Executor: r.Executor}); e == nil {
				if f, ok := executor.(extension.ForgetExecutor); ok {
					if e = f.Forget(ctx, r.ExecutionID); e != nil {
						forgotten = false
					}
				}
			}
		}
		key := retentionKey(r.ExecutionID)
		if e := s.transact(ctx, nil, func(tx storage.Tx) error {
			var current retentionRecord
			if e := getJSON(tx, retentionNamespace, key, &current); e != nil {
				return e
			}
			current.ArtifactsReleased = artifactsDone
			current.Forgotten = forgotten
			if !artifactsDone && !result.Held {
				current.ArtifactFailures++
			}
			if !forgotten {
				current.ForgetError++
			}
			if e := putJSON(tx, retentionNamespace, key, current); e != nil {
				return e
			}
			if artifactsDone && forgotten {
				return tx.Delete(retentionPendingNamespace, key)
			}
			return nil
		}); e != nil {
			return result, e
		}
		if artifactsDone && forgotten {
			result.Released = append(result.Released, r.ExecutionID)
		} else {
			result.Pending++
		}
	}
	return result, nil
}
