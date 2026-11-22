package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func policyDoc(concurrency, secret string) string {
	return `{"version":1,"settings":{"concurrency":` + concurrency + `},"connections":[],"scopes":[{"id":"eng","name":"Eng","secret_refs":{"token":"` + secret + `"}}]}`
}

// commitGeneration stores text as the active generation, as a daemon that
// ran before the policy changed would have left it.
func commitGeneration(t *testing.T, store storage.Store, text string) {
	t.Helper()
	s, err := config.Validate([]byte(text), config.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	active, _ := json.Marshal(map[string]any{"Generation": 1, "Hash": s.Hash, "ActivatedAt": now})
	if err := store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := tx.Put("core.config", "generation/"+strconv.Itoa(1), s.Normalized); err != nil {
			return err
		}
		return tx.Put("core.config", "active", active)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyAtStartAndApply(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Now().UTC() }
	open := func(t *testing.T, stored, file, policy string) (*management.ConfigService, error) {
		t.Helper()
		dir := t.TempDir()
		store, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		commitGeneration(t, store, stored)
		path := filepath.Join(dir, "config.json")
		if file != "" {
			if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		p, err := config.ParsePolicy([]byte(policy))
		if err != nil {
			t.Fatal(err)
		}
		return management.OpenConfig(ctx, store, now, config.Registry{Policy: p}, path, nil)
	}

	t.Run("a stored generation the policy refuses doesn't start, and says why", func(t *testing.T) {
		_, err := open(t, policyDoc("4", "cmd:echo hunter2"), "", `{"version":1,"secret_schemes":["file"]}`)
		if err == nil || !strings.Contains(err.Error(), config.PolicyDenied) || !strings.Contains(err.Error(), "scopes/0/secret_refs/token") {
			t.Fatalf("start: %v", err)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("the refusal echoes the reference: %v", err)
		}
	})

	t.Run("a config file that fits replaces it", func(t *testing.T) {
		c, err := open(t, policyDoc("4", "cmd:echo x"), policyDoc("4", "file:token"), `{"version":1,"secret_schemes":["file"]}`)
		if err != nil {
			t.Fatal(err)
		}
		if c.Adopted == nil || !strings.Contains(c.Adopted.Cause.Error(), config.PolicyDenied) {
			t.Fatalf("adoption: %+v", c.Adopted)
		}
	})

	t.Run("a lowered ceiling clamps the stored generation and refuses an apply", func(t *testing.T) {
		text := policyDoc("8", "file:token")
		c, err := open(t, text, text, `{"version":1,"limits":{"concurrency":2}}`)
		if err != nil {
			t.Fatal(err)
		}
		s, generation := c.Current()
		if s.Settings.Concurrency != 2 || len(s.PolicyClamps) != 1 {
			t.Fatalf("clamp: %d %v", s.Settings.Concurrency, s.PolicyClamps)
		}
		_, _, err = c.Apply(ctx, []byte(policyDoc("3", "file:token")), "", generation, nil)
		var invalid *config.ValidationError
		if !errors.As(err, &invalid) || invalid.Diagnostics[0].Code != config.PolicyDenied || invalid.Diagnostics[0].Path != "settings/concurrency" {
			t.Fatalf("apply above the ceiling: %v", err)
		}
		fits := []byte(policyDoc("2", "file:token"))
		candidate, err := c.Validate(fits)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = c.Apply(ctx, fits, candidate.FileHash, generation, nil); err != nil {
			t.Fatalf("an apply inside the ceiling: %v", err)
		}
	})
}

// TestNoManagementRouteTouchesThePolicy: the policy is read from its file at
// start, so no route may name it.
func TestNoManagementRouteTouchesThePolicy(t *testing.T) {
	for _, r := range management.APIRoutes() {
		if strings.Contains(strings.ToLower(r.Path), "policy") {
			t.Errorf("%s %s", r.Method, r.Path)
		}
	}
}
