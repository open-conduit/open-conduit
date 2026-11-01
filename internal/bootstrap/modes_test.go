package bootstrap

import (
	"strings"
	"testing"
)

// TestModeWarnings: offline, a claude-code binding whose git resource sets a
// mode the executor's own workspace_mode overrides is warned about, and the
// same binding asking for the resource's mode, or a resource naming no mode,
// is not.
func TestModeWarnings(t *testing.T) {
	config := func(executorMode string) []byte {
		return []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"review","name":"review","bindings":[{"name":"reviewer","executor":"claude-code",
			"requires":{"workspace":true},"operations":[],"secret_names":[],"config":{"playbook":"-"` + executorMode + `},
			"resource":{"provider":"git","config":{"checkout":"./checkout","mode":"fresh"}}}]}]}`)
	}
	if diags, err := ValidateConfigAt("", config("")); err != nil || len(diags) != 0 {
		t.Fatalf("config invalid: %v %+v", err, diags)
	}
	got := ModeWarnings("", config(""))
	if len(got) != 1 || !strings.HasPrefix(got[0], `scope review binding reviewer: the git resource's mode "fresh" never applies, because executor claude-code requests mode "work"`) {
		t.Fatalf("warnings %q", got)
	}
	if got := ModeWarnings("", config(`,"workspace_mode":"fresh"`)); len(got) != 0 {
		t.Fatalf("agreeing modes warned: %q", got)
	}
	// A resource that names no mode is quiet, though its default is shared.
	unset := strings.Replace(string(config("")), `,"mode":"fresh"`, "", 1)
	if got := ModeWarnings("", []byte(unset)); len(got) != 0 {
		t.Fatalf("unset mode warned: %q", got)
	}
}
