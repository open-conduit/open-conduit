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

func passFixture(t *testing.T) (conformance.RuntimeFixture, storage.EventArchive) {
	t.Helper()
	// An Event with no correlation key matches no route, so it opens no Work.
	f := waitFixture(t, "reopen", waitOptionsWith(false, routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		if in.Event.Event.CorrelationKey == nil {
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
