package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// refusingArchive fails PutEvents while refuse is set, as a full disk would.
type refusingArchive struct {
	storage.EventArchive
	refuse *bool
}

func (a refusingArchive) PutEvents(ctx context.Context, events []domain.AcceptedEvent) error {
	if *a.refuse {
		return errors.New("archive unavailable")
	}
	return a.EventArchive.PutEvents(ctx, events)
}

// unownedSource is a correlation source no route opens Work for.
const unownedSource = "fixture/unowned"

func passFixture(t *testing.T) (conformance.RuntimeFixture, storage.EventArchive) {
	t.Helper()
	// An Event with no key, or an unowned one, matches no route and opens no Work.
	f := waitFixture(t, "reopen", waitOptionsWith(false, routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		if k := in.Event.Event.CorrelationKey; k == nil || k.Source == unownedSource {
			return extension.RouteDecision{Reason: "no key"}, nil
		}
		return routingDecision(in)
	})))
	archive, err := sqlite.ArchiveBackend.Open(context.Background(), filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	return f, archive
}

func acceptUnmatched(t *testing.T, f conformance.RuntimeFixture, id string) domain.ID {
	t.Helper()
	e := routingEvent(f, id, nil)
	e.Data = domain.Payload{Schema: "fixture.private.v1", Data: []byte(`{"message":"` + id + `"}`)}
	if err := f.Driver.Accept(e); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, e.ID)
	return e.ID
}

func readEvent(t *testing.T, f conformance.RuntimeFixture, id domain.ID) domain.AcceptedEvent {
	t.Helper()
	var a domain.AcceptedEvent
	routingRead(t, f, func(tx storage.Tx) error {
		var er error
		a, er = tx.Event(id)
		return er
	})
	return a
}

func everything(f conformance.RuntimeFixture) runtime.ArchivePolicy {
	later := f.Clock.Now().Add(time.Hour)
	return runtime.ArchivePolicy{FinishedBefore: later, UnmatchedBefore: later}
}

func TestArchivePassMovesAnUnmatchedEventPastItsHorizon(t *testing.T) {
	ctx := context.Background()
	f, archive := passFixture(t)
	id := acceptUnmatched(t, f, "unmatched-old")
	want := readEvent(t, f, id)
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = archive })
	pass, err := s.ArchiveEvents(ctx, everything(f), 200)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Archived != 1 || !pass.Wrapped {
		t.Fatalf("pass = %+v, want one Event archived", pass)
	}
	if got := readEvent(t, f, id); !got.Retired || len(got.Event.Data.Data) != 0 {
		t.Fatalf("live record after the pass: %+v", got)
	}
	held, err := archive.Event(ctx, id)
	if err != nil || !storage.SameAcceptedEvent(held, want) {
		t.Fatalf("archive holds %+v, %v, want %+v", held, err, want)
	}
}

func TestArchivePassKeepsAnEventInsideItsHorizon(t *testing.T) {
	f, archive := passFixture(t)
	id := acceptUnmatched(t, f, "unmatched-new")
	earlier := f.Clock.Now().Add(-time.Hour)
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = archive })
	pass, err := s.ArchiveEvents(context.Background(), runtime.ArchivePolicy{FinishedBefore: earlier, UnmatchedBefore: earlier}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Archived != 0 || pass.Kept[runtime.ArchiveKeptRecent] != 1 {
		t.Fatalf("pass = %+v, want the Event kept as recent", pass)
	}
	if readEvent(t, f, id).Retired {
		t.Fatal("an Event inside its horizon was retired")
	}
}

func TestArchivePassKeepsUnfinishedWorkUntilItFinishes(t *testing.T) {
	ctx := context.Background()
	f, archive := passFixture(t)
	work := seedSchedulerWork(t, f, "owner")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "owner"}
	input := routingEvent(f, "owner-input", &key)
	if err := f.Driver.Accept(input); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, input.ID)
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = archive })
	if _, err := s.ArchiveEvents(ctx, everything(f), 200); err != nil {
		t.Fatal(err)
	}
	if readEvent(t, f, input.ID).Retired {
		t.Fatal("an Event of unfinished Work was retired")
	}
	if err := f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		w, er := tx.Work(work)
		if er != nil {
			return er
		}
		previous := w.Revision
		w.State, w.UpdatedAt = domain.WorkCancelled, f.Clock.Now()
		return tx.PutWork(w, previous)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveEvents(ctx, everything(f), 200); err != nil {
		t.Fatal(err)
	}
	if !readEvent(t, f, input.ID).Retired {
		t.Fatal("an Event of finished Work past its horizon stayed live")
	}
}

