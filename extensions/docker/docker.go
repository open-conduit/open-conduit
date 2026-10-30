// Package docker is the container provider for Docker (Engine 28 or later,
// on Linux or through OrbStack or Docker Desktop on macOS). It creates each
// launch's container on a per-execution internal network, hardened and
// with the granted host paths mounted at the same paths, beside a gateway
// container that is its only way out: an allowlisting proxy for the
// grant's network.allow and a forward to the execution's toolbelt. It
// returns the argv of `open-conduit container run`, the host helper that
// starts the container, attached, relays signals to it and exits with its
// code.
package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

const (
	// Type is the provider's name in a binding's container.
	Type = "docker"
	// PlanSchema identifies the plan's Data, the effective policy.
	PlanSchema = "open-conduit.container.docker.plan.v1"
	// MinServerMajor is the oldest Docker Engine the provider runs on.
	MinServerMajor = 28
	// DefaultPIDs is the container's process limit when the grant sets none.
	DefaultPIDs = 4096
	// labelNetwork marks the networks the provider makes.
	labelNetwork = "dev.open-conduit.network"
	// healthTTL is how long a passing health check lets launches go ahead
	// without running it again.
	healthTTL = 10 * time.Minute
	// labelRole marks a launch's gateway container ("gateway"), which an
	// execution's listing leaves out: it is not the launch's container.
	labelRole = "dev.open-conduit.role"
	// GatewayAlias is the gateway's name on the execution's network, and
	// GatewayPort its proxy's port.
	GatewayAlias = "gateway"
	GatewayPort  = "3128"
	// ToolbeltDir is where the gateway's forward socket is, in a volume
	// both containers mount, and ToolbeltSocket the socket: a container's
	// OS_ENDPOINT. BinDir is the gateway image's toolbelt directory (the
	// Linux os), mounted read-only into every container and first on PATH.
	ToolbeltDir    = "/run/open-conduit/toolbelt"
	ToolbeltSocket = ToolbeltDir + "/toolbelt.sock"
	BinDir         = "/run/open-conduit/bin"
	// PortFile, beside the execution's toolbelt socket, holds the loopback
	// port the host serves that toolbelt on for a tcp forward.
	PortFile = "toolbelt.port"
	// ForwardUnix and ForwardTCP are how a gateway reaches the toolbelt:
	// the host's socket, mounted (Linux), or a loopback port reached as
	// host.docker.internal (macOS, where a socket can't cross the VM).
	ForwardUnix = "unix"
	ForwardTCP  = "tcp"
)

// GatewayImage is the gateway image this build runs, pinned by digest. A
// release sets it (-ldflags -X); a build without one runs container
// bindings only with OPEN_CONDUIT_GATEWAY_IMAGE set (make gateway-image).
var GatewayImage = ""

// gatewayRef is an image pinned by digest, or a local image's ID.
var gatewayRef = regexp.MustCompile(`^([^@\s]+@)?sha256:[0-9a-f]{64}$`)

// Options are host-trusted, never from binding config.
type Options struct {
	// Docker is the docker CLI, resolved on PATH when not absolute
	// (default "docker").
	Docker string
	// Helper is the open-conduit binary whose hidden `container run`
	// starts a container for an executor's wrapper.
	Helper string
	// Host is this daemon's ContainerLabelHost value.
	Host string
	// UID and GID the container runs as (the operator's when zero and
	// UserSet is false).
	UID, GID int
	UserSet  bool
	// Contexts are the Docker contexts the config's bindings use, for a
	// listing of every container of the host.
	Contexts func() []string
	// Environ is the docker CLI's environment (os.Environ when nil).
	Environ func() []string
	// Home expands ~ in the grant (the user's home when empty).
	Home string
	// Gateway is the gateway image, pinned by digest (GatewayImage when
	// empty). Without one, every launch is refused.
	Gateway string
	// Forward is ForwardUnix or ForwardTCP (tcp on macOS, unix elsewhere,
	// when empty).
	Forward string
	Now     func() time.Time
}

// Provider implements extension.ContainerProvider.
type Provider struct {
	o      Options
	mu     sync.Mutex
	health map[string]time.Time
}

