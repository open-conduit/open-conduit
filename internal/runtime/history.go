package runtime

import (
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Run is the unified operator history record of one attempt. Every executor
// (shell, agent or a future one) produces exactly this shape from generic
// runtime state; nothing here interprets executor payloads or work kinds.
type Run struct {
	Execution domain.Execution
	ScopeID   domain.ID
	WorkKind  string
	WorkState domain.WorkState
	// Trigger is the event that started the attempt; CurrentTrigger differs
	// only after a resumed segment was woken by another event.
	Trigger, CurrentTrigger *domain.Event
	Executor, Generation    string
	LaunchState             string
	ResourceExcluded        bool
	Deadline                time.Time
	RemainingTimeout        time.Duration
	Usage                   *extension.Usage
	// Workspace is the resource the attempt ran in, as its executor reported
	// it (B07): the same record for every executor.
	Workspace           *extension.WorkspaceRecord
	Inputs              int
	WaitID, WakeEventID domain.ID
	// Segments are the attempt's kept active stretches
	// (AttemptState.Segments), the last ended at the execution's finish once
	// it is terminal. ActiveTime sums the ended ones and the dropped ones
	// (DroppedSegments of them).
	Segments        []Segment
	DroppedSegments int
	ActiveTime      time.Duration
	// Retry is the Work's retry eligibility when this attempt is the latest
	// recorded failure.
	Retry     *RetryState
	Retention RetentionStatus
}

func envelope(tx storage.Tx, id domain.ID) *domain.Event {
	if id == "" {
		return nil
	}
	a, err := tx.Event(id)
	if err != nil {
		return nil
	}
	e := a.Event
	e.Data = domain.Payload{}
	return &e
}

func runInTx(tx storage.Tx, x domain.Execution, works map[domain.ID]domain.Work) (Run, error) {
	w, ok := works[x.WorkID]
	if !ok {
		var err error
		if w, err = tx.Work(x.WorkID); err != nil {
			return Run{}, err
		}
		if works != nil {
			works[x.WorkID] = w
		}
	}
	run := Run{Execution: x, ScopeID: w.ScopeID, WorkKind: w.Kind, WorkState: w.State, Trigger: envelope(tx, x.TriggerID)}
	state, err := attemptState(tx, x.ID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return Run{}, err
	}
	if err == nil {
		run.Executor, run.Generation, run.LaunchState = state.Executor, state.Generation, state.LaunchState
		run.ResourceExcluded = state.Excluded
		run.Deadline, run.RemainingTimeout = state.Deadline, state.RemainingTimeout
		run.Inputs = len(state.InputEventIDs)
		run.WaitID, run.WakeEventID = state.WaitID, state.WakeEventID
		run.Segments, run.ActiveTime = segments(state.Segments, x)
		run.DroppedSegments, run.ActiveTime = state.DroppedSegments, run.ActiveTime+state.DroppedActive
		if state.CurrentTriggerID != "" && state.CurrentTriggerID != x.TriggerID {
			run.CurrentTrigger = envelope(tx, state.CurrentTriggerID)
		}
		if u := state.Usage(); !usageEmpty(u) {
			run.Usage = &u
		}
		if state.Result != nil {
			run.Workspace = state.Result.Workspace
		}
	}
	retry, err := retryState(tx, x.WorkID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return Run{}, err
	}
	if err == nil && retry.FailedExecutionID == x.ID {
		run.Retry = &retry
	}
	if run.Retention, err = RetentionInTx(tx, x.ID); err != nil {
		return Run{}, err
	}
	return run, nil
}

// segments closes the open segment of a finished execution at its finish
// and sums the ended segments.
func segments(recorded []Segment, x domain.Execution) ([]Segment, time.Duration) {
	out := append([]Segment(nil), recorded...)
	var active time.Duration
	for i := range out {
		if out[i].EndedAt == nil && x.State.Terminal() && x.FinishedAt != nil {
			end := *x.FinishedAt
			out[i].EndedAt = &end
		}
		if out[i].EndedAt != nil {
			active += out[i].EndedAt.Sub(out[i].StartedAt)
		}
	}
	return out, active
}

// RunInTx builds the unified history record for one execution.
func RunInTx(tx storage.Tx, id domain.ID) (Run, error) {
	x, err := tx.Execution(id)
	if err != nil {
		return Run{}, err
	}
	return runInTx(tx, x, nil)
}

// RunsInTx returns the unified history for every attempt, optionally limited
// to one Work, in storage order.
func RunsInTx(tx storage.Tx, work domain.ID) ([]Run, error) {
	xs, err := tx.ListExecutions(work)
	if err != nil {
		return nil, err
	}
	works := map[domain.ID]domain.Work{}
	out := make([]Run, 0, len(xs))
	for _, x := range xs {
		run, err := runInTx(tx, x, works)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}
