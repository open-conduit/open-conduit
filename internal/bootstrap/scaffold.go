package bootstrap

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// `open-conduit extension new` (#267): a package repository whose
// `make check` passes on its first run. The files are templates in
// scaffold/, with {{NAME}}, {{MODULE}}, {{CORE_VERSION}} and {{REPLACE}}
// substituted; a .tmpl suffix keeps Go from building or embedding them as a
// module of their own.

//go:embed scaffold
var scaffoldFiles embed.FS

// scaffold-ts/ is the TypeScript package (--lang ts), with {{NAME}} and
// {{SDK}} (the @open-conduit/sdk dependency) substituted.
//
//go:embed scaffold-ts
var scaffoldTSFiles embed.FS

const coreModule = "github.com/open-conduit/open-conduit"

// unresolved is the version go.mod requires when a replace directive
// supplies the module.
const unresolved = "v0.0.0-00010101000000-000000000000"

var releaseVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// scaffoldPath is where a template is written, relative to the new
// repository: dot files are stored without their dot, and the playbook is
// named after the package.
func scaffoldPath(name, tmpl string) string {
	p := strings.TrimSuffix(tmpl, ".tmpl")
	switch p {
	case "gitignore":
		return ".gitignore"
	case "playbooks/playbook.md":
		return "playbooks/" + name + ".md"
	}
	if p == "prettierignore" {
		return ".prettierignore"
	}
	if rest, ok := strings.CutPrefix(p, "github/"); ok {
		return ".github/" + rest
	}
	return p
}

// newPackage writes the repository and runs `go mod tidy` in it, so its
// go.sum is complete.
func newPackage(version, commit string, r cli.NewRequest) (cli.NewResult, error) {
	out := cli.NewResult{Name: r.Name, Dir: r.Dir, Module: r.Module, Lang: "go"}
	if err := checkPackageName(r.Name); err != nil {
		return out, err
	}
	if err := checkEmpty(out.Dir); err != nil {
		return out, err
	}
	if r.Lang == "ts" {
		return newTSPackage(version, r)
	}
	if out.Module == "" {
		out.Module = "example.com/" + r.Name
	}
	gocmd, goErr := exec.LookPath("go")
	replace := ""
	switch {
	case r.Replace != "":
		if _, err := os.Stat(filepath.Join(r.Replace, "go.mod")); err != nil {
			return out, fmt.Errorf("--replace %s: not an Open Conduit checkout (no go.mod)", r.Replace)
		}
		out.Requires = unresolved
		replace = "\nreplace " + coreModule + " => " + r.Replace + "\n"
	case releaseVersion.MatchString(version):
		out.Requires = "v" + strings.TrimPrefix(version, "v")
	default:
		// A build that is not a release: require the commit it was built
		// from, as the pseudo-version Go resolves it to.
		ref := commit
		if ref == "" || ref == "unknown" {
			ref = "main"
		}
		if goErr != nil {
			return out, fmt.Errorf("this open-conduit is a development build, so the version to require comes from `go list`, and go is not on PATH: %w", goErr)
		}
		cmd := exec.Command(gocmd, "list", "-m", "-f", "{{.Version}}", coreModule+"@"+ref)
		cmd.Dir = os.TempDir()
		raw, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("this open-conduit is a development build of %s and Go cannot resolve that commit (%s); build against a checkout with --replace DIR", ref, strings.TrimSpace(string(raw)))
		}
		out.Requires = strings.TrimSpace(string(raw))
	}
	values := strings.NewReplacer("{{NAME}}", r.Name, "{{MODULE}}", out.Module, "{{CORE_VERSION}}", out.Requires, "{{REPLACE}}", replace)
	var err error
	if out.Files, err = writeScaffold(scaffoldFiles, "scaffold", r.Name, out.Dir, values); err != nil {
		return out, err
	}
	if goErr != nil {
		out.Notes = append(out.Notes, "go is not on PATH: run `go mod tidy` in "+out.Dir+" before `make check`")
		return out, nil
	}
	if r.Replace != "" {
		// Against a checkout, start from its requirements and go.sum, so
		// the package builds from the module cache the checkout builds
		// from even when tidy cannot run: tidy also wants the modules a
		// dependency's tests import, which that cache may not hold.
		if err := seedFromCheckout(gocmd, r.Replace, out.Dir); err != nil {
			return out, err
		}
	}
	cmd := exec.Command(gocmd, "mod", "tidy")
	cmd.Dir = out.Dir
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		if r.Replace != "" {
			out.Notes = append(out.Notes, "`go mod tidy` failed, so go.mod and go.sum carry "+r.Replace+"'s requirements; run it once the module cache or network allows: "+strings.TrimSpace(lastLine(log.String())))
			return out, nil
		}
		return out, fmt.Errorf("wrote %s, but `go mod tidy` failed there (%v): %s; run it again once that is fixed", out.Dir, err, strings.TrimSpace(log.String()))
	}
	return out, nil
}

