package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
)

// setupProbes are the probes a setup session may run besides the setup
// extension's own, by full name: each extension's and executor's
// (extension.SetupProbes), and the host's own init and doctor probes,
// under HostProbeOwner.
func (d *daemon) setupProbes() map[string]extension.SetupProbe {
	out := map[string]extension.SetupProbe{}
	for owner, p := range map[string]extension.SetupProbes{ClaudeCodeExecutor: claudecode.Probes{}, gh.Type: gh.Probes{}} {
		for _, x := range p.SetupProbes() {
			out[owner+"."+x.Name] = x
		}
	}
	out[HostProbeOwner+".init"] = d.initProbe()
	out[HostProbeOwner+".doctor"] = d.doctorProbe()
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
	all := Setups()
	d.external.mu.Lock()
	for _, p := range d.external.packages {
		if s, ok := p.r.Setup(); ok {
			all = append(all, s)
		}
	}
	d.external.mu.Unlock()
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
				text = b.Text
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
