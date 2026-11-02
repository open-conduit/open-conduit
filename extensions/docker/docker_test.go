package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestDockerSecrets: a launch's credentials are written as shell exports,
// readable only by the operator, and the entry stub reads and removes them
// before running the command, which sees them though the container's own
// environment never held them. A secret whose name could be one of the
// stub's own variables, or that holds a NUL, refuses the plan.
func TestDockerSecrets(t *testing.T) {
	paths, _ := layout(t)
	file := filepath.Join(paths.Run, SecretsFile)
	if err := writeSecrets(file, []string{"CLAUDE_CODE_OAUTH_TOKEN=tok'en $x", "GH_TOKEN=gh"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v %v", fi, err)
	}
	marker := filepath.Join(paths.Run, extension.ContainerTermMarker)
	out, err := exec.Command("/bin/sh", "-c", entryStub, "open-conduit-entry", marker, file, "/bin/sh", "-c", `printf '%s|%s' "$CLAUDE_CODE_OAUTH_TOKEN" "$GH_TOKEN"`).Output()
	if err != nil || string(out) != "tok'en $x|gh" {
		t.Fatalf("the command saw %q (%v)", out, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the entry stub left the secrets file: %v", err)
	}
	// Without a secrets file, the stub runs the command as before.
	if out, err := exec.Command("/bin/sh", "-c", entryStub, "open-conduit-entry", marker, "", "/bin/echo", "hi").Output(); err != nil || string(out) != "hi\n" {
		t.Errorf("without secrets: %q %v", out, err)
	}
	_ = writeSecrets(file, []string{"A=1"})
	if out, err := exec.Command("/bin/sh", "-c", attachStub, "open-conduit-attach", file, "/bin/sh", "-c", `echo "$A"`).Output(); err != nil || string(out) != "1\n" {
		t.Errorf("the attach stub: %q %v", out, err)
	}

	config := `{"image":"` + image + `","write":["{workspace}"],"provider":{"context":"orbstack"}}`
	p := New(Options{Host: "h", Gateway: "sha256:" + strings.Repeat("a", 64)})
	for _, bad := range []string{"m=1", "lower_case=1", "TOKEN=a\x00b"} {
		launch := extension.SandboxLaunch{Argv: []string{"true"}, Dir: paths.Workspace, RunDir: paths.Run, Secrets: []string{bad}}
		if _, err := p.Plan(context.Background(), extension.ResourceOwner{ExecutionID: "x"}, extension.ContainerPolicy{Config: domain.Payload{Data: []byte(config)}, Paths: paths}, extension.SandboxNeeds{}, launch); !errors.Is(err, extension.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "secret") {
			t.Errorf("secret %q: %v, want refused", bad, err)
		}
	}
}

// TestDockerMountsTheWorkHome: the Work's home is mounted read-write.
func TestDockerMountsTheWorkHome(t *testing.T) {
	paths, p := layout(t)
	paths.Home = filepath.Join(filepath.Dir(paths.Run), "homes", "w")
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	mounts, err := p.mounts(extension.ContainerConfig{Write: []string{"{workspace}"}}, paths, extension.SandboxNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mounts {
		found = found || m == Mount{Target: paths.Home}
	}
	if !found {
		t.Errorf("mounts %+v have no read-write home", mounts)
	}
}

// stubDocker writes a docker CLI stand-in that runs script, and a Provider
// that uses it on one context.
func stubDocker(t *testing.T, script string, timeout time.Duration) *Provider {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return New(Options{Docker: bin, Host: "host-1", QueryTimeout: timeout, Contexts: func() []string { return []string{"default"} }, Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH")} }})
}

// TestDockerQueriesHaveADeadline: a docker that never answers (a wedged
// Docker Desktop) fails List and Inspect at the query deadline instead of
// holding the caller (issue #342). The control: the same stub answering
// at once lists.
func TestDockerQueriesHaveADeadline(t *testing.T) {
	answering := stubDocker(t, `case "$3" in info) echo 'daemon-1|28.0.0|linux';; ps) ;; esac`, time.Second)
	if _, err := answering.List(context.Background(), extension.ResourceOwner{}); err != nil {
		t.Fatalf("control: the answering stub fails: %v", err)
	}
	hung := stubDocker(t, `exec sleep 600`, 50*time.Millisecond)
	done := make(chan error, 2)
	go func() { _, err := hung.List(context.Background(), extension.ResourceOwner{}); done <- err }()
	go func() {
		_, err := hung.Inspect(context.Background(), extension.ContainerRef{ID: "default/abc"})
		done <- err
	}()
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a docker that never answered succeeded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a query to a docker that never answers is not bounded")
		}
	}
}