// New returns the provider.
func New(o Options) *Provider {
	if o.Docker == "" {
		o.Docker = "docker"
	}
	if !o.UserSet {
		o.UID, o.GID = os.Getuid(), os.Getgid()
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Contexts == nil {
		o.Contexts = func() []string { return nil }
	}
	if o.Gateway == "" {
		o.Gateway = GatewayImage
	}
	if o.Forward == "" {
		o.Forward = ForwardUnix
		if runtime.GOOS == "darwin" {
			o.Forward = ForwardTCP
		}
	}
	return &Provider{o: o, health: map[string]time.Time{}}
}

// Forward is how gateways reach the toolbelt: ForwardUnix or ForwardTCP.
// With tcp the host serves each container execution's toolbelt on a
// loopback port (ForwardListen) and writes it in PortFile.
func (p *Provider) Forward() string { return p.o.Forward }

// ForwardListen is the address the host serves a tcp toolbelt forward on
// for a context: loopback on macOS, which Docker's VM reaches as
// host.docker.internal, and elsewhere the default bridge's address, which
// a gateway reaches through host-gateway.
func (p *Provider) ForwardListen(ctx context.Context, dctx string) (string, error) {
	if runtime.GOOS == "darwin" {
		return "127.0.0.1", nil
	}
	out, err := p.docker(ctx, dctx, "network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}", "bridge")
	if err != nil {
		return "", err
	}
	for _, ip := range strings.Fields(string(out)) {
		if !strings.Contains(ip, ":") {
			return ip, nil
		}
	}
	return "", fmt.Errorf("the default bridge on context %s has no IPv4 gateway", dctx)
}

// Mount is one bind mount: a host path at the same path in the container,
// or Source (an empty file or directory the provider keeps) over Target
// to hide it.
type Mount struct {
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// Plan is the plan's Data: everything the container is created with,
// except the environment's values, which are in EnvFile until Create
// reads them.
type Plan struct {
	Context string                    `json:"context"`
	User    string                    `json:"user"`
	Workdir string                    `json:"workdir"`
	Mounts  []Mount                   `json:"mounts"`
	Argv    []string                  `json:"argv"`
	EnvKeys []string                  `json:"env_keys"`
	EnvFile string                    `json:"env_file"`
	Records string                    `json:"records"`
	Marker  string                    `json:"marker"`
	Limits  extension.ContainerLimits `json:"limits"`
	Network string                    `json:"network"`
	Gateway Gateway                   `json:"gateway"`
}

// Gateway is the plan's gateway: its image, the hosts its proxy allows,
// and, when the launch needs its toolbelt, the forward (unix:<socket> or
// tcp:<port file>) and the host directory holding them, mounted read-only
// into the gateway.
type Gateway struct {
	Image   string   `json:"image"`
	Allow   []string `json:"allow"`
	Forward string   `json:"forward,omitempty"`
	Mount   string   `json:"mount,omitempty"`
}

func (p *Provider) Plan(ctx context.Context, owner extension.ResourceOwner, policy extension.ContainerPolicy, needs extension.SandboxNeeds, l extension.SandboxLaunch) (extension.ContainerPlan, error) {
	cfg, s, err := ParseConfig(policy.Config.Data)
	if err != nil {
		return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	paths := policy.Paths
	if !filepath.IsAbs(paths.Records) || !filepath.IsAbs(paths.Run) || owner.ExecutionID == "" {
		return extension.ContainerPlan{}, fmt.Errorf("%w: incomplete launch", extension.ErrSandboxUnavailable)
	}
	if p.o.Gateway == "" {
		return extension.ContainerPlan{}, fmt.Errorf("%w: this build has no gateway image: set OPEN_CONDUIT_GATEWAY_IMAGE to one pinned by digest (make gateway-image builds one)", extension.ErrSandboxUnavailable)
	}
	if !gatewayRef.MatchString(p.o.Gateway) {
		return extension.ContainerPlan{}, fmt.Errorf("%w: the gateway image %s is not pinned by digest", extension.ErrSandboxUnavailable, p.o.Gateway)
	}
	// The only socket a container reaches is its toolbelt, through the
	// gateway's forward.
	gw := Gateway{Image: p.o.Gateway, Allow: append([]string{}, cfg.Network.Allow...)}
	for _, s := range needs.Sockets {
		if paths.Toolbelt == "" || extension.CanonicalPath(s) != extension.CanonicalPath(paths.Toolbelt) {
			return extension.ContainerPlan{}, fmt.Errorf("%w: the docker provider forwards only the execution's toolbelt socket", extension.ErrSandboxUnavailable)
		}
		socket := extension.CanonicalPath(paths.Toolbelt)
		gw.Mount = filepath.Dir(socket)
		gw.Forward = "unix:" + socket
		if p.o.Forward == ForwardTCP {
			gw.Forward = "tcp:" + filepath.Join(gw.Mount, PortFile)
		}
	}
	for _, kv := range l.Env {
		if strings.ContainsAny(kv, "\n\r\x00") || !strings.Contains(kv, "=") || strings.HasPrefix(kv, "=") {
			return extension.ContainerPlan{}, fmt.Errorf("%w: an environment entry can't be passed to a container", extension.ErrSandboxUnavailable)
		}
	}
	mounts, err := p.mounts(cfg, paths, needs)
	if err != nil {
		return extension.ContainerPlan{}, err
	}
	if h := p.Health(ctx, s.Context, cfg.Image, paths.Records, true); h.Status == "fail" {
		return extension.ContainerPlan{}, fmt.Errorf("%w: docker on context %s: %s", extension.ErrSandboxUnavailable, s.Context, h.Detail)
	}
	plan := Plan{Context: s.Context, User: fmt.Sprintf("%d:%d", p.o.UID, p.o.GID), Workdir: l.Dir, Mounts: mounts, Argv: l.Argv,
		Records: paths.Records, Marker: filepath.Join(paths.Run, extension.ContainerTermMarker), Limits: cfg.Limits, Network: "internal", Gateway: gw}
	if plan.Limits.PIDs == 0 {
		plan.Limits.PIDs = DefaultPIDs
	}
	env := containerEnv(l.Env, paths.Toolbelt, gw.Forward != "")
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		plan.EnvKeys = append(plan.EnvKeys, k)
	}
	sort.Strings(plan.EnvKeys)
	sum := sha256.Sum256([]byte(strings.Join(env, "\n") + "\x00" + strings.Join(l.Argv, "\x00")))
	plan.EnvFile = filepath.Join(paths.Records, "env-"+hex.EncodeToString(sum[:8]))
	if err := os.WriteFile(plan.EnvFile, []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	data, _ := json.Marshal(plan)
	reach := "no hosts"
	if len(gw.Allow) > 0 {
		reach = strings.Join(gw.Allow, ", ")
	}
	summary := fmt.Sprintf("docker %s: %s, %d mounts, gateway allowing %s", s.Context, shortImage(cfg.Image), len(mounts), reach)
	if gw.Forward != "" {
		summary += ", toolbelt forwarded (" + p.o.Forward + ")"
	}
	return extension.ContainerPlan{Image: cfg.Image, Summary: summary, Data: domain.Payload{Schema: PlanSchema, Data: data}}, nil
}

// proxyVars are the variables that point a process at a proxy: a
// container's are always the gateway's.
var proxyVars = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true}

// containerEnv is a launch's environment as the container sees it: the
// gateway's proxy, the gateway image's toolbelt first on PATH and, with a
// forward, the toolbelt endpoint moved to the forward's socket.
func containerEnv(env []string, toolbelt string, forward bool) []string {
	hosts := map[string]bool{}
	if toolbelt != "" {
		hosts["unix://"+toolbelt] = true
		hosts["unix://"+extension.CanonicalPath(toolbelt)] = true
	}
	out := make([]string, 0, len(env)+8)
	path := false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case proxyVars[k]:
			continue
		case k == "PATH":
			path, v = true, BinDir+":"+v
		case forward && hosts[v]:
			v = "unix://" + ToolbeltSocket
		}
		out = append(out, k+"="+v)
	}
	if !path {
		out = append(out, "PATH="+BinDir+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	proxy := "http://" + GatewayAlias + ":" + GatewayPort
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		out = append(out, k+"="+proxy)
	}
	return append(out, "NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1")
}

func shortImage(image string) string {
	name, digest, _ := strings.Cut(image, "@sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return name + "@" + digest
}

// mounts are the grant as bind mounts at the same paths: the write grant
// and the run directory read-write, the read grant, what the host shares
// and the records read-only. Every read-only need and every denied path
// inside a mount is covered by a read-only bind over it or an empty one
// hiding it; one that can't be (missing, or a symlink) refuses the launch.
func (p *Provider) mounts(cfg extension.ContainerConfig, paths extension.SandboxPaths, needs extension.SandboxNeeds) ([]Mount, error) {
	expand := func(list []string) []string {
		var out []string
		for _, s := range list {
			s = strings.NewReplacer("{workspace}", paths.Workspace, "{run}", paths.Run).Replace(s)
			if s == "~" || strings.HasPrefix(s, "~/") {
				s = p.o.Home + s[1:]
			}
			out = append(out, extension.CanonicalPath(s))
		}
		return out
	}
	var rw, ro []string
	for _, w := range append(expand(cfg.Write), extension.CanonicalPath(paths.Run)) {
		if fi, err := os.Stat(w); err != nil || !fi.IsDir() && !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: the write grant %s is not a file or directory here", extension.ErrSandboxUnavailable, w)
		}
		rw = append(rw, w)
	}
	for _, r := range append(append(expand(cfg.Read), needs.Read...), extension.CanonicalPath(paths.Records)) {
		if _, err := os.Stat(r); err == nil {
			ro = append(ro, r)
		}
	}
	rw = outermost(rw)
	var keep []string
	for _, r := range outermost(ro) {
		if !extension.Within(r, rw) {
			keep = append(keep, r)
		}
	}
	ro = keep
	byTarget := map[string]Mount{}
	for _, r := range ro {
		byTarget[r] = Mount{Target: r, ReadOnly: true}
	}
	for _, w := range rw {
		byTarget[w] = Mount{Target: w}
	}
	all := append(append([]string{}, rw...), ro...)
	cover := func(path, why string) (os.FileInfo, error) {
		fi, err := os.Lstat(path)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w: %s is %s but doesn't exist, so it can't be held", extension.ErrSandboxUnavailable, path, why)
		case fi.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("%w: %s is %s but is a symlink, so it can't be held", extension.ErrSandboxUnavailable, path, why)
		}
		return fi, nil
	}
	for _, r := range needs.ReadOnly {
		if !extension.Within(r, rw) {
			continue
		}
		if _, err := cover(r, "read-only"); err != nil {
			return nil, err
		}
		byTarget[r] = Mount{Target: r, ReadOnly: true}
	}
	empty := filepath.Join(paths.Records, "empty")
	emptyDir := filepath.Join(paths.Records, "empty.d")
	for _, d := range needs.Deny {
		if !extension.Within(d, all) {
			continue
		}
		for _, m := range all {
			if d == m {
				return nil, fmt.Errorf("%w: %s is mounted and denied", extension.ErrSandboxDenied, d)
			}
		}
		fi, err := cover(d, "denied")
		if err != nil {
			return nil, err
		}
		src := empty
		if fi.IsDir() {
			src = emptyDir
			_ = os.MkdirAll(emptyDir, 0o555)
		} else if err := os.WriteFile(empty, nil, 0o444); err != nil && !errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
		}
		byTarget[d] = Mount{Source: src, Target: d, ReadOnly: true}
	}
	out := make([]Mount, 0, len(byTarget))
	for _, m := range byTarget {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out, nil
}

