package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/internal/certify"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/weakened"
)

// 07-03 packaging and certification: the first-party subprocess packages
// (Slack, shell) and the examples pass the public conformance runner, and
// every negative control fails the criterion it weakens.

var (
	pkgOnce sync.Once
	pkgBins map[string]string
	pkgErr  error
)

// packageBinaries builds the first-party packages' and the negative
// controls' commands once per test binary.
func packageBinaries(t *testing.T) map[string]string {
	t.Helper()
	pkgOnce.Do(func() {
		dir, err := os.MkdirTemp("", "oc-package-bin-")
		if err != nil {
			pkgErr = err
			return
		}
		out := map[string]string{}
		for _, b := range []struct{ out, pkg string }{
			{"slack-extension", "./packages/slack/cmd/slack-extension"},
			{"shell-extension", "./packages/shell/cmd/shell-extension"},
			{"codex-extension", "./packages/codex/cmd/codex-extension"},
			{"weakened-extension", "./protocol/examples/weakened/cmd/weakened-extension"},
			{"os", "./cmd/os"},
		} {
			args := []string{"build", "-o", filepath.Join(dir, b.out)}
			if raceEnabled && b.out != "os" {
				args = append(args, "-race")
			}
			cmd := exec.Command("go", append(args, b.pkg)...)
			cmd.Dir = ".."
			if o, err := cmd.CombinedOutput(); err != nil {
				pkgErr = fmt.Errorf("build %s: %v\n%s", b.out, err, o)
				return
			}
			out[b.out] = filepath.Join(dir, b.out)
		}
		pkgBins = out
	})
	if pkgErr != nil {
		t.Fatal(pkgErr)
	}
	notesPath, runnerPath := extensionBinaries(t)
	out := map[string]string{"notes-extension": notesPath, "runner-extension": runnerPath}
	for k, v := range pkgBins {
		out[k] = v
	}
	return out
}

// assemble lays out a package directory under root from a packaged binary's
// own package-manifest and package-certification output (what make packages
// does), linking the binary as the manifest's command.
func assemble(t *testing.T, root, bin string, args ...string) (string, protocol.Manifest) {
	t.Helper()
	raw, err := exec.Command(bin, append([]string{"package-manifest"}, args...)...).Output()
	if err != nil {
		t.Fatalf("%s package-manifest: %v", bin, err)
	}
	m, err := protocol.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, m.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(dir, filepath.Base(m.Command[0]))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cert, err := exec.Command(bin, append([]string{"package-certification"}, args...)...).Output()
	if err != nil {
		t.Fatalf("%s package-certification: %v", bin, err)
	}
	if len(cert) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "certification.json"), cert, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, m
}

