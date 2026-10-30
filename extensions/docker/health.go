package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
// that the daemon is Engine 28 or later, and that a file the image writes
// under --user into dir, a host directory shared as workspaces are, is
// the operator's on the host. Any failure blocks the provider, whatever
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
	}
	return h
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
