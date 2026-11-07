package runtime

import (
	"context"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// executorBackfillNamespace holds the mark that BackfillExecutors has run.
const executorBackfillNamespace = "core.scheduler.executor_backfill"

// executorBackfillPage is how many Work one backfill transaction takes.
const executorBackfillPage = 200

// BackfillExecutors records the executor of every attempt registered
// before attempts recorded it (domain.Execution.Executor), from the
// attempt's own state, so storage can select Work by its latest attempt's
// executor. It walks every Work's attempts once per store, a page of Work
// per transaction, then marks itself done; an attempt with no state (one
// that never launched) stays without. It reports how many it recorded.
func (r *Router) BackfillExecutors(ctx context.Context) (int, error) {
	const marker = "v1"
	done := false
	if err := r.transact(ctx, nil, func(tx storage.Tx) error {
		var marked struct{}
		e := getJSON(tx, executorBackfillNamespace, marker, &marked)
		if done = e == nil; done || errors.Is(e, storage.ErrNotFound) {
			return nil
		}
		return e
	}); err != nil || done {
		return 0, err
	}
	given := 0
	var after *storage.WorkKey
	for {
		var page []domain.Work
		err := r.transact(ctx, nil, func(tx storage.Tx) error {
			var e error
			if page, e = tx.QueryWorks(storage.WorkQuery{Order: storage.WorkCreated, After: after, Limit: executorBackfillPage}); e != nil {
				return e
			}
			for _, w := range page {
				xs, e := tx.ListExecutions(w.ID)
				if e != nil {
					return e
				}
				for _, x := range xs {
					if x.Executor != "" {
						continue
					}
					st, e := attemptState(tx, x.ID)
					if errors.Is(e, storage.ErrNotFound) || e == nil && st.Executor == "" {
						continue
					}
					if e != nil {
						return e
					}
					if e = tx.SetExecutionExecutor(x.ID, st.Executor); e != nil {
						return e
					}
					given++
				}
			}
			if len(page) < executorBackfillPage {
				return putJSON(tx, executorBackfillNamespace, marker, struct{}{})
			}
			return nil
		})
		if err != nil {
			return given, err
		}
		if len(page) < executorBackfillPage {
			return given, nil
		}
		k := storage.WorkCreated.Key(page[len(page)-1])
		after = &k
	}
}
