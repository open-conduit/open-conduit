package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// pluginName names the Claude Code plugin the host writes the playbooks
// into; a turn starts with /open-conduit:<playbook>.
const pluginName = "open-conduit"

// writePlugin writes books as a Claude Code plugin named pluginName into
// dir (.claude-plugin/plugin.json and skills/<name>/SKILL.md), one skill
// each. It rewrites every file each time, so an upgrade replaces old text,
// and removes skills it no longer carries. The playbooks every extension
// offers are written this way.
func writePlugin(dir, description string, books map[string]string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		return err
	}
	manifest, _ := json.MarshalIndent(map[string]string{"name": pluginName, "description": description, "version": "1.0.0"}, "", "  ")
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
