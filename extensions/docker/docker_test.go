package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

const image = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

func TestDockerConfig(t *testing.T) {
	ok := `{"image":"` + image + `","write":["{workspace}"],"provider":{"context":"orbstack"}}`
	if _, s, err := ParseConfig([]byte(ok)); err != nil || s.Context != "orbstack" {
		t.Fatalf("valid config: %+v %v", s, err)
	}
	for name, raw := range map[string]string{
		"no settings":     `{"image":"` + image + `"}`,
		"no context":      `{"image":"` + image + `","provider":{}}`,
		"bad context":     `{"image":"` + image + `","provider":{"context":"a b"}}`,
		"unknown setting": `{"image":"` + image + `","provider":{"context":"x","host":"tcp://h"}}`,
		"unpinned image":  `{"image":"alpine:3","provider":{"context":"x"}}`,
	} {
		if err := ValidateConfig([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDockerRefsAndMounts(t *testing.T) {
	if c, id, err := splitRef("orbstack/abc"); err != nil || c != "orbstack" || id != "abc" {
		t.Fatalf("splitRef: %q %q %v", c, id, err)
	}
	for _, bad := range []string{"abc", "/abc", "orb stack/abc", "orbstack/"} {
		if _, _, err := splitRef(bad); err == nil {
			t.Errorf("splitRef(%q) accepted", bad)
		}
	}
	if got := mountArg(Mount{Target: "/a,b", ReadOnly: true}); got != `type=bind,"source=/a,b","target=/a,b",readonly` {
		t.Errorf("mountArg quoted %s", got)
	}
}

// layout is a workspace with a clone's read-only paths, a run directory
// and records, as the host hands them to Plan.
func layout(t *testing.T) (extension.SandboxPaths, *Provider) {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	paths := extension.SandboxPaths{Workspace: root + "/ws", Run: root + "/run", Records: root + "/records"}
	for _, d := range []string{paths.Workspace + "/.git/hooks", paths.Workspace + "/.git/info", paths.Run, paths.Records, root + "/home/.cfg"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(paths.Workspace+"/.git/config", nil, 0o600)
	return paths, New(Options{Home: root + "/home", Host: "h"})
}

func TestDockerMountsCoverReadOnlyAndDenied(t *testing.T) {
	paths, p := layout(t)
	ws := paths.Workspace
	cfg := extension.ContainerConfig{Write: []string{"{workspace}"}, Read: []string{"~/.cfg", "~/missing", "{workspace}/sub"}}
	needs := extension.SandboxNeeds{ReadOnly: []string{ws + "/.git/config", ws + "/.git/hooks", ws + "/.git/info"}, Deny: []string{filepath.Dir(ws), ws + "/.git/info"}}
	mounts, err := p.mounts(cfg, paths, needs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Mount{}
	for _, m := range mounts {
		got[m.Target] = m
	}
	want := map[string]Mount{
		ws:                  {Target: ws},
		paths.Run:           {Target: paths.Run},
		p.o.Home + "/.cfg":  {Target: p.o.Home + "/.cfg", ReadOnly: true},
		paths.Records:       {Target: paths.Records, ReadOnly: true},
		ws + "/.git/config": {Target: ws + "/.git/config", ReadOnly: true},
		ws + "/.git/hooks":  {Target: ws + "/.git/hooks", ReadOnly: true},
		ws + "/.git/info":   {Source: paths.Records + "/empty.d", Target: ws + "/.git/info", ReadOnly: true},
	}
	if len(got) != len(want) {
		t.Errorf("mounts %+v, want %+v", mounts, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("mount %s: %+v, want %+v", k, got[k], w)
		}
	}
	for name, n := range map[string]extension.SandboxNeeds{
		"missing read-only": {ReadOnly: []string{ws + "/.git/absent"}},
		"missing denied":    {Deny: []string{ws + "/absent"}},
	} {
		if _, err := p.mounts(cfg, paths, n); !errors.Is(err, extension.ErrSandboxUnavailable) {
			t.Errorf("%s: %v, want sandbox_unavailable", name, err)
		}
	}
	if err := os.Symlink(ws+"/.git/config", ws+"/link"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.mounts(cfg, paths, extension.SandboxNeeds{ReadOnly: []string{ws + "/link"}}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlinked read-only need: %v, want refused", err)
	}
	if _, err := p.mounts(extension.ContainerConfig{Write: []string{"/no/such/dir"}}, paths, extension.SandboxNeeds{}); !errors.Is(err, extension.ErrSandboxUnavailable) {
		t.Errorf("a missing write grant: %v", err)
	}
}

func TestDockerNamesAreHostScoped(t *testing.T) {
	p := New(Options{})
	c1, n1 := p.names(map[string]string{extension.ContainerLabelHost: "a", extension.ContainerLabelExecution: "x", extension.ContainerLabelSegment: "2"})
	c2, _ := p.names(map[string]string{extension.ContainerLabelHost: "b", extension.ContainerLabelExecution: "x", extension.ContainerLabelSegment: "2"})
	if c1 == c2 || !strings.HasPrefix(c1, n1+"-") || !strings.HasSuffix(c1, "-2") {
		t.Errorf("names %s %s %s", c1, c2, n1)
	}
}

// TestDockerContainerEnv: a container's environment points at the
// gateway's proxy (whatever proxy the launch had), finds the gateway
// image's os first on PATH and, with a forward, reaches the toolbelt
// through the forward's socket.
func TestDockerContainerEnv(t *testing.T) {
	env := containerEnv([]string{"PATH=/host/toolbelt:/usr/bin", "OS_ENDPOINT=unix:///state/runs/k/toolbelt.sock", "HTTPS_PROXY=http://elsewhere:1", "KEEP=unix:///other.sock"}, "/state/runs/k/toolbelt.sock", true)
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Errorf("%s set twice", k)
		}
		got[k] = v
	}
	for k, want := range map[string]string{
		"PATH": BinDir + ":/host/toolbelt:/usr/bin", "OS_ENDPOINT": "unix://" + ToolbeltSocket, "KEEP": "unix:///other.sock",
		"HTTPS_PROXY": "http://gateway:3128", "http_proxy": "http://gateway:3128", "NO_PROXY": "localhost,127.0.0.1",
	} {
		if got[k] != want {
			t.Errorf("%s=%q, want %q", k, got[k], want)
		}
	}
	if env := containerEnv([]string{"OS_ENDPOINT=unix:///state/runs/k/toolbelt.sock"}, "/state/runs/k/toolbelt.sock", false); !strings.Contains(strings.Join(env, "\n"), "OS_ENDPOINT=unix:///state/runs/k/toolbelt.sock\nPATH="+BinDir+":") {
		t.Errorf("without a forward: %v", env)
	}
}

// TestDockerPlanNeedsAGateway: a build with no gateway image, or one not
// pinned by digest, refuses every launch; so does a socket other than the
// toolbelt.
func TestDockerPlanNeedsAGateway(t *testing.T) {
	paths, _ := layout(t)
	config := `{"image":"` + image + `","write":["{workspace}"],"provider":{"context":"orbstack"}}`
	policy := extension.ContainerPolicy{Config: domain.Payload{Data: []byte(config)}, Paths: paths}
	owner := extension.ResourceOwner{ExecutionID: "x"}
	launch := extension.SandboxLaunch{Argv: []string{"true"}, Dir: paths.Workspace, RunDir: paths.Run}
	for name, c := range map[string]struct {
		gateway string
		needs   extension.SandboxNeeds
	}{
		"no gateway":            {"", extension.SandboxNeeds{}},
		"an unpinned gateway":   {"ghcr.io/open-conduit/open-conduit-gateway:1", extension.SandboxNeeds{}},
		"a socket not the belt": {"sha256:" + strings.Repeat("a", 64), extension.SandboxNeeds{Sockets: []string{paths.Run + "/other.sock"}}},
	} {
		p := New(Options{Host: "h", Gateway: c.gateway})
		if c.gateway == "" {
			p.o.Gateway = ""
		}
		if _, err := p.Plan(context.Background(), owner, policy, c.needs, launch); !errors.Is(err, extension.ErrSandboxUnavailable) {
			t.Errorf("%s: %v, want sandbox_unavailable", name, err)
		}
	}
	if !gatewayRef.MatchString("ghcr.io/open-conduit/open-conduit-gateway@sha256:"+strings.Repeat("0", 64)) || !gatewayRef.MatchString("sha256:"+strings.Repeat("0", 64)) {
		t.Error("a pinned gateway reference was refused")
	}
}
