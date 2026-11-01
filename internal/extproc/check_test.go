package extproc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// checkPackage is a package that passes every check: an operation, a
// playbook naming it, a core operation and a host probe, and an inline
// fixture with a case for it.
func checkPackage(t *testing.T) (string, protocol.Manifest) {
	t.Helper()
	m := manifest("acme", "1.0.0", 0)
	m.Provides.Operations = []protocol.OperationDecl{{Name: "acme.forecast", Kind: "handler"}}
	m.Provides.Playbooks = "playbooks"
	dir := writePackage(t, t.TempDir(), m)
	write(t, filepath.Join(dir, "playbooks", "acme.md"), "Call `acme.forecast`, then `github.issue.comment`. Run the `github.repos` probe.\n"+
		"Set `settings.reconcile_seconds` in `config.json`; ask with `human.ask`.\n")
	write(t, filepath.Join(dir, CertificationFile), `{"fixture":{"operations":[{"operation":"acme.forecast","input":{}}]}}`)
	return dir, m
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkKnown() Known {
	taken := Taken{}
	taken.Add("operation", "github.issue.comment", "create_work")
	taken.Add("name", "github")
	return Known{Taken: taken, Probes: []string{"github.repos", "open-conduit.doctor"}}
}

// codes lists the findings' codes, with "!" before each warning's.
// Every finding must say what is wrong, what to change and where to read.
func codes(t *testing.T, r CheckReport) string {
	t.Helper()
	var out []string
	for _, f := range r.Findings {
		if f.Fix == "" || f.Docs == "" || f.Problem == "" {
			t.Errorf("finding without a problem, a fix or docs: %+v", f)
		}
		c := f.Code
		if f.Warning {
			c = "!" + c
		}
		out = append(out, c)
	}
	return strings.Join(out, ",")
}

func TestCheckCleanPackage(t *testing.T) {
	dir, _ := checkPackage(t)
	r := Check(dir, checkKnown())
	if !r.OK || codes(t, r) != "" || r.Name != "acme" || r.Version != "1.0.0" {
		t.Fatalf("clean package: %+v", r)
	}
}

func TestCheckExplainsEachProblem(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(t *testing.T, dir string, m protocol.Manifest)
		codes string
		ok    bool
		say   string
	}{
		{"newer field", func(t *testing.T, dir string, m protocol.Manifest) {
			write(t, filepath.Join(dir, protocol.ManifestFile), `{"protocol":[1],"name":"acme","version":"1","command":["acme-extension"],"teleport":true}`)
		}, "needs_newer_host", false, `"teleport"`},
		{"protocol", func(t *testing.T, dir string, m protocol.Manifest) {
			m.Protocol = []int{99}
			writeManifest(t, dir, m)
		}, "incompatible_protocol", false, "[99]"},
		{"every manifest rule", func(t *testing.T, dir string, m protocol.Manifest) {
			m.Version = ""
			m.Provides.Operations = append(m.Provides.Operations, protocol.OperationDecl{Name: "acme.bad", Kind: "magic"})
			writeManifest(t, dir, m)
		}, "invalid_package,invalid_package", false, "magic"},
		{"command not executable", func(t *testing.T, dir string, m protocol.Manifest) {
			if err := os.Chmod(filepath.Join(dir, "acme-extension"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "invalid_package", false, "chmod +x"},
		{"command missing", func(t *testing.T, dir string, m protocol.Manifest) {
			if err := os.Remove(filepath.Join(dir, "acme-extension")); err != nil {
				t.Fatal(err)
			}
		}, "invalid_package", false, "not a file"},
		{"playbooks missing", func(t *testing.T, dir string, m protocol.Manifest) {
			if err := os.RemoveAll(filepath.Join(dir, "playbooks")); err != nil {
				t.Fatal(err)
			}
		}, "invalid_package", false, "playbooks"},
		{"no playbook files", func(t *testing.T, dir string, m protocol.Manifest) {
			if err := os.Remove(filepath.Join(dir, "playbooks", "acme.md")); err != nil {
				t.Fatal(err)
			}
		}, "!no_playbooks", true, "no <name>.md"},
		{"unknown operations", func(t *testing.T, dir string, m protocol.Manifest) {
			write(t, filepath.Join(dir, "playbooks", "acme.md"), "Call `acme.forcast` and \"github.issue.coment\", twice: `acme.forcast`. Read `acme.md` and `open-conduit.doctor`.")
		}, "unknown_operation,unknown_operation", false, "acme.forcast"},
		{"conflict", func(t *testing.T, dir string, m protocol.Manifest) {
			m.Provides.Operations = append(m.Provides.Operations, protocol.OperationDecl{Name: "github.issue.comment", Kind: "handler"})
			writeManifest(t, dir, m)
		}, "invalid_package,registration_conflict", false, "github"},
		{"no certification", func(t *testing.T, dir string, m protocol.Manifest) {
			if err := os.Remove(filepath.Join(dir, CertificationFile)); err != nil {
				t.Fatal(err)
			}
		}, "!no_certification", true, "certify"},
		{"certification does not parse", func(t *testing.T, dir string, m protocol.Manifest) {
			write(t, filepath.Join(dir, CertificationFile), `{"fixtures":{}}`)
		}, "invalid_certification", false, "fixtures"},
		{"fixture misses cases", func(t *testing.T, dir string, m protocol.Manifest) {
			m.Provides.Extensions = []protocol.ExtensionDecl{{Type: "acme", Ingress: true}}
			m.Provides.Families = []protocol.Family{{Type: "acme.observed", ReDerivable: true}}
			m.Provides.Executors = []protocol.ExecutorDecl{{Name: "acme", Capabilities: protocol.Capabilities{Toolbelt: "none"}}}
			writeManifest(t, dir, m)
			write(t, filepath.Join(dir, CertificationFile), `{"fixture":{"operations":[{"operation":"acme.gone","input":{}}]}}`)
		}, "fixture_missing_case,fixture_missing_case,fixture_missing_case", false, "ingress case"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, m := checkPackage(t)
			c.edit(t, dir, m)
			r := Check(dir, checkKnown())
			var text []string
			for _, f := range r.Findings {
				text = append(text, f.Problem+" / "+f.Fix)
			}
			if got := codes(t, r); got != c.codes || r.OK != c.ok || !strings.Contains(strings.Join(text, "\n"), c.say) {
				t.Fatalf("codes %q ok %v, want %q ok %v mentioning %q:\n%s", got, r.OK, c.codes, c.ok, c.say, strings.Join(text, "\n"))
			}
		})
	}
}

func TestCheckAllowsUnseenNames(t *testing.T) {
	dir, _ := checkPackage(t)
	write(t, filepath.Join(dir, "playbooks", "acme.md"), "Probe `acme.fs.list` first.")
	k := checkKnown()
	if r := Check(dir, k); codes(t, r) != "unknown_operation" {
		t.Fatalf("without --allow: %+v", r.Findings)
	}
	k.Allow = []string{"acme.fs.list"}
	if r := Check(dir, k); !r.OK || codes(t, r) != "" {
		t.Fatalf("with --allow: %+v", r.Findings)
	}
}

func TestCheckNotesFixtureCommand(t *testing.T) {
	dir, _ := checkPackage(t)
	write(t, filepath.Join(dir, CertificationFile), `{"command":["acme-extension","package-certification"]}`)
	if r := Check(dir, checkKnown()); !r.OK || len(r.Notes) != 1 {
		t.Fatalf("fixture command: %+v", r)
	}
}

func TestCheckWithoutManifest(t *testing.T) {
	r := Check(t.TempDir(), checkKnown())
	if r.OK || codes(t, r) != "invalid_package" {
		t.Fatalf("empty directory: %+v", r)
	}
}

func TestDiagnosticsLinkTheDocs(t *testing.T) {
	d := Diagnose("acme", protocol.ErrNewerManifest, Paths{})
	if d.Docs != Troubleshooting || !strings.Contains(d.String(), Troubleshooting) {
		t.Fatalf("diagnostic %+v", d)
	}
}

func writeManifest(t *testing.T, dir string, m protocol.Manifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, protocol.ManifestFile), string(raw))
}

func TestCheckWarnsOfAMisnamedDirectory(t *testing.T) {
	dir, _ := checkPackage(t)
	renamed := filepath.Join(filepath.Dir(dir), "dist")
	if err := os.Rename(dir, renamed); err != nil {
		t.Fatal(err)
	}
	if r := Check(renamed, checkKnown()); !r.OK || codes(t, r) != "!invalid_package" {
		t.Fatalf("misnamed directory: %+v", r.Findings)
	}
}
