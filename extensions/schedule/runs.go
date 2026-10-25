package schedule

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// Run-ended Events (05-02). Each finished run of a job is announced by one
// Event on the job's own key, typed by how it ended, so another Work can wait
// for "the next run of the backup job to end" (v1 `job:<name>` waits) through
// an ordinary B04 dependency subscription. The host derives them from durable
// execution outcomes after a per-job checkpoint, accepting each as a derived
// Event of the run's trigger in one transaction with the checkpoint, so a
// crash only repeats a deduplicated delivery. No route delivers them: the
// schedule route never matches these types.
const (
	TypeRunCompleted = "schedule.run.completed"
	TypeRunFailed    = "schedule.run.failed"
	TypeRunCancelled = "schedule.run.cancelled"
	TypeRunHandoff   = "schedule.run.handoff"
	// SchemaRun is the payload schema of a run-ended Event.
	SchemaRun = "schedule.run.v1"
)

// RunTypes are every run-ended type: a wait on a job takes any of them.
var RunTypes = []string{TypeRunCompleted, TypeRunFailed, TypeRunCancelled, TypeRunHandoff}

// RunPayload is a run-ended Event's payload.
type RunPayload struct {
	Job       string `json:"job"`
	Work      string `json:"work"`
	Execution string `json:"execution"`
	Attempt   uint32 `json:"attempt"`
	Outcome   string `json:"outcome"`
	Summary   string `json:"summary,omitempty"`
}

// Ended is a run-ended Event to accept in scope, derived from Parent (the
// run's trigger).
type Ended struct {
	Scope, Parent domain.ID
	Event         domain.Event
	// Work and Attempt advance the job's checkpoint once accepted.
	Work    domain.ID
	Attempt uint32
}

const runsNamespace = "schedule.runs"

func runType(s domain.ExecutionState) (string, bool) {
	switch s {
	case domain.ExecutionCompleted:
		return TypeRunCompleted, true
	case domain.ExecutionFailed:
		return TypeRunFailed, true
	case domain.ExecutionCancelled:
		return TypeRunCancelled, true
	case domain.ExecutionHandoff:
		return TypeRunHandoff, true
	}
	return "", false
}

func checkpointKey(work domain.ID) string { return "ended/" + digest(string(work))[:40] }

// EndedRunsInTx lists finished job runs not yet announced, in attempt order
// per job, at most limit. An attempt that ended waiting is passed over: the
// job has not finished, and its next attempt will announce.
func EndedRunsInTx(tx storage.Tx, now time.Time, limit int) ([]Ended, error) {
	works, err := tx.ListWorks()
	if err != nil {
		return nil, err
	}
	var out []Ended
	for _, w := range works {
		if w.Kind != WorkKind {
			continue
		}
		var key *domain.CorrelationKey
		job := ""
		for i := range w.CorrelationKeys {
			if name, ok := ParseJobKey(w.CorrelationKeys[i].ExternalID); ok {
				key, job = &w.CorrelationKeys[i], name
				break
			}
		}
		if key == nil {
			continue
		}
		var last uint32
		if raw, err := tx.Get(runsNamespace, checkpointKey(w.ID)); err == nil {
			if json.Unmarshal(raw, &last) != nil {
				last = 0
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}
		xs, err := tx.ListExecutions(w.ID)
		if err != nil {
			return nil, err
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
		for _, x := range xs {
			if x.Attempt <= last {
				continue
			}
			if !x.State.Terminal() {
				break
			}
			if len(out) >= limit {
				return out, nil
			}
			e := Ended{Scope: w.ScopeID, Parent: x.TriggerID, Work: w.ID, Attempt: x.Attempt}
			if typ, ok := runType(x.State); ok {
				p := RunPayload{Job: job, Work: string(w.ID), Execution: string(x.ID), Attempt: x.Attempt, Outcome: string(x.State)}
				if x.Outcome != nil {
					p.Summary = x.Outcome.Summary
				}
				data, _ := json.Marshal(p)
				at := x.UpdatedAt
				if x.FinishedAt != nil {
					at = *x.FinishedAt
				}
				k := *key
				delivery := "ended/" + string(x.ID)
				e.Event = domain.Event{ID: domain.ID("schedule-" + digest(key.Source, delivery)[:40]), Type: typ, Source: key.Source,
					DeliveryID: delivery, CorrelationKey: &k, CausationID: x.TriggerID, OccurredAt: at.UTC(), ReceivedAt: now.UTC(),
					Data: domain.Payload{Schema: SchemaRun, Data: data}}
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkEndedInTx advances a job's checkpoint past an announced attempt.
func MarkEndedInTx(tx storage.Tx, work domain.ID, attempt uint32) error {
	raw, _ := json.Marshal(attempt)
	return tx.Put(runsNamespace, checkpointKey(work), raw)
}
