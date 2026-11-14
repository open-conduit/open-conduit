package storage

import (
	"slices"

	"github.com/open-conduit/open-conduit/domain"
)

// PublishedArtifacts is x with refs published as its latest turn's
// artifacts, the rule PublishFinishedArtifacts applies: refs become
// Artifacts, and the ones x held that refs does not repeat move to
// EarlierArtifacts, so none is lost. changed is false when x already holds
// exactly refs. Refs that are empty, hold an empty ID or a negative size,
// or repeat an ID, are ErrInvalid.
func PublishedArtifacts(x domain.Execution, refs []domain.ArtifactRef) (out domain.Execution, changed bool, err error) {
	if len(refs) == 0 {
		return x, false, ErrInvalid
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if r.ID == "" || r.Size < 0 || seen[r.ID] {
			return x, false, ErrInvalid
		}
		seen[r.ID] = true
	}
	if slices.Equal(x.Artifacts, refs) {
		return x, false, nil
	}
	held := func(list []domain.ArtifactRef, id string) bool {
		return slices.ContainsFunc(list, func(r domain.ArtifactRef) bool { return r.ID == id })
	}
	earlier := slices.Clone(x.EarlierArtifacts)
	for _, old := range x.Artifacts {
		if !seen[old.ID] && !held(earlier, old.ID) {
			earlier = append(earlier, old)
		}
	}
	x.EarlierArtifacts = earlier
	x.Artifacts = slices.Clone(refs)
	return x, true, nil
}