func runCertify(t *testing.T, dir string) certify.Report {
	t.Helper()
	rep, err := certify.Run(context.Background(), certify.Options{Dir: dir, HostVersion: "test", HostCommit: "test", HandshakeTimeout: extensionHandshakeMax})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestCertificationFirstPartyPackages(t *testing.T) {
	bins := packageBinaries(t)
	for _, c := range []struct {
		name, bin string
		must      []string // criteria that must pass (not skip)
	}{
		{"slack", "slack-extension", []string{"ingress.conformance", "routes.decisions", "operations.cases", "surfaces.served"}},
		{"shell", "shell-extension", []string{"executor.conformance", "executor.inject", "executor.controls"}},
		{"codex", "codex-extension", []string{"executor.conformance", "executor.inject", "executor.controls", "executor.resource_mode"}},
		{"notes", "notes-extension", []string{"storage.kv_migration", "ingress.conformance", "routes.decisions", "operations.cases", "surfaces.served", "resources.workspace"}},
		{"runner", "runner-extension", []string{"executor.conformance", "executor.inject", "executor.controls", "executor.resource_mode"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			dir, m := assemble(t, root, bins[c.bin])
			if c.name == "shell" {
				if err := os.Symlink(bins["os"], filepath.Join(dir, "os")); err != nil {
					t.Fatal(err)
				}
			}
			rep := runCertify(t, dir)
			if !rep.Certified {
				t.Fatalf("not certified:\n%s", rep.Text())
			}
			// The report states the tested versions and the limits.
			if rep.Package.Name != m.Name || rep.Package.Version != m.Version || rep.Package.Digest == "" || rep.Protocol.Negotiated != protocol.Version ||
				rep.Host.Version != "test" || rep.Host.Go == "" || rep.Host.OS == "" || rep.CriteriaVersion != conformance.CriteriaVersion || len(rep.Limits) == 0 {
				t.Fatalf("report misses tested versions or limits: %+v", rep)
			}
			if len(rep.Results) != len(conformance.Criteria) {
				t.Fatalf("report covers %d of %d criteria", len(rep.Results), len(conformance.Criteria))
			}
			for _, id := range append([]string{"manifest.valid", "handshake.manifest", "handshake.version_refusal", "lifecycle.stdin_eof", "lifecycle.restart", "transport.clean_stdout", "scope.host_surface"}, c.must...) {
				if r := rep.Result(id); r.Status != certify.Pass {
					t.Errorf("%s: %s %s", id, r.Status, r.Detail)
				}
			}
			if _, err := json.Marshal(rep); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCertificationNegativeControls(t *testing.T) {
	bins := packageBinaries(t)
	for _, mode := range weakened.Modes {
		t.Run(mode.Name, func(t *testing.T) {
			t.Parallel()
			dir, m := assemble(t, t.TempDir(), bins["weakened-extension"], mode.Name)
			if m.Name != mode.Base {
				t.Fatalf("package %s, want %s", m.Name, mode.Base)
			}
			rep := runCertify(t, dir)
			if rep.Certified {
				t.Fatalf("weakened (%s) was certified:\n%s", mode.Defect, rep.Text())
			}
			if r := rep.Result(mode.Fails); r.Status != certify.Fail {
				t.Fatalf("%s did not fail %s (%s %s):\n%s", mode.Name, mode.Fails, r.Status, r.Detail, rep.Text())
			}
		})
	}
}

// TestCertificationInvalidPackage: a broken layout fails manifest.valid
// before anything starts.
func TestCertificationInvalidPackage(t *testing.T) {
	bins := packageBinaries(t)
	root := t.TempDir()
	dir, _ := assemble(t, root, bins["runner-extension"])
	misnamed := filepath.Join(root, "renamed")
	if err := os.Rename(dir, misnamed); err != nil {
		t.Fatal(err)
	}
	if rep := runCertify(t, misnamed); rep.Certified || rep.Result("manifest.valid").Status != certify.Fail {
		t.Fatalf("misnamed package: %+v", rep.Result("manifest.valid"))
	}
	if err := os.Remove(filepath.Join(misnamed, "runner-extension")); err != nil {
		t.Fatal(err)
	}
	if rep := runCertify(t, misnamed); rep.Certified || rep.Result("manifest.valid").Status != certify.Fail {
		t.Fatalf("package without its command: %+v", rep.Result("manifest.valid"))
	}
}

// TestExtensionCheckCommand: `extension check` passes a good package and,
// for a broken one, exits 1 naming each problem with what to change and
// where the docs explain it.
func TestExtensionCheckCommand(t *testing.T) {
	bins := packageBinaries(t)
	dir, _ := assemble(t, t.TempDir(), bins["runner-extension"])
	check := func(args ...string) (string, error) {
		out, err := exec.Command(daemonBinary(t), append([]string{"extension", "check", dir}, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := check(); err != nil || !strings.Contains(out, "no problems found") {
		t.Fatalf("a good package: %v\n%s", err, out)
	}
	// The package links the runner binary every test in this process
	// shares; chmod through the link would leave it unexecutable for the
	// tests after this one, so the package gets its own copy first.
	cmdPath := filepath.Join(dir, "runner-extension")
	raw, err := os.ReadFile(cmdPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(cmdPath); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cmdPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := check()
	if err == nil || !strings.Contains(out, "error manifest: command \"runner-extension\" is not executable (invalid_package)") || !strings.Contains(out, "Fix: chmod +x runner-extension") || !strings.Contains(out, "Docs: https://") {
		t.Fatalf("a package whose command is not executable: %v\n%s", err, out)
	}
	out, _ = check("--json")
	var r struct {
		OK       bool
		Findings []struct{ Code, Fix, Docs string }
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.OK || len(r.Findings) != 1 || r.Findings[0].Code != "invalid_package" || r.Findings[0].Fix == "" {
		t.Fatalf("--json: %v\n%s", err, out)
	}
}
