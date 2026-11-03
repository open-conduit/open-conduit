package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
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
	// CallTimeout bounds a route's decision and a container runtime's
	// list and inspect (default 5s).
	CallTimeout time.Duration
	// BackupInterval paces the scheduled backups into BackupDir, keeping
	// BackupsKept (24h, StateDir/backups and 7 by default); negative turns
	// them off.
	BackupInterval time.Duration
	BackupDir      string
	BackupsKept    int
	// WebhookAddr enables the webhook listener; TunnelKind (a tunnel type
	// a package declares) with its opaque TunnelConfig, or PublicURL,
	// expose it. Empty values fall back to the config's settings.webhooks.
	WebhookAddr, TunnelKind, PublicURL string
	TunnelConfig                       json.RawMessage
	NoTunnel, DryRun                   bool
	// NoTelemetry turns off the anonymous usage reports: --no-telemetry,
	// or OPEN_CONDUIT_NO_TELEMETRY or DO_NOT_TRACK set in the environment.
	NoTelemetry bool
	// Web serves the web view from the daemon's process on WebAddr, named
	// WebHost (the web command's defaults when empty), signed in through
	// WebSignIn.
	Web                         bool
	WebAddr, WebHost, WebSignIn string
}

// tunnelType is a tunnel type's name, as a manifest declares it.
var tunnelType = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// retiredTunnelFlags are the per-type tunnel flags serve took before a
// tunnel's config became its package's, each with its replacement.
var retiredTunnelFlags = map[string]string{
	"tunnel-domain":   `use --tunnel ngrok --tunnel-config '{"domain": "…"}'`,
	"tunnel-name":     `use --tunnel cloudflared --tunnel-config '{"name": "…", "hostname": "…"}'`,
	"tunnel-hostname": `use --tunnel cloudflared --tunnel-config '{"name": "…", "hostname": "…"}'`,
	"tunnel-binary":   `put the executable in the tunnel's config: --tunnel-config '{"binary": "…"}'`,
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
	fs := newFlags("serve", "config", "toolbelt-dir", "lifecycle-log", "backup-dir", "webhook-addr", "tunnel", "tunnel-config", "public-url", "web-addr", "web-host")
	noTunnel := fs.fs.Bool("no-tunnel", false, "")
	dryRun := fs.fs.Bool("dry-run", false, "")
	// --claude-usage is retired: a claude-code binding sets usage.
	usage := fs.fs.Bool("claude-usage", false, "")
	noTelemetry := fs.fs.Bool("no-telemetry", false, "")
	web := fs.fs.Bool("web", false, "")
	poll := fs.fs.Duration("poll-interval", time.Second, "")
	lease := fs.fs.Duration("lease-duration", time.Minute, "")
	retention := fs.fs.Duration("retention-interval", 10*time.Minute, "")
	sweep := fs.fs.Duration("sweep-interval", 6*time.Hour, "")
	call := fs.fs.Duration("call-timeout", 5*time.Second, "")
	backups := fs.fs.Duration("backup-interval", 24*time.Hour, "")
	kept := fs.fs.Int("backups-kept", 7, "")
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if msg, ok := retiredTunnelFlags[name]; ok && strings.HasPrefix(a, "-") {
			return ServeRequest{}, usageError(o.errOut, "--"+name+" is retired: "+msg)
		}
	}
	if pos, err := fs.parse(args); err != nil || len(pos) != 0 || *poll <= 0 || *lease <= 0 || *retention <= 0 || *sweep <= 0 || *call <= 0 || *backups < 0 || *kept < 1 {
		return ServeRequest{}, usageError(o.errOut, "invalid serve flags")
	}
	kind, rawConfig := fs.get("tunnel"), fs.get("tunnel-config")
	var tunnelConfig json.RawMessage
	switch {
	case kind == "static":
		return ServeRequest{}, usageError(o.errOut, "--tunnel static is retired: give --public-url alone for a URL that already reaches the listener")
	case kind != "" && !tunnelType.MatchString(kind):
		return ServeRequest{}, usageError(o.errOut, "--tunnel is a tunnel type an installed or bundled package provides, such as ngrok or cloudflared")
	case rawConfig != "" && kind == "":
		return ServeRequest{}, usageError(o.errOut, "--tunnel-config is the config of the --tunnel type: give --tunnel too")
	case kind != "" && fs.get("public-url") != "":
		return ServeRequest{}, usageError(o.errOut, "--public-url is a static URL the operator already routes to the listener; give it or --tunnel, not both")
	case rawConfig != "":
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(rawConfig), &obj) != nil || obj == nil {
			return ServeRequest{}, usageError(o.errOut, `--tunnel-config is a JSON object, for example '{"domain": "hooks.example.ngrok.app"}'`)
		}
		tunnelConfig = json.RawMessage(rawConfig)
	}
	r := ServeRequest{StateDir: o.stateDir(), ConfigPath: o.abs(fs.get("config")), Socket: o.abs(o.socket), ToolbeltDir: o.abs(fs.get("toolbelt-dir")), LifecycleLog: o.abs(fs.get("lifecycle-log")), PollInterval: *poll, LeaseDuration: *lease, RetentionInterval: *retention, SweepInterval: *sweep, CallTimeout: *call,
		WebhookAddr: fs.get("webhook-addr"), TunnelKind: kind, TunnelConfig: tunnelConfig, PublicURL: fs.get("public-url"), NoTunnel: *noTunnel, DryRun: *dryRun}
	if *usage {
		fmt.Fprintln(o.errOut, `open-conduit: --claude-usage is retired and does nothing: set "usage": true in a claude-code binding's config`)
	}
	// --backup-interval 0 turns the scheduled backups off.
	r.BackupInterval, r.BackupDir, r.BackupsKept = *backups, o.abs(fs.get("backup-dir")), *kept
	if r.BackupInterval == 0 {
		r.BackupInterval = -1
	}
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
