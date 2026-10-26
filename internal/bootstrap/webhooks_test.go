package bootstrap

import (
	"testing"

	"github.com/open-conduit/open-conduit/internal/config"
)

// exposure takes the address and the tunnel from the serve flags over
// settings.webhooks, each on its own; a tunnel flag replaces the whole
// stored tunnel.
func TestExposure(t *testing.T) {
	stored := config.Webhooks{Addr: "127.0.0.1:8787", Tunnel: "ngrok", TunnelDomain: "hooks.ngrok.app"}
	for _, c := range []struct {
		name           string
		o              Options
		stored         config.Webhooks
		addr, addrFrom string
		tunnelFrom     string
		t              TunnelOptions
	}{
		{name: "nothing"},
		{name: "stored", stored: stored, addr: "127.0.0.1:8787", addrFrom: fromSettings, tunnelFrom: fromSettings,
			t: TunnelOptions{Kind: "ngrok", Domain: "hooks.ngrok.app"}},
		{name: "flags only", o: Options{WebhookAddr: ":9000", Tunnel: TunnelOptions{PublicURL: "https://x.test"}}, addr: ":9000", addrFrom: fromFlags, tunnelFrom: fromFlags,
			t: TunnelOptions{Kind: "static", PublicURL: "https://x.test"}},
		{name: "address flag keeps the stored tunnel", o: Options{WebhookAddr: ":9000"}, stored: stored, addr: ":9000", addrFrom: fromFlags, tunnelFrom: fromSettings,
			t: TunnelOptions{Kind: "ngrok", Domain: "hooks.ngrok.app"}},
		{name: "tunnel flag replaces the stored tunnel", o: Options{Tunnel: TunnelOptions{Kind: "cloudflared"}}, stored: stored, addr: "127.0.0.1:8787", addrFrom: fromSettings, tunnelFrom: fromFlags,
			t: TunnelOptions{Kind: "cloudflared"}},
		{name: "binary flag applies to the stored tunnel", o: Options{Tunnel: TunnelOptions{Binary: "/opt/ngrok"}}, stored: stored, addr: "127.0.0.1:8787", addrFrom: fromSettings, tunnelFrom: fromSettings,
			t: TunnelOptions{Kind: "ngrok", Binary: "/opt/ngrok", Domain: "hooks.ngrok.app"}},
		{name: "stored public URL means static", stored: config.Webhooks{Addr: ":1", PublicURL: "https://s.test"}, addr: ":1", addrFrom: fromSettings, tunnelFrom: fromSettings,
			t: TunnelOptions{Kind: "static", PublicURL: "https://s.test"}},
	} {
		addr, addrFrom, tun, tunnelFrom := exposure(c.o, c.stored)
		if addr != c.addr || addrFrom != c.addrFrom || tun != c.t || tunnelFrom != c.tunnelFrom {
			t.Errorf("%s: %q %q %+v %q", c.name, addr, addrFrom, tun, tunnelFrom)
		}
	}
}
