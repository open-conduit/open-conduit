package bootstrap

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// legacyKV are the KV namespaces a first-party extension wrote while it ran
// in process, under a name its package may not own (a package owns only the
// namespaces under its own name), mapped to the namespace the package keeps
// the same records in (#428). The claude-code executor's continuity, each
// Work's session, lived under executor.claudecode. Remove an entry once no
// supported release upgrades from a state that holds it.
var legacyKV = map[string]string{"executor.claudecode": "claude-code"}

// adoptLegacyKV moves the records of each legacy namespace into the
// package's own, in one transaction, before any package starts, so an
// upgrade keeps them. A record the new namespace already holds wins: it was
// moved, or written since. Run again, it finds nothing to move.
func adoptLegacyKV(ctx context.Context, store storage.Store, now time.Time) (int, error) {
	moved := 0
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		moved = 0
		olds := make([]string, 0, len(legacyKV))
		for old := range legacyKV {
			olds = append(olds, old)
		}
		sort.Strings(olds)
		for _, old := range olds {
			entries, err := tx.List(old, "")
			if err != nil {
				return err
			}
			for _, e := range entries {
				if _, err := tx.Get(legacyKV[old], e.Key); errors.Is(err, storage.ErrNotFound) {
					if err := tx.Put(legacyKV[old], e.Key, e.Value); err != nil {
						return err
					}
					moved++
				} else if err != nil {
					return err
				}
				if err := tx.Delete(old, e.Key); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return moved, err
}
