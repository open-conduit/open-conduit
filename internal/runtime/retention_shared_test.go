package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

// Content two attempts share stays until the last of them expires (#913).
func TestProductionRetentionKeepsContentAnotherAttemptHolds(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	shared := domain.ArtifactRef{ID: "shared", Size: 1, MediaType: "text/plain"}
	finish := func(name string) domain.ID {
		attempt, err := d.Scheduler.Start(ctx, seedSchedulerWork(t, f, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = d.Scheduler.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "done"}, Artifacts: []domain.ArtifactRef{shared}}); err != nil {
			t.Fatal(err)
		}
		return attempt.Lease.ExecutionID
	}
	older := finish("shared-older")
	f.Clock.Advance(90 * time.Minute)
	newer := finish("shared-newer")

	options := d.Options
	deleted := &deletedArtifacts{}
	options.Artifacts, options.KeptFinished = deleted, time.Hour
	sweeper, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := sweeper.SweepRetention(ctx, 10); err != nil || !slices.Equal(r.Expired, []domain.ID{older}) {
		t.Fatalf("first sweep: %+v %v", r, err)
	}
	if len(deleted.deleted) != 0 {
		t.Fatalf("deleted %v while attempt %s still holds it", deleted.deleted, newer)
	}

	// The first attempt's release is settled: a later sweep doesn't retry it.
	if r, err := sweeper.SweepRetention(ctx, 10); err != nil || r.Pending != 0 {
		t.Fatalf("second sweep: %+v %v", r, err)
	}

	// Content goes with the last attempt to hold it.
	f.Clock.Advance(2 * time.Hour)
	if r, err := sweeper.SweepRetention(ctx, 10); err != nil || !slices.Equal(r.Expired, []domain.ID{newer}) {
		t.Fatalf("last sweep: %+v %v", r, err)
	}
	if !slices.Equal(deleted.deleted, []string{"shared"}) {
		t.Fatalf("deleted %v, want the shared content once", deleted.deleted)
	}

	// A result naming released content doesn't attach it, so a release
	// can't delete what an attempt has just come to hold.
	late := finish("shared-late")
	routingRead(t, f, func(tx storage.Tx) error {
		x, e := tx.Execution(late)
		if e != nil {
			return e
		}
		if len(x.AllArtifacts()) != 0 {
			t.Errorf("attempt attached released content: %+v", x.AllArtifacts())
		}
		return nil
	})
}

// A hold taken after a sweep read what to release stops the delete of
// content whose last holder has expired (#913).
func TestProductionRetentionHoldStopsSharedRelease(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	attempt, err := d.Scheduler.Start(ctx, seedSchedulerWork(t, f, "held"))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.ArtifactRef{ID: "held", Size: 1, MediaType: "text/plain"}
	if err = d.Scheduler.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "done"}, Artifacts: []domain.ArtifactRef{ref}}); err != nil {
		t.Fatal(err)
	}
	options := d.Options
	deleted := &deletedArtifacts{}
	options.Artifacts, options.KeptFinished = deleted, time.Hour
	sweeper, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(2 * time.Hour)
	if err = f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		return runtime.HoldRetention(tx, "backup", f.Clock.Now().Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := sweeper.SweepRetention(ctx, 10); err != nil || !r.Held || r.Pending != 1 {
		t.Fatalf("held sweep: %+v %v", r, err)
	}
	if len(deleted.deleted) != 0 {
		t.Fatalf("deleted %v under a hold", deleted.deleted)
	}
	if err = f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		return runtime.ReleaseRetention(tx, "backup")
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := sweeper.SweepRetention(ctx, 10); err != nil || r.Pending != 0 || !slices.Equal(deleted.deleted, []string{"held"}) {
		t.Fatalf("after the hold: %+v %v, deleted %v", r, err, deleted.deleted)
	}
}

