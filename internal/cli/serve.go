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
	// UpgradeBundlesKept and UpgradeBundlesMaxAge bound the bundles taken
	// before a start raises the stored format (3, and no age limit, by
	// default). NoUpgradeBundle starts without taking one, deliberately.
	UpgradeBundlesKept   int
	UpgradeBundlesMaxAge time.Duration
	NoUpgradeBundle      bool
	// WebhookAddr enables the webhook listener; TunnelKind (a tunnel type
	// a package declares) with its opaque TunnelConfig, or PublicURL,
	// expose it. Empty values fall back to the config's settings.webhooks.
	WebhookAddr, TunnelKind, PublicURL string
	TunnelConfig                       json.RawMessage
	// ProbeAddr enables the probe listener (/livez and /readyz, no
	// credential) on a TCP address (--probe-addr); off when empty.
	ProbeAddr        string
	NoTunnel, DryRun bool
	// NoTelemetry turns off the anonymous usage reports: --no-telemetry,
	// or OPEN_CONDUIT_NO_TELEMETRY or DO_NOT_TRACK set in the environment.
	NoTelemetry bool
	// NoUpdates leaves installing releases out, for a binary managed some
	// other way (a package manager, an image, a fleet): the daemon answers
	// updates_off and the web view has no update page (--no-updates).
	NoUpdates bool
	// Web serves the web view from the daemon's process on WebAddr, named
	// WebHost (the web command's defaults when empty), signed in through
	// WebSignIn. WebSubjectHeader makes it act for the person a sign-in
	// proxy passes the credential of in that header, with the credential
	// in WebTokenFile, which may act for operators, in place of the
	// owner's (web.Options.SubjectHeader).
	Web                         bool
	WebAddr, WebHost, WebSignIn string
	// ManagementAddr opens the management listener on a TCP address, with
	// its TLS certificate and key, an optional client CA for mutual TLS,
	// or, on a loopback address only, ManagementTrustedProxy for a local
	// proxy that terminates TLS. Off unless ManagementAddr is set.
	ManagementAddr, ManagementTLSCert, ManagementTLSKey, ManagementClientCA string
	ManagementTrustedProxy                                                  bool
	WebSubjectHeader                                                        string
	WebTokenFile                                                            string
	// Restore is a bundle to restore into StateDir in place before
	// serving, once (serve --restore): a container restores from a mounted
	// bundle with no shell and no separate job.
	Restore string
	// RestoreArtifacts and RestoreArtifactsLocation are --restore's
	// artifact store, as backup restore's --artifacts and
	// --artifacts-location.
	RestoreArtifacts, RestoreArtifactsLocation string
	// RestoreTrustKeys and RestoreAcceptUnsigned are --restore's
	// --restore-trust-key and --restore-accept-unsigned.
	RestoreTrustKeys      []string
	RestoreAcceptUnsigned bool
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
	if r.Restore != "" && !o.restoreBeforeServe(r) {
		return 1
	}
	return o.env.Serve(r)
}

// restoreBeforeServe restores serve --restore's bundle into the state
// directory in place, under the same lock the daemon then takes, and
// reports what it did on stderr. A failed restore, or a restored directory
// whose state check finds problems, stops the start.
func (o *operator) restoreBeforeServe(r ServeRequest) bool {
	if o.env.State == nil || o.env.State.RestoreInPlace == nil {
		fmt.Fprintln(o.errOut, "open-conduit: serve --restore: offline state commands are not available in this binary")
		return false
	}
	cx, cancel := offlineCtx()
	defer cancel()
	info, err := o.env.State.RestoreInPlace(cx, r.Restore, r.StateDir, RestoreOptions{ConfigPath: r.ConfigPath, ArtifactBackend: r.RestoreArtifacts, ArtifactLocation: r.RestoreArtifactsLocation, Once: true, TrustKeys: r.RestoreTrustKeys, AcceptUnsigned: r.RestoreAcceptUnsigned})
	if err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: serve --restore: %s\n", err)
		return false
	}
	report, _ := json.Marshal(info)
	fmt.Fprintf(o.errOut, "open-conduit: serve --restore: %s\n", report)
	if p := info.InPlace; p != nil && p.Check != nil && !p.Check.OK {
		fmt.Fprintln(o.errOut, "open-conduit: serve --restore: the restored state directory has problems (in_place.check); not serving it")
		return false
	}
	return true
}

