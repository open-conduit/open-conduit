package cli

import (
	"fmt"
	"path/filepath"
	"strings"
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
	// NoTelemetry turns off the anonymous usage reports: --no-telemetry,
	// or OPEN_CONDUIT_NO_TELEMETRY or DO_NOT_TRACK set in the environment.
	NoTelemetry bool
	// Web serves the web view from the daemon's process on WebAddr, named
	// WebHost (the web command's defaults when empty), signed in through
	// WebSignIn.
	Web                         bool
	WebAddr, WebHost, WebSignIn string
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
	r, code := o.parseServe(args)
	if code >= 0 {
		return code
	}
	if o.env.Serve == nil {
		fmt.Fprintln(o.errOut, "open-conduit: serve is not available in this binary")
		return 1
	}
	return o.env.Serve(r)
}

// parseServe resolves serve's flags (shared with service install); code is
// the exit code when they are invalid, else -1.
func (o *operator) parseServe(args []string) (ServeRequest, int) {
	fs := newFlags("serve", "config", "toolbelt-dir", "lifecycle-log", "webhook-addr", "tunnel", "tunnel-binary", "tunnel-domain", "tunnel-name", "tunnel-hostname", "public-url", "web-addr", "web-host")
	noTunnel := fs.fs.Bool("no-tunnel", false, "")
	dryRun := fs.fs.Bool("dry-run", false, "")
	usage := fs.fs.Bool("claude-usage", false, "")
	noTelemetry := fs.fs.Bool("no-telemetry", false, "")
	web := fs.fs.Bool("web", false, "")
	poll := fs.fs.Duration("poll-interval", time.Second, "")
	lease := fs.fs.Duration("lease-duration", time.Minute, "")
	retention := fs.fs.Duration("retention-interval", 10*time.Minute, "")
	sweep := fs.fs.Duration("sweep-interval", 6*time.Hour, "")
	if pos, err := fs.parse(args); err != nil || len(pos) != 0 || *poll <= 0 || *lease <= 0 || *retention <= 0 || *sweep <= 0 {
		return ServeRequest{}, usageError(o.errOut, "invalid serve flags")
	}
	switch fs.get("tunnel") {
	case "", "ngrok", "cloudflared", "static":
	default:
		return ServeRequest{}, usageError(o.errOut, "--tunnel is ngrok, cloudflared or static")
	}
	r := ServeRequest{StateDir: o.stateDir(), ConfigPath: o.abs(fs.get("config")), Socket: o.abs(o.socket), ToolbeltDir: o.abs(fs.get("toolbelt-dir")), LifecycleLog: o.abs(fs.get("lifecycle-log")), PollInterval: *poll, LeaseDuration: *lease, RetentionInterval: *retention, SweepInterval: *sweep,
		WebhookAddr: fs.get("webhook-addr"), TunnelKind: fs.get("tunnel"), TunnelBinary: fs.get("tunnel-binary"), TunnelDomain: fs.get("tunnel-domain"),
		TunnelName: fs.get("tunnel-name"), TunnelHostname: fs.get("tunnel-hostname"), PublicURL: fs.get("public-url"), NoTunnel: *noTunnel, DryRun: *dryRun, ClaudeUsage: *usage}
	// --web-addr or --web-host alone also turn the view on.
	r.WebAddr, r.WebHost = fs.get("web-addr"), fs.get("web-host")
	r.Web = *web || r.WebAddr != "" || r.WebHost != ""
	r.WebSignIn = WebSignInPath(r.StateDir)
	r.NoTelemetry = *noTelemetry || optedOut(o.env.Getenv("OPEN_CONDUIT_NO_TELEMETRY")) || optedOut(o.env.Getenv("DO_NOT_TRACK"))
	if r.ConfigPath == "" {
		r.ConfigPath = o.abs(o.env.Getenv("OPEN_CONDUIT_CONFIG"))
	}
	if r.ConfigPath == "" {
		r.ConfigPath = defaultConfigPath(o.env)
	}
	if r.Socket == "" {
		r.Socket = o.abs(o.env.Getenv("OPEN_CONDUIT_SOCKET"))
	}
	return r, -1
}

// optedOut reads an opt-out variable: set to anything but "", "0" or
// "false" (the DO_NOT_TRACK convention).
func optedOut(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}
