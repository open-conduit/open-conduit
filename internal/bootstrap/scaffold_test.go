package bootstrap

import (
	"encoding/json"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	for in, want := range map[string]string{"gitignore.tmpl": ".gitignore", "github/workflows/ci.yml.tmpl": ".github/workflows/ci.yml", "playbooks/playbook.md.tmpl": "playbooks/acme.md", "prettierignore.tmpl": ".prettierignore", "main.go.tmpl": "main.go", "Makefile.tmpl": "Makefile"} {
		if got := scaffoldPath("acme", in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// A name the manifest rules or a first-party registration rule out is
// refused before anything is written.
func TestNewPackageRefusesNames(t *testing.T) {
	for _, name := range []string{"Acme", "core", "manual", "has.dot"} {
		dir := filepath.Join(t.TempDir(), "out")
		if _, err := newPackage("1.0.0", "", cli.NewRequest{Name: name, Dir: dir}); err == nil || !strings.Contains(err.Error(), "cannot name a package") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: wrote %s", name, dir)
		}
	}
}

// --lang ts writes a TypeScript package whose package.json depends on the
// SDK released with this binary, or with --replace on the checkout's
// sdk/typescript, and whose package script writes a CommonJS bundle and a
// launcher. TestExtensionNewTS runs what it writes.
func TestNewTSPackage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	res, err := newPackage("v1.4.0", "", cli.NewRequest{Name: "acme", Dir: dir, Lang: "ts"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Lang != "ts" || res.Requires != "1.4.0" {
		t.Errorf("result %+v", res)
	}
	for _, f := range []string{"package.json", "src/extension.ts", "src/extension.test.ts", "src/main.ts", "scripts/package.mjs", "Makefile", ".gitignore", ".prettierignore", ".github/workflows/ci.yml", "playbooks/acme.md", "README.md", "tsconfig.json"} {
		if !slices.Contains(res.Files, f) {
			t.Errorf("no %s in %v", f, res.Files)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json: %v", err)
	}
	if got := pkg.Dependencies["@open-conduit/sdk"]; got != "1.4.0" {
		t.Errorf("@open-conduit/sdk %q", got)
	}
	script, _ := os.ReadFile(filepath.Join(dir, "scripts", "package.mjs"))
	if !strings.Contains(string(script), "process.execPath") || !strings.Contains(string(script), "--portable") || !strings.Contains(string(script), "${bin}.cjs") {
		t.Error("package.mjs no longer writes a .cjs bundle with a launcher that records node unless --portable")
	}
	for path := range map[string]bool{"src/extension.ts": true, "README.md": true} {
		b, _ := os.ReadFile(filepath.Join(dir, path))
		if strings.Contains(string(b), "{{") {
			t.Errorf("%s has an unsubstituted placeholder", path)
		}
	}

	// Against a checkout, the SDK is the checkout's.
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "sdk", "typescript"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "sdk", "typescript", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = newPackage("dev", "", cli.NewRequest{Name: "acme", Dir: filepath.Join(t.TempDir(), "acme"), Lang: "ts", Replace: checkout})
	if err != nil || res.Requires != "file:"+filepath.ToSlash(filepath.Join(checkout, "sdk", "typescript")) {
		t.Errorf("--replace: %+v, %v", res, err)
	}
}

// A TypeScript package refuses what it can't honour before writing anything.
func TestNewTSPackageRefuses(t *testing.T) {
	for name, r := range map[string]cli.NewRequest{
		"a development build without a checkout": {Name: "acme", Lang: "ts"},
		"a Go module path":                       {Name: "acme", Lang: "ts", Module: "example.com/acme"},
		"a checkout without the SDK":             {Name: "acme", Lang: "ts", Replace: t.TempDir()},
	} {
		r.Dir = filepath.Join(t.TempDir(), "out")
		if _, err := newPackage("dev", "", r); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
			t.Errorf("%s: wrote %s", name, r.Dir)
		}
	}
}

// The scaffold's tool versions are the SDK's own, so a new package builds
// with what the SDK is tested with.
func TestTSScaffoldMatchesSDK(t *testing.T) {
	var sdk, tmpl struct {
		DevDependencies map[string]string `json:"devDependencies"`
		Engines         map[string]string `json:"engines"`
	}
	raw, err := os.ReadFile("../../sdk/typescript/package.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &sdk); err != nil {
		t.Fatal(err)
	}
	raw, _ = scaffoldTSFiles.ReadFile("scaffold-ts/package.json.tmpl")
	if err := json.Unmarshal([]byte(strings.NewReplacer("{{NAME}}", "acme", "{{SDK}}", "1.0.0").Replace(string(raw))), &tmpl); err != nil {
		t.Fatal(err)
	}
	for _, dep := range []string{"typescript", "@types/node", "prettier"} {
		if tmpl.DevDependencies[dep] != sdk.DevDependencies[dep] {
			t.Errorf("%s: scaffold %q, SDK %q", dep, tmpl.DevDependencies[dep], sdk.DevDependencies[dep])
		}
	}
	// The SDK doesn't use esbuild; the weather example is what pins it.
	var weather struct {
		DevDependencies map[string]string `json:"devDependencies"`
	}
	raw, err = os.ReadFile("../../protocol/examples/weather/package.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &weather); err != nil {
		t.Fatal(err)
	}
	if got, want := tmpl.DevDependencies["esbuild"], weather.DevDependencies["esbuild"]; want == "" || got != want {
		t.Errorf("esbuild: scaffold %q, weather %q", got, want)
	}
	if tmpl.Engines["node"] != sdk.Engines["node"] {
		t.Errorf("node engine: scaffold %q, SDK %q", tmpl.Engines["node"], sdk.Engines["node"])
	}
}
