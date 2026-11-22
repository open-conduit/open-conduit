package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// setupProbes are the probes a setup session may run besides the setup
// extension's own, by full name: each package's (provides.probes, through
// probe.serve), and the host's own init and doctor probes, under
// HostProbeOwner.
func (d *daemon) setupProbes() map[string]extension.SetupProbe {
	out := map[string]extension.SetupProbe{}
	for _, c := range d.catalogued() {
		if r := d.runningRemote(c.Name()); r != nil {
			for _, x := range r.Probes() {
				out[c.Name()+"."+x.Name] = x
			}
			continue
		}
		for _, x := range d.dormantProbes(c.Name()) {
			out[c.Name()+"."+x.Name] = x
		}
	}
	out[HostProbeOwner+".init"] = d.initProbe()
	out[HostProbeOwner+".doctor"] = d.doctorProbe()
	return out
}

// hostProbeNames are the full names of setupProbes, offline: the host's
// and the bundled packages'.
func hostProbeNames() []string {
	out := []string{HostProbeOwner + ".init", HostProbeOwner + ".doctor"}
	found, _ := discoverPackages("", "")
	for _, f := range found {
		for _, x := range f.Manifest.Provides.Probes {
			out = append(out, f.Manifest.Name+"."+x.Name)
		}
	}
	return out
}

// HostProbeOwner names the host's own probes (open-conduit.init and
// open-conduit.doctor), so none is named after the extension that runs it.
const HostProbeOwner = "open-conduit"

// initProbe is `init TYPE --dry-run` for a registered setup, with a host
// that never prompts, stores nothing and runs nothing: what the scope would
// be, for the agent to propose. Reference text in it is redacted.
func (d *daemon) initProbe() extension.SetupProbe {
	var types []string
	setups := map[string]extension.Setup{}
	all := d.setups()
	for _, s := range all {
		info := s.SetupInfo()
		if _, dup := setups[info.Type]; !info.FirstRun && !dup {
			types = append(types, info.Type)
			setups[info.Type] = s
		}
	}
	return extension.SetupProbe{Name: "init", Summary: "the connections and scope `init TYPE --dry-run` would add (" + strings.Join(types, ", ") + "), from flags with references, never values; it prompts for nothing and stores nothing",
		Args: []extension.ProbeArg{{Name: "type", Summary: strings.Join(types, ", "), Required: true}, {Name: "flags", Summary: "init's flags, as `open-conduit adapters TYPE` lists them (--repo OWNER/NAME --scope ID --token keyring:NAME …)"}},
		Run: func(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
			s, ok := setups[args["type"]]
			if !ok {
				return extension.ProbeResult{}, fmt.Errorf("%w: type is one of %s", extension.ErrProbeInput, strings.Join(types, ", "))
			}
			flags := strings.Fields(args["flags"])
			var text []byte
			if b, err := d.configHost().Base(ctx); err == nil {
				text = []byte(b.Text)
			}
			r, err := s.Setup(ctx, probeSetupHost{config: text, state: d.o.StateDir}, flags)
			if err != nil {
				msg := err.Error()
				if len(msg) > 300 {
					msg = msg[:300]
				}
				return extension.ProbeResult{}, errors.New(msg)
			}
			var conns []json.RawMessage
			for _, c := range r.Connections {
				conns = append(conns, redactRefs(c))
			}
			return extension.ProbeResult{Command: "open-conduit init " + args["type"] + " --dry-run " + strings.Join(flags, " "),
				Output: map[string]any{"connections": conns, "scope": redactRefs(r.Scope), "next": r.Next}}, nil
		}}
}

// redactRefs replaces every secret_refs value in a JSON document.
func redactRefs(doc json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return nil
	}
	var walk func(v any, refs bool) any
	walk = func(v any, refs bool) any {
		switch x := v.(type) {
		case map[string]any:
			for k, m := range x {
				if refs {
					x[k] = "[redacted]"
					continue
				}
				x[k] = walk(m, k == "secret_refs")
			}
		case []any:
			for i, m := range x {
				x[i] = walk(m, false)
			}
		}
		return v
	}
	b, _ := json.Marshal(walk(v, false))
	return b
}

