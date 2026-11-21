package runtime

import (
	"context"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// ForgetInvoke makes an attempt's open segment look like one an earlier
// release began, which recorded no Invoke and no Isolated.
func ForgetInvoke(tx storage.Tx, id domain.ID) error {
	state, err := attemptState(tx, id)
	if err != nil {
		return err
	}
	if n := len(state.Segments); n > 0 {
		state.Segments[n-1].Invoke, state.Segments[n-1].Isolated = "", false
	}
	return putAttemptState(tx, state)
}

// PutLegacyAttemptState records an attempt's state naming its executor, as
// a release from before attempts recorded it on the Execution did.
func PutLegacyAttemptState(tx storage.Tx, execution, work domain.ID, executor string) error {
	return putAttemptState(tx, AttemptState{ExecutionID: execution, WorkID: work, Executor: executor})
}

// UsageRoutableInTx exposes the depth check routing applies to usage records.
func UsageRoutableInTx(tx storage.Tx, id domain.ID, limits CausalLimits) (bool, error) {
	return usageRoutableInTx(tx, id, limits)
}

// WaitScanned is the highest sequence the wake scans of Wait id have
// already judged.
func WaitScanned(tx storage.Tx, id domain.ID) (uint64, error) {
	var meta waitMetadata
	err := getJSON(tx, waitNamespace, identity(string(id)), &meta)
	return meta.Scanned, err
}

// HoldForCapacity exposes holdForCapacity to the external test package.
func HoldForCapacity(ctx context.Context, s *Scheduler, ref domain.LeaseRef, r *extension.NoCapacityError) error {
	return s.holdForCapacity(ctx, ref, r)
}
