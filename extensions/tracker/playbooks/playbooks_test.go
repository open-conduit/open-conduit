package playbooks

import (
	"regexp"
	"strings"
	"testing"
)

// The tracker kit reaches no other extension, and neither does its playbook:
// it names code-host operations only through the <host> placeholder.
func TestPlaybookNamesNoOtherExtension(t *testing.T) {
	text, ok := All()["work-tracker-issue"]
	if !ok || !strings.HasPrefix(text, "---\nname: work-tracker-issue\n") {
		t.Fatal("no work-tracker-issue playbook")
	}
	if m := regexp.MustCompile("`(bitbucket|github)\\.[a-z.]+`").FindString(text); m != "" {
		t.Fatalf("the playbook names %s", m)
	}
	for _, op := range []string{"`<tracker>.issue.view`", "`<host>.pr.create`", "`<host>.checks`"} {
		if !strings.Contains(text, op) {
			t.Errorf("the playbook does not explain %s", op)
		}
	}
}
