package integration_test

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestEntryOperationsPackage runs a package whose operations come from its
// entry's config, not its manifest, in the real daemon: the entry declares
// greet and gone into toolset "all", the binding is granted the toolset,
// and the agent calls each through the toolbelt. greet reaches the package
// with its entry; gone, which the entry no longer fronts, is refused by
// the package as drifted.
func TestEntryOperationsPackage(t *testing.T) {
	d := extensiontest.Daemon(t, extensiontest.Package("testdata/commands"),
		extensiontest.Entry("commands", map[string]any{
			"operations": []map[string]any{
				{"name": "greet", "summary": "Greet someone.", "toolset": "all", "input": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}},
				{"name": "gone", "summary": "Something the entry no longer offers.", "toolset": "all"},
			},
			"live": []string{"greet"},
		}),
		extensiontest.Grant("@commands.commands.all"))
	agent := d.Agent().
		Call("commands.commands.greet", map[string]any{"name": "Ada"}).
		Call("commands.commands.gone", map[string]any{}).
		Ends("completed")

	work := d.Start("commands.greet", nil)
	if r := agent.Played(1).Results[0]; r.IsError || !strings.Contains(r.Text, "hello Ada from commands.greet") {
		t.Fatalf("greet: %+v", r)
	}
	if r := agent.Played(1).Results[1]; !r.IsError || !strings.Contains(r.Text, "has drifted") {
		t.Fatalf("a drifted operation: %+v", r)
	}
	d.WaitWork(work.ID, "completed")
}
