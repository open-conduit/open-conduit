package extension

import (
	"context"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
)

// TunnelConfigSchema labels the tunnel config the host hands a provider:
// the operator's settings.webhooks (or serve flags) for that tunnel type.
const TunnelConfigSchema = "core.config.tunnel.v1"

// TunnelConfig is the config a tunnel provider runs with. Every field is
// optional and a provider reads only those of its own type; the host has
// already checked the combination (config settings.webhooks).
type TunnelConfig struct {
	// Binary overrides the provider's executable.
	Binary string `json:"binary,omitempty"`
	// Domain is a reserved domain (a stable URL).
	Domain string `json:"domain,omitempty"`
	// Name and Hostname select a named tunnel and its hostname.
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// TunnelRun is one run of a tunnel: the listener it exposes and the
// provider's config.
type TunnelRun struct {
	// Addr is the webhook listener's local address (host:port). The host
	// owns the listener and decides what it serves; the provider forwards
	// to this address and nothing else.
	Addr string
	// Config is the operator's tunnel config (schema TunnelConfigSchema,
	// data a TunnelConfig).
	Config domain.Payload
}

// ErrTunnelConfig is a tunnel config the provider can't run with. The host
// doesn't restart a run that ends with it: the config must change first.
var ErrTunnelConfig = errors.New("extension: the tunnel config is not one this provider can run")

// TunnelProvider exposes the host's webhook listener at a public URL (ngrok,
// a Cloudflare tunnel). A URL the operator already routes to the listener
// (settings.webhooks.public_url) needs no provider.
//
// Run keeps the tunnel to r.Addr up until ctx ends, calling publish with
// each public URL as it changes and with "" while the tunnel is down. It
// returns nil once ctx ends, and an error when the tunnel can't be kept up;
// the host then publishes no URL, records the failure and starts Run again
// after a growing wait (except after ErrTunnelConfig). publish never blocks
// for long and may be called from any goroutine, one call at a time.
type TunnelProvider interface {
	Run(ctx context.Context, r TunnelRun, publish func(url string)) error
}
