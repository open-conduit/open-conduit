package integration_test

// Real isolation tests for the Docker container provider: containers
// created through the host's container binding and started by the
// `open-conduit container run` helper, as an executor's wrapper would,
// probed from inside. They run only with OPEN_CONDUIT_CONTAINER_TESTS=1
// (make container-test and the container CI job), and need a Docker Engine
// 28 or later on the context OPEN_CONDUIT_DOCKER_CONTEXT names (the CLI's
// current one when unset): without it they fail. They build the gateway
// image from this tree. They call no real service: the container's only
// way out is its gateway, and the hosts it allows are stand-in containers
// on the gateway's egress network. OPEN_CONDUIT_CONTAINER_FORWARD=tcp
// forces the toolbelt forward macOS uses.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/docker"
	gitws "github.com/open-conduit/open-conduit/extensions/git"
)

// dockerTestImage is the image the tests run, pinned by digest: Alpine
// 3.20, for its busybox shell, wget and awk.
const dockerTestImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

var (
	dockerOnce    sync.Once
	dockerHelper  string
	dockerGateway string
	dockerErr     error
)

// dockerContext is the context the tests run on, with the helper built
// and the test image pulled.
func dockerContext(t *testing.T) string {
	t.Helper()
	if os.Getenv("OPEN_CONDUIT_CONTAINER_TESTS") != "1" {
		t.Skip("make container-test and the container CI job run these tests")
	}
	dctx := os.Getenv("OPEN_CONDUIT_DOCKER_CONTEXT")
	if dctx == "" {
		out, _ := exec.Command("docker", "context", "show").Output()
		dctx = strings.TrimSpace(string(out))
	}
	if dctx == "" {
		t.Fatal("there is no docker CLI or context")
	}
	if out, err := exec.Command("docker", "--context", dctx, "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Fatalf("docker on context %s is unreachable: %s", dctx, bytes.TrimSpace(out))
	}
	dockerOnce.Do(func() {
		dir, err := os.MkdirTemp("", "oc-helper")
		if err != nil {
			dockerErr = err
			return
		}
		dockerHelper = filepath.Join(dir, "open-conduit")
		if out, err := exec.Command("go", "build", "-o", dockerHelper, "github.com/open-conduit/open-conduit/cmd/open-conduit").CombinedOutput(); err != nil {
			dockerErr = fmt.Errorf("building the helper: %v\n%s", err, out)
			return
		}
		if out, err := exec.Command("docker", "--context", dctx, "pull", "-q", dockerTestImage).CombinedOutput(); err != nil {
			dockerErr = fmt.Errorf("pulling %s: %v\n%s", dockerTestImage, err, out)
			return
		}
		dockerGateway, dockerErr = buildGateway(dctx, dir)
	})
	if dockerErr != nil {
		t.Fatal(dockerErr)
	}
	return dctx
}

// buildGateway builds the gateway image from this tree for the context's
// architecture, as make gateway-image does, and returns its ID.
func buildGateway(dctx, dir string) (string, error) {
	out, err := exec.Command("docker", "--context", dctx, "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker server architecture: %v", err)
	}
	arch := strings.TrimSpace(string(out))
	build := filepath.Join(dir, "gateway")
	for _, b := range []string{"open-conduit-gateway", "os"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(build, "linux-"+arch, b), "github.com/open-conduit/open-conduit/cmd/"+b)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("building %s for linux/%s: %v\n%s", b, arch, err, out)
		}
	}
	dockerfile, _ := filepath.Abs("../extensions/docker/gateway/Dockerfile")
	out, err = exec.Command("docker", "--context", dctx, "build", "-q", "--build-arg", "TARGETARCH="+arch, "-f", dockerfile, build).Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		return "", fmt.Errorf("building the gateway image: %v\n%s", err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerLab is a daemon's state directory, a home and a workspace laid
// out as the daemon lays them out, with something to find in each place a
// container must not reach.
type dockerLab struct {
	t                                      *testing.T
	dctx, host                             string
	root, home, state, config, ws, outside string
	run, other, records, runRoot           string
	port, gateway, network                 string
	provider                               *docker.Provider
	mu                                     sync.Mutex
	pins                                   []extension.ContainerPin
}

func newDockerLab(t *testing.T) *dockerLab {
	t.Helper()
	dctx := dockerContext(t)
	root, err := os.MkdirTemp("/tmp", "ocdk")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", root).Run(); _ = os.RemoveAll(root) })
	l := &dockerLab{t: t, dctx: dctx, host: "lab-" + filepath.Base(root), root: root, home: root + "/home", state: root + "/state",
		config: root + "/config.json", outside: root + "/outside"}
	l.ws = l.state + "/resources/git/workspaces/mine"
	l.run = l.state + "/runs/own"
	l.other = l.state + "/runs/other"
	l.records = l.run + "/container"
	l.runRoot = l.state + "/executors/shell/runs"
	for _, d := range []string{l.home + "/.ssh", l.state + "/private", l.run + "/token", l.other + "/token", l.state + "/resources/git/workspaces/theirs",
		l.outside, l.ws, l.runRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		l.home + "/.ssh/id_test":                            "ssh-secret",
		l.state + "/private/operator.token":                 "operator-secret",
		l.other + "/token/token":                            "other-secret",
		l.run + "/token/token":                              "own-token",
		l.state + "/resources/git/workspaces/theirs/secret": "their-secret",
		l.config: "config-secret",
	} {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "init", "-q", l.ws)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	_ = os.Symlink(l.home+"/.ssh", l.ws+"/link-ssh")
	// A host listener the container must not reach: every interface on
	// Linux (the bridge), loopback on macOS (what Docker's VM forwards).
	addr := "0.0.0.0:0"
	if runtime.GOOS == "darwin" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "host") }))
	}()
	t.Cleanup(func() { ln.Close() })
	_, l.port, _ = net.SplitHostPort(ln.Addr().String())
	l.provider = l.newProvider(dockerGateway)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if out, _ := exec.Command("docker", "--context", dctx, "ps", "-aq", "--filter", "label="+extension.ContainerLabelHost+"="+l.host).Output(); len(out) > 0 {
			_ = exec.Command("docker", append([]string{"--context", dctx, "rm", "-f"}, strings.Fields(string(out))...)...).Run()
		}
		_ = extension.ReconcileContainers(ctx, l.provider, l.host, func([]domain.ID) (map[domain.ID]bool, error) { return nil, nil })
		if out, _ := exec.Command("docker", "--context", dctx, "network", "ls", "-q", "--filter", "label="+extension.ContainerLabelHost+"="+l.host).Output(); len(out) > 0 {
			_ = exec.Command("docker", append([]string{"--context", dctx, "network", "rm"}, strings.Fields(string(out))...)...).Run()
		}
		if out, _ := exec.Command("docker", "--context", dctx, "volume", "ls", "-q", "--filter", "label="+extension.ContainerLabelHost+"="+l.host).Output(); len(out) > 0 {
			_ = exec.Command("docker", append([]string{"--context", dctx, "volume", "rm", "-f"}, strings.Fields(string(out))...)...).Run()
		}
	})
	return l
}

