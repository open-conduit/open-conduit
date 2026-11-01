package extensiontest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/open-conduit/open-conduit/protocol"
)

// ScopeID is the scope Daemon generates for the packages it is given, and
// AgentBinding the binding in it that runs the scripted agent.
const (
	ScopeID      = "test"
	AgentBinding = "agent"
)

// docs is where the generated config is described, named in each failure
// so the test's author knows what to change.
const docs = "see \"Testing a package\" in https://github.com/open-conduit/open-conduit/blob/main/site/src/content/docs/architecture/extension-packaging.md"

// Option configures Daemon: Options, Package, Secret or Entry.
type Option interface{ apply(*settings) }

type settings struct {
	opts     Options
	packages []string
	secrets  []secret
	entries  map[string]any
}

type secret struct{ name, value string }

func (o Options) apply(s *settings) {
	pkgs := append(s.opts.Packages, o.Packages...)
	s.opts = o
	s.opts.Packages = pkgs
}

type optionFunc func(*settings)

func (f optionFunc) apply(s *settings) { f(s) }

// Package is a package to install and test. src is either a built package
// directory (one holding an open-conduit-extension.json) or a Go main
// package, as a directory: Daemon builds it, as `make packages` would,
// asking the binary for its manifest and certification with
// package-manifest and package-certification, and copies the manifest's
// playbooks directory from src or its module's root. A directory that is
// not a main package but has exactly one under cmd/ builds that one, so
// Package(".") works from a package repository's root.
//
// Without Options.Config, Daemon generates one scope, ScopeID, for the
// packages: an entry for each extension type a manifest declares (named
// after its type, with config {} unless Entry gives one, and every Secret
// selected), a binding AgentBinding running the scripted agent (Agent)
// granted every operation the manifests declare plus read_context, wait and
// declare_outcome, a route for each route handler they declare, and a
// manual route last. Options.Config is the escape hatch when a test needs
// more.
func Package(src string) Option {
	return optionFunc(func(s *settings) { s.packages = append(s.packages, src) })
}

// Secret adds a secret to the generated scope: its secret_refs name it,
// every generated entry selects it, and it resolves to value, so the
// package's secret.resolve returns it as it would an operator's. Use it to
// point a package at an httptest fake of its service.
func Secret(name, value string) Option {
	return optionFunc(func(s *settings) { s.secrets = append(s.secrets, secret{name, value}) })
}

// Entry sets the config of the generated entry for an extension type the
// package declares (a JSON-marshalable value; {} when not given).
func Entry(typ string, config any) Option {
	return optionFunc(func(s *settings) {
		if s.entries == nil {
			s.entries = map[string]any{}
		}
		s.entries[typ] = config
	})
}

var (
	packagesMu sync.Mutex
	packages   = map[string]builtPackage{}
)

type builtPackage struct {
	dir string
	m   protocol.Manifest
	err error
}

// buildPackage is src's package directory and manifest, built once per
// test binary.
func buildPackage(src string) (string, protocol.Manifest, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", protocol.Manifest{}, err
	}
	packagesMu.Lock()
	defer packagesMu.Unlock()
	if p, ok := packages[abs]; ok {
		return p.dir, p.m, p.err
	}
	var p builtPackage
	p.dir, p.m, p.err = build(abs)
	packages[abs] = p
	return p.dir, p.m, p.err
}

func build(src string) (string, protocol.Manifest, error) {
	if raw, err := os.ReadFile(filepath.Join(src, protocol.ManifestFile)); err == nil {
		m, err := protocol.ParseManifest(raw)
		if err != nil {
			return "", m, fmt.Errorf("extensiontest: package %s: its manifest is refused: %w (`open-conduit extension inspect %s` names each rule)", src, err, src)
		}
		return src, m, nil
	}
	main, err := mainPackage(src)
	if err != nil {
		return "", protocol.Manifest{}, err
	}
	out, err := os.MkdirTemp("", "oc-extensiontest-pkg-")
	if err != nil {
		return "", protocol.Manifest{}, err
	}
	bin := filepath.Join(out, ".build")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = main
	if o, err := cmd.CombinedOutput(); err != nil {
		return "", protocol.Manifest{}, fmt.Errorf("extensiontest: build package %s: %v\n%s", main, err, o)
	}
	raw, err := exec.Command(bin, "package-manifest").Output()
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return "", protocol.Manifest{}, fmt.Errorf("extensiontest: package %s: `package-manifest` printed no manifest (%v): its main must answer it with sdk.PackageCommand; %s", main, err, docs)
	}
	m, err := protocol.ParseManifest(raw)
	if err != nil {
		return "", m, fmt.Errorf("extensiontest: package %s: its manifest is refused: %w", main, err)
	}
	if len(m.Command) == 0 || filepath.IsAbs(m.Command[0]) || strings.Contains(m.Command[0], "/") {
		return "", m, fmt.Errorf("extensiontest: package %s: the manifest's command must start with the binary's own name, relative to the package directory", main)
	}
	dir := filepath.Join(out, m.Name)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", m, err
	}
	if err = os.Rename(bin, filepath.Join(dir, m.Command[0])); err != nil {
		return "", m, err
	}
	if err = os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o644); err != nil {
		return "", m, err
	}
	if cert, err := exec.Command(filepath.Join(dir, m.Command[0]), "package-certification").Output(); err == nil && len(bytes.TrimSpace(cert)) > 0 {
		if err = os.WriteFile(filepath.Join(dir, "certification.json"), cert, 0o644); err != nil {
			return "", m, err
		}
	}
	if pb := m.Provides.Playbooks; pb != "" {
		found := ""
		for _, base := range []string{main, src, moduleRoot(main)} {
			if fi, err := os.Stat(filepath.Join(base, pb)); base != "" && err == nil && fi.IsDir() {
				found = filepath.Join(base, pb)
				break
			}
		}
		if found == "" {
			return "", m, fmt.Errorf("extensiontest: package %s: the manifest's playbooks directory %q is in neither %s nor its module's root", main, pb, src)
		}
		if err = os.CopyFS(filepath.Join(dir, pb), os.DirFS(found)); err != nil {
			return "", m, err
		}
	}
	return dir, m, nil
}

