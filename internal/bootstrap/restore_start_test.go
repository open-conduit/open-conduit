package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// startBundle is a bundle of a config naming one scope, with open Work in it.
func startBundle(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(source, []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"old machine"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Now().UTC() }
	if _, err := management.OpenConfig(ctx, store, now, config.Registry{}, source, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "open"}
		if err := tx.PutWork(domain.Work{ID: "open", Kind: "fixture.work", State: domain.WorkReady, ScopeID: "eng", CorrelationKeys: []domain.CorrelationKey{key}}, 0); err != nil {
			return err
		}
		_, err := runtime.BeginDrainInTx(tx, now(), "old:/state", "moving")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := BackupState(ctx, state, bundle, "test", cli.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func writeStart(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A restore with a start config comes up on it, with the bundle's Work and
// the drain ended. One that doesn't validate, or that drops a scope with
// open Work, publishes nothing. A restore without one keeps the bundle's
// generation, the negative control.
func TestRestoreStartsOnTheGivenConfig(t *testing.T) {
	ctx := context.Background()
	bundle := startBundle(t)
	scopeName := func(t *testing.T, dir string) string {
		t.Helper()
		_, store, err := openState(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var name string
		if err := store.View(ctx, func(tx storage.Tx) error {
			s, err := tx.Scope("eng")
			name = s.Name
			if err != nil {
				return err
			}
			if _, err := tx.Work("open"); err != nil {
				return err
			}
			if _, on, err := runtime.DrainInTx(tx); err != nil || on {
				return errors.Join(err, errors.New("still draining"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return name
	}
	t.Run("starts on it", func(t *testing.T) {
		start := writeStart(t, `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"new machine"}]}`)
		dest := filepath.Join(t.TempDir(), "restored")
		info, err := RestoreState(ctx, bundle, dest, cli.RestoreOptions{ConfigPath: start, StartConfig: start})
		if err != nil {
			t.Fatal(err)
		}
		if info.ConfigGeneration != 2 || info.StartConfig != start || len(info.ConfigProblems) != 0 {
			t.Fatalf("restored %+v", info)
		}
		if got := scopeName(t, dest); got != "new machine" {
			t.Fatalf("scope %q", got)
		}
	})
	for name, doc := range map[string]string{
		"doesn't validate":        `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"x","parent":"gone"}]}`,
		"drops a scope with Work": `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"other","name":"other"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "restored")
			start := writeStart(t, doc)
			if _, err := RestoreState(ctx, bundle, dest, cli.RestoreOptions{ConfigPath: start, StartConfig: start}); err == nil {
				t.Fatal("restored")
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 0 {
				t.Fatalf("a refused start config left %v", entries)
			}
		})
	}
	t.Run("without one", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "restored")
		info, err := RestoreState(ctx, bundle, dest, cli.RestoreOptions{ConfigPath: filepath.Join(t.TempDir(), "config.json")})
		if err != nil {
			t.Fatal(err)
		}
		if info.ConfigGeneration != 1 || scopeName(t, dest) != "old machine" {
			t.Fatalf("restored %+v", info)
		}
	})
}

// TestServeRestoreStartConfigAppliesOnce: serve --restore keeps its flags
// across restarts, so the start config is published when the bundle is
// restored and never again over what was applied since.
func TestServeRestoreStartConfigAppliesOnce(t *testing.T) {
	ctx := context.Background()
	bundle := startBundle(t)
	state := filepath.Join(t.TempDir(), "state")
	name := func() string {
		t.Helper()
		_, store, err := openState(ctx, state)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var got string
		if err := store.View(ctx, func(tx storage.Tx) error {
			s, err := tx.Scope("eng")
			got = s.Name
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := writeStart(t, `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"first start"}]}`)
	if _, err := RestoreInPlace(ctx, bundle, state, cli.RestoreOptions{ConfigPath: first, StartConfig: first, Once: true}); err != nil {
		t.Fatal(err)
	}
	if got := name(); got != "first start" {
		t.Fatalf("after the restore the scope is %q", got)
	}
	later := writeStart(t, `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"a restart"}]}`)
	if _, err := RestoreInPlace(ctx, bundle, state, cli.RestoreOptions{ConfigPath: later, StartConfig: later, Once: true}); err != nil {
		t.Fatal(err)
	}
	if got := name(); got != "first start" {
		t.Fatalf("a restart published its start config again: the scope is %q", got)
	}
}
