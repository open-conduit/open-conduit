package bootstrap

import (
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/cli"
)

// The scaffold's Go files come out gofmt-clean, so the new repository's
// fmt-check passes, and its go line is this module's, so the toolchain
// that builds Open Conduit builds the package too.
func TestScaffoldTemplates(t *testing.T) {
	r := strings.NewReplacer("{{NAME}}", "acme", "{{MODULE}}", "example.com/acme", "{{CORE_VERSION}}", unresolved, "{{REPLACE}}", "")
	for _, f := range []string{"main.go.tmpl", "main_test.go.tmpl", "e2e_test.go.tmpl"} {
		raw, err := scaffoldFiles.ReadFile("scaffold/" + f)
		if err != nil {
			t.Fatal(err)
		}
		src := []byte(r.Replace(string(raw)))
		got, err := format.Source(src)
		if err != nil || string(got) != string(src) {
			t.Errorf("%s is not gofmt-clean once rendered: %v", f, err)
		}
	}
	goLine := regexp.MustCompile(`(?m)^go \S+$`)
	core, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, _ := scaffoldFiles.ReadFile("scaffold/go.mod.tmpl")
	if want, got := goLine.Find(core), goLine.Find(tmpl); string(want) != string(got) {
		t.Errorf("scaffold go.mod has %q, Open Conduit's go.mod %q", got, want)
	}
}

func TestScaffoldPaths(t *testing.T) {
	for in, want := range map[string]string{"gitignore.tmpl": ".gitignore", "github/workflows/ci.yml.tmpl": ".github/workflows/ci.yml", "playbooks/playbook.md.tmpl": "playbooks/acme.md", "main.go.tmpl": "main.go", "Makefile.tmpl": "Makefile"} {
		if got := scaffoldPath("acme", in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// A name the manifest rules or a first-party registration rule out is
// refused before anything is written.
func TestNewPackageRefusesNames(t *testing.T) {
	for _, name := range []string{"Acme", "core", "github", "has.dot"} {
		dir := filepath.Join(t.TempDir(), "out")
		if _, err := newPackage("1.0.0", "", cli.NewRequest{Name: name, Dir: dir}); err == nil || !strings.Contains(err.Error(), "cannot name a package") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: wrote %s", name, dir)
		}
	}
}
