package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

// The background service (`service install|uninstall|start|stop|status`)
// runs `serve` as a per-user launchd LaunchAgent on macOS or a systemd user
// unit on Linux, in the owner's session so the OS keyring and the gh and
// Claude logins work. The unit runs this binary with the paths and serve
// flags resolved at install time and the PATH the tools it starts need, and
// both its output streams go to <state>/logs/service.log.

const (
	launchdLabel = "com.open-conduit.daemon"
	systemdUnit  = "open-conduit.service"
)

// ServiceHost is what the service commands need from the machine; the
// open-conduit entrypoint supplies SystemServiceHost, tests a fake.
type ServiceHost struct {
	GOOS string
	// Executable is the open-conduit binary the unit runs.
	Executable string
	UID        int
	LookPath   func(string) (string, error)
	// Run runs launchctl or systemctl and returns its combined output.
	Run func(name string, args ...string) (string, error)
}

// SystemServiceHost is the real machine.
func SystemServiceHost() *ServiceHost {
	self, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
	}
	// A Homebrew install runs from a versioned Caskroom path that an upgrade
	// removes; the link on PATH keeps pointing at the current version.
	if found, err := exec.LookPath("open-conduit"); err == nil && strings.Contains(self, "/Caskroom/") {
		if f, err := filepath.EvalSymlinks(found); err == nil && f == self {
			self = found
		}
	}
	return &ServiceHost{GOOS: goruntime.GOOS, Executable: self, UID: os.Getuid(), LookPath: exec.LookPath,
		Run: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		}}
}

// serviceState is what status and doctor report.
type serviceState struct {
	Installed, Running bool
	PID                int
	Path, Log          string
}

const serviceHelp = `Usage: open-conduit service install [--no-start] [serve flags] | uninstall | start | stop | status

Runs the daemon in the background as a per-user service: a launchd LaunchAgent
on macOS (~/Library/LaunchAgents/` + launchdLabel + `.plist), a systemd user unit on
Linux (~/.config/systemd/user/` + systemdUnit + `). It starts at login and restarts
if the daemon exits with an error.

install takes the same flags as serve (for example --webhook-addr 127.0.0.1:8787
--tunnel cloudflared) and the global --data-dir and --socket, and records the
config path, the state directory and a PATH that finds claude, git, gh and the
tunnel binary. Run it again to change the flags or after installing a tool the
daemon needs. Output goes to <state>/logs/service.log.
`

func (o *operator) service(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(o.out, serviceHelp)
		return 0
	}
	h := o.env.Service
	if h == nil {
		fmt.Fprintln(o.errOut, "open-conduit: service is not available in this binary")
		return 1
	}
	if h.GOOS != "darwin" && h.GOOS != "linux" {
		fmt.Fprintf(o.errOut, "open-conduit: service supports macOS (launchd) and Linux (systemd); on %s run open-conduit serve under your own supervisor\n", h.GOOS)
		return 1
	}
	verb, rest := args[0], args[1:]
	if verb != "install" && len(rest) != 0 {
		return usageError(o.errOut, "service "+verb+" takes no arguments")
	}
	var err error
	switch verb {
	case "install":
		return o.serviceInstall(h, rest)
	case "uninstall":
		err = o.serviceUninstall(h)
	case "start":
		err = o.serviceStart(h)
	case "stop":
		err = o.serviceStop(h)
	case "status":
		return o.serviceStatus(h)
	default:
		return usageError(o.errOut, "expected service install|uninstall|start|stop|status")
	}
	if err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: service %s: %s\n", verb, err)
		return 1
	}
	return 0
}

func (o *operator) serviceLog() string { return filepath.Join(o.stateDir(), "logs", "service.log") }

func (o *operator) unitPath(h *ServiceHost) string {
	if h.GOOS == "darwin" {
		return filepath.Join(o.env.Getenv("HOME"), "Library", "LaunchAgents", launchdLabel+".plist")
	}
	dir := o.env.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(o.env.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "systemd", "user", systemdUnit)
}

