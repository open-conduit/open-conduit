// Package devscope builds an extension package from its source and
// generates the config of one scope for it: an entry for each extension
// type it declares, a claude-code binding granted its operations, a route
// for each route handler and a manual route. extensiontest's Daemon and
// `open-conduit dev` both run a package this way, so a package behaves the
// same under its tests as it does while its author watches it.
//
// It uses only the public protocol package and never imports testing.
package devscope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/protocol"
)

// ScopeID is the scope Generate writes, and AgentBinding the binding in it
// that runs the agent.
const (
	ScopeID      = "test"
	AgentBinding = "agent"
)

// Docs is where the generated config is described.
const Docs = "see \"Testing a package\" in https://github.com/open-conduit/open-conduit/blob/main/site/src/content/docs/architecture/extension-packaging.md"

// Build builds src into a package directory and returns it with its
// manifest. src is either a built package directory (one holding an
// open-conduit-extension.json), returned as it is, or a Go main package,
// as a directory: Build compiles it into a new temporary directory, as
// `make packages` would, asking the binary for its manifest and
// certification with package-manifest and package-certification, and
// copies the manifest's playbooks directory from src or its module's
// root. A directory that is not a main package but has exactly one under
// cmd/ builds that one, so Build(".") works from a package repository's
// root.
func Build(src string) (string, protocol.Manifest, error) {
	src, err := filepath.Abs(src)
	if err != nil {
		return "", protocol.Manifest{}, err
	}
	if raw, err := os.ReadFile(filepath.Join(src, protocol.ManifestFile)); err == nil {
		m, err := protocol.ParseManifest(raw)
		if err != nil {
			return "", m, fmt.Errorf("package %s: its manifest is refused: %w (`open-conduit extension inspect %s` names each rule)", src, err, src)
		}
		return src, m, nil
	}
	main, err := mainPackage(src)
	if err != nil {
		return "", protocol.Manifest{}, err
	}
	out, err := os.MkdirTemp("", "oc-devscope-pkg-")
	if err != nil {
		return "", protocol.Manifest{}, err
	}
	bin := filepath.Join(out, ".build")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = main
	if o, err := cmd.CombinedOutput(); err != nil {
		return "", protocol.Manifest{}, fmt.Errorf("build package %s: %v\n%s", main, err, o)
	}
	raw, err := exec.Command(bin, "package-manifest").Output()
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return "", protocol.Manifest{}, fmt.Errorf("package %s: `package-manifest` printed no manifest (%v): its main must answer it with sdk.PackageCommand; %s", main, err, Docs)
	}
	m, err := protocol.ParseManifest(raw)
	if err != nil {
		return "", m, fmt.Errorf("package %s: its manifest is refused: %w", main, err)
	}
	if len(m.Command) == 0 || filepath.IsAbs(m.Command[0]) || strings.Contains(m.Command[0], "/") {
		return "", m, fmt.Errorf("package %s: the manifest's command must start with the binary's own name, relative to the package directory", main)
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
			return "", m, fmt.Errorf("package %s: the manifest's playbooks directory %q is in neither %s nor its module's root", main, pb, src)
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
		return "", fmt.Errorf("package %s holds no %s and is not a Go main package (nor has one under cmd/): give a built package directory or the package's main; %s", src, protocol.ManifestFile, Docs)
	}
	return "", fmt.Errorf("package %s has more than one main package under cmd/ (%s): give the one to build", src, strings.Join(mains, ", "))
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

// Install copies a package directory to extensions/<manifest name>,
// replacing what is there.
func Install(dir, extensions string) error {
	raw, err := os.ReadFile(filepath.Join(dir, protocol.ManifestFile))
	if err != nil {
		return fmt.Errorf("package %s: %w", dir, err)
	}
	m, err := protocol.ParseManifest(raw)
	if err != nil {
		return fmt.Errorf("package %s: %w", dir, err)
	}
	dst := filepath.Join(extensions, m.Name)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.CopyFS(dst, os.DirFS(dir))
}

