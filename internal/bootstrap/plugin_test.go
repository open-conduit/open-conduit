package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writePlugin writes the plugin's manifest and one skill per playbook, and
// removes a skill it no longer carries.
func TestWritePlugin(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "skills", "sweep")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePlugin(dir, "Playbooks", map[string]string{"work-issue": "# Work an issue\n"}); err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest["name"] != pluginName {
		t.Fatalf("manifest: %s %v", data, err)
	}
	if skill, err := os.ReadFile(filepath.Join(dir, "skills", "work-issue", "SKILL.md")); err != nil || string(skill) != "# Work an issue\n" {
		t.Fatalf("skill: %q %v", skill, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a skill no longer shipped was kept")
	}
}
