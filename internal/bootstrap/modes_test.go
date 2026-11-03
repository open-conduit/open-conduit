package bootstrap

import (
	"testing"

	"github.com/open-conduit/open-conduit/internal/config"
)

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