func TestArchivePassNeverRetiresWhatTheArchiveDidNotStore(t *testing.T) {
	ctx := context.Background()
	f, archive := passFixture(t)
	id := acceptUnmatched(t, f, "unmatched-held")
	refuse := true
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = refusingArchive{archive, &refuse} })
	if _, err := s.ArchiveEvents(ctx, everything(f), 200); err == nil {
		t.Fatal("a failed copy was not reported")
	}
	if readEvent(t, f, id).Retired {
		t.Fatal("retired an Event the archive refused")
	}
	refuse = false
	if _, err := s.ArchiveEvents(ctx, everything(f), 200); err != nil {
		t.Fatal(err)
	}
	if !readEvent(t, f, id).Retired {
		t.Fatal("the next pass did not complete the move")
	}
}

func TestArchivePassRotatesThroughTheLog(t *testing.T) {
	ctx := context.Background()
	f, archive := passFixture(t)
	first, second := acceptUnmatched(t, f, "rotate-1"), acceptUnmatched(t, f, "rotate-2")
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = archive })
	for range 3 {
		if _, err := s.ArchiveEvents(ctx, everything(f), 1); err != nil {
			t.Fatal(err)
		}
	}
	if !readEvent(t, f, first).Retired || !readEvent(t, f, second).Retired {
		t.Fatal("a one-Event page did not reach every Event")
	}
}

// afterCopyArchive runs after once the copy is durable, in the gap between
// the pass's copy and the transaction that retires.
type afterCopyArchive struct {
	storage.EventArchive
	after func([]domain.AcceptedEvent)
}

func (a afterCopyArchive) PutEvents(ctx context.Context, events []domain.AcceptedEvent) error {
	err := a.EventArchive.PutEvents(ctx, events)
	if err == nil {
		a.after(events)
	}
	return err
}

func TestArchivePassKeepsAnEventADependencyNeedsAfterTheCopy(t *testing.T) {
	ctx := context.Background()
	f, archive := passFixture(t)
	_, ref, target := waitForWork(t, f, "late-waiter", false)
	source := domain.CorrelationKey{Source: unownedSource, ExternalID: "needed"}
	e := routingEvent(f, "unmatched-needed", &source)
	e.Type = "fixture.ready"
	if err := f.Driver.Accept(e); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, e.ID)
	wait := domain.Wait{ID: "late-wait", AfterSequence: readEvent(t, f, e.ID).Sequence, Match: domain.EventMatch{Keys: []domain.CorrelationKey{target}, Types: []string{"fixture.alias"}}}
	if err := f.Driver.Wait(ref, wait, false); err != nil {
		t.Fatal(err)
	}
	var s *runtime.Scheduler
	subscribed := false
	s = archiveScheduler(t, f, func(o *runtime.SchedulerOptions) {
		// A filter that can't answer holds the dependency before the Event.
		o.AliasFilter = func(string, domain.Payload, domain.Event) (bool, error) {
			return false, errors.New("filter unreachable")
		}
		o.EventArchive = afterCopyArchive{archive, func([]domain.AcceptedEvent) {
			if subscribed {
				return
			}
			subscribed = true
			request := runtime.SubscriptionRequest{ID: "late-dependency", WaitID: wait.ID, SourceScopeID: "routing-scope", SourceKey: source, Types: []string{"fixture.ready"}, TargetKey: target, AliasType: "fixture.alias", Filter: &domain.Payload{Schema: "fixture.filter.v1", Data: []byte(`{}`)}}
			if err := s.Subscribe(ctx, request); err != nil {
				t.Error(err)
			}
		}}
	})
	pass, err := s.ArchiveEvents(ctx, everything(f), 200)
	if err != nil {
		t.Fatal(err)
	}
	if !subscribed {
		t.Fatal("the pass copied nothing, so the re-check was not reached")
	}
	if readEvent(t, f, e.ID).Retired {
		t.Fatal("retired an Event a dependency began to need after the copy")
	}
	if pass.Archived != 0 || pass.Kept[runtime.RetireBlockedWaitScan] != 1 {
		t.Fatalf("pass = %+v, want the Event kept for the dependency's scan", pass)
	}
}

