package claudecode

import (
	"context"

	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// Checks() is doctor's claude-code: `--version` for every claude-code
// binding's binary on the host and, for a binding that sets usage, its
// login's subscription usage windows. A binding whose container provider
// runs its version probe in the binding's image (CheckBinding.Probed) runs
// the binary there, not on the host, so this one only names it; any other
// binding is looked for on the host. It reads only the configuration and the machine, so doctor
// runs it without a daemon too.
func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Offline: true, Run: x.diagnose}}
}

// usageRead reads the subscription usage windows of the login in a Claude
// config directory ("" for the default).
type usageRead func(ctx context.Context, configDir string) ([]UsageWindow, error)

func readUsage(ctx context.Context, configDir string) ([]UsageWindow, error) {
	return Usage(ctx, UsageOptions{ConfigDir: configDir, HTTP: &http.Client{Timeout: 10 * time.Second}})
}

// diagnose is claude-code.
func (x *Extension) diagnose(ctx context.Context, cfg protocol.CheckConfig) inproc.CheckResult {
	res := inproc.CheckResult{Name: "claude-code", Status: inproc.CheckOK, Code: "installed"}
	binaries := map[string][]string{}
	var contained []string
	logins := map[string]bool{}
	for _, scope := range cfg.Scopes {
		for _, b := range scope.Bindings {
			if b.Executor != Name {
				continue
			}
			c, err := ParseConfig(b.Config)
			if err != nil {
				continue
			}
			if b.Probed {
				contained = append(contained, scope.ID+"/"+b.Name)
			} else {
				binaries[c.Binary] = append(binaries[c.Binary], scope.ID+"/"+b.Name)
			}
			if c.Usage {
				logins[c.Env["CLAUDE_CONFIG_DIR"]] = true
			}
		}
	}
	if len(binaries) == 0 && len(contained) == 0 {
		res.Status, res.Code, res.Detail = inproc.CheckInfo, "not_configured", "no claude-code bindings"
		return res
	}
	if len(binaries) == 0 {
		res.Status, res.Code, res.Detail = inproc.CheckInfo, "in_container", "every claude-code binding runs claude in its container image, which the container provider's doctor entry tests"
	}
	names := make([]string, 0, len(binaries))
	for bin := range binaries {
		names = append(names, bin)
	}
	sort.Strings(names)
	for _, bin := range names {
		v, err := x.version(ctx, bin)
		if err != nil {
			res.Status, res.Code = inproc.CheckFail, "claude_missing"
			res.Items = append(res.Items, fmt.Sprintf("%s (bindings %s): not runnable", filepath.Base(bin), strings.Join(binaries[bin], ", ")))
			continue
		}
		res.Items = append(res.Items, fmt.Sprintf("%s: %s", filepath.Base(bin), v))
	}
	if len(contained) > 0 {
		res.Items = append(res.Items, fmt.Sprintf("in their container images, not on the host: %s", strings.Join(contained, ", ")))
	}
	if x.h.DryRun {
		res.Items = append(res.Items, "dry run: turns are stubs on a cheap model")
	}
	dirs := make([]string, 0, len(logins))
	for dir := range logins {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	read := x.usage
	if read == nil {
		read = readUsage
	}
	for _, dir := range dirs {
		label := "usage"
		if dir != "" {
			label = "usage (" + dir + ")"
		}
		cx, cancel := context.WithTimeout(ctx, 15*time.Second)
		windows, err := read(cx, dir)
		cancel()
		if err != nil {
			res.Items = append(res.Items, label+" unknown")
			continue
		}
		for _, w := range windows {
			if w.Used >= 90 && (res.Status == inproc.CheckOK || res.Status == inproc.CheckInfo) {
				res.Status, res.Code = inproc.CheckWarn, "usage_high"
			}
			res.Items = append(res.Items, fmt.Sprintf("%s %s: %.0f%% (resets %s)", label, w.Name, w.Used, w.ResetsAt.UTC().Format(time.RFC3339)))
		}
	}
	return res
}

// version is `<binary> --version`, or the test's stand-in.
func (x *Extension) version(ctx context.Context, bin string) (string, error) {
	if x.versionOf != nil {
		return x.versionOf(ctx, bin)
	}
	return Version(ctx, bin)
}

var _ inproc.Checker = (*Extension)(nil)
