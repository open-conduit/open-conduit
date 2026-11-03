package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/extension"
)

var _ extension.InteractiveLauncher = (*Executor)(nil)

// LaunchNames: `init --with claude` picks this executor.
func (e *Executor) LaunchNames() []string { return []string{"claude"} }

// InteractiveLaunch starts Claude Code on the playbook as the one skill of
// its own plugin, written into the request's files directory: the session
// opens with /open-conduit:<name>, and each command the host allows becomes
// a Bash(<command>:*) entry of --allowedTools, so nothing else goes without
// asking.
func (e *Executor) InteractiveLaunch(_ context.Context, r extension.InteractiveLaunchRequest) (extension.InteractiveLaunchSpec, error) {
	if r.Name == "" || r.Name != filepath.Base(r.Name) || r.Name == "." || r.Name == ".." || r.FilesDir == "" {
		return extension.InteractiveLaunchSpec{}, fmt.Errorf("%w: a launch needs a playbook name and a files directory", extension.ErrInvalid)
	}
	if err := writeLaunchPlugin(r.FilesDir, r.Summary, r.Name, r.Playbook); err != nil {
		return extension.InteractiveLaunchSpec{}, err
	}
	program := r.Program
	if program == "" {
		program = "claude"
	}
	// The prompt first and the tools last: --allowedTools takes every
	// argument after it.
	argv := []string{program, "/" + PluginName + ":" + r.Name, "--plugin-dir", r.FilesDir}
	if len(r.Allow) > 0 {
		argv = append(argv, "--allowedTools")
		for _, c := range r.Allow {
			argv = append(argv, "Bash("+c+":*)")
		}
	}
	return extension.InteractiveLaunchSpec{Argv: argv}, nil
}

// writeLaunchPlugin writes dir as a Claude Code plugin named PluginName with
// the playbook as its only skill, replacing any skill an earlier launch
// left there.
func writeLaunchPlugin(dir, summary, name, text string) error {
	if summary == "" {
		summary = "The " + name + " playbook"
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		return err
	}
	manifest, _ := json.MarshalIndent(map[string]string{"name": PluginName, "description": summary, "version": "1.0.0"}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest, 0o644); err != nil {
		return err
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "skills"))
	for _, e := range entries {
		if e.Name() != name {
			_ = os.RemoveAll(filepath.Join(dir, "skills", e.Name()))
		}
	}
	skill := filepath.Join(dir, "skills", name)
	if err := os.MkdirAll(skill, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(text), 0o644)
}
