package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// failingArchive refuses correlation reads while fail is set.
type failingArchive struct {
	storage.EventArchive
	fail *bool
}

func (a failingArchive) CorrelationEvents(ctx context.Context, key domain.CorrelationKey, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if *a.fail {
		return nil, errors.New("archive unavailable")
	}
	return a.EventArchive.CorrelationEvents(ctx, key, after, limit)
}

const archivedPayload = `{"message":"archived-source-body"}`

// retiredDependency parks a Work on a dependency whose source Event has
// already left the live log for archive, and returns what to subscribe with.
func retiredDependency(t *testing.T, name string) (conformance.RuntimeFixture, func(storage.Tx) (domain.Wait, []domain.AcceptedEvent, error), runtime.SubscriptionRequest, storage.EventArchive, domain.ID) {
	t.Helper()
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(false))
	_, ref, target := waitForWork(t, f, "dependent-"+name, false)
	seedSchedulerWork(t, f, "prerequisite-"+name)
	source := domain.CorrelationKey{Source: "fixture/account", ExternalID: "prerequisite-" + name}
	observed := routingEvent(f, "ready-"+name, &source)
	observed.Type = "fixture.ready"
	observed.Data = domain.Payload{Schema: "fixture.private.v1", Data: []byte(archivedPayload)}
	if err := f.Driver.Accept(observed); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, observed.ID)
	archive, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	var record domain.AcceptedEvent
	routingRead(t, f, func(tx storage.Tx) error {
		var er error
		record, er = tx.Event(observed.ID)
		return er
	})
	if err = archive.PutEvents(ctx, []domain.AcceptedEvent{record}); err != nil {
		t.Fatal(err)
	}
	if err = f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		return tx.RetireEvent(observed.ID)
	}); err != nil {
		t.Fatal(err)
	}
	wait := domain.Wait{ID: domain.ID("dependency-" + name), AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{target}, Types: []string{"fixture.alias"}}}
	if err = f.Driver.Wait(ref, wait, false); err != nil {
		t.Fatal(err)
	}
	read := func(tx storage.Tx) (domain.Wait, []domain.AcceptedEvent, error) {
		w, er := tx.Wait(wait.ID)
		if er != nil {
			return w, nil, er
		}
		all, er := tx.Events(0, 0)
		if er != nil {
			return w, nil, er
		}
		var aliases []domain.AcceptedEvent
		for _, a := range all {
			if a.Event.Type == "fixture.alias" {
				aliases = append(aliases, a)
			}
		}
		return w, aliases, nil
	}
	request := runtime.SubscriptionRequest{ID: "sub-" + name, WaitID: wait.ID, SourceScopeID: "routing-scope", SourceKey: source, Types: []string{"fixture.ready"}, TargetKey: target, AliasType: "fixture.alias"}
	return f, read, request, archive, observed.ID
}

func archiveScheduler(t *testing.T, f conformance.RuntimeFixture, change func(*runtime.SchedulerOptions)) *runtime.Scheduler {
	t.Helper()
	d := f.Driver.(*waitDriver)
	options := d.Options
	change(&options)
	s, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func aliasesOf(t *testing.T, f conformance.RuntimeFixture, read func(storage.Tx) (domain.Wait, []domain.AcceptedEvent, error)) (domain.Wait, []domain.AcceptedEvent) {
	t.Helper()
	var w domain.Wait
	var aliases []domain.AcceptedEvent
	if err := f.Store.Transact(context.Background(), storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		var er error
		w, aliases, er = read(tx)
		return er
	}); err != nil {
		t.Fatal(err)
	}
	return w, aliases
}

func TestDependencyCatchesUpFromTheArchive(t *testing.T) {
	f, read, request, archive, source := retiredDependency(t, "met")
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = archive })
	if err := s.Subscribe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	w, aliases := aliasesOf(t, f, read)
	if len(aliases) != 1 || aliases[0].Event.CausationID != source || w.WakeEventID != aliases[0].Event.ID {
		t.Fatalf("a met dependency on a retired Event did not wake: wait %+v aliases %+v", w, aliases)
	}
}

// The negative control: the live log alone can't see the retired Event.
func TestDependencyWithoutTheArchiveMissesARetiredEvent(t *testing.T) {
	f, read, request, _, _ := retiredDependency(t, "blind")
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = nil })
	if err := s.Subscribe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, aliases := aliasesOf(t, f, read); len(aliases) != 0 {
		t.Fatalf("the live log answered for a retired Event: %+v", aliases)
	}
}

func TestDependencyFilterSeesTheArchivedPayload(t *testing.T) {
	f, read, request, archive, _ := retiredDependency(t, "filtered")
	request.Filter = &domain.Payload{Schema: "fixture.filter.v1", Data: []byte(`{}`)}
	var seen []string
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) {
		o.EventArchive = archive
		o.AliasFilter = func(_ string, _ domain.Payload, e domain.Event) (bool, error) {
			seen = append(seen, string(e.Data.Data))
			return string(e.Data.Data) == archivedPayload, nil
		}
	})
	if err := s.Subscribe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, aliases := aliasesOf(t, f, read); len(aliases) != 1 {
		t.Fatalf("filter saw %q and the dependency did not match: %+v", seen, aliases)
	}
}

func TestDependencyHoldsItsPlaceWhenTheArchiveFails(t *testing.T) {
	ctx := context.Background()
	f, read, request, archive, _ := retiredDependency(t, "held")
	fail := true
	s := archiveScheduler(t, f, func(o *runtime.SchedulerOptions) { o.EventArchive = failingArchive{archive, &fail} })
	if err := s.Subscribe(ctx, request); err != nil {
		t.Fatalf("an installed dependency should wait for the next pass: %v", err)
	}
	if err := s.ReconcileSubscriptions(ctx, 10); err == nil {
		t.Fatal("a failed archive read was not reported")
	}
	if _, aliases := aliasesOf(t, f, read); len(aliases) != 0 {
		t.Fatalf("woke without the archive: %+v", aliases)
	}
	fail = false
	if err := s.ReconcileSubscriptions(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, aliases := aliasesOf(t, f, read); len(aliases) != 1 {
		t.Fatalf("the scan passed the archived Event by during the outage: %+v", aliases)
	}
}