func TestArchivePassKeepsAnEventThatChangedAfterTheCopy(t *testing.T) {
	f, archive := passFixture(t)
	id := acceptUnmatched(t, f, "unmatched-changed")
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) {
		o.EventArchive = afterCopyArchive{archive, func(events []domain.AcceptedEvent) {
			// The pass compares the live record with what it copied, so a
			// copy that differs stands for a live record that moved on.
			events[0].Event.Type = "fixture.moved-on"
		}}
	})
	pass, err := s.ArchiveEvents(context.Background(), everything(f), 200)
	if err != nil {
		t.Fatal(err)
	}
	if readEvent(t, f, id).Retired {
		t.Fatal("retired an Event that differs from the archive's copy")
	}
	if pass.Archived != 0 || pass.Kept[runtime.ArchiveKeptChanged] != 1 {
		t.Fatalf("pass = %+v, want the Event kept as changed", pass)
	}
}

func TestArchivePassKeepsAConflictAndMovesTheRestOfThePage(t *testing.T) {
	ctx := context.Background()
	f, archive := passFixture(t)
	conflicting, other := acceptUnmatched(t, f, "unmatched-conflict"), acceptUnmatched(t, f, "unmatched-other")
	different := readEvent(t, f, conflicting)
	different.Event.Data = domain.Payload{Schema: "fixture.private.v1", Data: []byte(`{"message":"a different copy"}`)}
	if err := archive.PutEvents(ctx, []domain.AcceptedEvent{different}); err != nil {
		t.Fatal(err)
	}
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = archive })
	pass, err := s.ArchiveEvents(ctx, everything(f), 200)
	if err != nil {
		t.Fatal(err)
	}
	if readEvent(t, f, conflicting).Retired {
		t.Fatal("retired an Event the archive holds differently")
	}
	if !readEvent(t, f, other).Retired {
		t.Fatal("one conflict held up the rest of the page")
	}
	if pass.Archived != 1 || pass.Kept[runtime.ArchiveKeptConflict] != 1 {
		t.Fatalf("pass = %+v, want one moved and one kept as a conflict", pass)
	}
}

// The live database keeps what moved and, for the last whole trip over the
// log, why the rest stayed.
func TestArchivePassLeavesATrail(t *testing.T) {
	ctx := context.Background()
	f, archive := passFixture(t)
	acceptUnmatched(t, f, "trail-old")
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = archive })
	read := func() runtime.ArchiveRecord {
		t.Helper()
		var r runtime.ArchiveRecord
		if err := f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
			var err error
			r, err = runtime.ArchiveRecordInTx(tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := read(); r.Archived != 0 || !r.At.IsZero() {
		t.Fatalf("record before any pass: %+v", r)
	}
	if _, err := s.ArchiveEvents(ctx, everything(f), 200); err != nil {
		t.Fatal(err)
	}
	if r := read(); r.Archived != 1 || !r.At.Equal(f.Clock.Now()) {
		t.Fatalf("record after the pass: %+v", r)
	}
	acceptUnmatched(t, f, "trail-recent")
	recent := runtime.ArchivePolicy{FinishedBefore: f.Clock.Now().Add(-time.Hour), UnmatchedBefore: f.Clock.Now().Add(-time.Hour)}
	if _, err := s.ArchiveEvents(ctx, recent, 200); err != nil {
		t.Fatal(err)
	}
	r := read()
	if r.Archived != 1 || r.LastLap[runtime.ArchiveKeptRecent] == 0 || len(r.Lap) != 0 {
		t.Fatalf("record after a pass that kept an Event: %+v", r)
	}
}
