package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A project's additions to a playbook are read without frontmatter and only
// ever added to a turn; nothing else in the checkout is read for them.
func TestProjectAdditions(t *testing.T) {
	root := t.TempDir()
	if projectAdditions(root, "work-issue") != "" {
		t.Fatal("additions without a file")
	}
	other := filepath.Join(root, ".other/playbooks")
	_ = os.MkdirAll(other, 0o755)
	_ = os.WriteFile(filepath.Join(other, "work-issue.md"), []byte("- Elsewhere.\n"), 0o644)
	if got := projectAdditions(root, "work-issue"); got != "" {
		t.Fatalf("additions read from outside %s: %q", AdditionsDir, got)
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
