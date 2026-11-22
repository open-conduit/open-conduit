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

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
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
	putGeneration(t, store, s)
}

func putGeneration(t *testing.T, store storage.Store, s *config.Snapshot) {
	t.Helper()
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

	t.Run("a config file that fits doesn't replace it", func(t *testing.T) {
		c, err := open(t, policyDoc("4", "cmd:echo x"), policyDoc("4", "file:token"), `{"version":1,"secret_schemes":["file"]}`)
		if err == nil || c != nil || !strings.Contains(err.Error(), config.PolicyDenied) {
			t.Fatalf("start with a fitting file: %v %v", c, err)
		}
	})

	t.Run("a generation that fails for another reason is still adopted", func(t *testing.T) {
		stored := `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"Eng","bindings":[{"name":"b","executor":"gone"}]}]}`
		dir := t.TempDir()
		store, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		seed := config.Registry{Executors: map[string]extension.Executor{"gone": noProcessExecutor{}}}
		s, err := config.Validate([]byte(stored), seed)
		if err != nil {
			t.Fatal(err)
		}
		putGeneration(t, store, s)
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(policyDoc("4", "file:token")), 0o600); err != nil {
			t.Fatal(err)
		}
		p, _ := config.ParsePolicy([]byte(`{"version":1,"secret_schemes":["file"]}`))
		c, err := management.OpenConfig(ctx, store, now, config.Registry{Policy: p}, path, nil)
		if err != nil || c.Adopted == nil {
			t.Fatalf("adoption: %v", err)
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

// TestRestoreChecksThePolicyItWasGiven: a restore's check uses the policy
// passed to it with OPEN_CONDUIT_POLICY unset, as serve --policy does.
func TestRestoreChecksThePolicyItWasGiven(t *testing.T) {
	ctx := context.Background()
	t.Setenv("OPEN_CONDUIT_POLICY", "")
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	commitGeneration(t, store, policyDoc("4", "cmd:echo hunter2"))
	configPath := filepath.Join(dir, "config.json")
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"secret_schemes":["file"]}`), 0o444); err != nil {
		t.Fatal(err)
	}
	policy, err := restorePolicy(cli.RestoreOptions{PolicyPath: path})
	if err != nil {
		t.Fatal(err)
	}
	_, problems, err := restoredConfig(ctx, store, configPath, policy)
	if err != nil || len(problems) != 1 || problems[0].Code != config.PolicyDenied {
		t.Fatalf("under the policy: %+v %v", problems, err)
	}
	// Negative control: the same generation with no policy has no problem.
	if _, problems, err = restoredConfig(ctx, store, configPath, nil); err != nil || len(problems) != 0 {
		t.Fatalf("with no policy: %+v %v", problems, err)
	}
	if _, err := RestoreState(ctx, filepath.Join(dir, "none.bundle"), filepath.Join(dir, "to"), cli.RestoreOptions{PolicyPath: filepath.Join(dir, "gone.json")}); err == nil || !strings.Contains(err.Error(), "gone.json") {
		t.Fatalf("an unreadable policy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "to")); !os.IsNotExist(err) {
		t.Fatalf("the refused restore left a directory: %v", err)
	}
}

// TestOfflineDoctorAppliesThePolicy: with no daemon, doctor reports what the
// daemon would refuse at start, and an unreadable policy fails the check.
func TestOfflineDoctorAppliesThePolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"secret_schemes":["file"]}`), 0o444); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	data := []byte(policyDoc("4", "cmd:echo hunter2"))
	t.Setenv("OPEN_CONDUIT_POLICY", "")
	if c := OfflineChecks(context.Background(), configPath, data)[0]; c.Code != "valid" {
		t.Fatalf("with no policy: %+v", c) // the negative control
	}
	t.Setenv("OPEN_CONDUIT_POLICY", path)
	c := OfflineChecks(context.Background(), configPath, data)[0]
	if c.Status != runtime.CheckFail || len(c.Items) != 1 || !strings.Contains(c.Items[0], config.PolicyDenied) || strings.Contains(c.Items[0], "hunter2") {
		t.Fatalf("under the policy: %+v", c)
	}
	t.Setenv("OPEN_CONDUIT_POLICY", filepath.Join(dir, "gone.json"))
	if c := OfflineChecks(context.Background(), configPath, data)[0]; c.Status != runtime.CheckFail || c.Code != "unreadable_policy" {
		t.Fatalf("an unreadable policy: %+v", c)
	}
}
