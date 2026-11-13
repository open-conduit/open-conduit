package runtime_test

import (
	"context"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// deletedArtifacts is a storage.Artifacts that records what retention
// deletes.
type deletedArtifacts struct {
	mu      sync.Mutex
	deleted []string
}

func (a *deletedArtifacts) Create(context.Context, string) (storage.ArtifactWriter, error) {
	return nil, storage.ErrInvalid
}
func (a *deletedArtifacts) Open(context.Context, storage.ArtifactRef) (io.ReadCloser, error) {
	return nil, storage.ErrNotFound
}
func (a *deletedArtifacts) Delete(_ context.Context, ref storage.ArtifactRef) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleted = append(a.deleted, ref.ID)
	return nil
}

// An attempt that parks and resumes keeps the artifacts of every turn: the
// parked turn's stay the attempt's when the last turn's result replaces
// them as its outcome's, each segment names its own, the listing gives
// each its segment, and retention releases them all (#592).
func TestProductionEveryTurnKeepsItsArtifacts(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "turn-artifacts")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "turn-artifacts"}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	ref := func(id string) domain.ArtifactRef {
		return domain.ArtifactRef{ID: id, Size: 10, MediaType: "application/jsonl"}
	}
	first := []domain.ArtifactRef{ref("first-transcript"), ref("first-activity")}
	last := []domain.ArtifactRef{ref("last-transcript"), ref("last-activity")}
	wait := domain.Wait{ID: "turn-artifacts-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.changed"}}}
	if err = d.Scheduler.WaitWithResult(ctx, attempt.Lease, wait, true, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "parked"}, Artifacts: first}); err != nil {
		t.Fatal(err)
	}
	requireAccept(t, f, "turn-artifacts-wake", &key)
	if err = d.Wake("turn-artifacts-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.Scheduler.Start(ctx, work)
	if err != nil || resumed.Lease.ExecutionID != attempt.Lease.ExecutionID {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	if err = d.Scheduler.Complete(ctx, resumed.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "done"}, Artifacts: last}); err != nil {
		t.Fatal(err)
	}
	id := attempt.Lease.ExecutionID
	routingRead(t, f, func(tx storage.Tx) error {
		r, e := runtime.RunInTx(tx, id)
		if e != nil {
			return e
		}
		x := r.Execution
		if !reflect.DeepEqual(x.Artifacts, last) || !reflect.DeepEqual(x.EarlierArtifacts, first) {
			t.Errorf("execution artifacts %+v, earlier %+v", x.Artifacts, x.EarlierArtifacts)
		}
		if len(r.Segments) != 2 || !reflect.DeepEqual(r.Segments[0].Artifacts, first) || !reflect.DeepEqual(r.Segments[1].Artifacts, last) {
			t.Errorf("segments: %+v", r.Segments)
		}
		all, e := runtime.ArtifactsInTx(tx, x)
		if e != nil {
			return e
		}
		want := []runtime.RunArtifact{{ArtifactRef: first[0], Segment: 1}, {ArtifactRef: first[1], Segment: 1}, {ArtifactRef: last[0], Segment: 2}, {ArtifactRef: last[1], Segment: 2}}
		if !reflect.DeepEqual(all, want) {
			t.Errorf("listed %+v", all)
		}
		return nil
	})

	// Retention of the finished Work releases every turn's content.
	options := d.Options
	deleted := &deletedArtifacts{}
	options.Artifacts, options.KeptFinished = deleted, time.Hour
	sweeper, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(2 * time.Hour)
	if result, err := sweeper.SweepRetention(ctx, 10); err != nil || len(result.Expired) != 1 || result.Expired[0] != id {
		t.Fatalf("sweep: %+v %v", result, err)
	}
	if want := []string{"first-transcript", "first-activity", "last-transcript", "last-activity"}; !reflect.DeepEqual(deleted.deleted, want) {
		t.Fatalf("retention deleted %v, want %v", deleted.deleted, want)
	}
}
