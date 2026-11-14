package runtime

import (
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// publishArtifacts makes refs the artifacts of x's outcome, the latest
// turn's result. The ones an earlier turn's result published move to
// EarlierArtifacts rather than going: they stay the attempt's, listed,
// backed up and released by retention with the rest (#592).
func publishArtifacts(x *domain.Execution, refs []domain.ArtifactRef) {
	for _, old := range x.Artifacts {
		if !hasArtifact(refs, old.ID) && !hasArtifact(x.EarlierArtifacts, old.ID) {
			x.EarlierArtifacts = append(x.EarlierArtifacts, old)
		}
	}
	x.Artifacts = append([]domain.ArtifactRef(nil), refs...)
}

// turnArtifacts records refs as published by the attempt's open (or last)
// segment, the turn whose result carried them. A repeated result adds
// nothing.
func (state *AttemptState) turnArtifacts(refs []domain.ArtifactRef) {
	n := len(state.Segments)
	if n == 0 {
		return
	}
	g := &state.Segments[n-1]
	for _, ref := range refs {
		if !hasArtifact(g.Artifacts, ref.ID) {
			g.Artifacts = append(g.Artifacts, ref)
		}
	}
}

func hasArtifact(refs []domain.ArtifactRef, id string) bool {
	for _, r := range refs {
		if r.ID == id {
			return true
		}
	}
	return false
}

// RunArtifact is one artifact an attempt published, with the number of
// the segment (from 1, counting dropped ones) whose turn published it; 0
// when that segment is no longer kept or the attempt predates the record.
type RunArtifact struct {
	domain.ArtifactRef
	Segment int
}

// ArtifactsInTx lists every artifact x published, earlier turns' first,
// each with its segment.
func ArtifactsInTx(tx storage.Tx, x domain.Execution) ([]RunArtifact, error) {
	all := x.AllArtifacts()
	if len(all) == 0 {
		return nil, nil
	}
	state, err := attemptState(tx, x.ID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	out := make([]RunArtifact, 0, len(all))
	for _, ref := range all {
		a := RunArtifact{ArtifactRef: ref}
		for i, g := range state.Segments {
			if hasArtifact(g.Artifacts, ref.ID) {
				a.Segment = state.DroppedSegments + i + 1
			}
		}
		out = append(out, a)
	}
	return out, nil
}