// mainPackage is src when it is a Go main package, else its one main
// package under cmd/.
func mainPackage(src string) (string, error) {
	if isMain(src) {
		return src, nil
	}
	var mains []string
	dirs, _ := filepath.Glob(filepath.Join(src, "cmd", "*"))
	for _, d := range dirs {
		if isMain(d) {
			mains = append(mains, d)
		}
	}
	switch len(mains) {
	case 1:
		return mains[0], nil
	case 0:
		return "", fmt.Errorf("extensiontest: package %s holds no %s and is not a Go main package (nor has one under cmd/): give Package a built package directory or the package's main; %s", src, protocol.ManifestFile, docs)
	}
	return "", fmt.Errorf("extensiontest: package %s has more than one main package under cmd/ (%s): give Package the one to build", src, strings.Join(mains, ", "))
}

func isMain(dir string) bool {
	cmd := exec.Command("go", "list", "-f", "{{.Name}}", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "main"
}

func moduleRoot(dir string) string {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// scope is what Daemon generated: the scope's agent.
type scope struct{ agent *Agent }

var identifier = regexp.MustCompile(`[^a-z0-9_-]+`)

// name is s as a config identifier.
func name(s string) string {
	n := identifier.ReplaceAllString(strings.ToLower(s), "-")
	n = strings.Trim(n, "-_")
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "x" + n
	}
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

// generate writes the config of one scope for the manifests into d.opts.
func (d *Instance) generate(set *settings, manifests []protocol.Manifest) {
	d.t.Helper()
	claude, err := FakeClaude(filepath.Join(d.root, "agent"))
	if err != nil {
		d.t.Fatal(err)
	}
	ops := []string{"read_context", "wait", "declare_outcome"}
	var routes, entries []any
	declared := map[string]bool{}
	for _, m := range manifests {
		for _, o := range m.Provides.Operations {
			if !contains(ops, o.Name) {
				ops = append(ops, o.Name)
			}
		}
		for _, r := range m.Provides.Routes {
			routes = append(routes, map[string]any{"name": name(r.Handler), "handler": r.Handler, "binding": AgentBinding, "active_policy": "queue", "config": map[string]any{}})
		}
		for _, e := range m.Provides.Extensions {
			declared[e.Type] = true
			config := set.entries[e.Type]
			if config == nil {
				config = map[string]any{}
			}
			names := []string{}
			for _, s := range set.secrets {
				names = append(names, s.name)
			}
			entries = append(entries, map[string]any{"name": name(e.Type), "type": e.Type, "secret_names": names, "config": config})
		}
	}
	for typ := range set.entries {
		if !declared[typ] {
			d.t.Fatalf("extensiontest: Entry(%q) names a type no package declares in provides.extensions; %s", typ, docs)
		}
	}
	routes = append(routes, map[string]any{"name": "manual", "handler": "manual", "binding": AgentBinding, "active_policy": "queue", "config": map[string]any{}})
	refs := map[string]any{}
	env := append([]string{}, d.opts.Env...)
	for i, s := range set.secrets {
		v := fmt.Sprintf("OC_EXTENSIONTEST_SECRET_%d", i)
		refs[s.name] = "env:" + v
		env = append(env, v+"="+s.value)
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": ScopeID, "name": "Test",
			"authorization": map[string]any{"connections": []string{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{
				"name": AgentBinding, "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp", "inject": true, "resume": true},
				"operations": ops, "secret_names": []any{},
				"config": map[string]any{"binary": claude.Path, "playbook": "-", "builtin_tools": []string{}, "cold_turns": "resume",
					"env": map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(d.root, "claude-config")}},
			}},
			"routes":      routes,
			"extensions":  nonNil(entries),
			"secret_refs": refs,
		}},
	}
	b, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		d.t.Fatal(err)
	}
	d.opts.Config, d.opts.Env = b, env
	d.scope = &scope{agent: &Agent{d: d, c: claude}}
}

// check runs `config check` on the generated config, so a refusal names
// its reason before the daemon starts.
func (d *Instance) check() {
	d.t.Helper()
	dir, err := binaries()
	if err != nil {
		d.t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "open-conduit"), "--data-dir", filepath.Join(d.root, "check-state"), "config", "check", "--file", d.config)
	cmd.Env = append(os.Environ(), d.opts.Env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		d.t.Fatalf("extensiontest: the config generated from the package's manifest is refused:\n%s\nGive the entry the config its package needs with extensiontest.Entry, or pass the whole config as Options.Config; %s\nThe generated config:\n%s", out, docs, d.opts.Config)
	} else if err != nil {
		d.t.Fatal(err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func nonNil(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}
