package tunnelext

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/tunnel"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

func next(t *testing.T, urls <-chan string) string {
	t.Helper()
	select {
	case u := <-urls:
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("no URL reported")
	}
	return ""
}

func fakeCloudflared(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared")
	body := "#!/bin/sh\ncase \"$1\" in --version) echo 'cloudflared version 2026.1.0'; exit 0 ;; esac\necho 'INF |  https://brave-fox-12.trycloudflare.com  |'\nexec sleep 30\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// tunnel.run reports the provider's URL through tunnel.url and answers only
// once the host cancels it; a config the provider can't run is refused as
// invalid, which the host doesn't start again.
func TestTunnelRunOverTheWire(t *testing.T) {
	h := sdktest.New(t, Build(tunnel.Cloudflared, provider(tunnel.Cloudflared)))
	ctx, cancel := context.WithCancel(context.Background())
	urls, done := make(chan string, 4), make(chan error, 1)
	go func() {
		done <- h.Tunnel(ctx, tunnel.Cloudflared, "127.0.0.1:8787", tunnel.Config(extension.TunnelConfig{Binary: fakeCloudflared(t)}), urls)
	}()
	if got := next(t, urls); got != "https://brave-fox-12.trycloudflare.com" {
		t.Fatalf("url %q", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("cancelled run: %v", err)
	}
	err := h.Tunnel(context.Background(), tunnel.Cloudflared, "127.0.0.1:8787", tunnel.Config(extension.TunnelConfig{Name: "hooks"}), urls)
	if protocol.ErrorCode(err) != protocol.CodeInvalid {
		t.Fatalf("refused config: %v", err)
	}
}

// The check reads the tunnel in its view: another type is not its
// concern, and a provider that isn't installed fails.
func TestCheck(t *testing.T) {
	p := provider(tunnel.Cloudflared)
	for _, c := range []struct {
		view *protocol.CheckTunnel
		code string
	}{
		{nil, "not_configured"},
		{&protocol.CheckTunnel{Type: tunnel.Ngrok}, "not_configured"},
		{&protocol.CheckTunnel{Type: tunnel.Cloudflared, Config: tunnel.Config(extension.TunnelConfig{Binary: fakeCloudflared(t)}).Data}, "ok"},
		{&protocol.CheckTunnel{Type: tunnel.Cloudflared, Config: tunnel.Config(extension.TunnelConfig{Binary: "/nonexistent/cloudflared"}).Data}, "tunnel_unavailable"},
	} {
		if got := check(context.Background(), tunnel.Cloudflared, p, c.view); got.Code != c.code {
			t.Errorf("%+v: %+v", c.view, got)
		}
	}
}
