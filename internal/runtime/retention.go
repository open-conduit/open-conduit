package runtime

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

const retentionNamespace = "core.retention"

// DefaultKeptExecutions is the B05 kept_executions default (per Work).
const DefaultKeptExecutions = 20

// retentionRecord is the durable expiration ledger entry. It is committed
// before any content is deleted, so a crash between phases is resumed rather
// than leaving references to silently missing content.
type retentionRecord struct {
	ExecutionID, WorkID           domain.ID
	Generation, Executor          string
	Artifacts                     []domain.ArtifactRef
	Kept                          int
	ExpiredAt                     time.Time
	ArtifactsReleased, Forgotten  bool
	ArtifactFailures, ForgetError int
}

// RetentionStatus reports whether an execution's retained content expired.
type RetentionStatus struct {
	Expired           bool
	ExpiredAt         time.Time
	ArtifactsReleased bool
	Forgotten         bool
}

// RetentionResult summarizes one sweep.
type RetentionResult struct {
	Expired  []domain.ID
	Released []domain.ID
	Pending  int
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
	return RetentionStatus{Expired: true, ExpiredAt: r.ExpiredAt, ArtifactsReleased: r.ArtifactsReleased, Forgotten: r.Forgotten}, nil
}

// SweepRetention applies the single kept-executions policy shared by every
// executor: per Work, the newest KeptExecutions attempts keep their artifact
// content and executor-private state; older terminal attempts whose resource
// exclusion is confirmed expire. Coordination history (records, outcomes,
// usage and artifact metadata) is never deleted. Expiration is committed
// first; content deletion and executor Forget follow outside the transaction
// and are retried by later sweeps until both are released.
func (s *Scheduler) SweepRetention(ctx context.Context, limit int) (result RetentionResult, err error) {
	if limit < 1 || limit > 1000 {
		return result, storage.ErrInvalid
	}
	kept := s.options.KeptExecutions
	now := s.now()
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		xs, e := tx.ListExecutions("")
		if e != nil {
			return e
		}
		byWork := map[domain.ID][]domain.Execution{}
		for _, x := range xs {
			byWork[x.WorkID] = append(byWork[x.WorkID], x)
		}
		works := make([]domain.ID, 0, len(byWork))
		for id := range byWork {
			works = append(works, id)
		}
		sort.Slice(works, func(i, j int) bool { return works[i] < works[j] })
		for _, work := range works {
			attempts := byWork[work]
			sort.Slice(attempts, func(i, j int) bool { return attempts[i].Attempt > attempts[j].Attempt })
			for i, x := range attempts {
				if i < kept || !x.State.Terminal() || len(result.Expired) >= limit {
					continue
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
					continue // Uncertain resource hold: never release private state.
				}
				record := retentionRecord{ExecutionID: x.ID, WorkID: x.WorkID, Generation: state.Generation, Executor: state.Executor, Artifacts: append([]domain.ArtifactRef(nil), x.Artifacts...), Kept: kept, ExpiredAt: now}
				if e = putJSON(tx, retentionNamespace, retentionKey(x.ID), record); e != nil {
					return e
				}
				result.Expired = append(result.Expired, x.ID)
			}
		}
		return nil
	})
	if err != nil {
		return RetentionResult{}, err
	}
	var pending []retentionRecord
	if err = s.transact(ctx, nil, func(tx storage.Tx) error {
		rows, e := tx.List(retentionNamespace, "")
		if e != nil {
			return e
		}
		for _, row := range rows {
			var r retentionRecord
			if e = getJSON(tx, retentionNamespace, row.Key, &r); e != nil {
				return e
			}
			if !r.ArtifactsReleased || !r.Forgotten {
				pending = append(pending, r)
			}
		}
		return nil
	}); err != nil {
		return result, err
	}
	for _, r := range pending {
		if len(result.Released) >= limit {
			result.Pending++
			continue
		}
		artifactsDone := r.ArtifactsReleased
		if !artifactsDone {
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
			if !artifactsDone {
				current.ArtifactFailures++
			}
			if !forgotten {
				current.ForgetError++
			}
			return putJSON(tx, retentionNamespace, key, current)
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
