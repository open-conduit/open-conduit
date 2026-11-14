package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
	"github.com/open-conduit/open-conduit/protocol"
)

// TestCheck: claude_code runs --version once per binding binary, fails
// naming the bindings of one that doesn't run, and reads usage only for
// the logins of bindings that set usage, warning at 90%.
func TestCheck(t *testing.T) {
	x := claudecode.NewExtension()
	var reads []string
	x.StubChecks(func(_ context.Context, dir string) ([]claudecode.UsageWindow, error) {
		reads = append(reads, dir)
		if dir == "/broken" {
			return nil, errors.New("unreachable")
		}
		return []claudecode.UsageWindow{{Name: "5h", Used: 93, ResetsAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}}, nil
	}, func(_ context.Context, bin string) (string, error) {
		if bin == "/opt/missing" {
			return "", errors.New("not runnable")
		}
		return "2.1.0 (Claude Code)", nil
	})
	checks := x.Checks()
	if len(checks) != 1 || !checks[0].Offline || checks[0].Deep {
		t.Fatalf("checks %+v, want one offline check", checks)
	}
	binding := func(name, executor string, cfg map[string]any) protocol.CheckBinding {
		raw, _ := json.Marshal(cfg)
		return protocol.CheckBinding{Name: name, Executor: executor, Config: raw}
	}
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		binding("agent", claudecode.Name, map[string]any{}),
		binding("watched", claudecode.Name, map[string]any{"usage": true}),
		binding("other", claudecode.Name, map[string]any{"usage": true, "env": map[string]string{"CLAUDE_CONFIG_DIR": "/broken"}}),
		binding("shell", "shell", map[string]any{"usage": true}),
	}}}}
	got := checks[0].Run(context.Background(), cfg)
	if got.Name != "claude-code" || got.Status != inproc.CheckWarn || got.Code != "usage_high" {
		t.Fatalf("check %+v, want a usage_high warning", got)
	}
	if !slices.Equal(reads, []string{"", "/broken"}) {
		t.Errorf("usage read for %q, want the default login and /broken", reads)
	}
	want := []string{"claude: 2.1.0 (Claude Code)", "usage 5h: 93% (resets 2026-11-03T12:00:00Z)", "usage (/broken) unknown"}
	if !slices.Equal(got.Items, want) {
		t.Errorf("items %q, want %q", got.Items, want)
	}

	cfg.Scopes[0].Bindings = []protocol.CheckBinding{binding("agent", claudecode.Name, map[string]any{"binary": "/opt/missing"})}
	got = checks[0].Run(context.Background(), cfg)
	if got.Status != inproc.CheckFail || got.Code != "claude_missing" || len(got.Items) != 1 || !strings.Contains(got.Items[0], "eng/agent") {
		t.Errorf("missing binary: %+v", got)
	}

	got = checks[0].Run(context.Background(), protocol.CheckConfig{})
	if got.Status != inproc.CheckInfo || got.Code != "not_configured" {
		t.Errorf("no bindings: %+v", got)
	}
}

// TestCheckContainerBindings: a binding whose container provider runs its
// version probe (Probed) is not looked for on the host, so with no host
// claude and only such bindings the check is info, not claude_missing; a
// host binding beside it is still run. The controls: the same binding on
// the host, or in a container whose provider runs no probe (as ssh), fails
// as missing.
func TestCheckContainerBindings(t *testing.T) {
	x := claudecode.NewExtension()
	var ran []string
	x.StubChecks(nil, func(_ context.Context, bin string) (string, error) {
		ran = append(ran, bin)
		if bin == "/opt/missing" {
			return "", errors.New("not runnable")
		}
		return "2.1.0 (Claude Code)", nil
	})
	check := x.Checks()[0]
	binding := func(name, isolation string, cfg map[string]any) protocol.CheckBinding {
		raw, _ := json.Marshal(cfg)
		return protocol.CheckBinding{Name: name, Executor: claudecode.Name, Config: raw, Isolation: isolation, Probed: isolation == protocol.IsolationContainer}
	}
	cfg := func(bs ...protocol.CheckBinding) protocol.CheckConfig {
		return protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: bs}}}
	}

	got := check.Run(context.Background(), cfg(binding("boxed", protocol.IsolationContainer, map[string]any{"binary": "/opt/missing"})))
	if got.Status != inproc.CheckInfo || got.Code != "in_container" || len(ran) != 0 {
		t.Fatalf("container binding only: %+v, ran %q", got, ran)
	}
	if !slices.Equal(got.Items, []string{"in their container images, not on the host: eng/boxed"}) {
		t.Errorf("items %q", got.Items)
	}

	got = check.Run(context.Background(), cfg(binding("boxed", protocol.IsolationContainer, map[string]any{"binary": "/opt/missing"}), binding("agent", "", map[string]any{})))
	if got.Status != inproc.CheckOK || !slices.Equal(ran, []string{"claude"}) {
		t.Errorf("mixed: %+v, ran %q", got, ran)
	}

	got = check.Run(context.Background(), cfg(binding("boxed", "", map[string]any{"binary": "/opt/missing"})))
	if got.Status != inproc.CheckFail || got.Code != "claude_missing" {
		t.Errorf("control: the binding on the host is %+v, want claude_missing", got)
	}
	unprobed := binding("remote", protocol.IsolationContainer, map[string]any{"binary": "/opt/missing"})
	unprobed.Probed = false
	got = check.Run(context.Background(), cfg(unprobed))
	if got.Status != inproc.CheckFail || got.Code != "claude_missing" {
		t.Errorf("control: a container binding no provider probes is %+v, want claude_missing", got)
	}
}

// TestVersionProbe: the probe is the binding's binary with --version and
// the PATH a container launch gives it, the binding's own when it sets one.
func TestVersionProbe(t *testing.T) {
	e := claudecode.Offline()
	argv, env := e.VersionProbe(json.RawMessage(`{}`))
	if !slices.Equal(argv, []string{"claude", "--version"}) || len(env) != 1 || !strings.HasPrefix(env[0], "PATH=/usr/local/sbin:") {
		t.Errorf("default probe %q %q", argv, env)
	}
	argv, env = e.VersionProbe(json.RawMessage(`{"binary":"/opt/claude/bin/claude","env":{"PATH":"/opt/claude/bin"}}`))
	if !slices.Equal(argv, []string{"/opt/claude/bin/claude", "--version"}) || !slices.Equal(env, []string{"PATH=/opt/claude/bin"}) {
		t.Errorf("configured probe %q %q", argv, env)
	}
	if argv, _ := e.VersionProbe(json.RawMessage(`{"model":`)); argv != nil {
		t.Errorf("a refused config probes %q", argv)
	}
}
