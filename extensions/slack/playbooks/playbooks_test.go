package playbooks

import (
	"strings"
	"testing"
)

// The thread playbook ships under its name and its earlier one, and neither
// assumes what the thread is for.
func TestThreadPlaybookAndAlias(t *testing.T) {
	books := All()
	for _, name := range []string{Thread, LegacyThread} {
		text, ok := books[name]
		if !ok || !strings.HasPrefix(text, "---\nname: "+name+"\n") {
			t.Fatalf("no %s playbook", name)
		}
		body := strings.ToLower(strings.TrimPrefix(text, "---\nname: "+name+"\n"))
		for _, word := range []string{"support", "asked for help", "no checkout", "only read and answer"} {
			if strings.Contains(body, word) {
				t.Errorf("%s still says %q", name, word)
			}
		}
	}
	if len(books) != 2 {
		t.Fatalf("playbooks: %d", len(books))
	}
}
