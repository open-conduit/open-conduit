package github_test

import (
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/playbooks"
)

// Every toolset is the extension's own and grants only registered
// operations, and the author toolset has everything the work-issue
// playbook says it uses, so a binding granted it is never refused one.
func TestToolsetsCoverThePlaybook(t *testing.T) {
	ops := gh.Operations()
	byName := map[string]extension.Toolset{}
	for _, ts := range gh.Toolsets() {
		if err := extension.CheckToolset(gh.Type, ts); err != nil {
			t.Error(err)
		}
		for _, op := range ts.Names() {
			if !extension.BuiltinOperation(op) && !slices.Contains(ops, op) {
				t.Errorf("toolset %s grants unregistered %s", ts.Name, op)
			}
		}
		byName[ts.Name] = ts
	}
	declared := extension.PlaybookOperations(playbooks.All()[gh.Playbook])
	if len(declared) == 0 {
		t.Fatal("the work-issue playbook declares no operations")
	}
	author := byName[gh.ToolsetAuthor].Names()
	for _, op := range declared {
		if !slices.Contains(author, op) {
			t.Errorf("the playbook uses %s, which the author toolset doesn't have", op)
		}
	}
	// github.pr.edit joined in version 4, so a list from before it is
	// recognisably version 3's.
	if v := byName[gh.ToolsetAuthor]; v.Version() != 4 || slices.Contains(v.At(3), "github.pr.edit") {
		t.Fatalf("author toolset versions: %+v", v)
	}
}
