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

// MaxReferenceQuery is the most IDs one ReferencedArtifacts call takes: a
// List page.
const MaxReferenceQuery = 1000

// ReferencedIn is ReferencedArtifacts over xs, for a store that answers it
// by reading its attempts: the IDs among ids that one of xs references, as
// one of its Artifacts or EarlierArtifacts, each once and in byte order.
// More than MaxReferenceQuery IDs, or an empty one, is ErrInvalid.
func ReferencedIn(xs []domain.Execution, ids []string) ([]string, error) {
	wanted, err := referenceQuery(ids)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, x := range xs {
		for _, ref := range x.AllArtifacts() {
			if wanted[ref.ID] {
				found[ref.ID] = true
			}
		}
	}
	out := make([]string, 0, len(found))
	for id := range found {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

// HoldersIn is ArtifactHolders over xs, for a store that answers it by
// reading its attempts: those of xs that reference id, as one of their
// Artifacts or EarlierArtifacts, each once and in ID order. An empty ID is
// ErrInvalid.
func HoldersIn(xs []domain.Execution, id string) ([]domain.ID, error) {
	if id == "" {
		return nil, ErrInvalid
	}
	out := []domain.ID{}
	for _, x := range xs {
		if slices.ContainsFunc(x.AllArtifacts(), func(r domain.ArtifactRef) bool { return r.ID == id }) {
			out = append(out, x.ID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// referenceQuery checks a ReferencedArtifacts query and returns its IDs as
// a set.
func referenceQuery(ids []string) (map[string]bool, error) {
	if len(ids) > MaxReferenceQuery {
		return nil, ErrInvalid
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, ErrInvalid
		}
		set[id] = true
	}
	return set, nil
}

// ValidReferenceQuery reports whether ids is a query ReferencedArtifacts
// takes, for a backend that answers it another way.
func ValidReferenceQuery(ids []string) error {
	_, err := referenceQuery(ids)
	return err
}