// newTSPackage writes a TypeScript package (--lang ts). Its package.json
// depends on this release's @open-conduit/sdk, or with --replace on the
// checkout's sdk/typescript. Nothing is installed: npm install needs the
// network, so the result says to run it.
func newTSPackage(version string, r cli.NewRequest) (cli.NewResult, error) {
	out := cli.NewResult{Name: r.Name, Dir: r.Dir, Lang: "ts"}
	if r.Module != "" {
		return out, errors.New("--module names a Go module; a TypeScript package has none")
	}
	switch {
	case r.Replace != "":
		sdk := filepath.Join(r.Replace, "sdk", "typescript")
		if _, err := os.Stat(filepath.Join(sdk, "package.json")); err != nil {
			return out, fmt.Errorf("--replace %s: not an Open Conduit checkout with the TypeScript SDK (no sdk/typescript/package.json)", r.Replace)
		}
		out.Requires = "file:" + filepath.ToSlash(sdk)
	case releaseVersion.MatchString(version):
		// The SDK is released with the core, under the same version.
		out.Requires = strings.TrimPrefix(version, "v")
	default:
		return out, errors.New("this open-conduit is a development build, which has no released @open-conduit/sdk to depend on; build against a checkout with --replace DIR")
	}
	values := strings.NewReplacer("{{NAME}}", r.Name, "{{SDK}}", out.Requires)
	var err error
	if out.Files, err = writeScaffold(scaffoldTSFiles, "scaffold-ts", r.Name, out.Dir, values); err != nil {
		return out, err
	}
	out.Notes = append(out.Notes, "run `npm install` in "+out.Dir+" before `make check`; `make lint` and `make certify` run the open-conduit on PATH")
	return out, nil
}

// checkEmpty refuses a directory that exists and holds anything.
func checkEmpty(dir string) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s already exists and is not empty; choose another with --dir", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeScaffold writes root's templates into dir with values substituted,
// returning the paths written, sorted.
func writeScaffold(files embed.FS, root, name, dir string, values *strings.Replacer) ([]string, error) {
	var written []string
	err := fs.WalkDir(files, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		rel := scaffoldPath(name, strings.TrimPrefix(p, root+"/"))
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		written = append(written, rel)
		return os.WriteFile(dst, []byte(values.Replace(string(raw))), 0o644)
	})
	sort.Strings(written)
	return written, err
}

// seedFromCheckout copies an Open Conduit checkout's requirements (as
// indirect) and go.sum into the new repository.
func seedFromCheckout(gocmd, checkout, dir string) error {
	cmd := exec.Command(gocmd, "mod", "edit", "-json")
	cmd.Dir = checkout
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("--replace %s: read its go.mod: %w", checkout, err)
	}
	var mod struct {
		Require []struct{ Path, Version string }
	}
	if err := json.Unmarshal(raw, &mod); err != nil {
		return fmt.Errorf("--replace %s: read its go.mod: %w", checkout, err)
	}
	var b strings.Builder
	b.WriteString("\nrequire (\n")
	for _, req := range mod.Require {
		fmt.Fprintf(&b, "\t%s %s // indirect\n", req.Path, req.Version)
	}
	b.WriteString(")\n")
	f, err := os.OpenFile(filepath.Join(dir, "go.mod"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	sum, err := os.ReadFile(filepath.Join(checkout, "go.sum"))
	if err != nil {
		return fmt.Errorf("--replace %s: %w", checkout, err)
	}
	return os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndex(s, "\n")+1:]
}

// checkPackageName refuses a name the manifest rules, Open Conduit's own
// registrations or the packages it bundles rule out, before anything is
// written.
func checkPackageName(name string) error {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: name, Version: "0.1.0", Command: []string{name + "-extension"},
		Provides: protocol.Provides{Operations: []protocol.OperationDecl{{Name: name + ".hello", Kind: protocol.OperationHandler}}}}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("%q cannot name a package: %w", name, err)
	}
	if err := extproc.CheckConflicts([]extproc.Found{{Manifest: m}}, hostTaken()); err != nil {
		return fmt.Errorf("%q cannot name a package: %w", name, err)
	}
	found, _ := discoverPackages("", "")
	for _, f := range found {
		if f.Manifest.Name == name {
			return fmt.Errorf("%q cannot name a package: open-conduit bundles a package of that name", name)
		}
	}
	return nil
}