// A scrubbed original no attempt holds goes at once under a retention hold:
// a hold protects content a backup's snapshot names, and no snapshot names
// this. Content an expired attempt holds still waits for the hold (#913).
func TestProductionScrubDeletesAnUnheldOriginalUnderAHold(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	store := newListedArtifacts()
	original := store.add("original", f.Clock.Now())
	expiredRef := store.add("expired-holder", f.Clock.Now())
	finishedWithArtifacts(t, f, d, "scrub-holder", []domain.ArtifactRef{expiredRef})
	options := d.Options
	options.Artifacts, options.KeptFinished = store, time.Hour
	sweeper, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(2 * time.Hour)
	if err = f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		return runtime.HoldRetention(tx, "backup", f.Clock.Now().Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := sweeper.SweepRetention(ctx, 10); err != nil || !r.Held {
		t.Fatalf("held sweep: %+v %v", r, err)
	}
	guarded := runtime.GuardedArtifacts(sweeper, store)
	if err = guarded.Delete(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err = guarded.Delete(ctx, expiredRef); err != nil {
		t.Fatal(err)
	}
	if got := store.taken(); !slices.Equal(got, []string{"original"}) {
		t.Fatalf("under a hold deleted %v, want only the unheld original", got)
	}
}

type recordedTelemetry struct {
	mu      sync.Mutex
	records []observability.Record
}

func (o *recordedTelemetry) Observe(_ context.Context, r observability.Record) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.records = append(o.records, r)
	return nil
}

func (o *recordedTelemetry) named(name string) []observability.Record {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []observability.Record
	for _, r := range o.records {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// A result naming released content doesn't attach it, and says so in the
// run's history and telemetry. The claim is pruned, and the history stays.
func TestProductionDroppedArtifactLeavesATrail(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	store := newListedArtifacts()
	released := store.add("released", f.Clock.Now())
	finishedWithArtifacts(t, f, d, "trail-first", []domain.ArtifactRef{released})
	observer := &recordedTelemetry{}
	instrumented, err := runtime.Instrument(f.Store, runtime.TelemetryOptions{Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	options := d.Options
	options.Artifacts, options.KeptFinished = store, time.Hour
	s, err := runtime.NewScheduler(instrumented, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(2 * time.Hour)
	if _, err = s.SweepRetention(ctx, 10); err != nil {
		t.Fatal(err)
	}
	fresh := store.add("fresh", f.Clock.Now())
	attempt, err := s.Start(ctx, seedSchedulerWork(t, f, "trail-late"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "done"}, Artifacts: []domain.ArtifactRef{released, fresh}}); err != nil {
		t.Fatal(err)
	}
	id := attempt.Lease.ExecutionID
	check := func() {
		t.Helper()
		routingRead(t, f, func(tx storage.Tx) error {
			r, e := runtime.RunInTx(tx, id)
			if e != nil {
				return e
			}
			if !slices.Equal(r.DroppedArtifacts, []string{"released"}) || !slices.Equal(r.Execution.Artifacts, []domain.ArtifactRef{fresh}) {
				t.Errorf("dropped %v, artifacts %+v", r.DroppedArtifacts, r.Execution.Artifacts)
			}
			return nil
		})
	}
	check()
	records := observer.named("execution.artifacts_dropped")
	if len(records) != 1 || records[0].Attributes["artifacts.dropped_ids"] != "released" || records[0].Correlation.ExecutionID != id {
		t.Fatalf("telemetry: %+v", records)
	}

	f.Clock.Advance(8 * 24 * time.Hour)
	if _, err = s.SweepRetention(ctx, 10); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		rows, e := tx.List("core.retention.released", "")
		for _, row := range rows {
			if e == nil {
				var c struct{ At time.Time }
				if e = json.Unmarshal(row.Value, &c); e == nil && !c.At.After(f.Clock.Now().Add(-7*24*time.Hour)) {
					t.Errorf("claim from %v outlived its window", c.At)
				}
			}
		}
		return e
	})
	check()
}

// refusingDelete refuses every delete as a ref that doesn't match.
type refusingDelete struct{ *listedArtifacts }

func (refusingDelete) Delete(context.Context, storage.ArtifactRef) error { return storage.ErrInvalid }

// A delete refused for a mismatched ref removed nothing, so it leaves no
// claim and the ID's content can still be attached.
func TestProductionRefusedDeleteClearsItsClaim(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	store := newListedArtifacts()
	ref := store.add("mismatched", f.Clock.Now())
	s, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, d.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.GuardedArtifacts(s, refusingDelete{store}).Delete(ctx, ref); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("delete: got %v, want ErrInvalid", err)
	}
	id := finishedWithArtifacts(t, f, d, "refused", []domain.ArtifactRef{ref})
	routingRead(t, f, func(tx storage.Tx) error {
		x, e := tx.Execution(id)
		if e == nil && !slices.Equal(x.Artifacts, []domain.ArtifactRef{ref}) {
			t.Errorf("artifacts %+v, want the ref attached", x.Artifacts)
		}
		return e
	})
}

// Collection claims what it deletes, so a late result can't attach it.
func TestProductionCollectionClaimsWhatItDeletes(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	store := newListedArtifacts()
	orphan := store.add("orphan", f.Clock.Now())
	f.Clock.Advance(time.Hour)
	if r, err := d.Scheduler.CollectArtifacts(ctx, store, f.Clock.Now()); err != nil || r.Deleted != 1 {
		t.Fatalf("collect: %+v %v", r, err)
	}
	id := finishedWithArtifacts(t, f, d, "late-orphan", []domain.ArtifactRef{orphan})
	routingRead(t, f, func(tx storage.Tx) error {
		x, e := tx.Execution(id)
		if e == nil && len(x.AllArtifacts()) != 0 {
			t.Errorf("attached collected content: %+v", x.AllArtifacts())
		}
		return e
	})
}
