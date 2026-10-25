package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reserved toolbelt variables are never supplied by a provider layer.
func reserved(name string) bool { return strings.HasPrefix(strings.ToUpper(name), "OS_") }

// within reports whether path is dir or below it, after resolving symlinks of
// both, so a tracked symlink cannot redirect a copy outside the workspace.
func within(dir, path string) bool {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return p == d || strings.HasPrefix(p, d+string(filepath.Separator))
}

// copyFiles copies the configured checkout files into a workspace on every
// acquisition, so a reused workspace picks up keys added since. A destination
// that is a symlink, or whose parent leaves the workspace, is skipped.
func copyFiles(log io.Writer, checkout, path string, names []string) {
	for _, name := range names {
		from, to := filepath.Join(checkout, name), filepath.Join(path, name)
		data, err := os.ReadFile(from)
		if err != nil {
			continue // Absent in the checkout: nothing to copy.
		}
		info, err := os.Stat(from)
		if err != nil {
			continue
		}
		if fi, err := os.Lstat(to); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			logf(log, "not copying %s: the workspace has a symlink there", name)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(to), 0o755); err != nil || !within(path, filepath.Dir(to)) {
			logf(log, "not copying %s: its directory is outside the workspace", name)
			continue
		}
		if old, err := os.ReadFile(to); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err = os.WriteFile(to, data, info.Mode().Perm()); err == nil {
			_ = os.Chmod(to, info.Mode().Perm())
			logf(log, "copied %s", name)
		}
	}
}

// readDotEnv parses KEY=value lines, ignoring comments, blanks, malformed
// lines, invalid names and reserved toolbelt variables. One pair of matching
// surrounding quotes is removed and an `export ` prefix is accepted.
func readDotEnv(file string) map[string]string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !envName.MatchString(k) || reserved(k) || strings.ContainsRune(v, 0) {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// environment is the provider layer (B05): the opted-in checkout .env, then the
// signing policy, which a .env cannot undo.
func environment(checkout string, dotenv, sign bool) map[string]string {
	env := map[string]string{}
	if dotenv {
		for k, v := range readDotEnv(filepath.Join(checkout, ".env")) {
			env[k] = v
		}
	}
	if !sign {
		for k := range env {
			if strings.HasPrefix(k, "GIT_CONFIG_") {
				delete(env, k)
			}
		}
		env["GIT_CONFIG_COUNT"] = "2"
		env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"] = "commit.gpgsign", "false"
		env["GIT_CONFIG_KEY_1"], env["GIT_CONFIG_VALUE_1"] = "tag.gpgsign", "false"
	}
	return env
}

const setupStamp = "open-conduit-setup"

// runSetup runs the setup command when what it depends on changed since it
// last succeeded in this workspace. With watched files configured and none of
// them present (a pnpm install in a repo without pnpm-lock.yaml) it is skipped.
// It reports whether the command ran.
func (p *Provider) runSetup(ctx context.Context, log io.Writer, cfg Config, path string, env map[string]string) (bool, error) {
	if cfg.Setup == "" {
		return false, nil
	}
	h := sha256.New()
	h.Write([]byte(cfg.Setup))
	found := false
	for _, name := range cfg.SetupWhen {
		if data, err := os.ReadFile(filepath.Join(path, name)); err == nil {
			h.Write([]byte{0})
			h.Write([]byte(name))
			h.Write([]byte{0})
			h.Write(data)
			found = true
		}
	}
	if len(cfg.SetupWhen) > 0 && !found {
		logf(log, "skipping setup: none of %s is in the workspace", strings.Join(cfg.SetupWhen, ", "))
		return false, nil
	}
	sum := hex.EncodeToString(h.Sum(nil))
	stamp := filepath.Join(p.gitDir(ctx, path), setupStamp)
	if old, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(old)) == sum {
		return false, nil
	}
	logf(log, "running setup")
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.SetupTimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", cfg.Setup)
	cmd.Dir = path
	cmd.Env = p.setupEnv(env)
	if log == nil {
		log = io.Discard
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := runGroup(ctx, cmd); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return true, fmt.Errorf("setup timed out after %ds", cfg.SetupTimeoutSeconds)
		}
		return true, fmt.Errorf("setup command failed: %s", firstLine(err.Error()))
	}
	return true, os.WriteFile(stamp, []byte(sum+"\n"), 0o644)
}

// setupEnv is the daemon environment without toolbelt variables, plus the
// provider layer. Setup is trusted operator configuration, like v1.
func (p *Provider) setupEnv(layer map[string]string) []string {
	vars := map[string]string{}
	for _, kv := range p.o.Environ() {
		if name, value, ok := strings.Cut(kv, "="); ok && !reserved(name) {
			vars[name] = value
		}
	}
	for k, v := range layer {
		vars[k] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	return out
}

func logf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, "open-conduit: workspace: "+format+"\n", args...)
	}
}
