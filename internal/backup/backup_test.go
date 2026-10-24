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

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/internal/artifacts"
	"github.com/alsiola/open-conduit/internal/storage/sqlite"
	"github.com/alsiola/open-conduit/storage"
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
	if err := Create(ctx, bundle, db, files, "test-build"); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, bundle, db, files, "test-build"); err == nil {
		t.Fatal("overwrote existing backup")
	}
	// Later coordination state is deliberately outside the captured snapshot.
	if err := db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.Put("fixture", "later", []byte("new")) }); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "restored")
	if err := Restore(ctx, bundle, destination, now); err != nil {
		t.Fatal(err)
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
	if err := Restore(ctx, bundle, destination, now); err == nil {
		t.Fatal("overwrote restored state")
	}
	manifestPath := filepath.Join(bundle, "manifest.json")
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*manifest)
	}{
		{"missing_reference", func(m *manifest) { m.Artifacts = nil }},
		{"changed_artifact_digest", func(m *manifest) { m.Artifacts[0].SHA256 = strings.Repeat("0", 64) }},
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
			if err := Restore(ctx, bundle, target, now); err == nil {
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
	if err := Create(ctx, failed, db, files, "test-build"); err == nil {
		t.Fatal("accepted backup missing referenced content")
	}
	if _, err := os.Lstat(failed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published partial backup: %v", err)
	}
}