// outermost drops each path inside another.
func outermost(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		if len(out) == 0 || !extension.Within(p, out) {
			out = append(out, p)
		}
	}
	return out
}

// entryStub is the container's entry point under docker's init: it runs
// the launch's argv with the container's stdin, writes the TERM marker in
// the run directory when the container is sent TERM, passes TERM, INT and
// HUP on, and exits with the command's status. A trap cuts a wait short
// (its status is the signal's), so the stub waits again for the command's
// own; a wait with nothing left to collect (127) after one a trap ran
// during keeps that one's status.
const entryStub = `m=$1; shift; c=; p=; g=; l=
f() { g=1; if [ -n "$c" ]; then kill -"$1" "$c" 2>/dev/null; else p=$1; fi; }
trap ': > "$m"; f TERM' TERM
trap 'f INT' INT
trap 'f HUP' HUP
exec 3<&0
"$@" <&3 3<&- &
c=$!
exec 3<&-
if [ -n "$p" ]; then kill -"$p" "$c" 2>/dev/null; fi
while :; do g=; wait "$c"; s=$?; if [ -z "$g" ]; then if [ "$s" = 127 ] && [ -n "$l" ]; then s=$l; fi; exit "$s"; fi; l=$s; done`

// names are the container's and its network's names: an execution key
// (a hash of the host and the execution, so two hosts never collide) and
// the segment.
func (p *Provider) names(labels map[string]string) (container, network string) {
	sum := sha256.Sum256([]byte(labels[extension.ContainerLabelHost] + "/" + labels[extension.ContainerLabelExecution]))
	key := hex.EncodeToString(sum[:8])
	return "oc-" + key + "-" + labels[extension.ContainerLabelSegment], "oc-" + key
}