// Secret is a secret of the generated scope: its secret_refs name it and
// every generated entry selects it. Ref, when set, is the reference it
// resolves through (env:NAME); otherwise it resolves to Value, through a
// variable Generate adds to the daemon's environment.
type Secret struct {
	Name, Value, Ref string
}

// Options are what Generate puts in the scope beyond the manifests.
type Options struct {
	// Agent is the claude-code binding's binary.
	Agent string
	// AgentEnv is the binding's env (CLAUDE_CONFIG_DIR, say).
	AgentEnv map[string]string
	Secrets  []Secret
	// Entries are the configs of the generated entries, by extension type
	// ({} for a type not here). Each must name a type a manifest declares.
	Entries map[string]json.RawMessage
}

// Generate is the config of one scope, ScopeID, for the manifests: an
// entry for each extension type they declare (named after its type, with
// its config from Entries, and every secret selected), a binding
// AgentBinding running Agent granted every operation they declare plus
// read_context, wait and declare_outcome, a route for each route handler
// they declare, and a manual route last. env is what the daemon's
// environment needs for the secrets given by value.
func Generate(manifests []protocol.Manifest, o Options) (config []byte, env []string, err error) {
	ops := []string{"read_context", "wait", "declare_outcome"}
	var routes, entries []any
	declared := map[string]bool{}
	names := []string{}
	for _, s := range o.Secrets {
		names = append(names, s.Name)
	}
	for _, m := range manifests {
		for _, op := range m.Provides.Operations {
			if !contains(ops, op.Name) {
				ops = append(ops, op.Name)
			}
		}
		for _, r := range m.Provides.Routes {
			routes = append(routes, map[string]any{"name": Name(r.Handler), "handler": r.Handler, "binding": AgentBinding, "active_policy": "queue", "config": map[string]any{}})
		}
		for _, e := range m.Provides.Extensions {
			declared[e.Type] = true
			var config any = map[string]any{}
			if raw, ok := o.Entries[e.Type]; ok {
				config = raw
			}
			entries = append(entries, map[string]any{"name": Name(e.Type), "type": e.Type, "secret_names": names, "config": config})
		}
	}
	var undeclared []string
	for typ := range o.Entries {
		if !declared[typ] {
			undeclared = append(undeclared, typ)
		}
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		return nil, nil, fmt.Errorf("an entry config names a type no package declares in provides.extensions: %s; %s", strings.Join(undeclared, ", "), Docs)
	}
	routes = append(routes, map[string]any{"name": "manual", "handler": "manual", "binding": AgentBinding, "active_policy": "queue", "config": map[string]any{}})
	refs := map[string]any{}
	for i, s := range o.Secrets {
		if s.Ref != "" {
			refs[s.Name] = s.Ref
			continue
		}
		v := "OC_EXTENSIONTEST_SECRET_" + strconv.Itoa(i)
		refs[s.Name] = "env:" + v
		env = append(env, v+"="+s.Value)
	}
	agentEnv := o.AgentEnv
	if agentEnv == nil {
		agentEnv = map[string]string{}
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": ScopeID, "name": "Test",
			"authorization": map[string]any{"connections": []string{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{
				"name": AgentBinding, "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp", "inject": true, "resume": true},
				"operations": ops, "secret_names": []any{},
				"config": map[string]any{"binary": o.Agent, "playbook": "-", "builtin_tools": []string{}, "cold_turns": "resume", "env": agentEnv},
			}},
			"routes":      routes,
			"extensions":  nonNil(entries),
			"secret_refs": refs,
		}},
	}
	config, err = json.MarshalIndent(cfg, "", "    ")
	return config, env, err
}

var identifier = regexp.MustCompile(`[^a-z0-9_-]+`)

// Name is s as a config identifier.
func Name(s string) string {
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