// newProvider is the lab's provider with a gateway image, forwarding the
// toolbelt as OPEN_CONDUIT_CONTAINER_FORWARD says (the platform's way when
// unset).
func (l *dockerLab) newProvider(gateway string) *docker.Provider {
	return docker.New(docker.Options{Helper: dockerHelper, Host: l.host, Home: l.home, Gateway: gateway, Forward: os.Getenv("OPEN_CONDUIT_CONTAINER_FORWARD"),
		Contexts: func() []string { return []string{l.dctx} }})
}

func (l *dockerLab) grant(extra string) string {
	return `{"image":"` + dockerTestImage + `","write":["{workspace}"]` + extra + `,"provider":{"context":"` + l.dctx + `"}}`
}

// binding is the host's container for one attempt, as bootstrap binds it.
func (l *dockerLab) binding(config string, exec domain.ID, pinned *extension.ContainerPin, p extension.ContainerProvider) *extension.BoundContainer {
	if p == nil {
		p = l.provider
	}
	return extension.BindContainer(extension.ContainerBinding{
		Provider:  p,
		Owner:     extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: exec},
		Config:    domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(config)},
		Host:      l.host,
		Segment:   1,
		Protected: []string{l.state, l.config},
		Shared:    []string{l.run},
		RunRoot:   l.runRoot,
		Records:   l.records,
		Home:      l.home,
		Pin: func(c extension.ContainerPin) error {
			l.mu.Lock()
			l.pins = append(l.pins, c)
			l.mu.Unlock()
			return nil
		},
		Pinned: pinned,
	})
}

func (l *dockerLab) pinned() extension.ContainerPin {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pins) == 0 {
		l.t.Fatal("no container was pinned")
	}
	return l.pins[len(l.pins)-1]
}

// launch is the executor's Launch: the workspace acquired through the
// binding, its clone needs, and a command in the workspace.
func (l *dockerLab) launch(b *extension.BoundContainer, segment, command string, needs extension.SandboxNeeds) (extension.SandboxPlan, string, error) {
	res, err := b.Resources(dockerWorkspace{l.ws}).Acquire(context.Background(), extension.ResourceRequest{Sandboxed: true})
	if err != nil {
		l.t.Fatal(err)
	}
	runDir := filepath.Join(l.runRoot, segment)
	if err := os.MkdirAll(runDir+"/inbox", 0o700); err != nil {
		l.t.Fatal(err)
	}
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LAB_STATE=" + l.state, "LAB_CONFIG=" + l.config, "LAB_OUTSIDE=" + l.outside,
		"LAB_HOME=" + l.home, "LAB_TOKEN=" + l.run + "/token/token", "LAB_OTHER_TOKEN=" + l.other + "/token/token", "LAB_RECORDS=" + l.records,
		"LAB_RUNDIR=" + runDir, "LAB_PORT=" + l.port, "LAB_UID=" + strconv.Itoa(os.Getuid()), "OS_INBOX=" + runDir + "/inbox"}
	plan, err := b.Launch(context.Background(), needs, extension.SandboxLaunch{Argv: []string{"/bin/sh", "-c", command}, Dir: res.Dir, Env: env, RunDir: runDir})
	return plan, runDir, err
}

type dockerWorkspace struct{ dir string }

