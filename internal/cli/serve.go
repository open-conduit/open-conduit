package cli

import (
	"fmt"
	"path/filepath"
	"time"
)

// ServeRequest carries resolved daemon paths and tuning to the composition
// root. The open-conduit entrypoint supplies Env.Serve, so this package (and
// the os binary) never links the runtime implementation.
type ServeRequest struct {
	StateDir, ConfigPath, Socket, ToolbeltDir, LifecycleLog string
	PollInterval, LeaseDuration, RetentionInterval          time.Duration
	// SweepInterval paces the spare-workspace sweep (default 6h).
	SweepInterval time.Duration
	// WebhookAddr enables the webhook listener; Tunnel* expose it. Empty
	// values fall back to the config's settings.webhooks.
	WebhookAddr                                                   string
	TunnelKind, TunnelBinary, TunnelDomain, TunnelName, PublicURL string
	TunnelHostname                                                string
	NoTunnel, DryRun, ClaudeUsage                                 bool
}

func defaultConfigPath(env Env) string {
	if v := env.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "open-conduit", "config.json")
	}
	return filepath.Join(env.Getenv("HOME"), ".config", "open-conduit", "config.json")
}

// serve resolves B05 path precedence: --config → OPEN_CONDUIT_CONFIG →
// default; --data-dir → OPEN_CONDUIT_DATA_DIR → default; --socket →
// OPEN_CONDUIT_SOCKET → <state>/management.sock.
func (o *operator) serve(args []string) int {
	fs := newFlags("serve", "config", "toolbelt-dir", "lifecycle-log", "webhook-addr", "tunnel", "tunnel-binary", "tunnel-domain", "tunnel-name", "tunnel-hostname", "public-url")
	noTunnel := fs.fs.Bool("no-tunnel", false, "")
	dryRun := fs.fs.Bool("dry-run", false, "")
	usage := fs.fs.Bool("claude-usage", false, "")
	poll := fs.fs.Duration("poll-interval", time.Second, "")
	lease := fs.fs.Duration("lease-duration", time.Minute, "")
	retention := fs.fs.Duration("retention-interval", 10*time.Minute, "")
	sweep := fs.fs.Duration("sweep-interval", 6*time.Hour, "")
	if pos, err := fs.parse(args); err != nil || len(pos) != 0 || *poll <= 0 || *lease <= 0 || *retention <= 0 || *sweep <= 0 {
		return usageError(o.errOut, "invalid serve flags")
	}
	switch fs.get("tunnel") {
	case "", "ngrok", "cloudflared", "static":
	default:
		return usageError(o.errOut, "--tunnel is ngrok, cloudflared or static")
	}
	if o.env.Serve == nil {
		fmt.Fprintln(o.errOut, "open-conduit: serve is not available in this binary")
		return 1
	}
	r := ServeRequest{StateDir: o.stateDir(), ConfigPath: o.abs(fs.get("config")), Socket: o.abs(o.socket), ToolbeltDir: o.abs(fs.get("toolbelt-dir")), LifecycleLog: o.abs(fs.get("lifecycle-log")), PollInterval: *poll, LeaseDuration: *lease, RetentionInterval: *retention, SweepInterval: *sweep,
		WebhookAddr: fs.get("webhook-addr"), TunnelKind: fs.get("tunnel"), TunnelBinary: fs.get("tunnel-binary"), TunnelDomain: fs.get("tunnel-domain"),
		TunnelName: fs.get("tunnel-name"), TunnelHostname: fs.get("tunnel-hostname"), PublicURL: fs.get("public-url"), NoTunnel: *noTunnel, DryRun: *dryRun, ClaudeUsage: *usage}
	if r.ConfigPath == "" {
		r.ConfigPath = o.abs(o.env.Getenv("OPEN_CONDUIT_CONFIG"))
	}
	if r.ConfigPath == "" {
		r.ConfigPath = defaultConfigPath(o.env)
	}
	if r.Socket == "" {
		r.Socket = o.abs(o.env.Getenv("OPEN_CONDUIT_SOCKET"))
	}
	return o.env.Serve(r)
}
