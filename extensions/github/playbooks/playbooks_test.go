package playbooks_test

import (
	"regexp"
	"strings"
	"testing"

	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/playbooks"
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
// github extension registers, and no `agent …` command remains.
func TestPlaybooksNameRegisteredOperations(t *testing.T) {
	ops := registered()
	books := playbooks.All()
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
	}
	// The agent-facing tools the work-issue playbook relies on.
	for _, op := range []string{gh.OpIssueView, gh.OpIssueComment, gh.OpIssueCreate, gh.OpPRCreate, gh.OpPRView, gh.OpPRComment, gh.OpPRReply, gh.OpChecks, gh.OpUpload, gh.OpFetch, gh.OpWait} {
		if !strings.Contains(books["work-issue"], "`"+op+"`") {
			t.Errorf("work-issue does not explain %s", op)
		}
	}
}

// The playbooks are skills: each has a description, and work-issue's body
// is the playbook. The setup playbook is the setup package's, not GitHub's.
func TestPlaybooksAreSkills(t *testing.T) {
	books := playbooks.All()
	skill := books["work-issue"]
	if playbooks.Description(skill) == "" || !strings.HasPrefix(playbooks.Body(skill), "# Work an issue") {
		t.Fatalf("work-issue: %q", skill[:min(len(skill), 80)])
	}
	if _, ok := books["setup"]; ok {
		t.Fatal("the GitHub playbooks still carry the setup playbook")
	}
}
