// Package playbooks holds the instructions agents follow under Open Conduit
// (ported from Open Supervisor's internal/playbooks): how to work a GitHub
// issue in turns. Everything that touches GitHub goes
// through the toolbelt operations the github extension registers. Claude
// Code receives them as a plugin of skills (Prepare); a turn starts with
// /open-conduit:<playbook>.
package playbooks

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// PluginName is the plugin the playbooks load as, so the skill prefix.
const PluginName = "open-conduit"

// All returns every agent playbook by name ("work-issue").
func All() map[string]string {
	out := map[string]string{}
	entries, _ := files.ReadDir(".")
	for _, e := range entries {
		data, err := files.ReadFile(e.Name())
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(e.Name(), ".md")] = string(data)
	}
	return out
}

// Prepare writes the playbooks as a Claude Code plugin into dir:
// .claude-plugin/plugin.json and skills/<name>/SKILL.md (v1 Prepare). It
// rewrites every file each time, so an upgrade replaces old text, and
// removes skills that are no longer shipped.
func Prepare(dir string) error {
	return WritePlugin(dir, "Playbooks for agents run by Open Conduit", All())
}

// WritePlugin writes books as a Claude Code plugin named PluginName into dir,
// one skill each, and removes skills it no longer carries. `init --with
// claude` writes the setup playbook this way as its own plugin.
func WritePlugin(dir, description string, books map[string]string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		return err
	}
	manifest, _ := json.MarshalIndent(map[string]string{"name": PluginName, "description": description, "version": "1.0.0"}, "", "  ")
	if err := atomicWrite(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest); err != nil {
		return err
	}
	names := make([]string, 0, len(books))
	for name := range books {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		skill := filepath.Join(dir, "skills", name)
		if err := os.MkdirAll(skill, 0o755); err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(skill, "SKILL.md"), []byte(books[name])); err != nil {
			return err
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "skills"))
	for _, e := range entries {
		if _, ok := books[e.Name()]; !ok && e.IsDir() {
			_ = os.RemoveAll(filepath.Join(dir, "skills", e.Name()))
		}
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Body is a playbook without its frontmatter.
func Body(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return text
	}
	if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
		return strings.TrimLeft(text[4+i+5:], "\n")
	}
	return text
}

// Description is a playbook's frontmatter description.
func Description(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return ""
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return ""
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if v, ok := strings.CutPrefix(line, "description:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