func (p *Provider) Create(ctx context.Context, plan extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	var d Plan
	if err := json.Unmarshal(plan.Data.Data, &d); err != nil || d.Context == "" || len(d.Argv) == 0 {
		return extension.ContainerCreated{}, errors.New("the plan isn't a docker plan")
	}
	defer os.Remove(d.EnvFile)
	daemon, _, err := p.info(ctx, d.Context)
	if err != nil {
		return extension.ContainerCreated{}, err
	}
	name, network := p.names(labels)
	if err := p.network(ctx, d.Context, network, labels, true); err != nil {
		return extension.ContainerCreated{}, err
	}
	if err := p.network(ctx, d.Context, network+"-egress", labels, false); err != nil {
		return extension.ContainerCreated{}, err
	}
	volume, err := p.gateway(ctx, d, name, network, labels)
	if err != nil {
		p.removeGateway(context.WithoutCancel(ctx), d.Context, name)
		return extension.ContainerCreated{}, err
	}
	cidfile := filepath.Join(d.Records, name+".cid")
	_ = os.Remove(cidfile)
	args := []string{"create", "--name", name, "--init", "--cidfile", cidfile, "--restart", "no", "--interactive",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,mode=1777",
		"--user", d.User, "--network", network, "--workdir", d.Workdir, "--env-file", d.EnvFile,
		"--pids-limit", strconv.Itoa(d.Limits.PIDs)}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	if d.Limits.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(d.Limits.CPUs, 'f', -1, 64))
	}
	if d.Limits.Memory != "" {
		args = append(args, "--memory", d.Limits.Memory)
	}
	for _, m := range d.Mounts {
		args = append(args, "--mount", mountArg(m))
	}
	// The gateway's forward socket, and the gateway image's toolbelt (the
	// Linux os), both read-only.
	args = append(args, "--mount", "type=volume,source="+volume+",target="+ToolbeltDir+",readonly",
		"--mount", "type=image,source="+d.Gateway.Image+",target="+BinDir+",image-subpath=toolbelt")
	args = append(args, "--entrypoint", "/bin/sh", plan.Image, "-c", entryStub, "open-conduit-entry", d.Marker)
	args = append(args, d.Argv...)
	out, err := p.docker(ctx, d.Context, args...)
	id := strings.TrimSpace(string(out))
	if err != nil {
		// A container docker made before failing is removed by the ID it
		// wrote.
		if raw, rerr := os.ReadFile(cidfile); rerr == nil && len(bytes.TrimSpace(raw)) > 0 {
			_, _ = p.docker(context.WithoutCancel(ctx), d.Context, "rm", "-f", strings.TrimSpace(string(raw)))
		}
		p.removeGateway(context.WithoutCancel(ctx), d.Context, name)
		return extension.ContainerCreated{}, err
	}
	ref := extension.ContainerRef{ID: d.Context + "/" + id, Labels: labels}
	argv := []string{p.o.Helper, "container", "run", "--records", d.Records, ref.ID}
	return extension.ContainerCreated{Ref: ref, Daemon: daemon, Argv: argv, Env: p.cliEnv()}, nil
}

