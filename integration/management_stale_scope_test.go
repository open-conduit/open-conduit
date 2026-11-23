package integration_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/storage"
)

const staleScopeConfig = `{"version": 1, "settings": {}, "connections": [], "scopes": [
  {"id": "keep", "name": "keep"},
  {"id": "gone", "name": "gone",
   "authorization": {"connections": [], "operations": ["emit", "read_context", "declare_outcome"], "read_scopes": []},
   "bindings": [{"name": "run", "executor": "fixture", "requires": {"toolbelt": "cli"}, "operations": ["emit"], "config": {}}],
   "routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}]}]}`

// TestManagementStaleScopeDoesNotStopTheDaemon: open Work in a scope the
// committed generation no longer contains doesn't stop the upgrade's
// admission_limit migration, and the operator can still see and cancel it.
func TestManagementStaleScopeDoesNotStopTheDaemon(t *testing.T) {
	h := newManagementHarnessWith(t, staleScopeConfig, nil)
	ctx := context.Background()
	owner := h.operatorClient()
	created, err := owner.CreateWork(ctx, client.WorkRequest{ScopeID: "gone", Kind: "manual.task", Binding: "run"}, "stale-work")
	if err != nil {
		t.Fatal(err)
	}

	registry := config.Registry{Executors: map[string]extension.Executor{"fixture": h.executor}, Handlers: map[string]bool{"manual": true}}
	without, err := config.Validate([]byte(`{"version": 1, "settings": {"admission_limit": 10}, "connections": [], "scopes": [{"id": "keep", "name": "keep"}]}`), registry)
	if err != nil {
		t.Fatal(err)
	}
	_, generation := h.configs.Current()
	committed := generation + 1
	active, err := json.Marshal(map[string]any{"Generation": committed, "Hash": without.Hash, "ActivatedAt": h.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.store.Transact(ctx, storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
		if err := tx.Put("core.config", "generation/"+strconv.FormatUint(committed, 10), without.Normalized); err != nil {
			return err
		}
		if err := tx.Delete("core.config", "migrated/admission-limit-default"); err != nil {
			return err
		}
		return tx.Put("core.config", "active", active)
	}); err != nil {
		t.Fatal(err)
	}

	started, err := management.OpenConfig(ctx, h.store, h.clock.Now, registry, filepath.Join(h.state, "config", "config.json"), nil)
	if err != nil {
		t.Fatalf("the daemon doesn't start: %v", err)
	}
	if s, gen := started.Current(); gen != committed+1 || s.Settings.AdmissionLimit != 0 {
		t.Fatalf("after the migration: generation %d, limit %d", gen, s.Settings.AdmissionLimit)
	}

	if w, err := owner.Work(ctx, created.Work.ID); err != nil || w.ScopeID != "gone" {
		t.Fatalf("the stale scope's Work isn't visible: %+v %v", w, err)
	}
	if _, err = owner.CancelWork(ctx, created.Work.ID, "", "scope was removed", "cancel-stale"); err != nil {
		t.Fatalf("the stale scope's Work can't be cancelled: %v", err)
	}
	w, err := owner.Work(ctx, created.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !domain.WorkState(w.State).Terminal() {
		t.Fatalf("cancelled Work is %s", w.State)
	}
}
