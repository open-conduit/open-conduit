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

// Given a provider's base URL, the status probe says the Claude sign-in
// doesn't apply, and reads no login; a bad URL says what to fix.
func TestStatusProbeProvider(t *testing.T) {
	found := func(string) (string, error) { return "/usr/local/bin/claude", nil }
	version := func(context.Context, string) (string, error) { return "2.1.0 (Claude Code)", nil }
	read := false
	login := func(context.Context, string) (string, error) { read = true; return "", errors.New("no login") }
	p := Probes{LookPath: found, Version: version, LoginToken: login, Getenv: func(string) string { return "" }}
	probe := p.SetupProbes()[0]
	if len(probe.Args) != 1 || probe.Args[0].Name != "base_url" || probe.Args[0].Required {
		t.Fatalf("args %+v", probe.Args)
	}
	r, _ := probe.Run(context.Background(), map[string]string{"base_url": "https://openrouter.ai/api"})
	b, _ := json.Marshal(r.Output)
	if !strings.Contains(string(b), `"signed_in":true,"login":"provider","provider":"openrouter.ai"`) || read {
		t.Errorf("provider: %s (login read %v)", b, read)
	}
	r, _ = probe.Run(context.Background(), map[string]string{"base_url": "openrouter.ai"})
	if b, _ = json.Marshal(r.Output); !strings.Contains(string(b), `"signed_in":false`) || !strings.Contains(string(b), "base URL must be") {
		t.Errorf("bad URL: %s", b)
	}
}
