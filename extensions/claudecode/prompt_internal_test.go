package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
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

// A turn cut off at its time limit is told so in the next turn's prompt, with
// the limit, rather than shown as a raw core Event (#564). The control: any
// other Event renders as before.
func TestRenderTurnLimit(t *testing.T) {
	e := domain.Event{Type: "core.execution.timeout", Data: domain.Payload{Schema: "core.execution.timeout.v1", Data: []byte(`{"ExecutionID":"x","TimeoutSeconds":3600}`)}}
	got := RenderEvents([]domain.Event{e})
	if !strings.HasPrefix(got, "### Your last turn ended at its time limit") || !strings.Contains(got, "time limit of 1 hour") || !strings.Contains(got, OpTaskStart) {
		t.Fatalf("turn limit rendered as %q", got)
	}
	if other := RenderEvents([]domain.Event{{Type: "core.retry.due"}}); strings.Contains(other, "time limit") {
		t.Fatalf("another Event rendered as a turn limit: %q", other)
	}
	for d, want := range map[time.Duration]string{time.Hour: "1 hour", 2 * time.Hour: "2 hours", 90 * time.Minute: "90 minutes", 45 * time.Second: "45s"} {
		if got := spell(d); got != want {
			t.Errorf("spell(%s) = %q, want %q", d, got, want)
		}
	}
}
