package bootstrap

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// TestStoredAdmissionDefaultIsDropped: generations stored while
// admission_limit defaulted to 10 (and was not enforced) carry a 10 nobody
// chose. Opening the config drops it, once, so no scope is capped after an
// upgrade; a value an operator wrote (3) stays, and a 10 committed after
// the migration is kept (the negative control).
func TestStoredAdmissionDefaultIsDropped(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Now().UTC() }
	registry := config.Registry{}
	doc := func(limit string) *config.Snapshot {
		t.Helper()
		s, err := config.Validate([]byte(`{"version":1,"settings":{"admission_limit":`+limit+`},"connections":[],"scopes":[{"id":"eng","name":"eng"}]}`), registry)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old, chosen := doc("10"), doc("3")
	put := func(gen uint64, s *config.Snapshot) {
		t.Helper()
		active, _ := json.Marshal(map[string]any{"Generation": gen, "Hash": s.Hash, "ActivatedAt": now()})
		if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
			if err := tx.Put("core.config", "generation/"+map[uint64]string{1: "1", 2: "2", 3: "3"}[gen], s.Normalized); err != nil {
				return err
			}
			return tx.Put("core.config", "active", active)
		}); err != nil {
			t.Fatal(err)
		}
	}
	put(2, chosen)
	put(1, old)
	configs, err := management.OpenConfig(ctx, store, now, registry, filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := configs.Current(); s.Settings.AdmissionLimit != 0 || s.Scopes["eng"].AdmissionLimit != 0 {
		t.Fatalf("the stored default still caps: %d", s.Settings.AdmissionLimit)
	}
	// Generation 2 was stored before the migration; only point at it.
	active, _ := json.Marshal(map[string]any{"Generation": 2, "Hash": chosen.Hash, "ActivatedAt": now()})
	if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error { return tx.Put("core.config", "active", active) }); err != nil {
		t.Fatal(err)
	}
	configs, err = management.OpenConfig(ctx, store, now, registry, filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := configs.Current(); s.Settings.AdmissionLimit != 3 {
		t.Fatalf("an operator's admission_limit was dropped: %d", s.Settings.AdmissionLimit)
	}
	put(3, old)
	configs, err = management.OpenConfig(ctx, store, now, registry, filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := configs.Current(); s.Settings.AdmissionLimit != 10 {
		t.Fatalf("a 10 committed after the migration was dropped: %d", s.Settings.AdmissionLimit)
	}
}
