package runtime

import (
	"context"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// An attempt that ends while its turn runs (an operator's interrupt, an
// input's, a turn's deadline, the Work's cancel) is fenced first: its
// execution is terminal before its process stops, so the result the
// process then writes can no longer complete it. What that turn wrote up
// to the stop (its transcript, its activity) is still the record of what
// it did, and the stop is when an operator most needs it (#647).
//
// RecordStopped keeps it: the artifacts of the stopped turn's result are
// published to the execution and to its last segment, exactly as a turn
// that ended normally publishes them. Nothing else of the result is
// applied: the execution's state and outcome stay those of the fence, and
// its proved inputs and usage are not taken from a result that arrived
// after it. The record is accepted only from the attempt's own epoch, only
// once the execution has ended, and only until its stop is confirmed
// (AttemptState.Excluded): from then on retention may release the
// attempt's content, and the segment's artifacts are final. A repeated
// result adds nothing, and one without artifacts records nothing. It
// reports whether the attempt took the record.
func (s *Scheduler) RecordStopped(ctx context.Context, ref domain.LeaseRef, result extension.Result) (bool, error) {
	if err := extension.ValidateResult(result); err != nil {
		return false, err
	}
	if len(result.Artifacts) == 0 {
		// Nothing to keep: a refused handoff, or a turn that wrote
		// nothing. The segment stays pending until the stop is confirmed.
		return false, nil
	}
	recorded := false
	err := s.transact(ctx, nil, func(tx storage.Tx) error {
		state, err := attemptState(tx, ref.ExecutionID)
		if err != nil {
			return err
		}
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		if state.Epoch != ref.Epoch || x.Lease.Epoch != ref.Epoch || !x.State.Terminal() || state.Excluded || state.StoppedTurnRecorded {
			return nil
		}
		if err = tx.PublishFinishedArtifacts(x.ID, result.Artifacts); err != nil {
			return err
		}
		state.turnArtifacts(result.Artifacts)
		state.StoppedTurnRecorded = true
		recorded = true
		return putAttemptState(tx, state)
	})
	return recorded, err
}

// CollectStopped waits, under ctx, for the result of an ended attempt
// whose process the host has verified stopped, and records what its
// stopped turn published (RecordStopped). A handle answers once, so the
// host calls it only for an attempt whose handle has not answered yet (the
// hold ended first), between the verified stop and ConfirmStopped. One that
// answered with a result had it kept by Complete; the host passes that
// result to RecordStopped again rather than waiting. An attempt that
// already took the record is not waited on. A result the executor cannot
// give (an error, ctx ending) leaves the segment without artifacts, which
// ConfirmStopped then makes final.
func (s *Scheduler) CollectStopped(ctx context.Context, attempt Attempt) error {
	if attempt.Handle == nil {
		return nil
	}
	state, err := s.InspectAttempt(ctx, attempt.Lease.ExecutionID)
	if err != nil {
		return err
	}
	if state.Epoch != attempt.Lease.Epoch || state.Excluded || state.StoppedTurnRecorded {
		return nil
	}
	result, err := attempt.Handle.Wait(ctx)
	if err != nil {
		return err
	}
	_, err = s.RecordStopped(ctx, attempt.Lease, result)
	return err
}

// recordLate keeps the stopped turn of a result Complete refused (err)
// because the attempt had already ended at its epoch: a fence took the
// lease before the result arrived. RecordStopped takes
// nothing from an attempt still running, so a refusal for any other
// reason records nothing; an invalid result is not tried again. The
// refusal is returned as it was.
func (s *Scheduler) recordLate(ctx context.Context, ref domain.LeaseRef, result extension.Result, err error) error {
	if errors.Is(err, extension.ErrInvalid) {
		return err
	}
	if _, e := s.RecordStopped(context.WithoutCancel(ctx), ref, result); e != nil {
		return errors.Join(err, e)
	}
	return err
}
