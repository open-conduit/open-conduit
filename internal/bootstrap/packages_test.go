package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

func writeManifest(t *testing.T, root, name string, p protocol.Provides) {
	t.Helper()
	m := protocol.Manifest{Protocol: protocol.Versions, Name: name, Version: "1.0.0", Command: []string{"./" + name + "-extension"}, Provides: p}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.ParseManifest(raw); err != nil {
		t.Fatalf("test manifest: %v", err)
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func withBundled(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	prev := bundledRoot
	bundledRoot = func() string { return root }
	t.Cleanup(func() { bundledRoot = prev })
	return root
}

// Packages bundled with the binary are found beside the installed ones.
// An installed package replaces the bundled one of its name, even when the
// installed one is refused; removal's check (exclude) brings it back.
func TestDiscoverPackagesBundled(t *testing.T) {
	bundled := withBundled(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	installed := extensionsRoot(configPath)
	writeManifest(t, bundled, "acme", protocol.Provides{})
	writeManifest(t, bundled, "zeta", protocol.Provides{})
	writeManifest(t, installed, "acme", protocol.Provides{})
	if err := os.MkdirAll(filepath.Join(installed, "zeta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "zeta", protocol.ManifestFile), []byte(`{"protocol":[1],"name":"zeta","version":"9","command":["./z"],"provides":{},"from_the_future":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	names := func(exclude string) (out []string, errs []error) {
		found, errs := discoverPackages(configPath, exclude)
		for _, f := range found {
			where := "installed"
			if f.Bundled {
				where = "bundled"
			}
			out = append(out, f.Manifest.Name+"@"+where)
		}
		return out, errs
	}
	got, errs := names("")
	if strings.Join(got, ",") != "acme@installed" || len(errs) != 1 || !errors.Is(errs[0], protocol.ErrNewerManifest) {
		t.Fatalf("found %v, problems %v", got, errs)
	}
	if got, _ := names("acme"); strings.Join(got, ",") != "acme@bundled" {
		t.Fatalf("without the installed acme: %v", got)
	}
	if err := os.RemoveAll(installed); err != nil {
		t.Fatal(err)
	}
	if got, _ := names(""); strings.Join(got, ",") != "acme@bundled,zeta@bundled" {
		t.Fatalf("nothing installed: %v", got)
	}
}

type fakeSetup struct {
	info  extension.SetupInfo
	scope string
}

func (s fakeSetup) SetupInfo() extension.SetupInfo { return s.info }
func (s fakeSetup) Setup(_ context.Context, h extension.SetupHost, args []string) (extension.SetupResult, error) {
	if h == nil || len(args) != 0 {
		return extension.SetupResult{}, errors.New("a first-run setup gets a host and no flags")
	}
	if _, err := h.Prompt("?", extension.Prompt{}); !errors.Is(err, extension.ErrSetupNoInput) {
		return extension.SetupResult{}, errors.New("the first-run host prompted")
	}
	return extension.SetupResult{Scope: json.RawMessage(`{"id":"` + s.scope + `"}`)}, nil
}

// The default configuration is the first first-run setup's, and there is
// none without one.
func TestFirstRunConfig(t *testing.T) {
	other := fakeSetup{info: extension.SetupInfo{Type: "other"}, scope: "other"}
	first := fakeSetup{info: extension.SetupInfo{Type: "starter", FirstRun: true}, scope: "starter"}
	if doc := firstRunConfig([]extension.Setup{other}); doc != nil {
		t.Fatalf("no first-run setup gave %s", doc)
	}
	doc := firstRunConfig([]extension.Setup{other, first})
	if !strings.Contains(string(doc), `"id": "starter"`) || strings.Contains(string(doc), `"other"`) {
		t.Fatalf("default config: %s", doc)
	}
}

// An installed package's operations are entry-gated when its manifest says
// so, as an in-process inproc.EntryGated extension's are.
func TestInstalledEntryGated(t *testing.T) {
	if entryGated(&installed{m: protocol.Manifest{Name: "acme"}}) {
		t.Fatal("ungated package gated")
	}
	if !entryGated(&installed{m: protocol.Manifest{Name: "acme", Provides: protocol.Provides{EntryGated: true}}}) {
		t.Fatal("entry_gated package not gated")
	}
}
