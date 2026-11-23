package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// TestStoredAdmissionDefaultIsDropped: a generation committed while
// admission_limit defaulted to 10 (unenforced) carries a 10 nobody chose.
// Opening the config publishes the same config without it as the next
// generation, with an audit record naming both, and leaves the applied
// generation's text and hash as they were. It runs once: a 10 committed
// afterwards is kept, and a value an operator chose (3) is never touched
// (the negative controls).
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
	open := func(t *testing.T) (storage.Store, func() *management.ConfigService) {
		t.Helper()
		store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return store, func() *management.ConfigService {
			t.Helper()
			c, err := management.OpenConfig(ctx, store, now, registry, filepath.Join(t.TempDir(), "config.json"), nil)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	commit := func(t *testing.T, store storage.Store, gen uint64, s *config.Snapshot) {
		t.Helper()
		active, _ := json.Marshal(map[string]any{"Generation": gen, "Hash": s.Hash, "ActivatedAt": now()})
		if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
			if err := tx.Put("core.config", "generation/"+strconv.FormatUint(gen, 10), s.Normalized); err != nil {
				return err
			}
			return tx.Put("core.config", "active", active)
		}); err != nil {
			t.Fatal(err)
		}
	}
	audits := func(t *testing.T, store storage.Store) (out []domain.AuditRecord) {
		t.Helper()
		if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) (err error) { out, err = tx.AuditRecords(0, 100); return }); err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("default dropped", func(t *testing.T) {
		store, openConfig := open(t)
		old := doc("10")
		commit(t, store, 1, old)
		s, gen := openConfig().Current()
		if gen != 2 || s.Settings.AdmissionLimit != 0 || s.Scopes["eng"].AdmissionLimit != 0 {
			t.Fatalf("generation %d caps at %d after the upgrade", gen, s.Settings.AdmissionLimit)
		}
		var stored []byte
		if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) (err error) { stored, err = tx.Get("core.config", "generation/1"); return }); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, old.Normalized) {
			t.Fatalf("the applied generation was rewritten: %s", stored)
		}
		if again, err := config.Validate(stored, registry); err != nil || again.Hash != old.Hash {
			t.Fatalf("the applied generation's hash changed: %v", err)
		}
		records := audits(t, store)
		if len(records) != 1 || records[0].Action != management.AuditActionAdmissionDefault || records[0].Actor != "host:open-conduit" ||
			records[0].Detail["generation"] != "2" || records[0].Detail["hash"] != s.Hash || records[0].Detail["from_generation"] != "1" || records[0].Detail["from_hash"] != old.Hash {
			t.Fatalf("the published generation's audit record: %+v", records)
		}
		// A 10 applied after the upgrade is the operator's, and stays.
		commit(t, store, 3, old)
		if s, gen := openConfig().Current(); gen != 3 || s.Settings.AdmissionLimit != 10 {
			t.Fatalf("a 10 committed after the upgrade: generation %d, limit %d", gen, s.Settings.AdmissionLimit)
		}
		if len(audits(t, store)) != 1 {
			t.Fatal("the upgrade ran twice")
		}
	})
	t.Run("chosen value kept", func(t *testing.T) {
		store, openConfig := open(t)
		commit(t, store, 1, doc("3"))
		if s, gen := openConfig().Current(); gen != 1 || s.Settings.AdmissionLimit != 3 {
			t.Fatalf("an operator's admission_limit: generation %d, limit %d", gen, s.Settings.AdmissionLimit)
		}
		if len(audits(t, store)) != 0 {
			t.Fatal("a chosen value was published again")
		}
	})
	// A scope an earlier generation already dropped can still hold open Work.
	// The migration doesn't remove it, so it must not stop the daemon; the
	// Work stays cancellable. Removing a scope the committed generation does
	// contain is still refused, naming the scope and the Work (the negative
	// control).
	t.Run("stale scope with open Work", func(t *testing.T) {
		store, openConfig := open(t)
		parse := func(text string) *config.Snapshot {
			t.Helper()
			s, err := config.Validate([]byte(text), registry)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
		putScopes := func(s *config.Snapshot) {
			t.Helper()
			if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
				for _, scope := range s.DomainScopes() {
					if err := tx.PutScope(scope, 0); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		putWork := func(id, scope domain.ID) {
			t.Helper()
			if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
				return tx.PutWork(domain.Work{ID: id, ScopeID: scope, Kind: "manual.task", State: domain.WorkReady}, 0)
			}); err != nil {
				t.Fatal(err)
			}
		}
		putScopes(parse(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"eng"},{"id":"gone","name":"gone"}]}`))
		putWork("work-stale", "gone")
		commit(t, store, 1, doc("10"))

		c := openConfig()
		if s, gen := c.Current(); gen != 2 || s.Settings.AdmissionLimit != 0 {
			t.Fatalf("generation %d, limit %d after the upgrade", gen, s.Settings.AdmissionLimit)
		}

		putWork("work-live", "eng")
		without := []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"other","name":"other"}]}`)
		candidate, err := c.Validate(without)
		if err != nil {
			t.Fatal(err)
		}
		_, gen := c.Current()
		_, _, err = c.Apply(ctx, without, candidate.FileHash, gen, nil)
		var inUse *management.ScopeInUseError
		if !errors.Is(err, management.ErrScopeInUse) || !errors.As(err, &inUse) || inUse.Scope != "eng" || len(inUse.Work) != 1 || inUse.Work[0] != "work-live" {
			t.Fatalf("removing a scope with open Work: %v", err)
		}
		if !strings.Contains(err.Error(), "eng") || !strings.Contains(err.Error(), "work-live") {
			t.Fatalf("the refusal doesn't name the scope and Work: %v", err)
		}
	})
}