func (w dockerWorkspace) Acquire(_ context.Context, r extension.ResourceRequest) (extension.Resource, error) {
	if !r.Sandboxed {
		return extension.Resource{}, errors.New("acquired without the sandbox flag")
	}
	return extension.Resource{Kind: "workspace", Ref: "lease", Dir: w.dir, Sandbox: gitws.CloneSandboxNeeds(w.dir)}, nil
}
func (w dockerWorkspace) Lookup(ctx context.Context, _ string) (extension.Resource, error) {
	return w.Acquire(ctx, extension.ResourceRequest{Sandboxed: true})
}

// wrapper runs a plan's argv as an executor's wrapper does: in a process
// group of its own, with the plan's environment.
type wrapper struct {
	cmd  *exec.Cmd
	out  lockedBuffer
	done chan struct{}
	code int
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func startWrapper(t *testing.T, plan extension.SandboxPlan, stdin io.Reader) *wrapper {
	t.Helper()
	w := &wrapper{done: make(chan struct{})}
	w.cmd = exec.Command(plan.Argv[0], plan.Argv[1:]...)
	w.cmd.Env, w.cmd.Stdin, w.cmd.Stdout, w.cmd.Stderr = plan.Env, stdin, &w.out, &w.out
	w.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The attached docker CLI, in a group of its own, can outlive a
	// killed helper and hold the output open.
	w.cmd.WaitDelay = 2 * time.Second
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = w.cmd.Wait()
		w.code = w.cmd.ProcessState.ExitCode()
		close(w.done)
	}()
	t.Cleanup(func() { _ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL) })
	return w
}

func (w *wrapper) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-w.done:
		return w.code
	case <-time.After(2 * time.Minute):
		t.Fatalf("the wrapper never exited:\n%s", w.out.String())
	}
	return 0
}

