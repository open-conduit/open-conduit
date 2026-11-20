package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// TestRestoreReportsTheConfigAndWhatItNeeds: a bundle carries the committed
// config generation, and a restore says which one it brought back and every
// reason the daemon would refuse it here: a type no package installed
// beside the config file provides, and any other diagnostic validation
// gives (a generation altered so a scope's parent is gone). A generation
// that validates reports nothing (the negative control).
func TestRestoreReportsTheConfigAndWhatItNeeds(t *testing.T) {
	const (
		needsAcme = `{"version":1,"settings":{},"connections":[{"id":"acme","type":"acme","config":{},"secret_refs":{}}],"scopes":[{"id":"eng","name":"eng"}]}`
		plain     = `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"eng"},{"id":"web","name":"web","parent":"eng"}]}`
	)
	orphan := func(normalized []byte) []byte {
		out := bytes.Replace(normalized, []byte(`"parent":"eng"`), []byte(`"parent":"gone"`), 1)
		if bytes.Equal(out, normalized) {
			t.Fatalf("no parent to alter in %s", normalized)
		}
		return out
	}
	for _, c := range []struct {
		name, config string
		alter        func([]byte) []byte
		want         []string // path:code
	}{
		{"needs a package", needsAcme, nil, []string{"connections/0/type:unknown_registration"}},
		{"refused for another reason", plain, orphan, []string{"scopes/1/parent:unknown_scope"}},
		{"needs nothing", plain, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			state := t.TempDir()
			if err := os.Chmod(state, 0o700); err != nil {
				t.Fatal(err)
			}
			// The generation was committed where the acme package was
			// installed.
			source := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(source, []byte(c.config), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
			if err != nil {
				t.Fatal(err)
			}
			now := func() time.Time { return time.Now().UTC() }
			if _, err := management.OpenConfig(ctx, store, now, config.Registry{Connections: map[string]func(json.RawMessage) error{"acme": nil}}, source, nil); err != nil {
				t.Fatal(err)
			}
			if c.alter != nil {
				gen, normalized, err := management.StoredGeneration(ctx, store, now)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
					return tx.Put("core.config", "generation/"+strconv.FormatUint(gen, 10), c.alter(normalized))
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(t.TempDir(), "bundle")
			if _, err := BackupState(ctx, state, bundle, "test"); err != nil {
				t.Fatal(err)
			}
			// The new host has a config folder with no packages installed
			// and no config file: the bundle is the config's only copy.
			host := filepath.Join(t.TempDir(), "config.json")
			info, err := RestoreState(ctx, bundle, filepath.Join(t.TempDir(), "restored"), cli.RestoreOptions{ConfigPath: host})
			if err != nil {
				t.Fatal(err)
			}
			if info.ConfigGeneration != 1 {
				t.Fatalf("restored generation %d, want 1", info.ConfigGeneration)
			}
			var got []string
			for _, e := range info.ConfigProblems {
				got = append(got, e.Path+":"+e.Code)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("config problems %v, want %v", info.ConfigProblems, c.want)
			}
		})
	}
}
