package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testGateway = "ghcr.io/open-conduit/open-conduit-gateway@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// gatewayDocker is a docker CLI stand-in whose daemon has the gateway image
// once a pull has made the file have; each pull appends a line to pulls,
// and a pull fails while the file fail exists.
func gatewayDocker(t *testing.T, o Options) (p *Provider, dir string, logs func() []string) {
	t.Helper()
	dir = t.TempDir()
	script := `d=` + dir + `
case "$3" in
image) [ -f "$d/have" ] && echo sha256:x && exit 0; echo "No such image" >&2; exit 1;;
pull) echo "$4 $5" >> "$d/pulls"; [ -f "$d/fail" ] && { echo "net/http: TLS handshake timeout" >&2; exit 1; }; touch "$d/have";;
esac`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	o.Docker, o.Host, o.Gateway = bin, "host-1", testGateway
	o.Environ = func() []string { return []string{"PATH=" + os.Getenv("PATH")} }
	o.Log = func(msg string) { mu.Lock(); lines = append(lines, msg); mu.Unlock() }
	return New(o), dir, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, lines...) }
}

func pulls(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "pulls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// TestGatewayPulledWhenMissing: a context without the pinned gateway image
// gets it by digest from one pull, however many callers ask at once, and
// the pull is logged; once it is there, nothing is pulled again (issue
// #598). Health, doctor's view, only looks.
func TestGatewayPulledWhenMissing(t *testing.T) {
	p, dir, logs := gatewayDocker(t, Options{})
	if code, err := p.gatewayPresent(context.Background(), "default"); code != "gateway_missing" || !strings.Contains(err.Error(), "pulls it when the daemon applies its config") {
		t.Fatalf("before a pull: %s %v", code, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = p.EnsureGateway(context.Background(), "default") }()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}
	if got := pulls(t, dir); len(got) != 1 || got[0] != "--quiet "+testGateway {
		t.Fatalf("pulls %q, want one by digest", got)
	}
	if err := p.EnsureGateway(context.Background(), "default"); err != nil || len(pulls(t, dir)) != 1 {
		t.Fatalf("a present image was pulled again: %v %q", err, pulls(t, dir))
	}
	if l := logs(); len(l) != 2 || !strings.Contains(l[0], "pulling the gateway image "+testGateway+" on docker context default") || !strings.HasPrefix(l[1], "pulled") {
		t.Fatalf("logs %q", l)
	}
}

// TestGatewayPullFailureIsRetried: a pull that fails (a network error) is
// reported in doctor's detail with the command to run, and the next call
// pulls again and succeeds.
func TestGatewayPullFailureIsRetried(t *testing.T) {
	p, dir, logs := gatewayDocker(t, Options{})
	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := p.EnsureGateway(context.Background(), "default")
	if err == nil || !strings.Contains(err.Error(), "last pull failed") || !strings.Contains(err.Error(), "TLS handshake timeout") ||
		!strings.Contains(err.Error(), "docker --context default pull "+testGateway) {
		t.Fatalf("failed pull: %v", err)
	}
	if l := logs(); len(l) != 2 || !strings.HasPrefix(l[1], "could not pull") {
		t.Fatalf("logs %q", l)
	}
	if _, err := p.gatewayPresent(context.Background(), "default"); err == nil || !strings.Contains(err.Error(), "last pull failed") {
		t.Fatalf("doctor's detail after a failed pull: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "fail"))
	if err := p.EnsureGateway(context.Background(), "default"); err != nil || len(pulls(t, dir)) != 2 {
		t.Fatalf("retry: %v %q", err, pulls(t, dir))
	}
}

// TestGatewayNoPull is the negative control: with pulls off, or a local
// image ID that names no repository, nothing is pulled and the error says
// what the operator does instead.
func TestGatewayNoPull(t *testing.T) {
	p, dir, _ := gatewayDocker(t, Options{NoPull: true})
	if err := p.EnsureGateway(context.Background(), "default"); err == nil || !strings.Contains(err.Error(), "pulls are off") ||
		!strings.Contains(err.Error(), "docker --context default pull "+testGateway) {
		t.Fatalf("pulls off: %v", err)
	}
	local, ldir, _ := gatewayDocker(t, Options{})
	local.o.Gateway = "sha256:" + strings.Repeat("a", 64)
	if err := local.EnsureGateway(context.Background(), "default"); err == nil || !strings.Contains(err.Error(), "make gateway-image") {
		t.Fatalf("local image: %v", err)
	}
	if got := append(pulls(t, dir), pulls(t, ldir)...); len(got) != 0 {
		t.Fatalf("pulled with pulls off or nothing to pull: %q", got)
	}
}