func (l *dockerLab) docker(args ...string) (string, error) {
	out, err := exec.Command("docker", append([]string{"--context", l.dctx}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func dockerID(ref string) string { _, id, _ := strings.Cut(ref, "/"); return id }

// networkOf records a created container's network and that network's
// bridge address.
func (l *dockerLab) networkOf(t *testing.T, ref string) {
	t.Helper()
	network, err := l.docker("inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}", dockerID(ref))
	if err != nil {
		t.Fatalf("container network: %v %s", err, network)
	}
	l.network = network
	l.gateway, _ = l.docker("network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}", network)
}

// egress makes an execution's egress network before its launch, as the
// provider would but on a subnet that isn't private (198.18.0.0/15, set
// aside for testing), so stand-in hosts on it pass the gateway's check of
// where an allowed name resolves. The provider uses it as it finds it.
func (l *dockerLab) egress(t *testing.T, id domain.ID, subnet int) string {
	t.Helper()
	name := docker.ExecutionNetwork(l.host, string(id)) + "-egress"
	if out, err := l.docker("network", "create", "--driver", "bridge", "--ipv6=false", "--subnet", fmt.Sprintf("198.18.%d.0/24", subnet),
		"--label", extension.ContainerLabelHost+"="+l.host, "--label", extension.ContainerLabelExecution+"="+string(id), name); err != nil {
		t.Fatalf("egress network: %v %s", err, out)
	}
	return name
}

// upstream starts a stand-in for outside hosts on an egress network, the
// gateway's only one out, answering as each of names, and returns its
// address there.
func (l *dockerLab) upstream(t *testing.T, egress string, names ...string) string {
	t.Helper()
	args := []string{"run", "-d", "--rm", "--label", extension.ContainerLabelHost + "=" + l.host, "--network", egress}
	for _, n := range names {
		args = append(args, "--network-alias", n)
	}
	args = append(args, "--read-only", "--cap-drop", "ALL", "--user", "65534", "--entrypoint", "/bin/sh", dockerTestImage,
		"-c", `while :; do printf 'HTTP/1.0 200 OK\r\nContent-Length: 8\r\n\r\nupstream' | nc -l -p 8080; done`)
	id, err := l.docker(args...)
	if err != nil {
		t.Fatalf("stand-in upstream: %v %s", err, id)
	}
	ip, _ := l.docker("inspect", "--format", `{{(index .NetworkSettings.Networks "`+egress+`").IPAddress}}`, id)
	return ip
}

// containerProbes print "name: ALLOWED|DENIED" for each attempt from
// inside the container.
const containerProbes = `t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n: ALLOWED"; else echo "$n: DENIED"; fi; }
rd() { [ "$(cat "$1" 2>/dev/null)" = "$2" ]; }
up() { i=0; while [ $i -lt 10 ]; do wget -q -T 2 -O /dev/null "$1" && return 0; i=$((i+1)); sleep 0.5; done; return 1; }
t read_own_token rd "$LAB_TOKEN" own-token
t write_own_token sh -c 'echo x > "$LAB_TOKEN"'
t read_operator_token rd "$LAB_STATE/private/operator.token" operator-secret
t read_other_workspace rd "$LAB_STATE/resources/git/workspaces/theirs/secret" their-secret
t read_other_token rd "$LAB_OTHER_TOKEN" other-secret
t read_config rd "$LAB_CONFIG" config-secret
t read_ssh rd "$LAB_HOME/.ssh/id_test" ssh-secret
t read_symlink_ssh rd ./link-ssh/id_test ssh-secret
t read_dotdot rd ../theirs/secret their-secret
t write_workspace sh -c 'echo x > ./probe'
t write_run sh -c 'echo x > "$LAB_RUNDIR/probe"'
t write_tmp sh -c 'echo x > /tmp/probe'
t write_root sh -c 'echo x > /probe'
t write_outside sh -c 'echo x > "$LAB_OUTSIDE/probe"'
t write_records sh -c 'echo x > "$LAB_RECORDS/probe"'
t write_git_config sh -c 'echo "[core]" >> .git/config'
t write_git_hook sh -c 'echo x > .git/hooks/post-commit'
t write_git_info sh -c 'echo x >> .git/info/exclude'
t move_git_config mv .git/config .git/config.moved
t move_git_hooks mv .git/hooks .git/hooks.moved
t docker_socket sh -c 'test -S /var/run/docker.sock || test -S /run/docker.sock'
t default_route sh -c 'awk "\$2 == \"00000000\"" /proc/net/route | grep -q .'
t host_internal wget -q -T 3 -O /dev/null "http://host.docker.internal:$LAB_PORT/"
t host_bridge wget -q -T 3 -O /dev/null "http://$LAB_GATEWAY:$LAB_PORT/"
t host_docker0 wget -q -T 3 -O /dev/null "http://172.17.0.1:$LAB_PORT/"
t gateway_proxy sh -c 'i=0; while [ $i -lt 20 ]; do nc -z -w 2 gateway 3128 && exit 0; i=$((i+1)); sleep 0.25; done; exit 1'
t toolbelt_bin sh -c '[ "$(command -v os)" = /run/open-conduit/bin/os ]'
t write_bin sh -c 'echo x > /run/open-conduit/bin/probe'
t write_toolbelt_dir sh -c 'echo x > /run/open-conduit/toolbelt/probe' 
t no_capabilities grep -Eq 'CapEff:[[:space:]]+0+$' /proc/self/status
t operator_uid sh -c '[ "$(id -u)" = "$LAB_UID" ]'
`

var containerExpect = map[string]string{
	"read_own_token": "ALLOWED", "write_own_token": "DENIED", "read_operator_token": "DENIED", "read_other_workspace": "DENIED",
	"read_other_token": "DENIED", "read_config": "DENIED", "read_ssh": "DENIED", "read_symlink_ssh": "DENIED", "read_dotdot": "DENIED",
	"write_workspace": "ALLOWED", "write_run": "ALLOWED", "write_tmp": "ALLOWED", "write_root": "DENIED", "write_outside": "DENIED",
	"write_records": "DENIED", "write_git_config": "DENIED", "write_git_hook": "DENIED", "write_git_info": "DENIED",
	"move_git_config": "DENIED", "move_git_hooks": "DENIED", "docker_socket": "DENIED", "default_route": "DENIED",
	"host_internal": "DENIED", "host_bridge": "DENIED", "host_docker0": "DENIED", "gateway_proxy": "ALLOWED",
	"toolbelt_bin": "ALLOWED", "write_bin": "DENIED", "write_toolbelt_dir": "DENIED",
	"no_capabilities": "ALLOWED", "operator_uid": "ALLOWED",
}

// TestDockerIsolation: what a container can reach. Only its workspace,
// run directory and /tmp are writable, the clone's .git/config, hooks and
// info are read-only over the workspace, it reads its own token and
// nothing else of the host, runs as the operator with no capabilities,
// and reaches nothing but the gateway: no route out and no host port. The
// gateway image's os is first on PATH, and neither it nor the forward's
// directory is writable.
func TestDockerIsolation(t *testing.T) {
	l := newDockerLab(t)
	config, _ := os.ReadFile(l.ws + "/.git/config")
	b := l.binding(l.grant(""), "iso", nil, nil)
	plan, _, err := l.launch(b, "iso", `sleep 1; LAB_GATEWAY=$(cat "$LAB_RUNDIR/gateway"); export LAB_GATEWAY; `+containerProbes, extension.SandboxNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	pin := l.pinned()
	l.networkOf(t, pin.ID)
	_ = os.WriteFile(filepath.Join(l.runRoot, "iso", "gateway"), []byte(l.gateway), 0o600)
	w := startWrapper(t, plan, nil)
	if code := w.wait(t); code != 0 {
		t.Fatalf("the probes exited %d:\n%s", code, w.out.String())
	}
	out := w.out.String()
	got := probeResults(out)
	for k, want := range containerExpect {
		if got[k] != want {
			t.Errorf("%s: %q from inside the container, want %s", k, got[k], want)
		}
	}
	if t.Failed() {
		t.Logf("probes:\n%s", out)
	}
	for _, p := range []string{l.outside + "/probe", l.records + "/probe", l.ws + "/.git/hooks/post-commit", l.ws + "/.git/config.moved", l.ws + "/.git/hooks.moved"} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written from inside the container", p)
		}
	}
	if after, _ := os.ReadFile(l.ws + "/.git/config"); !bytes.Equal(after, config) {
		t.Errorf(".git/config was written from inside the container:\n%s", after)
	}
	fi, err := os.Stat(l.ws + "/probe")
	if err != nil {
		t.Fatal("the workspace write did not reach the host")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		t.Errorf("the workspace write is owned by uid %d on the host, want %d", st.Uid, os.Getuid())
	}
	if !strings.Contains(plan.Summary, "docker "+l.dctx) || plan.Signals != extension.SandboxTerm || pin.Daemon == "" || pin.Image != dockerTestImage {
		t.Errorf("plan %+v pin %+v", plan, pin)
	}
	// The created container carries the hardening and the labels.
	inspect, _ := l.docker("inspect", "--format", "{{json .HostConfig}}|{{json .Config.Labels}}", dockerID(pin.ID))
	for _, want := range []string{`"CapDrop":["ALL"]`, `"ReadonlyRootfs":true`, `no-new-privileges`, `"Init":true`, `"Name":"no"`,
		extension.ContainerLabelExecution + `":"iso"`, extension.ContainerLabelSegment + `":"1"`, extension.ContainerLabelDigest + `":"` + pin.Digest} {
		if !strings.Contains(inspect, want) {
			t.Errorf("the container lacks %s: %s", want, inspect)
		}
	}
	if net, _ := l.docker("network", "inspect", "--format", "{{.Internal}} {{.EnableIPv6}} {{index .Options \"com.docker.network.bridge.inhibit_ipv4\"}}", l.network); net != "true false true" {
		t.Errorf("the execution's network %s is %q, want internal, no IPv6, no IPv4 on the bridge", l.network, net)
	}
	// The gateway is hardened too, and on the execution's network and its
	// own egress one only.
	name, _ := l.docker("inspect", "--format", "{{.Name}}", dockerID(pin.ID))
	gw := strings.TrimPrefix(name, "/") + "-gw"
	ginspect, _ := l.docker("inspect", "--format", "{{json .HostConfig}}|{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}|{{.Config.User}}", gw)
	for _, want := range []string{`"CapDrop":["ALL"]`, `"ReadonlyRootfs":true`, `no-new-privileges`, l.network + " ", l.network + "-egress ", "|" + strconv.Itoa(os.Getuid()) + ":"} {
		if !strings.Contains(ginspect, want) {
			t.Errorf("the gateway lacks %s: %s", want, ginspect)
		}
	}
}

// TestDockerFailClosed: each launch that can't be enforced is refused and
// creates nothing.
func TestDockerFailClosed(t *testing.T) {
	l := newDockerLab(t)
	none := func(t *testing.T, exec domain.ID) {
		t.Helper()
		if out, _ := l.docker("ps", "-aq", "--filter", "label="+extension.ContainerLabelExecution+"="+string(exec)); out != "" {
			t.Errorf("a refused launch created %s", out)
		}
	}
	for _, c := range []struct {
		name, grant string
		needs       extension.SandboxNeeds
		want        error
	}{
		{"write_outside_the_grant", l.grant(""), extension.SandboxNeeds{Write: []string{l.outside}}, extension.ErrSandboxDenied},
		{"read_outside_the_grant", l.grant(""), extension.SandboxNeeds{Read: []string{l.home + "/.ssh"}}, extension.ErrSandboxDenied},
		{"grant_reaching_the_state", l.grant(`,"read":["` + l.state + `/private"]`), extension.SandboxNeeds{}, extension.ErrSandboxDenied},
		{"a_host_outside_network_allow", l.grant(`,"network":{"allow":["allowed.test"]}`), extension.SandboxNeeds{Hosts: []string{"other.test"}}, extension.ErrSandboxDenied},
		{"a_socket", l.grant(`,"read":["` + l.outside + `"]`), extension.SandboxNeeds{Sockets: []string{l.outside + "/s.sock"}}, extension.ErrSandboxDenied},
		{"unreachable_context", strings.Replace(l.grant(""), `"context":"`+l.dctx+`"`, `"context":"open-conduit-no-such-context"`, 1), extension.SandboxNeeds{}, extension.ErrSandboxUnavailable},
		{"unpinned_image", strings.Replace(l.grant(""), dockerTestImage, "alpine:3.20", 1), extension.SandboxNeeds{}, extension.ErrSandboxUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := domain.ID("fc-" + strings.ReplaceAll(c.name, "_", "-"))
			_, _, err := l.launch(l.binding(c.grant, id, nil, nil), string(id), "true", c.needs)
			if !errors.Is(err, c.want) {
				t.Fatalf("%v, want %v", err, c.want)
			}
			none(t, id)
		})
	}
	t.Run("no_gateway_image", func(t *testing.T) {
		_, _, err := l.launch(l.binding(l.grant(""), "fc-nogw", nil, l.newProvider("")), "fc-nogw", "true", extension.SandboxNeeds{})
		if !errors.Is(err, extension.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "gateway") {
			t.Fatalf("%v, want sandbox_unavailable for the gateway", err)
		}
		none(t, "fc-nogw")
	})
	t.Run("symlinked_git_config", func(t *testing.T) {
		cfg := l.ws + "/.git/config"
		real := l.ws + "/.git/config.real"
		_ = os.Rename(cfg, real)
		_ = os.Symlink(real, cfg)
		defer func() { _ = os.Remove(cfg); _ = os.Rename(real, cfg) }()
		_, _, err := l.launch(l.binding(l.grant(""), "fc-link", nil, nil), "fc-link", "true", extension.SandboxNeeds{})
		if !errors.Is(err, extension.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("%v, want sandbox_unavailable for the symlink", err)
		}
		none(t, "fc-link")
	})
	t.Run("missing_git_info", func(t *testing.T) {
		// The clone's needs make hooks and info; one removed after is
		// refused rather than left writable.
		b := l.binding(l.grant(""), "fc-missing", nil, nil)
		_, _, err := l.launch(b, "fc-missing", "true", extension.SandboxNeeds{ReadOnly: []string{l.ws + "/.git/absent"}})
		if !errors.Is(err, extension.ErrSandboxUnavailable) {
			t.Fatalf("%v, want sandbox_unavailable", err)
		}
		none(t, "fc-missing")
	})
	t.Run("a_second_container", func(t *testing.T) {
		plan, _, err := l.launch(l.binding(l.grant(""), "fc-two", nil, nil), "fc-two", "sleep 30", extension.SandboxNeeds{})
		if err != nil {
			t.Fatal(err)
		}
		w := startWrapper(t, plan, nil)
		waitRunning(t, l, l.pinned().ID)
		if _, _, err := l.launch(l.binding(l.grant(""), "fc-two", nil, nil), "fc-two-b", "true", extension.SandboxNeeds{}); !errors.Is(err, extension.ErrContainerRunning) {
			t.Fatalf("%v, want container_running", err)
		}
		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGTERM)
		w.wait(t)
	})
}

