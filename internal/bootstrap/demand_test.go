package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
)

// A package counts as used through anything a scope gives it, directly or
// through a binding: an entry, a route, an executor, a resource, sandbox
// or container provider, an operation a binding or its grant is given, or
// a connection of its type a binding names. Each case has a negative
// control: the same scope without that use doesn't count.
func TestPackageUsedThroughEverythingAScopeGivesIt(t *testing.T) {
	m := protocol.Manifest{Name: "acme", Provides: protocol.Provides{
		Connections:   []protocol.TypeDecl{{Type: "acme-api"}},
		Extensions:    []protocol.ExtensionDecl{{Type: "acme"}},
		Routes:        []protocol.RouteDecl{{Handler: "acme"}},
		Executors:     []protocol.ExecutorDecl{{Name: "acme-agent"}},
		Resources:     []protocol.ResourceDecl{{Type: "acme-disk"}},
		Sandboxes:     []protocol.SandboxDecl{{Type: "acme-jail"}},
		Containers:    []protocol.ContainerDecl{{Type: "acme-box"}},
		Operations:    []protocol.OperationDecl{{Name: "acme.ping"}},
		SecretSchemes: []protocol.SecretSchemeDecl{{Scheme: "acme-vault"}},
	}}
	conns := []config.Connection{{ID: "api", Type: "acme-api"}, {ID: "other", Type: "other"}}
	snap := func(scope config.EffectiveScope) *config.Snapshot {
		scope.ID = "ops"
		return &config.Snapshot{Order: []domain.ID{"ops"}, Scopes: map[domain.ID]*config.EffectiveScope{"ops": &scope}, Connections: conns}
	}
	bound := func(b config.Binding) config.EffectiveScope {
		if b.Executor == "" {
			b.Executor = "shell"
		}
		b.Name = "run"
		return config.EffectiveScope{Bindings: []config.Binding{b}}
	}
	for name, c := range map[string]struct{ uses, not config.EffectiveScope }{
		"entry":          {config.EffectiveScope{Extensions: []config.Extension{{Name: "a", Type: "acme"}}}, config.EffectiveScope{Extensions: []config.Extension{{Name: "a", Type: "other"}}}},
		"route":          {config.EffectiveScope{Routes: []config.Route{{Name: "r", Handler: "acme"}}}, config.EffectiveScope{Routes: []config.Route{{Name: "r", Handler: "manual"}}}},
		"executor":       {bound(config.Binding{Executor: "acme-agent"}), bound(config.Binding{})},
		"resource":       {bound(config.Binding{Resource: &config.Resource{Provider: "acme-disk"}}), bound(config.Binding{Resource: &config.Resource{Provider: "git"}})},
		"sandbox":        {bound(config.Binding{Sandbox: &config.Sandbox{Provider: "acme-jail"}}), bound(config.Binding{Sandbox: &config.Sandbox{Provider: "srt"}})},
		"container":      {bound(config.Binding{Container: &config.Container{Provider: "acme-box"}}), bound(config.Binding{Container: &config.Container{Provider: "docker"}})},
		"operation":      {bound(config.Binding{Operations: []string{"read_context", "acme.ping"}}), bound(config.Binding{Operations: []string{"read_context"}})},
		"grant":          {bound(config.Binding{Grants: []config.Grant{{Name: "g", Operations: []string{"acme.ping"}}}}), bound(config.Binding{Grants: []config.Grant{{Name: "g", Operations: []string{"wait"}}}})},
		"connection":     {bound(config.Binding{Connection: "api"}), bound(config.Binding{Connection: "other"})},
		"entry sandbox":  {config.EffectiveScope{Extensions: []config.Extension{{Name: "a", Type: "other", Sandbox: &config.Sandbox{Provider: "acme-jail"}}}}, config.EffectiveScope{Extensions: []config.Extension{{Name: "a", Type: "other", Sandbox: &config.Sandbox{Provider: "srt"}}}}},
		"secret scheme":  {config.EffectiveScope{SecretRefs: map[string]string{"token": "acme-vault:prod/token"}}, config.EffectiveScope{SecretRefs: map[string]string{"token": "env:TOKEN"}}},
		"disabled scope": {config.EffectiveScope{Enabled: false, Extensions: []config.Extension{{Name: "a", Type: "acme"}}}, config.EffectiveScope{}},
	} {
		if !packageUsed(snap(c.uses), m) {
			t.Errorf("%s: not counted as used", name)
		}
		if packageUsed(snap(c.not), m) {
			t.Errorf("%s: the negative control counted as used", name)
		}
	}
	if packageUsed(nil, m) {
		t.Error("no configuration uses a package")
	}
}

// The host keeps a resource or container provider running while its state
// directory holds anything, and only then.
func TestProviderWithStateIsWanted(t *testing.T) {
	state := t.TempDir()
	d := &daemon{o: Options{StateDir: state}}
	m := protocol.Manifest{Name: "acme", Provides: protocol.Provides{Resources: []protocol.ResourceDecl{{Type: "acme-disk"}}}}
	if d.holdsState(m) {
		t.Fatal("a provider with no state directory holds state")
	}
	dir := resourceDir(state, "acme-disk")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if d.holdsState(m) {
		t.Fatal("an empty state directory holds state")
	}
	if err := os.WriteFile(filepath.Join(dir, "lease"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.holdsState(m) {
		t.Fatal("a provider with a workspace doesn't hold state")
	}
}

// Without a config file, the first-run setup is wanted; with one, it isn't
// unless the configuration uses it.
func TestFirstRunSetupWantedOnlyWithoutAConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	setup := &installed{m: protocol.Manifest{Name: "setup", Provides: protocol.Provides{Setup: &protocol.SetupDecl{Type: "setup", FirstRun: true}}}}
	other := &installed{m: protocol.Manifest{Name: "linear"}}
	store, err := sqlite.Open(context.Background(), filepath.Join(dir, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	d := &daemon{o: Options{ConfigPath: path, StateDir: dir, NoTunnel: true}, coord: store, now: time.Now}
	d.external.catalogue = []*installed{setup, other}
	if want := d.packagesWanted(context.Background(), nil); !want["setup"] || want["linear"] {
		t.Fatalf("no config file: %v", want)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if want := d.packagesWanted(context.Background(), nil); len(want) != 0 {
		t.Fatalf("with a config file: %v", want)
	}
}

// A package's name is reserved as a subject kind whether or not its process
// is running: Host.Act vouches for that kind's people as the package, so a
// client credential may never act for them (issue #687).
func TestPackageNamesAreReservedRunningOrNot(t *testing.T) {
	d := &daemon{}
	d.external.catalogue = []*installed{{m: protocol.Manifest{Name: "slack"}}}
	if d.runningRemote("slack") != nil {
		t.Fatal("the catalogued package is running")
	}
	if !d.packageNamed("slack") {
		t.Fatal("a catalogued package that isn't running is not reserved")
	}
	if d.packageNamed("acme-ui") {
		t.Fatal("a kind no package has is reserved")
	}
}
