package dockerext

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/docker"
	"github.com/open-conduit/open-conduit/protocol"
)

func config(dctx string) json.RawMessage {
	return json.RawMessage(`{"image":"alpine@sha256:` + strings.Repeat("0", 64) + `","provider":{"context":"` + dctx + `"}}`)
}

// TestConfiguredContexts: the provider lists on the contexts the last
// configure's docker bindings name, once each, and nothing of another
// provider's or an invalid config.
func TestConfiguredContexts(t *testing.T) {
	x := &provider{}
	x.configure([]protocol.ContainerUse{
		{Provider: docker.Type, Config: config("a")},
		{Provider: docker.Type, Config: config("b")},
		{Provider: docker.Type, Config: config("a")},
		{Provider: "ssh", Config: config("c")},
		{Provider: docker.Type, Config: json.RawMessage(`{"image":"x"}`)},
	})
	if got := x.configured(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("contexts %v", got)
	}
	x.configure(nil)
	if got := x.configured(); len(got) != 0 {
		t.Fatalf("contexts after a configure with none: %v", got)
	}
}

// TestForward: a unix forward needs no address (the gateway mounts the
// socket); a tcp one is refused for a config that names no context.
func TestForward(t *testing.T) {
	cfg := domain.Payload{Data: config("default")}
	if addr, err := docker.New(docker.Options{Forward: docker.ForwardUnix}).Forward(context.Background(), cfg); err != nil || addr != "" {
		t.Fatalf("unix forward: %q %v", addr, err)
	}
	if _, err := docker.New(docker.Options{Forward: docker.ForwardTCP}).Forward(context.Background(), domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a tcp forward for an invalid config was answered")
	}
}

// TestCheckNotConfigured: with no docker binding the check says so and
// asks nothing of Docker.
func TestCheckNotConfigured(t *testing.T) {
	x := &provider{}
	got := x.check(context.Background(), protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "s", Bindings: []protocol.CheckBinding{{Name: "b", Executor: "shell"}}}}})
	if !got.OK || got.Code != "not_configured" {
		t.Fatalf("check %+v", got)
	}
}

// TestCheckProbes: each binding's version probe runs once per context,
// image and probe; one that runs is reported with its image, and one
// that fails fails the check naming the image and its bindings. A binding
// with no probe runs nothing.
func TestCheckProbes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	calls := filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
echo "$@" >> ` + calls + `
case "$*" in *"good@"*) echo '2.1.0 (Claude Code)';; *) echo 'claude: not found' >&2; exit 127;; esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	p := docker.New(docker.Options{Docker: bin, Host: "h", Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH")} }})
	probe := &protocol.Probe{Argv: []string{"claude", "--version"}, Env: []string{"PATH=/usr/bin"}}
	x := &provider{}

	out := protocol.CheckRunResult{OK: true, Status: "ok", Code: "ok"}
	x.probe(context.Background(), p, []use{{"s/a", "default", "good@sha256:1", probe}, {"s/b", "default", "good@sha256:1", probe}, {"s/c", "default", "good@sha256:1", nil}}, &out)
	if !out.OK || !slices.Equal(out.Items, []string{"claude in image good@sha256:1: 2.1.0 (Claude Code)"}) {
		t.Fatalf("an image with claude: %+v", out)
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "\n") != 1 {
		t.Errorf("docker ran %q, want one probe", b)
	}

	out = protocol.CheckRunResult{OK: true, Status: "ok", Code: "ok"}
	x.probe(context.Background(), p, []use{{"s/a", "default", "bare@sha256:2", probe}, {"s/b", "default", "bare@sha256:2", probe}}, &out)
	if out.OK || out.Status != "fail" || out.Code != "probe_failed" || len(out.Items) != 1 ||
		!strings.Contains(out.Items[0], "bare@sha256:2") || !strings.Contains(out.Items[0], "s/a, s/b") {
		t.Fatalf("an image without claude: %+v", out)
	}
}
