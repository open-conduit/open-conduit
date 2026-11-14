package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// stoppedTranscript is what a turn wrote up to its stop: the executor's
// result once the process ended, with the turn's transcript.
var stoppedTranscript = extension.Result{
	Outcome:   domain.Outcome{Kind: domain.OutcomeFailed, Summary: "interrupted"},
	Artifacts: []domain.ArtifactRef{{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 42, MediaType: "application/x-ndjson"}},
}

// interruptRunning interrupts the rig's running attempt as an operator.
func (r interruptRig) interruptRunning(t *testing.T) {
	t.Helper()
	ctx := runtime.WithActor(context.Background(), "operator:alex")
	if _, _, err := r.control.InterruptWork(ctx, r.work, r.revision(t), "it ran the wrong command", "interrupt-647"); err != nil {
		t.Fatal(err)
	}
}

// run reads the history record of the rig's first attempt.
func (r interruptRig) run(t *testing.T) runtime.Run {
	t.Helper()
	var run runtime.Run
	r.read(t, func(tx storage.Tx) error {
		var err error
		run, err = runtime.RunInTx(tx, r.attempt.Lease.ExecutionID)
		return err
	})
	return run
}

func lastSegment(t *testing.T, run runtime.Run) runtime.Segment {
	t.Helper()
	if len(run.Segments) == 0 {
		t.Fatal("run has no segments")
	}
	return run.Segments[len(run.Segments)-1]
}

// TestProductionInterruptedTurnKeepsItsTranscript (#647): an operator
// interrupts a turn mid tool call. Until the stop is confirmed the ended
// segment says its artifacts are pending. The result the stopped process
// then writes can no longer complete the fenced attempt, but its
// transcript is recorded on the run and on the segment, the run stays
// cancelled with no outcome, and the segment's artifacts are final.
func TestProductionInterruptedTurnKeepsItsTranscript(t *testing.T) {
	r := interruptFixture(t, domain.Capabilities{Inject: true, Interrupt: true}, "")
	r.interruptRunning(t)
	if g := lastSegment(t, r.run(t)); g.EndedAt == nil || !g.ArtifactsPending || len(g.Artifacts) != 0 {
		t.Fatalf("interrupted segment before its result: ended %v, pending %v, artifacts %v; want ended and pending", g.EndedAt, g.ArtifactsPending, g.Artifacts)
	}
	attempt := r.attempt
	attempt.Handle = &schedulerHandle{Result: stoppedTranscript}
	if err := r.s.Observe(context.Background(), attempt); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("observing the stopped turn: %v, want the fence's lost lease", err)
	}
	run := r.run(t)
	if run.Execution.State != domain.ExecutionCancelled || run.Execution.Outcome != nil {
		t.Fatalf("run after its stopped turn's result: %s %+v, want cancelled without an outcome", run.Execution.State, run.Execution.Outcome)
	}
	if len(run.Execution.Artifacts) != 1 || run.Execution.Artifacts[0] != stoppedTranscript.Artifacts[0] {
		t.Fatalf("run artifacts = %+v, want the stopped turn's transcript", run.Execution.Artifacts)
	}
	g := lastSegment(t, run)
	if g.ArtifactsPending || len(g.Artifacts) != 1 || g.Artifacts[0] != stoppedTranscript.Artifacts[0] {
		t.Fatalf("segment after the stopped turn's result: pending %v, artifacts %+v; want the transcript, final", g.ArtifactsPending, g.Artifacts)
	}
	if err := r.s.ConfirmStopped(context.Background(), attempt.Lease.ExecutionID, attempt.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	if g := lastSegment(t, r.run(t)); g.ArtifactsPending || len(g.Artifacts) != 1 {
		t.Fatalf("segment once stopped: pending %v, artifacts %+v", g.ArtifactsPending, g.Artifacts)
	}
}

