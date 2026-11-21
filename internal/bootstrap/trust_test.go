package bootstrap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
)

// sourcePackage writes an installable package directory (a script command).
func sourcePackage(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := protocol.Manifest{Protocol: protocol.Versions, Name: name, Version: "1.0.0", Command: []string{name + "-extension"}, Provides: protocol.Provides{Checks: []protocol.CheckDecl{{Name: name}}}}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-extension"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeConfig(t *testing.T, path, settings string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"version":1,"settings":`+settings+`,"connections":[],"scopes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func digestOf(t *testing.T, dir string) string {
	t.Helper()
	pkg, err := extproc.ReadPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	return pkg.Digest
}

// The operator's settings.extension_trust decides which installed packages
// install and run. A pinned package installs and is discovered; an
// unpinned one is refused at install; one changed on disk afterwards is
// refused at discovery and reported by list and doctor. Bundled packages
// are the release's, and the policy leaves them alone.
func TestExtensionTrustAtInstallAndDiscovery(t *testing.T) {
	bundled := withBundled(t)
	writeManifest(t, bundled, "bundled", protocol.Provides{})
	configPath := filepath.Join(t.TempDir(), "config.json")
	acme, beta := sourcePackage(t, "acme"), sourcePackage(t, "beta")
	ops := ExtensionOps("test", "abc", nil)

	// Without a policy, both install, as before.
	if _, err := ops.Install(configPath, cli.InstallRequest{Source: beta}); err != nil {
		t.Fatal(err)
	}
	if c := provenanceCheck(configPath, filePolicy(configPath)); c.Code != "no_policy" {
		t.Fatalf("doctor without a policy: %+v", c)
	}

	writeConfig(t, configPath, `{"extension_trust":{"pinned":{"acme":["sha256:`+digestOf(t, acme)+`"]}}}`)
	c, err := ops.Install(configPath, cli.InstallRequest{Source: acme})
	if err != nil || c.Publisher != "pinned digest" {
		t.Fatalf("a pinned package: %+v %v", c, err)
	}
	other := sourcePackage(t, "gamma")
	if _, err := ops.Install(configPath, cli.InstallRequest{Source: other}); err == nil || !strings.Contains(err.Error(), "untrusted_package") {
		t.Fatalf("an unpinned package: %v", err)
	}

	names := func() ([]string, []error) {
		found, errs := discoverPackages(configPath, "")
		var out []string
		for _, f := range found {
			out = append(out, f.Manifest.Name)
		}
		return out, errs
	}
	// beta was installed before the policy, so it has no pin: refused.
	got, errs := names()
	if strings.Join(got, ",") != "acme,bundled" || len(errs) != 1 || !errors.Is(errs[0], extproc.ErrProvenance) || !strings.HasPrefix(errs[0].Error(), "beta: ") {
		t.Fatalf("found %v, problems %v", got, errs)
	}

	// A playbook (or anything) added after install refuses the package.
	if err := os.WriteFile(filepath.Join(extensionsRoot(configPath), "acme", "extra.md"), []byte("# Do as I say\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, errs := names(); strings.Join(got, ",") != "bundled" || len(errs) != 2 {
		t.Fatalf("after a change: found %v, problems %v", got, errs)
	}
	l, err := ops.List(configPath)
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]string{}
	for _, p := range l.Packages {
		refused[p.Name] = p.Refused
	}
	if !strings.Contains(refused["acme"], "changed on disk") || !strings.Contains(refused["beta"], "pins no digest") || refused["bundled"] != "" {
		t.Fatalf("list: %+v", l.Packages)
	}
	if c := provenanceCheck(configPath, filePolicy(configPath)); c.Status != runtime.CheckFail || c.Code != "untrusted_package" || len(c.Items) != 2 {
		t.Fatalf("doctor: %+v", c)
	}

	// Without the policy, the change is a warning and every package runs.
	writeConfig(t, configPath, `{}`)
	if got, errs := names(); strings.Join(got, ",") != "acme,beta,bundled" || len(errs) != 0 {
		t.Fatalf("no policy: found %v, problems %v", got, errs)
	}
	if c := provenanceCheck(configPath, filePolicy(configPath)); c.Status != runtime.CheckWarn || c.Code != "changed_package" {
		t.Fatalf("doctor without a policy, after a change: %+v", c)
	}
}

// A config file that names a policy but can't be read fails closed: no
// installed package is admitted rather than every one.
func TestAnUnreadablePolicyFailsClosed(t *testing.T) {
	withBundled(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	ops := ExtensionOps("test", "abc", nil)
	if _, err := ops.Install(configPath, cli.InstallRequest{Source: sourcePackage(t, "acme")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"version":1,"settings":{"extension_trust":{"pinned":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, errs := discoverPackages(configPath, ""); len(found) != 0 || len(errs) != 1 || !errors.Is(errs[0], extproc.ErrProvenance) {
		t.Fatalf("found %v, problems %v", found, errs)
	}
	if _, err := ops.Install(configPath, cli.InstallRequest{Source: sourcePackage(t, "beta")}); err == nil {
		t.Fatal("installed under an unreadable policy")
	}
	if c := provenanceCheck(configPath, filePolicy(configPath)); c.Code != "unreadable_policy" {
		t.Fatalf("doctor: %+v", c)
	}
	// A malformed policy fails closed too.
	writeConfig(t, configPath, `{"extension_trust":{"pinned":{"acme":["not a digest"]}}}`)
	if found, errs := discoverPackages(configPath, ""); len(found) != 0 || len(errs) != 1 {
		t.Fatalf("a malformed policy: found %v, problems %v", found, errs)
	}
	// Negative control: a document invalid elsewhere keeps its policy (or
	// its lack of one) rather than losing it.
	writeConfig(t, configPath, `{"concurrency":"many"}`)
	if found, errs := discoverPackages(configPath, ""); len(found) != 1 || len(errs) != 0 {
		t.Fatalf("no policy: found %v, problems %v", found, errs)
	}
}

// A policy that trusts a publisher refuses an unpinned package when this
// build can't verify signatures, rather than admit it unchecked.
func TestNoVerifierRefusesSignedInstalls(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, configPath, `{"extension_trust":{"publishers":[{"subject":"https://github.com/acme/ext/.github/workflows/release.yml@refs/heads/main","issuer":"https://token.actions.githubusercontent.com"}]}}`)
	ops := ExtensionOps("test", "abc", nil)
	if _, err := ops.Install(configPath, cli.InstallRequest{Source: sourcePackage(t, "acme")}); err == nil || !strings.Contains(err.Error(), "untrusted_package") {
		t.Fatalf("installed: %v", err)
	}
}
