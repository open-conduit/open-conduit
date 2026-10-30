package srt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// Health is what doctor reports for srt: whether a sandboxed binding can
// run here, and why not.
type Health struct {
	// Status is ok, warn or fail; Code names the first problem.
	Status, Code, Detail string
	Items                []string
}

// Health checks srt is present at a known version and, on Linux, that the
// kernel lets it make namespaces and apply its seccomp filter, by running
// /usr/bin/true under an empty policy.
func (p *Provider) Health(ctx context.Context) Health {
	o := p.o
	if o.GOOS != "darwin" && o.GOOS != "linux" {
		return Health{Status: "fail", Code: "unsupported_platform", Detail: "srt does not support " + o.GOOS}
	}
	bin, version, err := p.binary(ctx)
	if err != nil {
		return Health{Status: "fail", Code: "srt_unavailable", Detail: strings.TrimPrefix(err.Error(), extension.ErrSandboxUnavailable.Error()+": ")}
	}
	h := Health{Status: "ok", Code: "ok", Detail: fmt.Sprintf("srt %s at %s", version, bin)}
	if compareVersions(version, TestedVersion) > 0 {
		h.Status, h.Code = "warn", "untested_version"
		h.Items = append(h.Items, fmt.Sprintf("srt %s is newer than %s, the version the isolation tests ran on", version, TestedVersion))
	}
	if o.GOOS == "linux" {
		for _, c := range []struct{ file, bad, why string }{
			{"/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1", "AppArmor restricts unprivileged user namespaces (Ubuntu 24.04): set kernel.apparmor_restrict_unprivileged_userns=0 or give bwrap an AppArmor profile"},
			{"/proc/sys/kernel/unprivileged_userns_clone", "0", "unprivileged user namespaces are off: set kernel.unprivileged_userns_clone=1"},
			{"/proc/sys/user/max_user_namespaces", "0", "user namespaces are off: raise user.max_user_namespaces"},
		} {
			if raw, err := os.ReadFile(c.file); err == nil && strings.TrimSpace(string(raw)) == c.bad {
				return Health{Status: "fail", Code: "userns_unavailable", Detail: c.why}
			}
		}
		if raw, err := os.ReadFile("/proc/self/status"); err != nil || !strings.Contains(string(raw), "Seccomp:") {
			return Health{Status: "fail", Code: "seccomp_unavailable", Detail: "the kernel has no seccomp, so srt can't restrict unix sockets"}
		}
	}
	dir, err := os.MkdirTemp("", "open-conduit-srt-")
	if err != nil {
		return Health{Status: "fail", Code: "preflight_failed", Detail: err.Error()}
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "srt.json")
	empty := `{"network":{"allowedDomains":[],"deniedDomains":[],"strictAllowlist":true,"allowLocalBinding":false},"filesystem":{"denyRead":[],"allowRead":[],"allowWrite":[],"denyWrite":[]}}`
	if err := os.WriteFile(file, []byte(empty), 0o600); err != nil {
		return Health{Status: "fail", Code: "preflight_failed", Detail: err.Error()}
	}
	if err := p.preflight(ctx, bin, file, dir, p.wrapperPath(bin, o.Environ())); err != nil {
		return Health{Status: "fail", Code: "preflight_failed", Detail: strings.TrimPrefix(err.Error(), extension.ErrSandboxUnavailable.Error()+": ")}
	}
	return h
}