func (o *operator) serviceInstall(h *ServiceHost, args []string) int {
	noStart := false
	var serveArgs []string
	for _, a := range args {
		if a == "--no-start" || a == "-no-start" {
			noStart = true
		} else {
			serveArgs = append(serveArgs, a)
		}
	}
	r, code := o.parseServe(serveArgs)
	if code >= 0 {
		return code
	}
	argv := []string{h.Executable, "--data-dir", r.StateDir}
	if r.Socket != "" {
		argv = append(argv, "--socket", r.Socket)
	}
	argv = append(argv, "serve", "--config", r.ConfigPath)
	argv = append(argv, withoutFlag(serveArgs, "config")...)
	path, missing := o.servicePath(h, r)
	unit, log := o.unitPath(h), o.serviceLog()
	var content string
	if h.GOOS == "darwin" {
		content = launchdPlist(argv, path, log, o.env.Getenv("HOME"))
	} else {
		content = systemdUnitFile(argv, path, log)
	}
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: service install: %s\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: service install: %s\n", err)
		return 1
	}
	// A loaded unit keeps its old definition until it is reloaded.
	before, _ := o.serviceQuery(h)
	if before.Installed && before.Running {
		if err := o.serviceStop(h); err != nil {
			fmt.Fprintf(o.errOut, "open-conduit: service install: %s\n", err)
			return 1
		}
	}
	if err := os.WriteFile(unit, []byte(content), 0o644); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: service install: %s\n", err)
		return 1
	}
	if h.GOOS == "linux" {
		if err := systemctl(h, "daemon-reload"); err != nil {
			fmt.Fprintf(o.errOut, "open-conduit: service install: %s\n", err)
			return 1
		}
		if err := systemctl(h, "enable", systemdUnit); err != nil {
			fmt.Fprintf(o.errOut, "open-conduit: service install: %s\n", err)
			return 1
		}
	}
	fmt.Fprintf(o.out, "installed %s\n  runs: %s\n  log:  %s\n", unit, strings.Join(argv, " "), log)
	for _, m := range missing {
		fmt.Fprintf(o.out, "  warning: %s is not on PATH; install it and run service install again\n", m)
	}
	if noStart {
		fmt.Fprintln(o.out, "start it with open-conduit service start")
		return 0
	}
	if err := o.serviceStart(h); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: service start: %s\n", err)
		return 1
	}
	return 0
}

// servicePath is the PATH the unit runs with: the installer's own, then the
// directories of the tools the daemon starts, then the system defaults.
// It also names the required tools it could not find.
func (o *operator) servicePath(h *ServiceHost, r ServeRequest) (string, []string) {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d != "" && filepath.IsAbs(d) && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, d := range filepath.SplitList(o.env.Getenv("PATH")) {
		add(d)
	}
	tools := []string{"claude", "git", "gh"}
	required := map[string]bool{"claude": true, "git": true}
	kind := r.TunnelKind
	if kind == "" && r.TunnelDomain == "" && r.TunnelName == "" && r.TunnelHostname == "" && r.PublicURL == "" {
		kind = storedTunnel(r.ConfigPath)
	}
	if kind == "ngrok" || kind == "cloudflared" {
		tools = append(tools, kind)
		required[kind] = r.TunnelBinary == ""
	}
	var missing []string
	for _, t := range tools {
		if p, err := h.LookPath(t); err == nil {
			add(filepath.Dir(p))
		} else if required[t] {
			missing = append(missing, t)
		}
	}
	if r.TunnelBinary != "" && filepath.IsAbs(r.TunnelBinary) {
		add(filepath.Dir(r.TunnelBinary))
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		add(d)
	}
	return strings.Join(dirs, string(os.PathListSeparator)), missing
}

// storedTunnel is the config's settings.webhooks.tunnel, which serve uses
// when no tunnel flag overrides it; "" when the file can't be read.
func storedTunnel(path string) string {
	var doc struct {
		Settings struct {
			Webhooks struct {
				Tunnel string `json:"tunnel"`
			} `json:"webhooks"`
		} `json:"settings"`
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &doc)
	}
	return doc.Settings.Webhooks.Tunnel
}

// withoutFlag drops --name and its value (both spellings) from args.
func withoutFlag(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		if a == name && strings.HasPrefix(args[i], "-") {
			i++
			continue
		}
		if strings.HasPrefix(a, name+"=") && strings.HasPrefix(args[i], "-") {
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func (o *operator) serviceUninstall(h *ServiceHost) error {
	unit := o.unitPath(h)
	if _, err := os.Stat(unit); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(o.out, "no service installed")
		return nil
	}
	if err := o.serviceStop(h); err != nil {
		return err
	}
	if h.GOOS == "linux" {
		if err := systemctl(h, "disable", systemdUnit); err != nil {
			return err
		}
	}
	if err := os.Remove(unit); err != nil {
		return err
	}
	if h.GOOS == "linux" {
		if err := systemctl(h, "daemon-reload"); err != nil {
			return err
		}
	}
	fmt.Fprintf(o.out, "removed %s (the log stays at %s)\n", unit, o.serviceLog())
	return nil
}

func (o *operator) serviceStart(h *ServiceHost) error {
	s, err := o.serviceQuery(h)
	if err != nil {
		return err
	}
	if !s.Installed {
		return errors.New("no service installed; run open-conduit service install")
	}
	if s.Running {
		fmt.Fprintf(o.out, "already running (pid %d)\n", s.PID)
		return nil
	}
	if h.GOOS == "darwin" {
		if out, err := h.Run("launchctl", "bootstrap", o.launchdDomain(h), s.Path); err != nil {
			// Already loaded but not running: start it in place.
			if _, kerr := h.Run("launchctl", "kickstart", o.launchdDomain(h)+"/"+launchdLabel); kerr != nil {
				return fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(out))
			}
		}
	} else if err := systemctl(h, "start", systemdUnit); err != nil {
		return err
	}
	fmt.Fprintf(o.out, "started; follow it with tail -f %s, check it with open-conduit doctor\n", s.Log)
	return nil
}

