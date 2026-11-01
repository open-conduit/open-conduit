package bootstrap

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/config"
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
	got := NewConfigCheck().ModeWarnings("", config(""))
	if len(got) != 1 || !strings.HasPrefix(got[0], `scope review binding reviewer: the git resource's mode "fresh" never applies, because executor claude-code requests mode "work"`) {
		t.Fatalf("warnings %q", got)
	}
	if got := NewConfigCheck().ModeWarnings("", config(`,"workspace_mode":"fresh"`)); len(got) != 0 {
		t.Fatalf("agreeing modes warned: %q", got)
	}
	// A resource that names no mode is quiet, though its default is shared.
	unset := strings.Replace(string(config("")), `,"mode":"fresh"`, "", 1)
	if got := NewConfigCheck().ModeWarnings("", []byte(unset)); len(got) != 0 {
		t.Fatalf("unset mode warned: %q", got)
	}
}

// TestConfigCheckValidatesOnce: a config check's warnings and then its
// diagnostics for the same config come from one validation; the kept result
// is used once, and other data validates afresh.
func TestConfigCheckValidatesOnce(t *testing.T) {
	calls := 0
	c := &ConfigCheck{check: func(path string, data []byte) ([]config.Diagnostic, []string, error) {
		calls++
		return []config.Diagnostic{{Path: path, Code: string(data)}}, []string{"w"}, nil
	}}
	if w := c.ModeWarnings("p", []byte("a")); len(w) != 1 || calls != 1 {
		t.Fatalf("warnings %q after %d calls", w, calls)
	}
	if d, err := c.Validate("p", []byte("a")); err != nil || len(d) != 1 || d[0].Code != "a" || calls != 1 {
		t.Fatalf("validate %+v %v after %d calls", d, err, calls)
	}
	// Negative controls: a second Validate, or another config, validates.
	if _, _ = c.Validate("p", []byte("a")); calls != 2 {
		t.Fatalf("kept result reused twice: %d calls", calls)
	}
	c.ModeWarnings("p", []byte("a"))
	if d, _ := c.Validate("p", []byte("b")); calls != 4 || d[0].Code != "b" {
		t.Fatalf("another config reused the kept result: %+v after %d calls", d, calls)
	}
}
