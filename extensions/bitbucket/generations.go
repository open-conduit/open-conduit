package bitbucket

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/storage"
)

// StoreGenerations resolves a tracker issue's generation from the Work the
// tracker keeps in the scope, through the public store, so the tracker can
// run in another process (#428). A tracker opens each reopened generation's
// Work under issue/<key>/generation/reopen-<unix> in its namespace, so the
// generation current at a time is the latest such reopen at or before it.
// One read of the scope's Work per call.
func StoreGenerations(store storage.Store, scope domain.ID, ns string, now func() time.Time) Generations {
	return func(ctx context.Context, key string, at time.Time) (string, error) {
		var works []domain.Work
		err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
			var err error
			works, err = tx.ListWorks()
			return err
		})
		if err != nil {
			return "", err
		}
		return GenerationFromWorks(works, scope, ns, key, at), nil
	}
}

// GenerationFromWorks is the generation of tracker issue key current at a
// time among works: the latest reopen generation, at or before at, that a
// Work of scope is keyed by under the tracker namespace ns. It is "" (the
// first generation) when there is none.
func GenerationFromWorks(works []domain.Work, scope domain.ID, ns, key string, at time.Time) string {
	best, bestAt := "", int64(-1)
	for _, w := range works {
		if w.ScopeID != scope {
			continue
		}
		for _, k := range w.CorrelationKeys {
			if k.Source != ns {
				continue
			}
			issue, gen, ok := tracker.ParseIssueKey(k.ExternalID)
			if !ok || issue != key || gen == "" {
				continue
			}
			unix, err := strconv.ParseInt(strings.TrimPrefix(gen, "reopen-"), 10, 64)
			if err != nil || unix > at.Unix() || unix <= bestAt {
				continue
			}
			best, bestAt = gen, unix
		}
	}
	return best
}
