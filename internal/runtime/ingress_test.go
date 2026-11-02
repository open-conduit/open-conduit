package runtime_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

var ingressTime = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

const changedFamily = "fixture.changed.v1"
const oneShotFamily = "fixture.receipt.v1"

// fixtureIngress emulates readable external state. Pages are selected by cursor;
// every request is recorded so restart tests can inspect the actual persisted
// generation, cursor and complete checkpoint handed back to the adapter.
type fixtureIngress struct {
	mu      sync.Mutex
	spec    extension.IngressSpec
	pushed  extension.PushResult
	pages   map[string]extension.Snapshot
	failure error
	calls   []extension.Scan
}

func newFixtureIngress() *fixtureIngress {
	return &fixtureIngress{
		spec:  extension.IngressSpec{Families: []extension.EventFamily{{Type: changedFamily, ReDerivable: true}, {Type: oneShotFamily, Loss: "An arbitrary POST receipt has no backing history; unaccepted receipts are lost when push is unavailable."}}},
		pages: map[string]extension.Snapshot{},
	}
}
func (f *fixtureIngress) Spec() extension.IngressSpec { return f.spec }
func (f *fixtureIngress) Push(ctx context.Context, _ extension.Source, _ extension.Delivery) (extension.PushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return extension.PushResult{}, err
	}
	return f.pushed, f.failure
}
func (f *fixtureIngress) Reconcile(ctx context.Context, scan extension.Scan) (extension.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, scan)
	if err := ctx.Err(); err != nil {
		return extension.Snapshot{}, err
	}
	if f.failure != nil {
		return extension.Snapshot{}, f.failure
	}
	p, ok := f.pages[scan.Cursor]
	if !ok {
		return extension.Snapshot{}, fmt.Errorf("unexpected fixture cursor %q", scan.Cursor)
	}
	return p, nil
}
func (f *fixtureIngress) lastScan(t *testing.T) extension.Scan {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("adapter never reconciled")
	}
	return f.calls[len(f.calls)-1]
}
func fixtureObservation(source extension.Source, id, version string) extension.Observation {
	return extension.Observation{ID: id, Version: version, Event: domain.Event{ID: domain.ID(source.Namespace + ":" + id + ":" + version), Source: source.Namespace, Type: changedFamily, DeliveryID: id + "/" + version, OccurredAt: ingressTime, ReceivedAt: ingressTime, CorrelationKey: &domain.CorrelationKey{Source: source.Namespace, ExternalID: id}, Data: domain.Payload{Schema: "fixture.item.v1", Data: []byte(`{"opaque":true}`)}}}
}

// Keep fixture errors stable and inspectable without service/network behavior.
var errIngressFailure = errors.New("fixture scan unavailable")

type ingressClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *ingressClock) Now() time.Time   { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *ingressClock) Set(at time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.at = at }
func openIngress(t *testing.T, path string, clock *ingressClock) (*sqlite.Store, *runtime.Ingress) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	err = db.Transact(context.Background(), storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		for _, id := range []domain.ID{"scope_a", "scope_b"} {
			if _, e := tx.Scope(id); errors.Is(e, storage.ErrNotFound) {
				if e = tx.PutScope(domain.Scope{ID: id, Name: string(id)}, 0); e != nil {
					return e
				}
			} else if e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ingress, err := runtime.NewIngress(db, clock.Now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return db, ingress
}
func registerIngress(t *testing.T, r *runtime.Ingress, name string, source extension.Source, f *fixtureIngress) {
	t.Helper()
	if err := r.Register(context.Background(), name, source, f, time.Minute); err != nil {
		t.Fatal(err)
	}
}
func ingressEvents(t *testing.T, db storage.Store) []domain.AcceptedEvent {
	t.Helper()
	var events []domain.AcceptedEvent
	if err := db.Transact(context.Background(), storage.Guard{Now: ingressTime}, func(tx storage.Tx) error { var err error; events, err = tx.Events(0, 0); return err }); err != nil {
		t.Fatal(err)
	}
	return events
}
func ingressStatus(t *testing.T, r *runtime.Ingress, name string) runtime.IngressStatus {
	t.Helper()
	statuses, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Name == name {
			return s
		}
	}
	t.Fatal("missing ingress status", name)
	return runtime.IngressStatus{}
}
func reconcilePage(t *testing.T, r *runtime.Ingress, name string) runtime.ReconcileResult {
	t.Helper()
	result, err := r.Reconcile(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDurableIngressReconcileOnlyAndGenerationRecovery(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, r := openIngress(t, path, clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", source, f)
	a1, b1 := fixtureObservation(source, "a", "v1"), fixtureObservation(source, "b", "v1")
	f.pages[""] = extension.Snapshot{ScanID: "baseline", Complete: true, Checkpoint: "baseline_checkpoint", Observations: []extension.Observation{a1, b1}}
	result := reconcilePage(t, r, "source_a")
	if !result.Complete || len(result.Accepted) != 2 {
		t.Fatalf("reconcile-only failed: %+v", result)
	}
	initial := ingressEvents(t, db)
	if len(initial) != 2 {
		t.Fatalf("reconcile-only events: %+v", initial)
	}
	// Pages stage separately from authoritative observations. The old complete
	// checkpoint remains intact until every page commits successfully.
	a2 := fixtureObservation(source, "a", "v2")
	f.pages[""] = extension.Snapshot{ScanID: "generation_two", NextCursor: "page_two", Checkpoint: "page_one_checkpoint", Observations: []extension.Observation{a2}}
	result = reconcilePage(t, r, "source_a")
	if result.Complete || len(result.Accepted) != 0 {
		t.Fatalf("partial page published prematurely: %+v", result)
	}
	status := ingressStatus(t, r, "source_a")
	if status.Checkpoint != "baseline_checkpoint" || status.Cursor != "page_two" || status.ScanID != "generation_two" {
		t.Fatalf("unsafe staged checkpoint: %+v", status)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, r = openIngress(t, path, clock)
	registerIngress(t, r, "source_a", source, f)
	f.failure = errIngressFailure
	if _, err := r.Reconcile(ctx, "source_a"); err == nil {
		t.Fatalf("failed page ignored: %v", err)
	}
	f.failure = nil
	status = ingressStatus(t, r, "source_a")
	if status.Checkpoint != "baseline_checkpoint" || status.Cursor != "page_two" {
		t.Fatalf("failed page advanced checkpoint: %+v", status)
	}
	scan := f.lastScan(t)
	if scan.ScanID != "generation_two" || scan.Cursor != "page_two" {
		t.Fatalf("restart lost staged generation: %+v", scan)
	}
	// A provider switching snapshot tokens midway cannot mix generations.
	f.pages["page_two"] = extension.Snapshot{ScanID: "wrong_generation", Complete: true, Checkpoint: "wrong"}
	if _, err := r.Reconcile(ctx, "source_a"); err == nil {
		t.Fatal("mixed generation accepted")
	}
	if got := len(ingressEvents(t, db)); got != 2 {
		t.Fatalf("failed page emitted %d events", got)
	}
	f.pages["page_two"] = extension.Snapshot{ScanID: "generation_two", Complete: true, Checkpoint: "complete_two"}
	result = reconcilePage(t, r, "source_a")
	if !result.Complete || len(result.Accepted) != 2 {
		t.Fatalf("complete diff should emit changed a and absent b: %+v", result)
	}
	events := ingressEvents(t, db)
	changed, absent := 0, 0
	for _, e := range events {
		if e.Event.Type == changedFamily {
			changed++
		}
		if e.Event.Type == "core.ingress.absent" {
			absent++
		}
	}
	if changed != 3 || absent != 1 {
		t.Fatalf("diff counts changed=%d absent=%d", changed, absent)
	}
	status = ingressStatus(t, r, "source_a")
	if status.Checkpoint != "complete_two" || status.ScanID != "" || status.Cursor != "" {
		t.Fatalf("generation did not publish atomically: %+v", status)
	}
	f.pages[""] = extension.Snapshot{ScanID: "same_state", Complete: true, Checkpoint: "complete_same", Observations: []extension.Observation{a2}}
	result = reconcilePage(t, r, "source_a")
	if len(result.Accepted) != 0 || len(ingressEvents(t, db)) != 4 {
		t.Fatalf("unchanged snapshot repeated actions: %+v", result)
	}
	// Truncated empty output is not proof that the sole authoritative item vanished.
	f.pages[""] = extension.Snapshot{ScanID: "truncated", Complete: false}
	result = reconcilePage(t, r, "source_a")
	if !result.Partial || len(result.Accepted) != 0 || len(ingressEvents(t, db)) != 4 {
		t.Fatalf("truncation inferred deletion: %+v", result)
	}
}

func TestDurableIngressOverlapLatePushAndIsolation(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	sourceA := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	sourceB := extension.Source{Namespace: "fixture/account-b", ScopeID: "scope_b"}
	a, b := newFixtureIngress(), newFixtureIngress()
	registerIngress(t, r, "source_a", sourceA, a)
	registerIngress(t, r, "source_b", sourceB, b)
	a1 := fixtureObservation(sourceA, "shared_item", "v1")
	a.pushed = extension.PushResult{Observations: []extension.Observation{a1}}
	accepted, err := r.Push(ctx, "source_a", extension.Delivery{ID: "push_v1", ReceivedAt: ingressTime})
	if err != nil || len(accepted) != 1 {
		t.Fatalf("push: %+v %v", accepted, err)
	}
	original := accepted[0]
	a.pages[""] = extension.Snapshot{ScanID: "overlap", Complete: true, Observations: []extension.Observation{a1}}
	reconcilePage(t, r, "source_a")
	if len(ingressEvents(t, db)) != 1 {
		t.Fatal("push/reconcile overlap duplicated domain input")
	}
	a2 := fixtureObservation(sourceA, "shared_item", "v2")
	a.pages[""] = extension.Snapshot{ScanID: "newer", Complete: true, Observations: []extension.Observation{a2}}
	reconcilePage(t, r, "source_a")
	// Receipt/internal IDs are not delivery identity; late v1 keeps the original
	// immutable accepted envelope and cannot rewind the complete v2 observation.
	a1.Event.ID = "alternate_internal_id"
	a1.Event.ReceivedAt = ingressTime.Add(time.Hour)
	a.pushed = extension.PushResult{Observations: []extension.Observation{a1}}
	accepted, err = r.Push(ctx, "source_a", extension.Delivery{ID: "late_v1", ReceivedAt: ingressTime.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 1 || !reflect.DeepEqual(accepted[0], original) {
		t.Fatalf("late duplicate rewrote original: %+v", accepted)
	}
	a.pages[""] = extension.Snapshot{ScanID: "after_late", Complete: true, Observations: []extension.Observation{a2}}
	result := reconcilePage(t, r, "source_a")
	if len(result.Accepted) != 0 || len(ingressEvents(t, db)) != 2 {
		t.Fatalf("late push rewound authoritative observation: %+v", result)
	}
	b1 := fixtureObservation(sourceB, "shared_item", "v1")
	b.pages[""] = extension.Snapshot{ScanID: "other_account", Complete: true, Observations: []extension.Observation{b1}}
	reconcilePage(t, r, "source_b")
	if len(ingressEvents(t, db)) != 3 {
		t.Fatal("source-local item/version collided across accounts")
	}
	a.pushed = extension.PushResult{Observations: []extension.Observation{b1}}
	if _, err = r.Push(ctx, "source_a", extension.Delivery{ID: "forged_source", ReceivedAt: ingressTime}); err == nil {
		t.Fatal("cross-source push accepted")
	}
	status := ingressStatus(t, r, "source_a")
	foundLoss := false
	for _, family := range status.Families {
		if family.Type == oneShotFamily {
			foundLoss = !family.ReDerivable && family.Loss != ""
		}
	}
	if !foundLoss {
		t.Fatalf("health/config inspection hides non-recoverable ingress: %+v", status)
	}
}

func TestDurableIngressReconcileDueSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, r := openIngress(t, path, clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	f.pages[""] = extension.Snapshot{ScanID: "first", Complete: true, Checkpoint: "complete_one", Observations: []extension.Observation{fixtureObservation(source, "a", "v1")}}
	registerIngress(t, r, "source_a", source, f)
	initial := ingressStatus(t, r, "source_a")
	clock.Set(initial.NextDue)
	results, err := r.ReconcileDue(ctx, 1)
	if err != nil || len(results) != 1 || !results[0].Complete {
		t.Fatalf("due scan: %+v %v", results, err)
	}
	complete := ingressStatus(t, r, "source_a")
	if !complete.NextDue.After(clock.Now()) {
		t.Fatalf("reconcile did not schedule next occurrence: %+v", complete)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, r = openIngress(t, path, clock)
	registerIngress(t, r, "source_a", source, f)
	reopened := ingressStatus(t, r, "source_a")
	if !reopened.NextDue.Equal(complete.NextDue) || reopened.Checkpoint != "complete_one" {
		t.Fatalf("restart reset reconcile cadence/checkpoint: %+v", reopened)
	}
	results, err = r.ReconcileDue(ctx, 1)
	if err != nil || len(results) != 0 {
		t.Fatalf("restart scanned before durable due time: %+v %v", results, err)
	}
	clock.Set(reopened.NextDue)
	f.pages[""] = extension.Snapshot{ScanID: "second", Complete: true, Checkpoint: "complete_two", Observations: []extension.Observation{fixtureObservation(source, "a", "v1")}}
	results, err = r.ReconcileDue(ctx, 1)
	if err != nil || len(results) != 1 || len(results[0].Accepted) != 0 {
		t.Fatalf("scheduled unchanged state emitted events: %+v %v", results, err)
	}
	if scan := f.lastScan(t); scan.Checkpoint != "complete_one" {
		t.Fatalf("adapter missed last complete checkpoint: %+v", scan)
	}
	if got := len(ingressEvents(t, db)); got != 1 {
		t.Fatalf("scheduled replay events=%d", got)
	}
}

func TestIngressCommitBeforeAcknowledgementAndPendingRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestIngressCrashHelper$")
	cmd.Env = append(os.Environ(), "CONDUIT_INGRESS_HELPER=1", "CONDUIT_INGRESS_DB="+path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	// This private pipe is the durable-commit barrier. No transport ack has been
	// sent: the helper waits for an explicit input that the parent never sends.
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() {
		t.Fatalf("missing commit barrier: %s", stderr.String())
	}
	var committed domain.AcceptedEvent
	if err = json.Unmarshal(scanner.Bytes(), &committed); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, path, clock)
	retry := committed.Event
	retry.ID = "retried_internal_id"
	retry.ReceivedAt = ingressTime.Add(time.Hour)
	accepted, added, err := r.Accept(ctx, "scope_a", retry)
	if err != nil || added || !reflect.DeepEqual(accepted, committed) {
		t.Fatalf("lost response duplicate unsafe: %+v %v %v", accepted, added, err)
	}
	pending, err := r.Pending(ctx, 0, 10)
	if err != nil || len(pending.Items) != 1 || pending.Items[0].Event.Event.ID != committed.Event.ID || pending.Items[0].ScopeID != "scope_a" {
		t.Fatalf("restart lost accepted-unrouted input: %+v %v", pending, err)
	}
	if err = db.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error { return tx.MarkRouted(committed.Event.ID, "", "drop") }); err != nil {
		t.Fatal(err)
	}
	pending, err = r.Pending(ctx, 0, 10)
	if err != nil || len(pending.Items) != 0 {
		t.Fatalf("committed processing checkpoint replayed: %+v %v", pending, err)
	}
}
func TestIngressCrashHelper(t *testing.T) {
	if os.Getenv("CONDUIT_INGRESS_HELPER") == "" {
		return
	}
	clock := &ingressClock{at: ingressTime}
	_, r := openIngress(t, os.Getenv("CONDUIT_INGRESS_DB"), clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	event := fixtureObservation(source, "crash_item", "v1").Event
	accepted, added, err := r.Accept(context.Background(), source.ScopeID, event)
	if err != nil || !added {
		t.Fatal(accepted, added, err)
	}
	data, err := json.Marshal(accepted)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(data))
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestDurableIngressRejectsIdentityAndOwnershipDrift(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, r := openIngress(t, path, clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", source, f)
	observation := fixtureObservation(source, "a", "v1")
	f.pushed = extension.PushResult{Observations: []extension.Observation{observation}}
	if _, err := r.Push(ctx, "source_a", extension.Delivery{ID: "first", ReceivedAt: ingressTime}); err != nil {
		t.Fatal(err)
	}
	forged := observation
	forged.Event.ID = "different_delivery_event"
	forged.Event.DeliveryID = "different_canonical_delivery"
	f.pushed = extension.PushResult{Observations: []extension.Observation{forged}}
	if _, err := r.Push(ctx, "source_a", extension.Delivery{ID: "drift", ReceivedAt: ingressTime}); err == nil {
		t.Fatal("same item/version acquired another canonical delivery identity")
	}
	// Namespace ownership survives a fresh runtime registration map and cannot
	// silently move to another scope after a process restart/configuration reload.
	_, other := openIngress(t, path, clock)
	foreign := source
	foreign.ScopeID = "scope_b"
	if err := other.Register(ctx, "other_owner", foreign, newFixtureIngress(), time.Minute); err == nil {
		t.Fatal("source namespace registered with another scope")
	}
	reserved := newFixtureIngress()
	reserved.spec = extension.IngressSpec{Families: []extension.EventFamily{{Type: "core.ingress.forged", ReDerivable: true}}}
	if err := r.Register(ctx, "reserved", extension.Source{Namespace: "fixture/reserved", ScopeID: "scope_a"}, reserved, time.Minute); err == nil {
		t.Fatal("extension registered reserved core family")
	}
	if got := len(ingressEvents(t, db)); got != 1 {
		t.Fatalf("rejected identity/owner changed accepted events: %d", got)
	}
}

func TestDurableIngressInvalidPagesDoNotPoisonStage(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", source, f)
	a1, a2, a3 := fixtureObservation(source, "a", "v1"), fixtureObservation(source, "a", "v2"), fixtureObservation(source, "a", "v3")
	f.pages[""] = extension.Snapshot{ScanID: "baseline", Complete: true, Checkpoint: "baseline", Observations: []extension.Observation{a1}}
	reconcilePage(t, r, "source_a")
	f.pages[""] = extension.Snapshot{ScanID: "next", NextCursor: "second", Checkpoint: "staged", Observations: []extension.Observation{a2}}
	reconcilePage(t, r, "source_a")
	for _, bad := range []extension.Snapshot{
		{ScanID: "next", NextCursor: "second", Checkpoint: "bad_repeat"},
		{ScanID: "next", Complete: true, Checkpoint: "bad_conflict", Observations: []extension.Observation{a2, a3}},
	} {
		f.pages["second"] = bad
		if _, err := r.Reconcile(ctx, "source_a"); err == nil {
			t.Fatal("malformed page accepted", bad)
		}
		status := ingressStatus(t, r, "source_a")
		if status.Cursor != "second" || status.ScanID != "next" || status.Checkpoint != "baseline" {
			t.Fatalf("bad page poisoned durable stage: %+v", status)
		}
		if len(ingressEvents(t, db)) != 1 {
			t.Fatal("bad page leaked domain input")
		}
	}
	f.pages["second"] = extension.Snapshot{ScanID: "next", Complete: true, Checkpoint: "complete"}
	result := reconcilePage(t, r, "source_a")
	if len(result.Accepted) != 1 || result.Accepted[0].Event.DeliveryID != "a/v2" {
		t.Fatalf("valid retry did not preserve original staged version: %+v", result)
	}
}

type blockedIngress struct {
	*fixtureIngress
	entered, release chan struct{}
	page             extension.Snapshot
}

func (f *blockedIngress) Reconcile(ctx context.Context, _ extension.Scan) (extension.Snapshot, error) {
	close(f.entered)
	select {
	case <-f.release:
		return f.page, nil
	case <-ctx.Done():
		return extension.Snapshot{}, ctx.Err()
	}
}
func TestDurableIngressStaleScanLeaseCannotPublish(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, first := openIngress(t, path, clock)
	_, second := openIngress(t, path, clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	held := &blockedIngress{fixtureIngress: newFixtureIngress(), entered: make(chan struct{}), release: make(chan struct{}), page: extension.Snapshot{ScanID: "old_scan", Complete: true, Observations: []extension.Observation{fixtureObservation(source, "a", "old")}}}
	if err := first.Register(ctx, "source_a", source, held, time.Minute); err != nil {
		t.Fatal(err)
	}
	newer := newFixtureIngress()
	newer.pages[""] = extension.Snapshot{ScanID: "new_scan", Complete: true, Checkpoint: "new_complete", Observations: []extension.Observation{fixtureObservation(source, "a", "new")}}
	registerIngress(t, second, "source_a", source, newer)
	var release sync.Once
	defer release.Do(func() { close(held.release) })
	done := make(chan error, 1)
	go func() { _, err := first.Reconcile(ctx, "source_a"); done <- err }()
	<-held.entered
	if err := second.RestartScan(ctx, "source_a", ""); !errors.Is(err, runtime.ErrIngressBusy) {
		t.Fatalf("live scan lease was reset: %v", err)
	}
	clock.Set(ingressTime.Add(2 * time.Minute))
	result := reconcilePage(t, second, "source_a")
	if !result.Complete {
		t.Fatal(result)
	}
	release.Do(func() { close(held.release) })
	if err := <-done; !errors.Is(err, runtime.ErrScanLeaseLost) {
		t.Fatalf("superseded provider committed: %v", err)
	}
	events := ingressEvents(t, db)
	if len(events) != 1 || events[0].Event.DeliveryID != "a/new" {
		t.Fatalf("stale scan leaked old event/state: %+v", events)
	}
	status := ingressStatus(t, second, "source_a")
	if status.Checkpoint != "new_complete" {
		t.Fatalf("stale callback changed completed checkpoint: %+v", status)
	}
}

func TestIngressRawUnboundPendingRemainsVisible(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	event := domain.Event{ID: "unbound", Source: "fixture/unbound", Type: changedFamily, DeliveryID: "unbound", OccurredAt: ingressTime, ReceivedAt: ingressTime}
	if err := db.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error { _, _, err := tx.AcceptEvent(event); return err }); err != nil {
		t.Fatal(err)
	}
	pending, err := r.Pending(ctx, 0, 10)
	if err != nil || len(pending.Items) != 1 {
		t.Fatal(pending, err)
	}
	if pending.Items[0].ScopeID != "" || pending.Items[0].Diagnostic != "unbound_scope" {
		t.Fatalf("unbound event acquired arbitrary scope: %+v", pending.Items[0])
	}
}

func TestIngressPendingIdentityConflictDoesNotHideIndependentWork(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	conflicted := fixtureObservation(source, "later_owned_elsewhere", "v1").Event
	if _, _, err := r.Accept(ctx, source.ScopeID, conflicted); err != nil {
		t.Fatal(err)
	}
	if err := db.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "other_scope_owner", ScopeID: "scope_b", Kind: "fixture.work", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{*conflicted.CorrelationKey}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	independent := fixtureObservation(source, "independent", "v1").Event
	if _, _, err := r.Accept(ctx, source.ScopeID, independent); err != nil {
		t.Fatal(err)
	}
	pending, err := r.Pending(ctx, 0, 10)
	if err != nil {
		t.Fatalf("one identity conflict blocked independent pending enumeration: %v", err)
	}
	if len(pending.Items) != 2 {
		t.Fatalf("pending items disappeared: %+v", pending)
	}
	if pending.Items[0].Event.Event.ID != conflicted.ID || pending.Items[0].Diagnostic != "identity_conflict" {
		t.Fatalf("conflicting identity not surfaced safely: %+v", pending.Items[0])
	}
	if pending.Items[1].Event.Event.ID != independent.ID || pending.Items[1].ScopeID != source.ScopeID || pending.Items[1].Diagnostic != "" {
		t.Fatalf("independent pending event blocked/changed: %+v", pending.Items[1])
	}
}

func TestIngressPushBatchValidationRollsBackEveryEvent(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", source, f)
	valid := fixtureObservation(source, "valid", "v1")
	invalid := fixtureObservation(source, "invalid", "v1")
	invalid.Event.Source = "fixture/wrong_source"
	f.pushed = extension.PushResult{Observations: []extension.Observation{valid, invalid}}
	if accepted, err := r.Push(ctx, "source_a", extension.Delivery{ID: "bad_batch", ReceivedAt: ingressTime}); err == nil || len(accepted) != 0 {
		t.Fatalf("partially acknowledged invalid delivery: %+v %v", accepted, err)
	}
	if len(ingressEvents(t, db)) != 0 {
		t.Fatal("valid prefix leaked from rolled-back delivery")
	}
	f.pushed = extension.PushResult{Observations: []extension.Observation{valid}}
	accepted, err := r.Push(ctx, "source_a", extension.Delivery{ID: "retry_valid", ReceivedAt: ingressTime})
	if err != nil || len(accepted) != 1 || len(ingressEvents(t, db)) != 1 {
		t.Fatalf("rolled-back canonical checkpoint poisoned retry: %+v %v", accepted, err)
	}
}

func TestIngressAbsenceRetainsCanonicalCorrelationAndWork(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", source, f)
	key := domain.CorrelationKey{Source: source.Namespace, ExternalID: "thread_identity_not_item_id"}
	if err := db.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "existing_work", ScopeID: source.ScopeID, Kind: "fixture.work", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{key}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	observation := fixtureObservation(source, "provider_row_42", "v1")
	observation.Event.CorrelationKey = &key
	observation.Event.WorkID = "existing_work"
	f.pages[""] = extension.Snapshot{ScanID: "with_item", Complete: true, Observations: []extension.Observation{observation}}
	reconcilePage(t, r, "source_a")
	f.pages[""] = extension.Snapshot{ScanID: "without_item", Complete: true}
	result := reconcilePage(t, r, "source_a")
	if len(result.Accepted) != 1 || result.Accepted[0].Event.Type != "core.ingress.absent" {
		t.Fatalf("absence not accepted: %+v", result)
	}
	absent := result.Accepted[0].Event
	if !reflect.DeepEqual(absent.CorrelationKey, &key) || absent.WorkID != "existing_work" {
		t.Fatalf("absence guessed provider item identity instead of canonical routing target: %+v", absent)
	}
	pending, err := r.Pending(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pending.Items {
		if p.Event.Event.ID == absent.ID {
			found = true
			if p.ScopeID != source.ScopeID || p.Diagnostic != "" {
				t.Fatalf("absence lost authoritative scope: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("absence missing from durable routing backlog")
	}
	if err := db.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error {
		work, err := tx.Work("existing_work")
		if err != nil {
			return err
		}
		if work.State != domain.WorkReady || !reflect.DeepEqual(work.CorrelationKeys, []domain.CorrelationKey{key}) {
			t.Fatalf("absence mutated Work outside route processing: %+v", work)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIngressExpiredGenerationRestartPreservesAuthoritativeState(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, r := openIngress(t, path, clock)
	source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", source, f)
	a1, b1, a2 := fixtureObservation(source, "a", "v1"), fixtureObservation(source, "b", "v1"), fixtureObservation(source, "a", "v2")
	f.pages[""] = extension.Snapshot{ScanID: "baseline", Complete: true, Checkpoint: "last_complete", Observations: []extension.Observation{a1, b1}}
	reconcilePage(t, r, "source_a")
	f.pages[""] = extension.Snapshot{ScanID: "expired_generation", NextCursor: "expired_cursor", Checkpoint: "page_only", Observations: []extension.Observation{a2}}
	reconcilePage(t, r, "source_a")
	f.failure = errIngressFailure
	if _, err := r.Reconcile(ctx, "source_a"); err == nil {
		t.Fatal("expired provider generation did not fail")
	}
	f.failure = nil
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, r = openIngress(t, path, clock)
	registerIngress(t, r, "source_a", source, f)
	if err := r.RestartScan(ctx, "source_a", "wrong_generation"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale generation reset accepted: %v", err)
	}
	before := ingressStatus(t, r, "source_a")
	if before.ScanID != "expired_generation" || before.Cursor != "expired_cursor" || before.Checkpoint != "last_complete" {
		t.Fatalf("failed reset changed stage: %+v", before)
	}
	if err := r.RestartScan(ctx, "source_a", "expired_generation"); err != nil {
		t.Fatal(err)
	}
	after := ingressStatus(t, r, "source_a")
	if after.ScanID != "" || after.Cursor != "" || after.Checkpoint != "last_complete" || after.NextDue.After(clock.Now()) {
		t.Fatalf("reset discarded complete authority or cadence: %+v", after)
	}
	// Discarding a staged generation cannot erase its known canonical identity.
	drift := a2
	drift.Event.ID = "drift_after_reset"
	drift.Event.DeliveryID = "drift_after_reset"
	f.pushed = extension.PushResult{Observations: []extension.Observation{drift}}
	if _, err := r.Push(ctx, "source_a", extension.Delivery{ID: "drift_after_reset", ReceivedAt: ingressTime}); err == nil {
		t.Fatal("reset forgot canonical item/version identity")
	}
	f.pages[""] = extension.Snapshot{ScanID: "replacement", Complete: true, Checkpoint: "new_complete", Observations: []extension.Observation{a1, b1}}
	result := reconcilePage(t, r, "source_a")
	if !result.Complete || len(result.Accepted) != 0 || len(ingressEvents(t, db)) != 2 {
		t.Fatalf("restart lost previous snapshot and repeated/missed changes: %+v", result)
	}
}

func TestIngressMalformedPartialDoesNotPoisonCanonicalLedger(t *testing.T) {
	for _, invalid := range []string{"timestamp", "schema", "explicit_null"} {
		t.Run(invalid, func(t *testing.T) {
			ctx := context.Background()
			clock := &ingressClock{at: ingressTime}
			db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
			source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
			f := newFixtureIngress()
			registerIngress(t, r, "source_a", source, f)
			valid := fixtureObservation(source, "a", "v1")
			bad := valid
			switch invalid {
			case "timestamp":
				bad.Event.OccurredAt = time.Time{}
			case "schema":
				bad.Event.Data.Schema = "invalid"
			case "explicit_null":
				bad.Event.Data = domain.Payload{Data: []byte("null")}
				valid.Event.Data = domain.Payload{}
			}
			f.pages[""] = extension.Snapshot{ScanID: "retryable", NextCursor: "second", Checkpoint: "bad_page", Observations: []extension.Observation{bad}}
			if _, err := r.Reconcile(ctx, "source_a"); err == nil {
				t.Fatal("malformed incomplete page accepted")
			}
			status := ingressStatus(t, r, "source_a")
			if status.ScanID != "" || status.Cursor != "" || status.Checkpoint != "" {
				t.Fatalf("invalid page persisted staging/checkpoint: %+v", status)
			}
			f.pages[""] = extension.Snapshot{ScanID: "retryable", Complete: true, Checkpoint: "corrected", Observations: []extension.Observation{valid}}
			result := reconcilePage(t, r, "source_a")
			if !result.Complete || len(result.Accepted) != 1 || len(ingressEvents(t, db)) != 1 {
				t.Fatalf("invalid page poisoned corrected same item/version: %+v", result)
			}
		})
	}
}

func TestIngressDurableRegistrationNameCannotChangeNamespace(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, r := openIngress(t, path, clock)
	registerIngress(t, r, "same_name", extension.Source{Namespace: "fixture/original", ScopeID: "scope_a"}, newFixtureIngress())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, fresh := openIngress(t, path, clock)
	if err := fresh.Register(ctx, "same_name", extension.Source{Namespace: "fixture/replacement", ScopeID: "scope_a"}, newFixtureIngress(), time.Minute); !errors.Is(err, runtime.ErrIngressNameTaken) || !errors.Is(err, extension.ErrConflict) {
		t.Fatalf("durable registration name changed namespace: %v", err)
	}
}

// A renamed configuration entry keeps its namespace and Scope; its stored
// source and checkpoint move to the new name instead of failing to register.
func TestIngressRenamedEntryKeepsStoredSource(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, r := openIngress(t, path, clock)
	source := extension.Source{Namespace: "fixture/renamed", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "slack/scope_a/support", source, f)
	f.pushed = extension.PushResult{Observations: []extension.Observation{fixtureObservation(source, "a", "v1")}}
	if _, err := r.Push(ctx, "slack/scope_a/support", extension.Delivery{ID: "first", ReceivedAt: ingressTime}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, renamed := openIngress(t, path, clock)
	registerIngress(t, renamed, "slack/scope_a/slack", source, newFixtureIngress())
	if got := ingressStatus(t, renamed, "slack/scope_a/slack"); got.Source != source {
		t.Fatalf("renamed source = %+v", got.Source)
	}
	if got := len(ingressEvents(t, db)); got != 1 {
		t.Fatalf("rename changed accepted events: %d", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// The old name is released: it may now name another source, and the
	// namespace still refuses another Scope with a specific error.
	_, again := openIngress(t, path, clock)
	registerIngress(t, again, "slack/scope_a/support", extension.Source{Namespace: "fixture/other", ScopeID: "scope_a"}, newFixtureIngress())
	moved := source
	moved.ScopeID = "scope_b"
	if err := again.Register(ctx, "slack/scope_b/slack", moved, newFixtureIngress(), time.Minute); !errors.Is(err, runtime.ErrIngressNamespaceOwned) || !errors.Is(err, extension.ErrConflict) {
		t.Fatalf("namespace moved scope: %v", err)
	}
}

func TestIngressReconcileDueFairnessPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, r := openIngress(t, path, clock)
	sourceA := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	sourceB := extension.Source{Namespace: "fixture/account-b", ScopeID: "scope_b"}
	a, b := newFixtureIngress(), newFixtureIngress()
	a.pages[""] = extension.Snapshot{ScanID: "long_scan", NextCursor: "second", Observations: []extension.Observation{fixtureObservation(sourceA, "a", "v1")}}
	a.pages["second"] = extension.Snapshot{ScanID: "long_scan", NextCursor: "third"}
	b.pages[""] = extension.Snapshot{ScanID: "short_scan", Complete: true, Checkpoint: "peer_complete", Observations: []extension.Observation{fixtureObservation(sourceB, "b", "v1")}}
	registerIngress(t, r, "a_first", sourceA, a)
	registerIngress(t, r, "b_second", sourceB, b)
	first, err := r.ReconcileDue(ctx, 1)
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	if ingressStatus(t, r, "a_first").Cursor != "second" {
		t.Fatal("first bounded poll did not stage first source")
	}
	if len(b.calls) != 0 {
		t.Fatal("poll exceeded page limit")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, r = openIngress(t, path, clock)
	registerIngress(t, r, "a_first", sourceA, a)
	registerIngress(t, r, "b_second", sourceB, b)
	second, err := r.ReconcileDue(ctx, 1)
	if err != nil || len(second) != 1 {
		t.Fatal(second, err)
	}
	if ingressStatus(t, r, "b_second").Checkpoint != "peer_complete" {
		t.Fatal("restarted fixed-clock poll starved due peer behind paginated source")
	}
	if ingressStatus(t, r, "a_first").Cursor != "second" {
		t.Fatal("second poll selected first source again instead of due peer")
	}
}

func TestIngressAbsentPayloadSurvivesCompleteAndStagedReopen(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged_%v", staged), func(t *testing.T) {
			clock := &ingressClock{at: ingressTime}
			path := filepath.Join(t.TempDir(), "conduit.db")
			db, r := openIngress(t, path, clock)
			source := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
			f := newFixtureIngress()
			registerIngress(t, r, "source_a", source, f)
			observation := fixtureObservation(source, "no_payload", "v1")
			observation.Event.Data = domain.Payload{}
			if staged {
				f.pages[""] = extension.Snapshot{ScanID: "no_payload", NextCursor: "second", Observations: []extension.Observation{observation}}
				result := reconcilePage(t, r, "source_a")
				if len(result.Accepted) != 0 {
					t.Fatal("partial page published")
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, r = openIngress(t, path, clock)
				registerIngress(t, r, "source_a", source, f)
				f.pages["second"] = extension.Snapshot{ScanID: "no_payload", Complete: true}
			} else {
				f.pages[""] = extension.Snapshot{ScanID: "no_payload", Complete: true, Observations: []extension.Observation{observation}}
			}
			result := reconcilePage(t, r, "source_a")
			if !result.Complete || len(result.Accepted) != 1 {
				t.Fatalf("absent payload rejected after persistence: %+v", result)
			}
			event := ingressEvents(t, db)[0].Event
			if event.Data.Schema != "" || len(event.Data.Data) != 0 {
				t.Fatalf("absent payload became literal null: %+v", event.Data)
			}
		})
	}
}