func (o *operator) serviceStop(h *ServiceHost) error {
	s, err := o.serviceQuery(h)
	if err != nil {
		return err
	}
	if !s.Installed {
		return errors.New("no service installed")
	}
	if h.GOOS == "darwin" {
		// Unloading stops it until the next start or login.
		if o.launchdLoaded(h) {
			if out, err := h.Run("launchctl", "bootout", o.launchdDomain(h)+"/"+launchdLabel); err != nil {
				return fmt.Errorf("launchctl bootout: %s", strings.TrimSpace(out))
			}
		}
	} else if err := systemctl(h, "stop", systemdUnit); err != nil {
		return err
	}
	fmt.Fprintln(o.out, "stopped")
	return nil
}

func (o *operator) serviceStatus(h *ServiceHost) int {
	s, err := o.serviceQuery(h)
	if err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: service status: %s\n", err)
		return 1
	}
	if !s.Installed {
		fmt.Fprintln(o.out, "not installed; open-conduit service install runs the daemon in the background")
		return 3
	}
	state := "stopped"
	if s.Running {
		state = "running (pid " + strconv.Itoa(s.PID) + ")"
	}
	fmt.Fprintf(o.out, "installed: %s\nstate:     %s\nlog:       %s\n", s.Path, state, s.Log)
	if !s.Running {
		return 3
	}
	return 0
}

func (o *operator) launchdDomain(h *ServiceHost) string { return "gui/" + strconv.Itoa(h.UID) }

func (o *operator) launchdLoaded(h *ServiceHost) bool {
	_, err := h.Run("launchctl", "print", o.launchdDomain(h)+"/"+launchdLabel)
	return err == nil
}

// serviceQuery reports whether the unit file exists and the daemon runs.
func (o *operator) serviceQuery(h *ServiceHost) (serviceState, error) {
	s := serviceState{Path: o.unitPath(h), Log: o.serviceLog()}
	if _, err := os.Stat(s.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	s.Installed = true
	if h.GOOS == "darwin" {
		out, err := h.Run("launchctl", "print", o.launchdDomain(h)+"/"+launchdLabel)
		if err != nil {
			return s, nil // not loaded
		}
		for _, line := range strings.Split(out, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
			switch {
			case ok && k == "state":
				s.Running = v == "running"
			case ok && k == "pid":
				s.PID, _ = strconv.Atoi(v)
			}
		}
		return s, nil
	}
	out, err := h.Run("systemctl", "--user", "show", systemdUnit, "--property=ActiveState,MainPID")
	if err != nil {
		return s, fmt.Errorf("systemctl --user show: %s", strings.TrimSpace(out))
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ActiveState":
			s.Running = v == "active" || v == "activating" || v == "reloading"
		case "MainPID":
			s.PID, _ = strconv.Atoi(v)
		}
	}
	return s, nil
}

func systemctl(h *ServiceHost, args ...string) error {
	if out, err := h.Run("systemctl", append([]string{"--user"}, args...)...); err != nil {
		return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), strings.TrimSpace(out))
	}
	return nil
}

// serviceCheck is doctor's report on the background service; ok is false
// when the platform has none.
func (o *operator) serviceCheck() (c client.Check, ok bool) {
	h := o.env.Service
	if h == nil || (h.GOOS != "darwin" && h.GOOS != "linux") {
		return c, false
	}
	c.Name = "service"
	s, err := o.serviceQuery(h)
	switch {
	case err != nil:
		c.Status, c.Code, c.Detail = "warn", "unknown", err.Error()
	case !s.Installed:
		c.Status, c.Code, c.Detail = "info", "not_installed", "no background service; open-conduit service install runs the daemon in the background"
	case s.Running:
		c.Status, c.Code, c.Detail = "ok", "running", fmt.Sprintf("running (pid %d) from %s; log %s", s.PID, s.Path, s.Log)
	default:
		c.Status, c.Code, c.Detail = "warn", "stopped", fmt.Sprintf("installed at %s but not running; start it with open-conduit service start and see %s", s.Path, s.Log)
	}
	return c, true
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

func launchdPlist(argv []string, path, log, home string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by open-conduit service install; run it again rather than editing this file. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range argv {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>` + xmlEscape(path) + `</string>
  </dict>
  <key>WorkingDirectory</key>
  <string>` + xmlEscape(home) + `</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>` + xmlEscape(log) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// systemdQuote quotes one word for a unit file: specifiers (%) and
// variables ($) are taken literally.
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s)
	return `"` + s + `"`
}

func systemdUnitFile(argv []string, path, log string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = systemdQuote(a)
	}
	log = strings.ReplaceAll(log, "%", "%%")
	return `# Written by open-conduit service install; run it again rather than editing this file.
[Unit]
Description=Open Conduit daemon
Documentation=https://github.com/open-conduit/open-conduit

[Service]
Type=simple
ExecStart=` + strings.Join(words, " ") + `
Environment=` + systemdQuote("PATH="+path) + `
WorkingDirectory=%h
Restart=on-failure
RestartSec=10
StandardOutput=append:` + log + `
StandardError=append:` + log + `

[Install]
WantedBy=default.target
`
}
