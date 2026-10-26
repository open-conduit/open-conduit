package playbooks

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	is "github.com/alsiola/open-conduit/extensions/introspection"
)

// Every introspection operation the triage playbook names is registered,
// and it never tells the agent to use Open Supervisor's commands.
func TestTriageNamesRegisteredOperations(t *testing.T) {
	text, ok := All()["triage-reports"]
	if !ok || !strings.HasPrefix(text, "---\nname: triage-reports\n") {
		t.Fatal("no triage-reports playbook")
	}
	found := 0
	for _, m := range regexp.MustCompile("`(introspection\\.[a-z.-]+)`").FindAllStringSubmatch(text, -1) {
		found++
		if !slices.Contains(is.Operations(), m[1]) {
			t.Errorf("names %s, which is not registered", m[1])
		}
	}
	if found == 0 {
		t.Fatal("names no operation")
	}
	for _, v1 := range []string{"$OPEN_SUPERVISOR", "agent problem", "--upstream"} {
		if strings.Contains(text, v1) {
			t.Errorf("still names %s", v1)
		}
	}
}
