package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The status probe says whether Claude Code can do the work, and never
// carries the login's token.
func TestStatusProbe(t *testing.T) {
	found := func(string) (string, error) { return "/usr/local/bin/claude", nil }
	version := func(context.Context, string) (string, error) { return "2.1.0 (Claude Code)", nil }
	token := func(context.Context, string) (string, error) { return "sk-ant-oat-secret", nil }
	none := func(context.Context, string) (string, error) { return "", errors.New("no login") }
	for name, c := range map[string]struct {
		p    Probes
		want string
	}{
		"signed in":   {Probes{LookPath: found, Version: version, LoginToken: token, Getenv: func(string) string { return "" }}, `"signed_in":true,"login":"subscription"`},
		"an API key":  {Probes{LookPath: found, Version: version, LoginToken: none, Getenv: func(string) string { return "sk-ant-api-secret" }}, `"login":"api_key"`},
		"signed out":  {Probes{LookPath: found, Version: version, LoginToken: none, Getenv: func(string) string { return "" }}, `"signed_in":false,"next":"run claude once`},
		"not on PATH": {Probes{LookPath: func(string) (string, error) { return "", errors.New("no") }}, `"installed":false`},
	} {
		probes := c.p.SetupProbes()
		if len(probes) != 1 || probes[0].Name != "status" {
			t.Fatalf("probes: %+v", probes)
		}
		r, err := probes[0].Run(context.Background(), nil)
		b, _ := json.Marshal(r.Output)
		if err != nil || !strings.Contains(string(b), c.want) || strings.Contains(string(b), "secret") {
			t.Errorf("%s: %s %v", name, b, err)
		}
	}
}
