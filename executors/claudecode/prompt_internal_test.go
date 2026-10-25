package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from Open Supervisor's TestProjectAdditions: a project's additions
// to a playbook are read without frontmatter and only ever added to a turn.
func TestProjectAdditions(t *testing.T) {
	root := t.TempDir()
	if projectAdditions(root, "work-issue") != "" {
		t.Fatal("additions without a file")
	}
	legacy := filepath.Join(root, legacyAdditionsDir)
	_ = os.MkdirAll(legacy, 0o755)
	_ = os.WriteFile(filepath.Join(legacy, "work-issue.md"), []byte("- Legacy rule.\n"), 0o644)
	if got := projectAdditions(root, "work-issue"); got != "- Legacy rule." {
		t.Fatalf("legacy additions %q", got)
	}
	dir := filepath.Join(root, AdditionsDir)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "work-issue.md"), []byte("---\nreplace: true\n---\n\n- Take screenshots at 1280px.\n"), 0o644)
	if got := projectAdditions(root, "work-issue"); got != "- Take screenshots at 1280px." {
		t.Fatalf("additions %q", got)
	}
	if projectAdditions(root, "-") != "" {
		t.Fatal("additions without a playbook")
	}
	body := withAdditions("## Since your last turn\n\nx", "- Take screenshots at 1280px.")
	if !strings.HasPrefix(body, "## Since your last turn") || !strings.Contains(body, "## This project's additions to the playbook\n\nFollow these as well as the playbook.") {
		t.Fatalf("turn body:\n%s", body)
	}
	if withAdditions("", "- a") == "" || withAdditions("b", "") != "b" {
		t.Fatal("empty parts")
	}
}
