package playbooks

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	is "github.com/alsiola/open-conduit/extensions/introspection"
)

// Every introspection operation a playbook names is registered, and none
// tells the agent to use Open Supervisor's commands.
func TestPlaybooksNameRegisteredOperations(t *testing.T) {
	for _, name := range []string{"sweep", "triage-reports", "fix-report"} {
		text, ok := All()[name]
		if !ok || !strings.HasPrefix(text, "---\nname: "+name+"\n") {
			t.Fatalf("no %s playbook", name)
		}
		found := 0
		for _, m := range regexp.MustCompile("`(introspection\\.[a-z.-]+)`").FindAllStringSubmatch(text, -1) {
			found++
			if !slices.Contains(is.Operations(), m[1]) {
				t.Errorf("%s names %s, which is not registered", name, m[1])
			}
		}
		if found == 0 {
			t.Fatalf("%s names no operation", name)
		}
		for _, v1 := range []string{"$OPEN_SUPERVISOR", "agent problem", "agent correction", "--upstream", "open-supervisor/"} {
			if strings.Contains(text, v1) {
				t.Errorf("%s still names %s", name, v1)
			}
		}
	}
	if len(All()) != 3 {
		t.Fatalf("playbooks: %d", len(All()))
	}
}

// The sweep and triage start from the evidence; only the triage requests
// fixes, and only the fix resolves the reports a fix covers.
func TestPlaybookRoles(t *testing.T) {
	books := All()
	for _, name := range []string{"sweep", "triage-reports"} {
		if !strings.Contains(books[name], "`"+is.OpEvidence+"`") || !strings.Contains(books[name], "`"+is.OpNotesResolve+"`") {
			t.Errorf("%s does not start from the evidence and resolve notes", name)
		}
	}
	if !strings.Contains(books["triage-reports"], "`"+is.OpFix+"`") || strings.Contains(books["sweep"], "`"+is.OpFix+"`") {
		t.Error("only the triage requests fixes")
	}
	if !strings.Contains(books["fix-report"], "`"+is.OpResolve+"`") || strings.Contains(books["fix-report"], "`"+is.OpFix+"`") {
		t.Error("the fix resolves its reports and requests no fix")
	}
}
