package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/open-conduit/open-conduit/extension"
)

// Health is what doctor reports for one Docker context: whether a
// container binding can run on it, and why not.
type Health struct {
	// Status is ok or fail; Code names the first problem.
	Status, Code, Detail string
	// Daemon is the ID of the daemon the context reaches.
	Daemon string
	Items  []string
}

// Health checks the context reaches a Docker daemon (and reports its ID),
// that the daemon is Engine 28 or later, that a file the image writes
// under --user into dir, a host directory shared as workspaces are, is
// the operator's on the host, that the gateway image is there by digest,
// and the preflight (Preflight). Any failure blocks the provider, whatever
// the Docker. With cached, a pass in the last ten minutes stands.
func (p *Provider) Health(ctx context.Context, dctx, image, dir string, cached bool) Health {
	key := dctx + "\x00" + image
	if cached {
		p.mu.Lock()
		at, ok := p.health[key]
		p.mu.Unlock()
		if ok && p.o.Now().Sub(at) < healthTTL {
			return Health{Status: "ok", Code: "ok"}
		}
	}
	h := p.check(ctx, dctx, image, dir)
	if h.Status == "ok" {
		p.mu.Lock()
		p.health[key] = p.o.Now()
		p.mu.Unlock()
	}
	return h
}

func (p *Provider) check(ctx context.Context, dctx, image, dir string) Health {
	out, err := p.docker(ctx, dctx, "info", "--format", "{{.ID}}|{{.ServerVersion}}|{{.OperatingSystem}}")
	if err != nil {
		return Health{Status: "fail", Code: "docker_unreachable", Detail: fmt.Sprintf("no Docker daemon on context %s: %v", dctx, err)}
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	id, version, system := parts[0], parts[1], parts[2]
	if id == "" {
		return Health{Status: "fail", Code: "docker_unreachable", Detail: "the daemon on context " + dctx + " reported no ID"}
	}
	h := Health{Status: "ok", Code: "ok", Daemon: id, Detail: fmt.Sprintf("Docker %s on context %s (%s), daemon %s", version, dctx, system, id)}
	major, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if major < MinServerMajor {
		h.Status, h.Code, h.Detail = "fail", "docker_too_old", fmt.Sprintf("Docker %s on context %s is older than %d", version, dctx, MinServerMajor)
		return h
	}
	if strings.Contains(system, "Docker Desktop") {
		h.Items = append(h.Items, "Docker Desktop has not been verified: run make container-test on it")
	}
	if image == "" {
		return h
	}
	if err := p.sharedPath(ctx, dctx, image, dir); err != nil {
		h.Status, h.Code, h.Detail = "fail", "shared_path_failed", fmt.Sprintf("context %s: %v", dctx, err)
		return h
	}
	if code, err := p.gatewayPresent(ctx, dctx); err != nil {
		h.Status, h.Code, h.Detail = "fail", code, fmt.Sprintf("context %s: %v", dctx, err)
		return h
	}
	if code, err := p.Preflight(ctx, dctx); err != nil {
		h.Status, h.Code, h.Detail = "fail", code, fmt.Sprintf("context %s: %v", dctx, err)
	}
	return h
}

// gatewayPresent checks the build has a gateway image pinned by digest and
// that the context's daemon has it.
func (p *Provider) gatewayPresent(ctx context.Context, dctx string) (string, error) {
	gw := p.o.Gateway
	switch {
	case gw == "":
		return "gateway_unset", fmt.Errorf("this build has no gateway image: set OPEN_CONDUIT_GATEWAY_IMAGE to one pinned by digest (make gateway-image builds one)")
	case !gatewayRef.MatchString(gw):
		return "gateway_unpinned", fmt.Errorf("the gateway image %s is not pinned by digest", gw)
	}
	if _, err := p.docker(ctx, dctx, "image", "inspect", "--format", "{{.Id}}", gw); err != nil {
		hint := ""
		if strings.Contains(gw, "@") {
			hint = fmt.Sprintf(": docker --context %s pull %s", dctx, gw)
		}
		return "gateway_missing", fmt.Errorf("the gateway image %s is not there%s", gw, hint)
	}
	return "", nil
}

// Preflight makes a network as an execution's internal one is made, and
// checks from inside it, with the gateway image's probe, that the gateway
// alias resolves, no outside name does, and neither the network's bridge
// address, the default bridge's nor host.docker.internal reaches a port the
// host is listening on. It fails if the daemon gives the network IPv6.
func (p *Provider) Preflight(ctx context.Context, dctx string) (string, error) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "oc-preflight-" + hex.EncodeToString(b)
	labels := map[string]string{extension.ContainerLabelHost: p.o.Host}
	if _, err := p.docker(ctx, dctx, append([]string{"network", "create"}, networkArgs(name, labels, true)...)...); err != nil {
		return "preflight_failed", fmt.Errorf("could not make an internal network: %v", err)
	}
	cleanup := context.WithoutCancel(ctx)
	defer func() { _, _ = p.docker(cleanup, dctx, "network", "rm", name) }()
	out, err := p.docker(ctx, dctx, "network", "inspect", "--format", "{{.EnableIPv6}}|{{range .IPAM.Config}}{{.Subnet}} {{.Gateway}};{{end}}", name)
	if err != nil {
		return "preflight_failed", err
	}
	v6, ipam, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	if v6 == "true" || strings.Contains(ipam, ":") {
		return "ipv6_gateway", fmt.Errorf("the daemon gives an internal network IPv6 (%s), which the gateway can't hold: turn off its default IPv6 networks", strings.TrimSuffix(ipam, ";"))
	}
	// A host listener the probe must not reach: loopback on macOS (what
	// Docker's VM forwards), every interface elsewhere (the bridges).
	addr := "0.0.0.0:0"
	if runtime.GOOS == "darwin" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "preflight_failed", err
	}
	defer ln.Close()
	var reached atomic.Bool
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			reached.Store(true)
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	targets := []string{net.JoinHostPort("host.docker.internal", port)}
	bridges := ipam
	if out, err := p.docker(ctx, dctx, "network", "inspect", "--format", "{{range .IPAM.Config}}_ {{.Gateway}};{{end}}", "bridge"); err == nil {
		bridges += string(out)
	}
	for _, entry := range strings.Split(bridges, ";") {
		if f := strings.Fields(entry); len(f) == 2 && net.ParseIP(f[1]) != nil && !strings.Contains(f[1], ":") {
			targets = append(targets, net.JoinHostPort(f[1], port))
		}
	}
	user := fmt.Sprintf("%d:%d", p.o.UID, p.o.GID)
	hard := []string{"--rm", "--network", name, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", user}
	if _, err := p.docker(ctx, dctx, append(append([]string{"run", "-d", "--name", name + "-gw", "--network-alias", GatewayAlias}, hard...), p.o.Gateway, "serve", "--listen", ":"+GatewayPort)...); err != nil {
		return "preflight_failed", fmt.Errorf("could not start the gateway image: %v", err)
	}
	defer func() { _, _ = p.docker(cleanup, dctx, "rm", "--force", name+"-gw") }()
	out, err = p.docker(ctx, dctx, append(append([]string{"run"}, hard...), p.o.Gateway, "probe", "--alias", GatewayAlias,
		"--outside", "example.com", "--unreachable", strings.Join(targets, ","))...)
	if err != nil {
		var fails []string
		for _, line := range strings.Split(string(out), "\n") {
			if f, ok := strings.CutPrefix(line, "fail "); ok {
				fails = append(fails, f)
			}
		}
		if len(fails) == 0 {
			fails = append(fails, err.Error())
		}
		return "preflight_failed", fmt.Errorf("an internal network is not closed: %s", strings.Join(fails, "; "))
	}
	if reached.Load() {
		return "preflight_failed", fmt.Errorf("an internal network is not closed: a container reached a host port")
	}
	return "", nil
}

// sharedPath writes a file into dir from a container running as the
// operator and checks the host sees it as the operator's.
func (p *Provider) sharedPath(ctx context.Context, dctx, image, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	file := filepath.Join(dir, ".open-conduit-probe-"+hex.EncodeToString(b))
	defer os.Remove(file)
	_, err := p.docker(ctx, dctx, "run", "--rm", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--user", fmt.Sprintf("%d:%d", p.o.UID, p.o.GID), "--mount", mountArg(Mount{Target: dir}),
		"--entrypoint", "/bin/sh", image, "-c", `echo open-conduit > "$1"`, "probe", file)
	if err != nil {
		return fmt.Errorf("a container could not write the shared path %s: %v", dir, err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		return fmt.Errorf("a file written in a container under %s is not on the host", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != p.o.UID) {
		return fmt.Errorf("a file written in a container under %s is owned by uid %d on the host, not %d", dir, st.Uid, p.o.UID)
	}
	return nil
}