// TestProductionCollectStoppedWaitsOnceForTheStoppedTurn: the hold lost
// the lease before the stopped process wrote its result, so the host
// collects it after the verified stop. A second collection does not wait
// on the executor again, and the record is not doubled.
func TestProductionCollectStoppedWaitsOnceForTheStoppedTurn(t *testing.T) {
	r := interruptFixture(t, domain.Capabilities{Inject: true, Interrupt: true}, "")
	r.interruptRunning(t)
	waits := 0
	attempt := r.attempt
	attempt.Handle = &schedulerHandle{WaitFn: func(context.Context) (extension.Result, error) {
		waits++
		return stoppedTranscript, nil
	}}
	for range 2 {
		if err := r.s.CollectStopped(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
	}
	if waits != 1 {
		t.Fatalf("waited on the stopped turn %d times, want once", waits)
	}
	run := r.run(t)
	if g := lastSegment(t, run); g.ArtifactsPending || len(g.Artifacts) != 1 || len(run.Execution.AllArtifacts()) != 1 {
		t.Fatalf("collected: segment pending %v artifacts %+v, run %+v", g.ArtifactsPending, g.Artifacts, run.Execution.AllArtifacts())
	}
}

// TestProductionRecordStoppedNeedsAnEndedUnstoppedAttempt is the negative
// control: a running attempt, another epoch's result and one that comes
// after the stop is confirmed record nothing, and a confirmed stop leaves
// nothing pending.
func TestProductionRecordStoppedNeedsAnEndedUnstoppedAttempt(t *testing.T) {
	ctx := context.Background()
	r := interruptFixture(t, domain.Capabilities{Inject: true, Interrupt: true}, "")
	if ok, err := r.s.RecordStopped(ctx, r.attempt.Lease, stoppedTranscript); err != nil || ok {
		t.Fatalf("running attempt took a stopped turn: %v %v", ok, err)
	}
	if g := lastSegment(t, r.run(t)); g.ArtifactsPending {
		t.Fatal("a running segment is pending")
	}
	r.interruptRunning(t)
	stale := r.attempt.Lease
	stale.Epoch++
	if ok, err := r.s.RecordStopped(ctx, stale, stoppedTranscript); err != nil || ok {
		t.Fatalf("another epoch's result was recorded: %v %v", ok, err)
	}
	if err := r.s.ConfirmStopped(ctx, r.attempt.Lease.ExecutionID, r.attempt.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.s.RecordStopped(ctx, r.attempt.Lease, stoppedTranscript); err != nil || ok {
		t.Fatalf("a result after the confirmed stop was recorded: %v %v", ok, err)
	}
	run := r.run(t)
	if g := lastSegment(t, run); g.ArtifactsPending || len(g.Artifacts) != 0 || len(run.Execution.AllArtifacts()) != 0 {
		t.Fatalf("after a confirmed stop: pending %v, segment %+v, run %+v", g.ArtifactsPending, g.Artifacts, run.Execution.AllArtifacts())
	}
}

// TestProductionCancelledWorkKeepsTheStoppedTurn: cancelling the Work
// ends its running attempt with it, and the turn's transcript is still
// recorded once its process stops, though the Work has finished.
func TestProductionCancelledWorkKeepsTheStoppedTurn(t *testing.T) {
	ctx := context.Background()
	r := interruptFixture(t, domain.Capabilities{Inject: true, Interrupt: true}, "")
	if err := r.s.Cancel(ctx, r.work); err != nil {
		t.Fatal(err)
	}
	run := r.run(t)
	if run.Execution.State != domain.ExecutionCancelled || !lastSegment(t, run).ArtifactsPending {
		t.Fatalf("cancelled run: %s, pending %v", run.Execution.State, lastSegment(t, run).ArtifactsPending)
	}
	if ok, err := r.s.RecordStopped(ctx, r.attempt.Lease, stoppedTranscript); err != nil || !ok {
		t.Fatalf("cancelled Work's stopped turn: %v %v", ok, err)
	}
	run = r.run(t)
	if g := lastSegment(t, run); g.ArtifactsPending || len(g.Artifacts) != 1 || len(run.Execution.Artifacts) != 1 || run.WorkState != domain.WorkCancelled {
		t.Fatalf("after the stopped turn: work %s, segment %+v, run %+v", run.WorkState, g.Artifacts, run.Execution.Artifacts)
	}
}
