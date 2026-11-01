package extension

import (
	"context"
	"encoding/json"

	"github.com/open-conduit/open-conduit/domain"
)

// Candidate is a configuration an extension's Work proposes, and the
// changes it is made of, for the operator to review (issue #242). Core
// draws the review: each change's before and after, approve and reject,
// and Apply. The decisions are the extension's own actions on the Work,
// named here, so each one is still a core.action Event it reads back; Apply
// is core's alone.
//
// Handle is the host's handle on the config file text with every approved
// change made (inproc.ConfigHost), BaseGeneration the generation it is
// based on and Hash its hash. The host serves the text to the operator,
// who applies it with POST /config/apply (config_text, expected_generation,
// candidate_hash); nothing else can. Handle is empty while nothing is
// approved: there is nothing to apply yet.
type Candidate struct {
	Handle         string
	BaseGeneration string
	Hash           string
	// Changes are every change proposed on the Work, in proposal order.
	Changes []CandidateChange
	// ApproveAll and Validate are the action IDs that approve every open
	// change and validate the approved ones, when offered now.
	ApproveAll, Validate string
}

// Change states core knows: a change waiting on the operator, and one
// approved and waiting to be applied. Others (rejected, withdrawn, applied)
// are shown as they are.
const (
	ChangeProposed = "proposed"
	ChangeApproved = "approved"
)

// CandidateChange is one change: an edit at a JSON pointer of the config
// file, with its reason, the redacted value before and after (nil when
// there is none: an add, a remove) and its problems. Approve and Reject
// are the action IDs that decide it, when offered now; Reject takes an
// optional reason as input.
type CandidateChange struct {
	ID, Op, Pointer, State, Why string
	Before, After               json.RawMessage
	Problems                    []string
	Approve, Reject             string
}

// ConfigCandidate is implemented by an extension whose Work proposes
// configuration (setup's change sets). GET /work/{id}/config-candidate, an
// operator-class read, serves it. A Work with no changes is
// storage.ErrNotFound.
type ConfigCandidate interface {
	ConfigCandidate(ctx context.Context, w domain.Work) (Candidate, error)
}