func waitRunning(t *testing.T, l *dockerLab, ref string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if out, _ := l.docker("inspect", "--format", "{{.State.Running}}", dockerID(ref)); out == "true" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the container never ran")
}

func waitPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// TestDockerProcessControl: the helper's exit code is the container's;
// Inject reaches the container through its run directory; an Interrupt
// (TERM to the wrapper's group) reaches the command's own trap and is the
// host's, while a TERM the host never sent makes the result interrupted;
// and the host's Stop is confirmed.
func TestDockerProcessControl(t *testing.T) {
	loop := `trap 'echo term > ./got-term; exit 3' TERM
while :; do
  for f in "$OS_INBOX"/*.json; do [ -e "$f" ] && cp "$f" ./inbox-seen; done
  sleep 0.1 & wait $!
done`
	completed := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}
	t.Run("exit_code_and_stdin", func(t *testing.T) {
		l := newDockerLab(t)
		plan, _, err := l.launch(l.binding(l.grant(""), "pc-exit", nil, nil), "pc-exit", `read line; echo "got $line"; exit 7`, extension.SandboxNeeds{})
		if err != nil {
			t.Fatal(err)
		}
		w := startWrapper(t, plan, strings.NewReader("hello\n"))
		if code := w.wait(t); code != 7 || !strings.Contains(w.out.String(), "got hello") {
			t.Fatalf("exit %d, output %q; want 7 and the line read from stdin", code, w.out.String())
		}
	})
	t.Run("inject_and_interrupt", func(t *testing.T) {
		l := newDockerLab(t)
		b := l.binding(l.grant(""), "pc-int", nil, nil)
		plan, runDir, err := l.launch(b, "pc-int", loop, extension.SandboxNeeds{})
		if err != nil {
			t.Fatal(err)
		}
		w := startWrapper(t, plan, nil)
		waitRunning(t, l, l.pinned().ID)
		if err := os.WriteFile(runDir+"/inbox/inj-1.json", []byte(`{"hello":"container"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		waitPath(t, l.ws+"/inbox-seen")
		if err := syscall.Kill(-w.cmd.Process.Pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if code := w.wait(t); code != 3 {
			t.Errorf("exit %d, want the command's own 3:\n%s", code, w.out.String())
		}
		if _, err := os.Stat(l.ws + "/got-term"); err != nil {
			t.Error("the command's TERM trap did not run")
		}
		if _, err := os.Stat(runDir + "/" + extension.ContainerTermMarker); err != nil {
			t.Error("the entry stub wrote no TERM marker")
		}
		if r := b.Settle(completed); r.Outcome.Kind != domain.OutcomeCompleted {
			t.Errorf("an Interrupt read as one the host didn't send: %+v", r.Outcome)
		}
	})
	t.Run("unrequested_term", func(t *testing.T) {
		l := newDockerLab(t)
		b := l.binding(l.grant(""), "pc-unreq", nil, nil)
		plan, _, err := l.launch(b, "pc-unreq", loop, extension.SandboxNeeds{})
		if err != nil {
			t.Fatal(err)
		}
		w := startWrapper(t, plan, nil)
		waitRunning(t, l, l.pinned().ID)
		// A TERM from outside Open Conduit, as a Docker restart sends.
		if out, err := l.docker("kill", "--signal", "TERM", dockerID(l.pinned().ID)); err != nil {
			t.Fatal(err, out)
		}
		w.wait(t)
		if r := b.Settle(completed); r.Outcome.Kind != domain.OutcomeFailed || !strings.Contains(r.Outcome.Summary, "interrupted") {
			t.Errorf("a TERM the host didn't send: %+v, want interrupted", r.Outcome)
		}
	})
	t.Run("stop", func(t *testing.T) {
		l := newDockerLab(t)
		b := l.binding(l.grant(""), "pc-stop", nil, nil)
		plan, _, err := l.launch(b, "pc-stop", "while :; do sleep 1; done", extension.SandboxNeeds{})
		if err != nil {
			t.Fatal(err)
		}
		w := startWrapper(t, plan, nil)
		pin := l.pinned()
		waitRunning(t, l, pin.ID)
		if err := extension.StopContainer(context.Background(), l.provider, &pin, l.records, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		w.wait(t)
		if r := b.Settle(completed); r.Outcome.Kind != domain.OutcomeCompleted {
			t.Errorf("the host's own stop read as unrequested: %+v", r.Outcome)
		}
	})
}

// TestDockerRecover: each Recover path against real Docker, with the
// host's Stop after it.
func TestDockerRecover(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T, l *dockerLab, id domain.ID, command string) (*wrapper, extension.ContainerPin) {
		t.Helper()
		plan, _, err := l.launch(l.binding(l.grant(""), id, nil, nil), string(id), command, extension.SandboxNeeds{})
		if err != nil {
			t.Fatal(err)
		}
		w := startWrapper(t, plan, nil)
		pin := l.pinned()
		waitRunning(t, l, pin.ID)
		return w, pin
	}
	t.Run("wrapper_alive", func(t *testing.T) {
		l := newDockerLab(t)
		w, pin := start(t, l, "rc-alive", "sleep 60")
		if _, err := l.binding(l.grant(""), "rc-alive", &pin, nil).Adopt(ctx, true); err != nil {
			t.Fatalf("the pinned container was not adopted: %v", err)
		}
		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGTERM)
		w.wait(t)
	})
	t.Run("wrapper_dead_container_exited", func(t *testing.T) {
		l := newDockerLab(t)
		w, pin := start(t, l, "rc-exited", "sleep 1; exit 5")
		w.wait(t)
		exit, err := l.binding(l.grant(""), "rc-exited", &pin, nil).Adopt(ctx, false)
		if err != nil || !exit.Exited || exit.ExitCode != 5 || exit.Interrupted {
			t.Fatalf("adopt %+v %v, want the exit code 5", exit, err)
		}
		if err := extension.StopContainer(ctx, l.provider, &pin, l.records, time.Second); err != nil {
			t.Fatalf("stop of an exited container: %v", err)
		}
	})
	t.Run("wrapper_dead_container_running", func(t *testing.T) {
		l := newDockerLab(t)
		w, pin := start(t, l, "rc-running", "while :; do sleep 1; done")
		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
		w.wait(t)
		if _, err := l.binding(l.grant(""), "rc-running", &pin, nil).Adopt(ctx, false); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("adopt %v, want ErrNotAdoptable", err)
		}
		if err := extension.StopContainer(ctx, l.provider, &pin, l.records, time.Second); err != nil {
			t.Fatalf("the host's stop: %v", err)
		}
		if out, _ := l.docker("inspect", "--format", "{{.State.Running}}", dockerID(pin.ID)); out != "false" {
			t.Errorf("the container still runs after the host's stop: %s", out)
		}
	})
	t.Run("container_gone", func(t *testing.T) {
		l := newDockerLab(t)
		w, pin := start(t, l, "rc-gone", "sleep 60")
		if out, err := l.docker("rm", "-f", dockerID(pin.ID)); err != nil {
			t.Fatal(err, out)
		}
		w.wait(t)
		if _, err := l.binding(l.grant(""), "rc-gone", &pin, nil).Adopt(ctx, false); !errors.Is(err, extension.ErrNotAdoptable) || errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("adopt %v, want ErrNotAdoptable, confirmed", err)
		}
		if err := extension.StopContainer(ctx, l.provider, &pin, l.records, time.Second); err != nil {
			t.Fatalf("a container gone from the pinned daemon is confirmed not running: %v", err)
		}
	})
	t.Run("another_daemon", func(t *testing.T) {
		l := newDockerLab(t)
		w, pin := start(t, l, "rc-other", "sleep 60")
		defer func() { _ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGTERM); w.wait(t) }()
		other := pin
		other.Daemon = "another-daemon"
		if _, err := l.binding(l.grant(""), "rc-other", &other, nil).Adopt(ctx, true); !errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("adopt %v, want unconfirmed", err)
		}
		if err := extension.StopContainer(ctx, l.provider, &other, l.records, time.Second); !errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("stop %v, want unconfirmed", err)
		}
	})
	t.Run("daemon_unreachable", func(t *testing.T) {
		l := newDockerLab(t)
		w, pin := start(t, l, "rc-unreach", "sleep 60")
		defer func() { _ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGTERM); w.wait(t) }()
		lost := pin
		lost.ID = "open-conduit-no-such-context/" + dockerID(pin.ID)
		if _, err := l.binding(l.grant(""), "rc-unreach", &lost, nil).Adopt(ctx, true); !errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("adopt %v, want unconfirmed", err)
		}
		if err := extension.StopContainer(ctx, l.provider, &lost, l.records, time.Second); !errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("stop %v, want unconfirmed", err)
		}
	})
}

// TestDockerRemoveAndReconcile: removal takes the container and its
// network; reconcile removes those of executions that are over; mounts
// are listed for a live one.
func TestDockerRemoveAndReconcile(t *testing.T) {
	ctx := context.Background()
	l := newDockerLab(t)
	plan, _, err := l.launch(l.binding(l.grant(""), "rm-live", nil, nil), "rm-live", "sleep 60", extension.SandboxNeeds{})
	if err != nil {
		t.Fatal(err)
	}
	w := startWrapper(t, plan, nil)
	live := l.pinned()
	waitRunning(t, l, live.ID)
	if _, _, err := l.launch(l.binding(l.grant(""), "rm-done", nil, nil), "rm-done", "true", extension.SandboxNeeds{}); err != nil {
		t.Fatal(err)
	}
	done := l.pinned()
	mounts, err := extension.ContainerMounts(ctx, l.provider, extension.ResourceOwner{ExecutionID: "rm-live"})
	if err != nil || !containsPath(mounts, l.ws) {
		t.Errorf("mounts %v %v, want the workspace", mounts, err)
	}
	err = extension.ReconcileContainers(ctx, l.provider, l.host, func(ids []domain.ID) (map[domain.ID]bool, error) {
		return map[domain.ID]bool{"rm-live": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := l.docker("ps", "-aq", "--filter", "id="+dockerID(done.ID)); out != "" {
		t.Error("reconcile left the finished execution's container")
	}
	if out, _ := l.docker("network", "ls", "-q", "--filter", "label="+extension.ContainerLabelExecution+"=rm-done"); out != "" {
		t.Error("reconcile left the finished execution's network")
	}
	if out, _ := l.docker("ps", "-aq", "--filter", "label="+extension.ContainerLabelExecution+"=rm-done"); out != "" {
		t.Error("reconcile left the finished execution's gateway")
	}
	if out, _ := l.docker("volume", "ls", "-q", "--filter", "label="+extension.ContainerLabelExecution+"=rm-done"); out != "" {
		t.Error("reconcile left the finished execution's toolbelt volume")
	}
	// A live execution's gateway runs, and is not one of its containers.
	if out, _ := l.docker("ps", "-q", "--filter", "label="+extension.ContainerLabelExecution+"=rm-live", "--filter", "label=dev.open-conduit.role=gateway"); out == "" {
		t.Error("reconcile removed a live execution's gateway")
	}
	if listed, err := l.provider.List(ctx, extension.ResourceOwner{ExecutionID: "rm-live"}); err != nil || len(listed) != 1 || listed[0].Ref.ID != live.ID {
		t.Errorf("the live execution lists %+v %v, want its container only", listed, err)
	}
	if out, _ := l.docker("ps", "-q", "--filter", "id="+dockerID(live.ID)); out == "" {
		t.Error("reconcile removed a live execution's container")
	}
	if err := extension.RemoveContainers(ctx, l.provider, extension.ResourceOwner{ExecutionID: "rm-live"}); !errors.Is(err, extension.ErrContainerRunning) {
		t.Errorf("removing a running container: %v, want container_running", err)
	}
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGTERM)
	w.wait(t)
	if err := extension.RemoveContainers(ctx, l.provider, extension.ResourceOwner{ExecutionID: "rm-live"}); err != nil {
		t.Fatal(err)
	}
	if out, _ := l.docker("network", "ls", "-q", "--filter", "label="+extension.ContainerLabelExecution+"=rm-live"); out != "" {
		t.Error("removal left the execution's networks")
	}
	if out, _ := l.docker("ps", "-aq", "--filter", "label="+extension.ContainerLabelExecution+"=rm-live"); out != "" {
		t.Error("removal left the execution's gateway")
	}
}

func containsPath(list []string, p string) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}

// TestDockerHealth: doctor's checks, the gateway image's and the
// internal-network preflight among them, pass on the context and fail on
// one that reaches no daemon, without a gateway image and with one the
// daemon doesn't have. The preflight leaves nothing behind.
func TestDockerHealth(t *testing.T) {
	l := newDockerLab(t)
	if h := l.provider.Health(context.Background(), l.dctx, dockerTestImage, l.ws, false); h.Status != "ok" || h.Daemon == "" {
		t.Fatalf("health on %s: %+v", l.dctx, h)
	}
	if out, _ := l.docker("network", "ls", "-q", "--filter", "name=oc-preflight-", "--filter", "label="+extension.ContainerLabelHost+"="+l.host); out != "" {
		t.Errorf("the preflight left networks: %s", out)
	}
	for _, c := range []struct{ dctx, gateway, code string }{
		{"open-conduit-no-such-context", dockerGateway, "docker_unreachable"},
		{l.dctx, "", "gateway_unset"},
		{l.dctx, "sha256:" + strings.Repeat("0", 64), "gateway_missing"},
	} {
		if h := l.newProvider(c.gateway).Health(context.Background(), c.dctx, dockerTestImage, l.ws, false); h.Status != "fail" || h.Code != c.code {
			t.Errorf("health with %s on %s: %+v, want %s", c.gateway, c.dctx, h, c.code)
		}
	}
}
