package extension

import (
	"context"

	"github.com/open-conduit/open-conduit/domain"
)

// Candidate is a configuration an extension's Work proposes: the host's
// handle on its config file text (inproc.ConfigHost), the generation it is
// based on and its hash. The host serves the text to the operator, who
// applies it with POST /config/apply (config_text, expected_generation,
// candidate_hash); nothing else can.
type Candidate struct {
	Handle         string
	BaseGeneration string
	Hash           string
}

// ConfigCandidate is implemented by an extension whose Work proposes
// configuration (setup's change sets). GET /work/{id}/config-candidate, an
// operator-class read, serves it. A Work with nothing to apply is
// storage.ErrNotFound.
type ConfigCandidate interface {
	ConfigCandidate(ctx context.Context, w domain.Work) (Candidate, error)
}
