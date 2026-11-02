package extensiontest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/open-conduit/open-conduit/extensiontest/devscope"
	"github.com/open-conduit/open-conduit/protocol"
)

// ScopeID is the scope Daemon generates for the packages it is given, and
// AgentBinding the binding in it that runs the scripted agent.
const (
	ScopeID      = devscope.ScopeID
	AgentBinding = devscope.AgentBinding
)

// docs is where the generated config is described, named in each failure
// so the test's author knows what to change.
const docs = devscope.Docs

// Option configures Daemon: Options, Package, Secret, Entry or
// UseFakeClaude.
type Option interface{ apply(*settings) }

type settings struct {
	opts     Options
	packages []string
	secrets  []secret
	entries  map[string]any
	claude   bool
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
// selected), a binding AgentBinding running the scripted agent (Agent: a
// ScriptedShell on the shell executor, or a FakeClaude with UseFakeClaude)
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

// UseFakeClaude runs the generated scope's agent as a FakeClaude on a
// claude-code binding (the MCP toolbelt, inject and resume) instead of the
// default ScriptedShell on a shell binding. Use it to test a package
// against an LLM executor's turns: Ends then takes the structured status
// each turn ends with.
func UseFakeClaude() Option {
	return optionFunc(func(s *settings) { s.claude = true })
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

// build is devscope.Build, its errors named as extensiontest's.
func build(src string) (string, protocol.Manifest, error) {
	dir, m, err := devscope.Build(src)
	if err != nil {
		return "", m, fmt.Errorf("extensiontest: %w", err)
	}
	return dir, m, nil
}

// scope is what Daemon generated: the scope's agent.
type scope struct{ agent *Agent }

// generate writes the config of one scope for the manifests into d.opts
// (devscope.Generate), its agent the scripted one.
func (d *Instance) generate(set *settings, manifests []protocol.Manifest) {
	d.t.Helper()
	var o devscope.Options
	dir := filepath.Join(d.root, "agent")
	if set.claude {
		claude, err := FakeClaude(dir)
		if err != nil {
			d.t.Fatal(err)
		}
		o.Agent, o.AgentEnv = claude.Path, map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(d.root, "claude-config")}
	} else {
		shell, err := ScriptedShell(dir)
		if err != nil {
			d.t.Fatal(err)
		}
		o.Shell = []string{shell.Path}
	}
	for _, s := range set.secrets {
		o.Secrets = append(o.Secrets, devscope.Secret{Name: s.name, Value: s.value})
	}
	for typ, config := range set.entries {
		b, err := json.Marshal(config)
		if err != nil {
			d.t.Fatalf("extensiontest: Entry(%q): %v", typ, err)
		}
		if o.Entries == nil {
			o.Entries = map[string]json.RawMessage{}
		}
		o.Entries[typ] = b
	}
	cfg, env, err := devscope.Generate(manifests, o)
	if err != nil {
		d.t.Fatalf("extensiontest: %v", err)
	}
	d.opts.Config, d.opts.Env = cfg, append(append([]string{}, d.opts.Env...), env...)
	d.scope = &scope{agent: &Agent{d: d, dir: dir}}
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
