package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func TestBundleRoundTripAndRefusal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, err := artifacts.Open(filepath.Join(root, "live-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	w, err := files.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "durable transcript"); err != nil {
		t.Fatal(err)
	}
	ref, err := w.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "thread"}
	var lease domain.LeaseRef
	err = db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return fmt.Errorf("scope: %w", err)
		}
		if err := tx.PutWork(domain.Work{ID: "work", Kind: "fixture.work", State: domain.WorkReady, ScopeID: "scope", CorrelationKeys: []domain.CorrelationKey{key}}, 0); err != nil {
			return fmt.Errorf("work: %w", err)
		}
		event := domain.Event{ID: "trigger", Type: "fixture.start", Source: "fixture", DeliveryID: "trigger", OccurredAt: now, ReceivedAt: now}
		if _, _, err := tx.AcceptEvent(event); err != nil {
			return fmt.Errorf("event: %w", err)
		}
		if err := tx.PutExecution(domain.Execution{ID: "run", WorkID: "work", ExecutorBinding: "shell", State: domain.ExecutionPending, Attempt: 1, TriggerID: "trigger", Artifacts: []domain.ArtifactRef{ref}}, 0); err != nil {
			return fmt.Errorf("execution: %w", err)
		}
		var err error
		lease, err = tx.Claim("run", "owner", time.Hour)
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		event.ID, event.DeliveryID = "due", "due"
		return tx.PutTimer(domain.Timer{ID: "timer", FireAt: now.Add(time.Hour), Event: event}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "backup")
	if err := Create(ctx, bundle, sqlite.Name, db, files, "test-build", kvRetention()); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, bundle, sqlite.Name, db, files, "test-build", kvRetention()); err == nil {
		t.Fatal("overwrote existing backup")
	}
	// Later coordination state is deliberately outside the captured snapshot.
	if err := db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.Put("fixture", "later", []byte("new")) }); err != nil {
		t.Fatal(err)
	}
	// What the caller records goes into the staged directory: a failure
	// publishes nothing, and success publishes it with the state.
	refused := filepath.Join(root, "refused")
	if err := Restore(ctx, bundle, refused, now, nil, storage.LookupBackend, artifacts.Backend, func(string, string) error { return errors.New("record failed") }); err == nil {
		t.Fatal("restore published despite a failed record")
	}
	if _, err := os.Lstat(refused); !os.IsNotExist(err) {
		t.Fatalf("a failed record left a published directory: %v", err)
	}
	destination := filepath.Join(root, "restored")
	record := func(dir, backend string) error {
		if dir == destination || backend != sqlite.Name {
			return fmt.Errorf("record got %s, %s", dir, backend)
		}
		return os.WriteFile(filepath.Join(dir, "recorded"), []byte(backend), 0o600)
	}
	if err := Restore(ctx, bundle, destination, now, nil, storage.LookupBackend, artifacts.Backend, record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "recorded")); err != nil {
		t.Fatalf("record not published with the state: %v", err)
	}
	restored, err := sqlite.Open(ctx, filepath.Join(destination, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		work, err := tx.WorkByCorrelation(key)
		if err != nil {
			return err
		}
		if work.ID != "work" {
			t.Fatal("correlation ownership lost")
		}
		if _, err := tx.Timer("timer"); err != nil {
			return err
		}
		if _, err := tx.Get("fixture", "later"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("snapshot included later KV: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := restored.Transact(ctx, storage.Guard{Now: now, Lease: &lease}, func(tx storage.Tx) error { return tx.Put("fixture", "stale", nil) }); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("restored stale authority: %v", err)
	}
	restoredFiles, err := artifacts.Open(filepath.Join(destination, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredFiles.Close()
	r, err := restoredFiles.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(data) != "durable transcript" {
		t.Fatalf("restored content: %q, %v", data, err)
	}
	if err := Restore(ctx, bundle, destination, now, nil, storage.LookupBackend, artifacts.Backend, nil); err == nil {
		t.Fatal("overwrote restored state")
	}
	manifestPath := filepath.Join(bundle, "manifest.json")
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(original, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 1 || m.Artifacts[0].Ref != ref || ref.SHA256 == "" || m.Artifacts[0].SHA256 != ref.SHA256 {
		t.Fatalf("manifest artifacts %+v, want %+v with its digest", m.Artifacts, ref)
	}
	for _, test := range []struct {
		name   string
		change func(*manifest)
	}{
		{"missing_reference", func(m *manifest) { m.Artifacts = nil }},
		{"omitted_unexpired_reference", func(m *manifest) { m.Omitted, m.Artifacts = []domain.ArtifactRef{m.Artifacts[0].Ref}, nil }},
		{"changed_artifact_digest", func(m *manifest) { m.Artifacts[0].SHA256 = strings.Repeat("0", 64) }},
		{"ref_digest_disagrees", func(m *manifest) { m.Artifacts[0].Ref.SHA256 = strings.Repeat("0", 64) }},
		{"changed_database_digest", func(m *manifest) { m.DatabaseSHA256 = strings.Repeat("0", 64) }},
		{"newer_manifest", func(m *manifest) { m.Version++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var m manifest
			if err := json.Unmarshal(original, &m); err != nil {
				t.Fatal(err)
			}
			test.change(&m)
			changed, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, changed, 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, test.name)
			if err := Restore(ctx, bundle, target, now, nil, storage.LookupBackend, artifacts.Backend, nil); err == nil {
				t.Fatal("accepted invalid bundle")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published failed restore: %v", err)
			}
		})
	}
	if err := os.WriteFile(manifestPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := files.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(root, "missing-content-backup")
	if err := Create(ctx, failed, sqlite.Name, db, files, "test-build", kvRetention()); err == nil {
		t.Fatal("accepted backup missing referenced content")
	}
	if _, err := os.Lstat(failed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published partial backup: %v", err)
	}
}

// treeOf fingerprints every path under root: its mode and, for a file,
// its bytes.
func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] += " " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRestoreLeavesAFlatBundleUnchanged: a bundle whose artifacts are in
// the flat layout an older binary wrote (objects/<ID>) restores, and
// restoring it (as backup verify does on trial) changes nothing in it, so
// the older binary can still restore it after a rollback.
func TestRestoreLeavesAFlatBundleUnchanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, err := artifacts.Open(filepath.Join(root, "live-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	w, err := files.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "kept flat")
	ref, err := w.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "work", Kind: "fixture.work", State: domain.WorkReady, ScopeID: "scope"}, 0); err != nil {
			return err
		}
		event := domain.Event{ID: "trigger", Type: "fixture.start", Source: "fixture", DeliveryID: "trigger", OccurredAt: now, ReceivedAt: now}
		if _, _, err := tx.AcceptEvent(event); err != nil {
			return err
		}
		return tx.PutExecution(domain.Execution{ID: "run", WorkID: "work", ExecutorBinding: "shell", State: domain.ExecutionPending, Attempt: 1, TriggerID: "trigger", Artifacts: []domain.ArtifactRef{ref}}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "backup")
	if err := Create(ctx, bundle, sqlite.Name, db, files, "test-build", kvRetention()); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(bundle, "artifacts", "objects")
	if err := os.Rename(filepath.Join(objects, ref.ID[:2], ref.ID), filepath.Join(objects, ref.ID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(objects, ref.ID[:2])); err != nil {
		t.Fatal(err)
	}
	before := treeOf(t, bundle)
	destination := filepath.Join(root, "restored")
	if err := Restore(ctx, bundle, destination, now, nil, storage.LookupBackend, artifacts.Backend, nil); err != nil {
		t.Fatal(err)
	}
	after := treeOf(t, bundle)
	for path, was := range before {
		if now, ok := after[path]; !ok || now != was {
			t.Errorf("restore changed or removed %s in the bundle", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("restore added %s to the bundle", path)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	restored, err := artifacts.Open(filepath.Join(destination, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	r, err := restored.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(data) != "kept flat" {
		t.Fatalf("restored %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "artifacts", "objects", ref.ID[:2], ref.ID)); err != nil {
		t.Fatalf("the restored store is not in the fan-out layout: %v", err)
	}
}

// holdNamespace is where kvRetention keeps its holds.
const holdNamespace = "test.hold"

// kvRetention is a Retention whose hold is a KV key with its expiry, as
// the runtime's is.
func kvRetention() Retention {
	return Retention{
		Hold: func(tx storage.Tx, id string, until time.Time) error {
			return tx.Put(holdNamespace, id, []byte(until.Format(time.RFC3339Nano)))
		},
		Release: func(tx storage.Tx, id string) error { return tx.Delete(holdNamespace, id) },
	}
}

// liveHolds lists the holds kvRetention has committed and not released.
func liveHolds(ctx context.Context, db storage.Database) ([]storage.Entry, error) {
	var rows []storage.Entry
	err := db.View(ctx, func(tx storage.Tx) (e error) {
		rows, e = tx.List(holdNamespace, "")
		return e
	})
	return rows, err
}

// heldReads is an artifact store that records, at each read, whether a
// retention hold was committed then.
type heldReads struct {
	storage.ArtifactStore
	db    storage.Database
	reads []int
}

func (h *heldReads) Open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	rows, err := liveHolds(ctx, h.db)
	if err != nil {
		return nil, err
	}
	h.reads = append(h.reads, len(rows))
	return h.ArtifactStore.Open(ctx, ref)
}

// A backup commits its retention hold before the snapshot, keeps it while
// it copies and releases it once done, published or not. A backup without
// a hold, or whose hold can't be committed, publishes nothing.
func TestBackupHoldsRetentionWhileItCopies(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	live, err := artifacts.Open(filepath.Join(root, "live-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	var refs []domain.ArtifactRef
	for _, text := range []string{"first", "second"} {
		w, err := live.Create(ctx, "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(w, text); err != nil {
			t.Fatal(err)
		}
		ref, err := w.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if err = db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "work", Kind: "fixture.work", State: domain.WorkReady, ScopeID: "scope"}, 0); err != nil {
			return err
		}
		event := domain.Event{ID: "trigger", Type: "fixture.start", Source: "fixture", DeliveryID: "trigger", OccurredAt: now, ReceivedAt: now}
		if _, _, err := tx.AcceptEvent(event); err != nil {
			return err
		}
		return tx.PutExecution(domain.Execution{ID: "run", WorkID: "work", ExecutorBinding: "shell", State: domain.ExecutionPending, Attempt: 1, TriggerID: "trigger", Artifacts: refs}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	files := &heldReads{ArtifactStore: live, db: db}
	retention := kvRetention()
	var heldAtSnapshot int
	hold := retention.Hold
	retention.Hold = func(tx storage.Tx, id string, until time.Time) error {
		if !strings.HasPrefix(id, "backup/") || !until.After(time.Now()) {
			t.Errorf("hold %q until %v", id, until)
		}
		return hold(tx, id, until)
	}
	retention.Expired = func(tx storage.Tx, _ domain.ID) (bool, error) {
		// Read in the snapshot: the hold committed before it.
		rows, err := tx.List(holdNamespace, "")
		heldAtSnapshot = len(rows)
		return false, err
	}
	if err = Create(ctx, filepath.Join(root, "held"), sqlite.Name, db, files, "test-build", retention); err != nil {
		t.Fatal(err)
	}
	if heldAtSnapshot != 1 || len(files.reads) != 2 || files.reads[0] != 1 || files.reads[1] != 1 {
		t.Fatalf("hold at the snapshot %d, at each read %v; want 1 throughout", heldAtSnapshot, files.reads)
	}
	if rows, err := liveHolds(ctx, db); err != nil || len(rows) != 0 {
		t.Fatalf("hold left after the backup: %v %v", rows, err)
	}

	// A copy that fails releases its hold too, and publishes nothing.
	if err = live.Delete(ctx, refs[1]); err != nil {
		t.Fatal(err)
	}
	if err = Create(ctx, filepath.Join(root, "missing"), sqlite.Name, db, live, "test-build", kvRetention()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("backup of missing content: %v", err)
	}
	if rows, err := liveHolds(ctx, db); err != nil || len(rows) != 0 {
		t.Fatalf("hold left after a failed backup: %v %v", rows, err)
	}

	refused := kvRetention()
	refused.Hold = func(storage.Tx, string, time.Time) error { return storage.ErrConflict }
	for name, r := range map[string]Retention{"no-hold": {}, "refused": refused} {
		if err = Create(ctx, filepath.Join(root, name), sqlite.Name, db, live, "test-build", r); err == nil {
			t.Fatalf("%s: backup published without a hold", name)
		}
		if _, err = os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s: published: %v", name, err)
		}
	}
}

// stalledReads is an artifact store whose reads wait until the backup is
// cancelled.
type stalledReads struct{ storage.ArtifactStore }

func (stalledReads) Open(ctx context.Context, _ storage.ArtifactRef) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A backup renews its hold while it copies, and one whose renewal fails
// stops copying, since the hold could lapse under it, and says why.
func TestBackupStopsWhenItsHoldCannotBeRenewed(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	root := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	live, err := artifacts.Open(filepath.Join(root, "live-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	ref := domain.ArtifactRef{ID: strings.Repeat("a", 64), Size: 1, MediaType: "text/plain"}
	if err = db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "work", Kind: "fixture.work", State: domain.WorkReady, ScopeID: "scope"}, 0); err != nil {
			return err
		}
		event := domain.Event{ID: "trigger", Type: "fixture.start", Source: "fixture", DeliveryID: "trigger", OccurredAt: now, ReceivedAt: now}
		if _, _, err := tx.AcceptEvent(event); err != nil {
			return err
		}
		return tx.PutExecution(domain.Execution{ID: "run", WorkID: "work", ExecutorBinding: "shell", State: domain.ExecutionPending, Attempt: 1, TriggerID: "trigger", Artifacts: []domain.ArtifactRef{ref}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	retention := kvRetention()
	hold := retention.Hold
	// Each commit or renewal names a new expiry, however often its
	// transaction runs.
	puts := map[time.Time]bool{}
	renewal := errors.New("renewal refused")
	retention.Hold = func(tx storage.Tx, id string, until time.Time) error {
		puts[until] = true
		if len(puts) > 2 {
			return renewal
		}
		return hold(tx, id, until)
	}
	retention.HoldFor = 3 * time.Millisecond
	err = Create(ctx, filepath.Join(root, "stalled"), sqlite.Name, db, stalledReads{live}, "test-build", retention)
	if !errors.Is(err, renewal) || len(puts) != 3 {
		t.Fatalf("backup with a failed renewal after %d holds: %v", len(puts), err)
	}
	if rows, err := liveHolds(ctx, db); err != nil || len(rows) != 0 {
		t.Fatalf("hold left after a failed renewal: %v %v", rows, err)
	}
}
