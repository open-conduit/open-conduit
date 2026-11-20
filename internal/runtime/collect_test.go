package runtime_test

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// listedArtifacts is a storage.ArtifactStore of metadata alone: enough to
// list and delete, which is all collection and retention use.
type listedArtifacts struct {
	mu      sync.Mutex
	items   map[string]storage.StoredArtifact
	deleted []string
}

func newListedArtifacts() *listedArtifacts {
	return &listedArtifacts{items: map[string]storage.StoredArtifact{}}
}
func (a *listedArtifacts) add(id string, at time.Time) domain.ArtifactRef {
	a.mu.Lock()
	defer a.mu.Unlock()
	ref := domain.ArtifactRef{ID: id, Size: 1, MediaType: "text/plain"}
	a.items[id] = storage.StoredArtifact{Ref: ref, PublishedAt: at}
	return ref
}
func (a *listedArtifacts) Create(context.Context, string) (storage.ArtifactWriter, error) {
	return nil, storage.ErrInvalid
}
func (a *listedArtifacts) Open(context.Context, storage.ArtifactRef) (io.ReadCloser, error) {
	return nil, storage.ErrNotFound
}
func (a *listedArtifacts) Import(context.Context, storage.ArtifactRef, io.Reader) error {
	return storage.ErrInvalid
}
func (a *listedArtifacts) Close() error { return nil }
func (a *listedArtifacts) Delete(_ context.Context, ref storage.ArtifactRef) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.items[ref.ID]; ok {
		delete(a.items, ref.ID)
		a.deleted = append(a.deleted, ref.ID)
	}
	return nil
}
func (a *listedArtifacts) List(_ context.Context, after string, limit int) ([]storage.StoredArtifact, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.items))
	for id := range a.items {
		if id > after {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := []storage.StoredArtifact{}
	for _, id := range ids {
		out = append(out, a.items[id])
	}
	return out, nil
}
func (a *listedArtifacts) taken() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]string(nil), a.deleted...)
	a.deleted = nil
	sort.Strings(out)
	return out
}

// finishedWithArtifacts runs one attempt of a new Work to completion with
// refs as its result's artifacts.
func finishedWithArtifacts(t *testing.T, f conformance.RuntimeFixture, d *waitDriver, name string, refs []domain.ArtifactRef) domain.ID {
	t.Helper()
	ctx := context.Background()
	work := seedSchedulerWork(t, f, name)
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Scheduler.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "done"}, Artifacts: refs}); err != nil {
		t.Fatal(err)
	}
	return attempt.Lease.ExecutionID
}

// A live retention hold defers every content release, and only content:
// the sweep still records expirations, and releases them once the hold is
// gone or lapses. A lapsed hold is removed.
func TestRetentionHoldDefersContentRelease(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	store := newListedArtifacts()
	refs := []domain.ArtifactRef{store.add("held-a", f.Clock.Now()), store.add("held-b", f.Clock.Now())}
	id := finishedWithArtifacts(t, f, d, "held", refs)
	options := d.Options
	options.Artifacts, options.KeptFinished = store, time.Hour
	sweeper, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	hold := func(fn func(storage.Tx) error) {
		t.Helper()
		if err := f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, fn); err != nil {
			t.Fatal(err)
		}
	}
	f.Clock.Advance(2 * time.Hour)
	hold(func(tx storage.Tx) error {
		return runtime.HoldRetention(tx, "backup-1", f.Clock.Now().Add(time.Minute))
	})
	result, err := sweeper.SweepRetention(ctx, 10)
	if err != nil || !result.Held || len(result.Expired) != 1 || result.Expired[0] != id || len(result.Released) != 0 || result.Pending != 1 {
		t.Fatalf("held sweep: %+v %v", result, err)
	}
	if got := store.taken(); len(got) != 0 {
		t.Fatalf("a held sweep deleted %v", got)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		status, e := runtime.RetentionInTx(tx, id)
		if e != nil || !status.Expired || status.ArtifactsReleased {
			t.Errorf("held status %+v %v", status, e)
		}
		return e
	})
	// Released, the next sweep deletes the content.
	hold(func(tx storage.Tx) error { return runtime.ReleaseRetention(tx, "backup-1") })
	if result, err = sweeper.SweepRetention(ctx, 10); err != nil || result.Held || len(result.Released) != 1 {
		t.Fatalf("released sweep: %+v %v", result, err)
	}
	if got := store.taken(); !reflect.DeepEqual(got, []string{"held-a", "held-b"}) {
		t.Fatalf("released sweep deleted %v", got)
	}

	// A hold its holder never released lapses at its expiry, and goes.
	more := []domain.ArtifactRef{store.add("lapsed", f.Clock.Now())}
	finishedWithArtifacts(t, f, d, "lapsed", more)
	f.Clock.Advance(2 * time.Hour)
	hold(func(tx storage.Tx) error { return runtime.HoldRetention(tx, "crashed", f.Clock.Now().Add(time.Minute)) })
	if result, err = sweeper.SweepRetention(ctx, 10); err != nil || !result.Held {
		t.Fatalf("sweep under a live hold: %+v %v", result, err)
	}
	f.Clock.Advance(time.Minute)
	if result, err = sweeper.SweepRetention(ctx, 10); err != nil || result.Held || len(result.Released) != 1 {
		t.Fatalf("sweep after the hold lapsed: %+v %v", result, err)
	}
	if got := store.taken(); !reflect.DeepEqual(got, []string{"lapsed"}) {
		t.Fatalf("sweep after the hold lapsed deleted %v", got)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		rows, e := tx.List("core.retention.hold", "")
		if e == nil && len(rows) != 0 {
			t.Errorf("lapsed hold kept: %+v", rows)
		}
		return e
	})
	hold(func(tx storage.Tx) error {
		if e := runtime.HoldRetention(tx, "", f.Clock.Now()); e != storage.ErrInvalid {
			t.Errorf("hold without an ID: %v", e)
		}
		if e := runtime.HoldRetention(tx, "zero", time.Time{}); e != storage.ErrInvalid {
			t.Errorf("hold without an expiry: %v", e)
		}
		return nil
	})
}

// Collection deletes aged content no attempt references, page by page, and
// keeps referenced content (latest or earlier, expired or not) and content
// still within its grace.
func TestCollectArtifactsSparesReferencedContent(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	store := newListedArtifacts()
	old := f.Clock.Now()
	// More orphans than one page holds, interleaved with referenced IDs.
	var orphans []string
	for i := 0; i < 600; i++ {
		id := fmt.Sprintf("orphan-%03d", i)
		store.add(id, old)
		orphans = append(orphans, id)
	}
	referenced := []domain.ArtifactRef{store.add("orphan-150-kept", old), store.add("orphan-450-kept", old)}
	finishedWithArtifacts(t, f, d, "collect", referenced)
	f.Clock.Advance(time.Hour)
	fresh := store.add("orphan-fresh", f.Clock.Now())
	result, err := d.Scheduler.CollectArtifacts(ctx, store, f.Clock.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.Listed != 603 || result.Deleted != 600 {
		t.Fatalf("collect: %+v", result)
	}
	if got := store.taken(); !reflect.DeepEqual(got, orphans) {
		t.Fatalf("collect deleted %d: %v", len(got), got)
	}
	left, err := store.List(ctx, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, item := range left {
		kept = append(kept, item.Ref.ID)
	}
	if want := []string{"orphan-150-kept", "orphan-450-kept", fresh.ID}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	if _, err = d.Scheduler.CollectArtifacts(ctx, store, time.Time{}); err != storage.ErrInvalid {
		t.Fatalf("collect without a cutoff: %v", err)
	}
}
