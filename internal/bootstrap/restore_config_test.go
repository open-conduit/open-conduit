package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

// TestRestoreReportsTheConfigAndWhatItNeeds: a bundle carries the committed
// config generation, and a restore says which one it brought back and which
// of its entries name a type no package installed beside the config file
// provides. A generation that names none reports nothing (the negative
// control).
func TestRestoreReportsTheConfigAndWhatItNeeds(t *testing.T) {
	const (
		needsAcme = `{"version":1,"settings":{},"connections":[{"id":"acme","type":"acme","config":{},"secret_refs":{}}],"scopes":[{"id":"eng","name":"eng"}]}`
		plain     = `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"eng","name":"eng"}]}`
	)
	for _, c := range []struct {
		name, config string
		want         []string
	}{
		{"needs a package", needsAcme, []string{"connections/0/type"}},
		{"needs nothing", plain, nil},
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
			for _, e := range info.Unregistered {
				got = append(got, e.Path)
			}
			if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
				t.Fatalf("unregistered %v, want %v", info.Unregistered, c.want)
			}
		})
	}
}
