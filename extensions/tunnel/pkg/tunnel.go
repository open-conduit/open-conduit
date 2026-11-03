// Package tunnelext serves the webhook tunnel providers (extensions/tunnel)
// as packages over protocol version 1, through protocol/sdk (#493): ngrok
// and cloudflared are each a package of that name, providing the tunnel
// type of the same name. The host holds tunnel.run open for the daemon's
// life with the webhook listener's address and the operator's tunnel
// config, and the package reports each public URL back through tunnel.url.
// Its offline check is doctor's view of whether the provider can run here.
// Nothing in the daemon knows about ngrok or cloudflared: settings.webhooks
// names the tunnel type, and the package of that name (bundled beside the
// binary, or installed) provides it. open-conduit serves them as
// `open-conduit extension-run ngrok` and `… cloudflared`.
//
// The host keeps the listener and decides what it serves; the package only
// forwards the address it is given. The provider's own variables
// (NGROK_AUTHTOKEN, say) reach it only through the operator's grant,
// settings.extensions.<type>.pass_env.
package tunnelext

import (
	"context"
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/tunnel"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Version is these packages' own version.
const Version = "0.1.0"

var titles = map[string][2]string{
	tunnel.Ngrok:       {"ngrok tunnel", "Exposes the webhook listener at a public URL through ngrok."},
	tunnel.Cloudflared: {"Cloudflare tunnel", "Exposes the webhook listener at a public URL through a quick or named Cloudflare tunnel."},
}

// Manifest declares the package of a tunnel type: the type, named after
// the package, and its offline check.
func Manifest(kind string) protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: kind, Version: Version, Title: titles[kind][0],
		Summary: titles[kind][1],
		Command: protocol.HostRunCommand(kind),
		Provides: protocol.Provides{
			Tunnels: []protocol.TunnelDecl{{Type: kind}},
			Checks:  []protocol.CheckDecl{{Name: kind, Offline: true}},
		}}
}

// Build returns the served package around p.
func Build(kind string, p *tunnel.Provider) *sdk.Extension {
	return &sdk.Extension{Manifest: Manifest(kind), Tunnels: map[string]extension.TunnelProvider{kind: p},
		CheckRuns: map[string]func(context.Context, *sdk.Host, protocol.CheckRunParams) (protocol.CheckRunResult, error){
			kind: func(ctx context.Context, _ *sdk.Host, c protocol.CheckRunParams) (protocol.CheckRunResult, error) {
				return check(ctx, kind, p, c.Config.Tunnel), nil
			},
		}}
}

// check is doctor's view of the configured tunnel: that its provider is
// installed and answers its version. Whether the running tunnel has a
// public URL is the host's webhooks check.
func check(ctx context.Context, kind string, p *tunnel.Provider, t *protocol.CheckTunnel) protocol.CheckRunResult {
	if t == nil || t.Type != kind {
		return protocol.CheckRunResult{OK: true, Status: "info", Code: "not_configured", Detail: "the webhook tunnel is not " + kind}
	}
	v, err := p.Check(ctx, t.Config)
	if err != nil {
		return protocol.CheckRunResult{OK: false, Status: "fail", Code: "tunnel_unavailable", Detail: err.Error() + "; webhooks arrive only through reconcile until this is fixed"}
	}
	return protocol.CheckRunResult{OK: true, Status: "ok", Code: "ok", Detail: v}
}

// provider returns kind's provider, logging to stderr (the host's log of
// the package).
func provider(kind string) *tunnel.Provider {
	p, err := tunnel.New(kind)
	if err != nil {
		panic(err)
	}
	p.Log = func(msg string, args ...any) { fmt.Fprintln(os.Stderr, append([]any{msg}, args...)...) }
	return p
}

// Main returns the entry point serving kind's package over protocol
// version 1 on stdin and stdout, as `open-conduit extension-run <kind>`
// does. Its subcommands package-manifest and package-certification print
// the package files.
func Main(kind string) func(args []string) int {
	return func(args []string) int {
		p := provider(kind)
		ext := Build(kind, p)
		if sdk.PackageCommand(args, ext.Manifest, protocol.CertificationFile{}, os.Stdout) {
			return 0
		}
		// Serve returns once the host closes the connection, perhaps before
		// the cancelled run has stopped ngrok or cloudflared.
		defer p.Close()
		if err := sdk.Serve(ext); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
}