// parseServe resolves serve's flags (shared with service install); code is
// the exit code when they are invalid, else -1.
func (o *operator) parseServe(args []string) (ServeRequest, int) {
	fs := newFlags("serve", "config", "toolbelt-dir", "lifecycle-log", "backup-dir", "restore", "restore-artifacts", "restore-artifacts-location", "restore-trust-key", "webhook-addr", "tunnel", "tunnel-config", "public-url", "web-addr", "web-host", "management-addr", "management-tls-cert", "management-tls-key", "management-client-ca", "web-subject-header", "web-token-file", "probe-addr")
	trustedProxy := fs.fs.Bool("management-trusted-proxy", false, "")
	noTunnel := fs.fs.Bool("no-tunnel", false, "")
	dryRun := fs.fs.Bool("dry-run", false, "")
	// --claude-usage is retired: a claude-code binding sets usage.
	usage := fs.fs.Bool("claude-usage", false, "")
	noTelemetry := fs.fs.Bool("no-telemetry", false, "")
	noUpdates := fs.fs.Bool("no-updates", false, "")
	web := fs.fs.Bool("web", false, "")
	poll := fs.fs.Duration("poll-interval", time.Second, "")
	lease := fs.fs.Duration("lease-duration", time.Minute, "")
	retention := fs.fs.Duration("retention-interval", 10*time.Minute, "")
	sweep := fs.fs.Duration("sweep-interval", 6*time.Hour, "")
	call := fs.fs.Duration("call-timeout", 5*time.Second, "")
	restoreAcceptUnsigned := fs.fs.Bool("restore-accept-unsigned", false, "")
	backups := fs.fs.Duration("backup-interval", 24*time.Hour, "")
	kept := fs.fs.Int("backups-kept", 7, "")
	upgradeKept := fs.fs.Int("upgrade-bundles-kept", 3, "")
	upgradeAge := fs.fs.Duration("upgrade-bundles-max-age", 0, "")
	noUpgradeBundle := fs.fs.Bool("no-upgrade-bundle", false, "")
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if msg, ok := retiredTunnelFlags[name]; ok && strings.HasPrefix(a, "-") {
			return ServeRequest{}, usageError(o.errOut, "--"+name+" is retired: "+msg)
		}
	}
	if pos, err := fs.parse(args); err != nil || len(pos) != 0 || *poll <= 0 || *lease <= 0 || *retention <= 0 || *sweep <= 0 || *call <= 0 || *backups < 0 || *kept < 1 || *upgradeKept < 1 || *upgradeAge < 0 {
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
		WebhookAddr: fs.get("webhook-addr"), TunnelKind: kind, TunnelConfig: tunnelConfig, PublicURL: fs.get("public-url"), ProbeAddr: fs.get("probe-addr"), NoTunnel: *noTunnel, DryRun: *dryRun}
	if *usage {
		fmt.Fprintln(o.errOut, `open-conduit: --claude-usage is retired and does nothing: set "usage": true in a claude-code binding's config`)
	}
	// --backup-interval 0 turns the scheduled backups off.
	r.BackupInterval, r.BackupDir, r.BackupsKept = *backups, o.abs(fs.get("backup-dir")), *kept
	if r.BackupInterval == 0 {
		r.BackupInterval = -1
	}
	r.UpgradeBundlesKept, r.UpgradeBundlesMaxAge, r.NoUpgradeBundle = *upgradeKept, *upgradeAge, *noUpgradeBundle
	// --web-addr or --web-host alone also turn the view on.
	r.WebAddr, r.WebHost = fs.get("web-addr"), fs.get("web-host")
	r.WebSubjectHeader, r.WebTokenFile = fs.get("web-subject-header"), o.abs(fs.get("web-token-file"))
	if (r.WebSubjectHeader == "") != (r.WebTokenFile == "") {
		return ServeRequest{}, usageError(o.errOut, "--web-subject-header and --web-token-file go together: the view acts for the person the proxy passes, with a credential issued --act-for operator")
	}
	r.Web = *web || r.WebAddr != "" || r.WebHost != "" || r.WebSubjectHeader != ""
	r.ManagementAddr, r.ManagementTrustedProxy = fs.get("management-addr"), *trustedProxy
	r.ManagementTLSCert, r.ManagementTLSKey, r.ManagementClientCA = o.abs(fs.get("management-tls-cert")), o.abs(fs.get("management-tls-key")), o.abs(fs.get("management-client-ca"))
	r.WebSignIn = WebSignInPath(r.StateDir)
	r.NoUpdates = *noUpdates
	r.Restore = o.abs(fs.get("restore"))
	r.RestoreArtifacts, r.RestoreArtifactsLocation = fs.get("restore-artifacts"), fs.get("restore-artifacts-location")
	if r.Restore == "" && (r.RestoreArtifacts != "" || r.RestoreArtifactsLocation != "") {
		return ServeRequest{}, usageError(o.errOut, "--restore-artifacts and --restore-artifacts-location go with --restore BUNDLE")
	}
	r.RestoreTrustKeys, r.RestoreAcceptUnsigned = splitKeys(fs.get("restore-trust-key")), *restoreAcceptUnsigned
	if r.Restore == "" && (len(r.RestoreTrustKeys) > 0 || r.RestoreAcceptUnsigned) {
		return ServeRequest{}, usageError(o.errOut, "--restore-trust-key and --restore-accept-unsigned go with --restore BUNDLE")
	}
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
