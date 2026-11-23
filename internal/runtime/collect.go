package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// collectPage is how many stored artifacts one collection step lists and
// asks coordination about.
const collectPage = 256

// CollectResult summarizes one collection.
type CollectResult struct {
	// Listed counts the stored artifacts collection read, and Deleted
	// those it removed.
	Listed, Deleted int
}

// CollectArtifacts deletes the content in artifacts that no attempt
// references and that was published before before: what an interrupted
// publication left, or content published and never attached. Executors
// attach what they publish in the same result, so a grace far longer than
// the publish-to-attach window keeps content still on its way to its
// record. It pages the store with List and asks coordination which IDs of
// each page are referenced (Tx.ReferencedArtifacts), one View a page, so
// its cost per step follows the page, never the history retained, and no
// backend needs a lock. What it deletes it first claims (claimOrphans), so
// a late result can't attach content on its way out. Content referenced by an expired attempt stays
// until retention releases it.
func (s *Scheduler) CollectArtifacts(ctx context.Context, artifacts storage.ArtifactStore, before time.Time) (CollectResult, error) {
	var result CollectResult
	if artifacts == nil || before.IsZero() {
		return result, storage.ErrInvalid
	}
	after := ""
	for {
		page, err := artifacts.List(ctx, after, collectPage)
		if err != nil || len(page) == 0 {
			return result, err
		}
		after = page[len(page)-1].Ref.ID
		result.Listed += len(page)
		var aged []string
		for _, item := range page {
			if item.PublishedAt.Before(before) {
				aged = append(aged, item.Ref.ID)
			}
		}
		if len(aged) == 0 {
			continue
		}
		var referenced []string
		if err = storage.View(ctx, s.store, s.now(), func(tx storage.Tx) (e error) {
			referenced, e = tx.ReferencedArtifacts(aged)
			return e
		}); err != nil {
			return result, err
		}
		live := make(map[string]bool, len(referenced))
		for _, id := range referenced {
			live[id] = true
		}
		var orphans []storage.ArtifactRef
		for _, item := range page {
			if !live[item.Ref.ID] && item.PublishedAt.Before(before) {
				orphans = append(orphans, item.Ref)
			}
		}
		if len(orphans) == 0 {
			continue
		}
		// The View above spares the common case a write. The claim checks
		// again, and a result naming a claimed ID doesn't attach it.
		free, err := s.claimOrphans(ctx, orphans)
		if err != nil {
			return result, err
		}
		for _, ref := range free {
			if err = s.deleteClaimed(ctx, artifacts, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return result, err
			}
			result.Deleted++
		}
	}
}
