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
	fs := newFlags("serve", "config", "toolbelt-dir", "lifecycle-log")
	poll := fs.fs.Duration("poll-interval", time.Second, "")
	lease := fs.fs.Duration("lease-duration", time.Minute, "")
	retention := fs.fs.Duration("retention-interval", 10*time.Minute, "")
	if pos, err := fs.parse(args); err != nil || len(pos) != 0 || *poll <= 0 || *lease <= 0 || *retention <= 0 {
		return usageError(o.errOut, "invalid serve flags")
	}
	if o.env.Serve == nil {
		fmt.Fprintln(o.errOut, "open-conduit: serve is not available in this binary")
		return 1
	}
	r := ServeRequest{StateDir: o.stateDir(), ConfigPath: o.abs(fs.get("config")), Socket: o.abs(o.socket), ToolbeltDir: o.abs(fs.get("toolbelt-dir")), LifecycleLog: o.abs(fs.get("lifecycle-log")), PollInterval: *poll, LeaseDuration: *lease, RetentionInterval: *retention}
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
