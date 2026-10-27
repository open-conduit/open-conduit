package playbooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	gh "github.com/open-conduit/open-conduit/extensions/github"
)

var opName = regexp.MustCompile("[`\"](github\\.[a-z.]+)[`\"]")

func registered() map[string]bool {
	ops := map[string]bool{}
	for _, op := range gh.Operations() {
		ops[op] = true
	}
	return ops
}

// Every toolbelt operation a playbook tells the agent to call is one the
// github extension registers, and v1's agent commands are gone.
func TestPlaybooksNameRegisteredOperations(t *testing.T) {
	ops := registered()
	books := All()
	books["setup"] = Setup("open-conduit")
	for name, text := range books {
		found := 0
		for _, m := range opName.FindAllStringSubmatch(text, -1) {
			found++
			if !ops[m[1]] {
				t.Errorf("%s names %s, which is not registered", name, m[1])
			}
		}
		if found == 0 {
			t.Errorf("%s names no operation", name)
		}
		for _, legacy := range []string{"$OPEN_SUPERVISOR", "agent worktree", "OPEN_SUPERVISOR_REMOTE", "open-supervisor:", ".local/"} {
			if strings.Contains(text, legacy) {
				t.Errorf("%s still says %q", name, legacy)
			}
		}
	}
	// The agent-facing tools the work-issue playbook relies on (v1 parity).
	for _, op := range []string{gh.OpIssueView, gh.OpIssueComment, gh.OpIssueCreate, gh.OpPRCreate, gh.OpPRView, gh.OpPRComment, gh.OpPRReply, gh.OpChecks, gh.OpUpload, gh.OpFetch, gh.OpWait} {
		if !strings.Contains(books["work-issue"], "`"+op+"`") {
			t.Errorf("work-issue does not explain %s", op)
		}
	}
}

func TestSetupGuide(t *testing.T) {
	g := Setup("/usr/local/bin/open-conduit")
	if strings.Contains(g, "{{bin}}") || !strings.Contains(g, "/usr/local/bin/open-conduit config validate") || Description(g) == "" {
		t.Fatal("guide not filled in")
	}
	if _, ok := All()["setup"]; ok {
		t.Fatal("the setup guide is among the playbooks agents get for turns")
	}
	// The example config in the guide is valid JSON naming only registered
	// operations (the full config contract is checked by the daemon tests).
	start, end := strings.Index(g, "```json\n"), strings.Index(g, "\n```\n\nOther `github`")
	if start < 0 || end < 0 {
		t.Fatal("no example config")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(g[start+8:end]), &cfg); err != nil {
		t.Fatalf("example config: %v", err)
	}
}

func TestPrepareWritesThePlugin(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "skills", "sweep")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest["name"] != PluginName {
		t.Fatalf("manifest: %s %v", data, err)
	}
	skill, err := os.ReadFile(filepath.Join(dir, "skills", "work-issue", "SKILL.md"))
	if err != nil || Description(string(skill)) == "" || !strings.HasPrefix(Body(string(skill)), "# Work an issue") {
		t.Fatalf("skill: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a skill no longer shipped was kept")
	}
	setup := t.TempDir()
	if err := PrepareSetup(setup, "oc"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(setup, "skills", "setup", "SKILL.md")); err != nil || !strings.Contains(string(data), "oc doctor") {
		t.Fatalf("setup skill: %v", err)
	}
}