// doctorProbe runs doctor's ordinary checks: none resolves a secret or
// calls a service. Proving a credential works stays the operator's doctor
// with secrets checked.
func (d *daemon) doctorProbe() extension.SetupProbe {
	return extension.SetupProbe{Name: "doctor", Summary: "doctor's checks that need no secret: config, webhooks, service, the executors",
		Run: func(ctx context.Context, _ map[string]string) (extension.ProbeResult, error) {
			type check struct {
				Name   string   `json:"name"`
				Status string   `json:"status"`
				Code   string   `json:"code,omitempty"`
				Detail string   `json:"detail,omitempty"`
				Items  []string `json:"items,omitempty"`
			}
			out := []check{}
			for _, run := range d.checks() {
				c := run(ctx)
				out = append(out, check{c.Name, c.Status, c.Code, c.Detail, c.Items})
			}
			return extension.ProbeResult{Command: "open-conduit doctor", Output: map[string]any{"checks": out}}, nil
		}}
}

// probeSetupHost hosts an extension.Setup for the init probe: it never
// prompts, stores nothing, opens nothing and runs nothing.
type probeSetupHost struct {
	config []byte
	state  string
}

var errProbeNoRun = errors.New("the init probe runs nothing: pass what it would ask for as flags")

func (probeSetupHost) Printf(string, ...any) {}
func (probeSetupHost) Prompt(string, extension.Prompt) (string, error) {
	return "", extension.ErrSetupNoInput
}
func (probeSetupHost) Choose(string, []string) (int, error) { return 0, extension.ErrSetupNoInput }
func (probeSetupHost) OpenURL(string) error                 { return errProbeNoRun }
func (probeSetupHost) StoreSecret(name, _ string) (string, error) {
	return "keyring:" + name, nil
}
func (probeSetupHost) LookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	return p, err == nil
}
func (probeSetupHost) Run(context.Context, string, string, ...string) error { return errProbeNoRun }
func (h probeSetupHost) Config() []byte                                     { return h.config }
func (probeSetupHost) Cwd() string {
	home, _ := os.UserHomeDir()
	return home
}
func (h probeSetupHost) StateDir() string { return h.state }
func (probeSetupHost) DryRun() bool       { return true }

// setups are the catalogued packages' setup flows: a running package's
// through its process, and one that isn't running started for the flow
// alone, as `init TYPE` starts it (packageSetup).
func (d *daemon) setups() []extension.Setup {
	var out []extension.Setup
	for _, c := range d.catalogued() {
		if r := d.runningRemote(c.Name()); r != nil {
			if s, ok := r.Setup(); ok {
				out = append(out, s)
			}
			continue
		}
		if decl := c.m.Provides.Setup; decl != nil {
			d.external.mu.Lock()
			f := d.external.found[c.Name()]
			d.external.mu.Unlock()
			out = append(out, packageSetup{f: f, configPath: d.o.ConfigPath, stateDir: d.o.StateDir, version: d.o.Version, info: extension.SetupInfo{Type: decl.Type, Summary: decl.Summary, Usage: decl.Usage, FirstRun: decl.FirstRun}})
		}
	}
	return out
}

// dormantProbes are the setup probes of a catalogued package that isn't
// running: each run starts the package for that probe alone and stops it
// after, as packageSetup does for a setup flow.
func (d *daemon) dormantProbes(name string) []extension.SetupProbe {
	d.external.mu.Lock()
	f, ok := d.external.found[name]
	d.external.mu.Unlock()
	if !ok {
		return nil
	}
	var out []extension.SetupProbe
	for _, info := range f.Manifest.Provides.Probes {
		probe := info.Name
		p := extension.SetupProbe{Name: probe, Summary: info.Summary}
		for _, a := range info.Args {
			p.Args = append(p.Args, extension.ProbeArg{Name: a.Name, Summary: a.Summary, Required: a.Required, Connection: a.Connection})
		}
		p.Run = func(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
			paths := extproc.StatePaths(d.o.StateDir, name)
			r, err := extproc.Load(ctx, f, extproc.Deps{Now: time.Now, ProbeConnection: d.probeConnection(f.Manifest), ProbeSecrets: d.probeSecrets(f.Manifest)}, extproc.Options{StateDir: paths.State, LogDir: filepath.Dir(paths.Log), Host: protocol.HostInfo{Name: "open-conduit", Version: d.o.Version},
				PassEnv: func() []string { return d.packagePassEnv(name) }})
			if err != nil {
				return extension.ProbeResult{}, diagnosed(name, err)
			}
			defer r.Process().Close()
			for _, x := range r.Probes() {
				if x.Name == probe {
					return x.Run(ctx, args)
				}
			}
			return extension.ProbeResult{}, errors.New("the package declares no probe " + probe)
		}
		out = append(out, p)
	}
	return out
}