func mountArg(m Mount) string {
	src := m.Source
	if src == "" {
		src = m.Target
	}
	fields := []string{"type=bind", "source=" + src, "target=" + m.Target}
	if m.ReadOnly {
		fields = append(fields, "readonly")
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write(fields)
	w.Flush()
	return strings.TrimSuffix(b.String(), "\n")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// network makes one of the execution's networks. The internal one, which
// its containers join, has no route out, no IPv4 on the bridge (so a Linux
// host's ports aren't reachable through it) and no IPv6. The egress one
// only the gateway joins. One already there must be the same.
func (p *Provider) network(ctx context.Context, dctx, name string, labels map[string]string, internal bool) error {
	want := "true false true"
	if !internal {
		want = "false false "
	}
	out, err := p.docker(ctx, dctx, "network", "inspect", "--format", "{{.Internal}} {{.EnableIPv6}} {{index .Options \"com.docker.network.bridge.inhibit_ipv4\"}}", name)
	if err == nil {
		if strings.TrimSpace(string(out)) != strings.TrimSpace(want) {
			return fmt.Errorf("network %s exists but isn't what the provider makes", name)
		}
		return nil
	}
	if !notFound(err) {
		return err
	}
	_, err = p.docker(ctx, dctx, append([]string{"network", "create"}, networkArgs(name, labels, internal)...)...)
	return err
}

func networkArgs(name string, labels map[string]string, internal bool) []string {
	args := []string{"--driver", "bridge", "--ipv6=false"}
	role := "egress"
	if internal {
		args = append(args, "--internal", "-o", "com.docker.network.bridge.inhibit_ipv4=true")
		role = "execution"
	}
	for _, k := range sortedKeys(labels) {
		if k == extension.ContainerLabelHost || k == extension.ContainerLabelExecution {
			args = append(args, "--label", k+"="+labels[k])
		}
	}
	return append(args, "--label", labelNetwork+"="+role, name)
}

// gateway creates and starts the launch's gateway (name-gw) and the
// volume its forward socket is in (name-tb), and returns the volume. The
// gateway joins the egress network, then the execution's as "gateway". It
// runs hardened as the operator, with the toolbelt's host directory
// read-only when it forwards.
func (p *Provider) gateway(ctx context.Context, d Plan, name, network string, labels map[string]string) (string, error) {
	gw, volume := name+"-gw", name+"-tb"
	// What a create that failed half way left goes first.
	p.removeGateway(ctx, d.Context, name)
	vargs := []string{"volume", "create", "--driver", "local", "--opt", "type=tmpfs", "--opt", "device=tmpfs",
		"--opt", fmt.Sprintf("o=size=1m,uid=%d,gid=%d,mode=0700", p.o.UID, p.o.GID)}
	for _, k := range sortedKeys(labels) {
		vargs = append(vargs, "--label", k+"="+labels[k])
	}
	if _, err := p.docker(ctx, d.Context, append(vargs, volume)...); err != nil {
		return "", err
	}
	args := []string{"create", "--name", gw, "--restart", "no", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--user", d.User, "--pids-limit", "256", "--memory", "256m", "--network", network + "-egress",
		"--add-host", "host.docker.internal:host-gateway", "--label", labelRole + "=gateway",
		"--mount", "type=volume,source=" + volume + ",target=" + ToolbeltDir}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	if d.Gateway.Mount != "" {
		args = append(args, "--mount", mountArg(Mount{Target: d.Gateway.Mount, ReadOnly: true}))
	}
	args = append(args, d.Gateway.Image, "serve", "--listen", ":"+GatewayPort, "--allow", strings.Join(d.Gateway.Allow, ","))
	if d.Gateway.Forward != "" {
		args = append(args, "--socket", ToolbeltSocket, "--forward", d.Gateway.Forward)
	}
	if _, err := p.docker(ctx, d.Context, args...); err != nil {
		return "", err
	}
	if _, err := p.docker(ctx, d.Context, "network", "connect", "--alias", GatewayAlias, network, gw); err != nil {
		return "", err
	}
	if _, err := p.docker(ctx, d.Context, "start", gw); err != nil {
		return "", err
	}
	// A gateway that exits at once (a bad flag, a socket it can't make)
	// fails the launch with what it said.
	time.Sleep(100 * time.Millisecond)
	out, err := p.docker(ctx, d.Context, "inspect", "--type", "container", "--format", "{{.State.Running}}", gw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(out)) != "true" {
		logs, _ := exec.CommandContext(ctx, p.o.Docker, "--context", d.Context, "logs", "--tail", "5", gw).CombinedOutput()
		return "", fmt.Errorf("the gateway did not start: %s", strings.TrimSpace(string(logs)))
	}
	return volume, nil
}

// removeGateway removes a launch's gateway and its volume; either being
// gone is not an error.
func (p *Provider) removeGateway(ctx context.Context, dctx, name string) error {
	if _, err := p.docker(ctx, dctx, "rm", "--force", name+"-gw"); err != nil && !notFound(err) {
		return err
	}
	if _, err := p.docker(ctx, dctx, "volume", "rm", "--force", name+"-tb"); err != nil && !notFound(err) {
		return err
	}
	return nil
}

func (p *Provider) Signal(ctx context.Context, ref extension.ContainerRef, signal string) error {
	dctx, id, err := splitRef(ref.ID)
	if err != nil {
		return err
	}
	_, err = p.docker(ctx, dctx, "kill", "--signal", signal, id)
	return err
}

func (p *Provider) Stop(ctx context.Context, ref extension.ContainerRef, grace time.Duration) error {
	dctx, id, err := splitRef(ref.ID)
	if err != nil {
		return err
	}
	_, err = p.docker(ctx, dctx, "stop", "--timeout", strconv.Itoa(int(grace.Seconds())), id)
	if notFound(err) {
		return nil
	}
	return err
}

func (p *Provider) Inspect(ctx context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	dctx, id, err := splitRef(ref.ID)
	if err != nil {
		return extension.ContainerState{}, err
	}
	daemon, _, err := p.info(ctx, dctx)
	if err != nil {
		return extension.ContainerState{}, err
	}
	st, err := p.inspect(ctx, dctx, id)
	if notFound(err) {
		return extension.ContainerState{Ref: ref, Daemon: daemon, Gone: true}, nil
	}
	st.Daemon = daemon
	return st, err
}

// inspected is what the provider reads of `docker inspect`.
type inspected struct {
	ID     string
	Config struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Running   bool
		ExitCode  int
		StartedAt time.Time
		Error     string
	}
	Mounts []struct{ Source string }
}

func (p *Provider) inspect(ctx context.Context, dctx, id string) (extension.ContainerState, error) {
	out, err := p.docker(ctx, dctx, "inspect", "--type", "container", id)
	if err != nil {
		return extension.ContainerState{}, err
	}
	var got []inspected
	if err := json.Unmarshal(out, &got); err != nil || len(got) != 1 {
		return extension.ContainerState{}, fmt.Errorf("docker inspect %s: unreadable output", id)
	}
	c := got[0]
	st := extension.ContainerState{Ref: extension.ContainerRef{ID: dctx + "/" + c.ID, Labels: c.Config.Labels}, Image: c.Config.Image,
		Running: c.State.Running, ExitCode: c.State.ExitCode}
	if c.State.StartedAt.Year() > 1 {
		st.StartedAt = c.State.StartedAt
	}
	for _, m := range c.Mounts {
		st.Mounts = append(st.Mounts, m.Source)
	}
	return st, nil
}

func (p *Provider) List(ctx context.Context, owner extension.ResourceOwner) ([]extension.ContainerState, error) {
	filters := []string{"--filter", "label=" + extension.ContainerLabelHost + "=" + p.o.Host}
	if owner.ExecutionID != "" {
		filters = append(filters, "--filter", "label="+extension.ContainerLabelExecution+"="+string(owner.ExecutionID))
	}
	seen := map[string]bool{}
	var out []extension.ContainerState
	for _, dctx := range p.o.Contexts() {
		if seen[dctx] {
			continue
		}
		seen[dctx] = true
		daemon, _, err := p.info(ctx, dctx)
		if err != nil {
			return nil, err
		}
		ids, err := p.docker(ctx, dctx, append([]string{"ps", "--all", "--quiet", "--no-trunc"}, filters...)...)
		if err != nil {
			return nil, err
		}
		for _, id := range strings.Fields(string(ids)) {
			st, err := p.inspect(ctx, dctx, id)
			if notFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			// An execution's gateways are not its containers; the host's
			// whole listing has them, so reconcile removes them.
			if owner.ExecutionID != "" && st.Ref.Labels[labelRole] == "gateway" {
				continue
			}
			st.Daemon = daemon
			out = append(out, st)
		}
	}
	return out, nil
}

func (p *Provider) Remove(ctx context.Context, ref extension.ContainerRef) error {
	dctx, id, err := splitRef(ref.ID)
	if err != nil {
		return err
	}
	labels := ref.Labels
	if labels[extension.ContainerLabelExecution] == "" {
		st, err := p.inspect(ctx, dctx, id)
		if err != nil && !notFound(err) {
			return err
		}
		labels = st.Ref.Labels
	}
	if _, err := p.docker(ctx, dctx, "rm", "--force", "--volumes", id); err != nil && !notFound(err) {
		return err
	}
	if labels[extension.ContainerLabelExecution] == "" {
		return nil
	}
	// The launch's gateway and its volume go with it, and the execution's
	// networks, unless a later segment's containers are already on them.
	name, network := p.names(labels)
	if err := p.removeGateway(ctx, dctx, name); err != nil {
		return err
	}
	for _, n := range []string{network, network + "-egress"} {
		if _, err := p.docker(ctx, dctx, "network", "rm", n); err != nil && !notFound(err) && !strings.Contains(err.Error(), "active endpoints") {
			return err
		}
	}
	return nil
}

func splitRef(ref string) (dctx, id string, err error) {
	dctx, id, ok := strings.Cut(ref, "/")
	if !ok || !contextPattern.MatchString(dctx) || id == "" {
		return "", "", fmt.Errorf("%q is not a docker container reference", ref)
	}
	return dctx, id, nil
}

// info is the daemon's ID and server version on a context.
func (p *Provider) info(ctx context.Context, dctx string) (id, version string, err error) {
	out, err := p.docker(ctx, dctx, "info", "--format", "{{.ID}}|{{.ServerVersion}}|{{.OperatingSystem}}")
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("docker on context %s reported no daemon ID", dctx)
	}
	return parts[0], parts[1], nil
}

// dockerError is a docker CLI failure, with what it said.
type dockerError struct {
	args   []string
	stderr string
	err    error
}

func (e *dockerError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return "docker " + strings.Join(e.args[:min(len(e.args), 2)], " ") + ": " + msg
}

func notFound(err error) bool {
	var d *dockerError
	if !errors.As(err, &d) {
		return false
	}
	s := strings.ToLower(d.stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such object") || strings.Contains(s, "not found")
}

// docker runs the CLI on a context and returns its stdout.
func (p *Provider) docker(ctx context.Context, dctx string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.o.Docker, append([]string{"--context", dctx}, args...)...)
	cmd.Env = p.cliEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &dockerError{args: args, stderr: stderr.String(), err: err}
	}
	return out, nil
}

// cliEnv is the docker CLI's environment: what it needs to find itself
// and its config, without a DOCKER_HOST or DOCKER_CONTEXT that would
// override the context named.
func (p *Provider) cliEnv() []string {
	var out []string
	for _, kv := range p.o.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "HOME", "DOCKER_CONFIG", "XDG_RUNTIME_DIR", "TMPDIR":
			out = append(out, kv)
		}
	}
	return out
}
