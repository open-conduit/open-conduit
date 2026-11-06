package runtime

import (
	"context"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// eventScopeBackfillPage is how many unscoped events one backfill
// transaction takes.
const eventScopeBackfillPage = 500

// BackfillEventScopes records the scope of every accepted Event whose record
// has none: those accepted before scopes were recorded, and any storage
// accepted on its own (FireTimer outside the runtime). Each takes the scope
// it was accepted into, as eventScopeInTx works it out. An Event whose scope
// can't be worked out stays unscoped, and readers keep resolving it as
// before. It walks only the unscoped events (ScopeEvents of ""), a page per
// transaction, so once it has run a later start costs one empty read. It
// reports how many events it gave a scope.
func (r *Router) BackfillEventScopes(ctx context.Context) (int, error) {
	given := 0
	for after := uint64(0); ; {
		var page []domain.AcceptedEvent
		err := r.transact(ctx, nil, func(tx storage.Tx) error {
			var e error
			page, e = tx.ScopeEvents([]domain.ID{""}, after, eventScopeBackfillPage)
			if e != nil {
				return e
			}
			for _, a := range page {
				scope, e := eventScopeInTx(tx, a)
				if e != nil || scope == "" {
					if e != nil && !unresolvable(e) {
						return e
					}
					continue
				}
				switch e = tx.SetEventScope(a.Event.ID, scope); {
				case e == nil:
					given++
				case !errors.Is(e, storage.ErrNotFound):
					return e
				}
			}
			return nil
		})
		if err != nil {
			return given, err
		}
		if len(page) < eventScopeBackfillPage {
			return given, nil
		}
		after = page[len(page)-1].Sequence
	}
}

// unresolvable is an error that says an Event's identity names no scope,
// rather than that storage failed.
func unresolvable(err error) bool {
	return errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalid) || errors.Is(err, storage.ErrConflict)
}
