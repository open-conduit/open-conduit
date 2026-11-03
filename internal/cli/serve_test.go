package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// Usage telemetry is on unless --no-telemetry, OPEN_CONDUIT_NO_TELEMETRY or
// DO_NOT_TRACK turns it off.
func TestServeTelemetryOptOut(t *testing.T) {
	root := t.TempDir()
	run := func(env map[string]string, args ...string) ServeRequest {
		t.Helper()
		var got ServeRequest
		e := Env{Getenv: func(k string) string { return env[k] }, Cwd: root, Serve: func(r ServeRequest) int { got = r; return 0 }}
		var out, errOut bytes.Buffer
		if code := RunOperator("test", "abc", append([]string{"--data-dir", root, "serve", "--config", filepath.Join(root, "c.json")}, args...), e, &out, &errOut); code != 0 {
			t.Fatalf("serve %v: %d %s", args, code, errOut.String())
		}
		return got
	}
	if r := run(nil); r.NoTelemetry {
		t.Fatal("telemetry off by default")
	}
	if r := run(nil, "--no-telemetry"); !r.NoTelemetry {
		t.Fatal("--no-telemetry left it on")
	}
	for _, c := range []struct {
		key, value string
		off        bool
	}{{"OPEN_CONDUIT_NO_TELEMETRY", "1", true}, {"DO_NOT_TRACK", "1", true}, {"DO_NOT_TRACK", "true", true}, {"DO_NOT_TRACK", "0", false}, {"OPEN_CONDUIT_NO_TELEMETRY", "false", false}} {
		if r := run(map[string]string{c.key: c.value}); r.NoTelemetry != c.off {
			t.Errorf("%s=%s: NoTelemetry %v, want %v", c.key, c.value, r.NoTelemetry, c.off)
		}
	}
}

// --claude-usage is retired: serve still starts with it, saying where the
// setting went (a claude-code binding's usage).
func TestServeClaudeUsageRetired(t *testing.T) {
	root := t.TempDir()
	served := false
	e := Env{Getenv: func(string) string { return "" }, Cwd: root, Serve: func(ServeRequest) int { served = true; return 0 }}
	var out, errOut bytes.Buffer
	if code := RunOperator("test", "abc", []string{"--data-dir", root, "serve", "--config", filepath.Join(root, "c.json"), "--claude-usage"}, e, &out, &errOut); code != 0 || !served {
		t.Fatalf("serve --claude-usage: %d %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), `"usage": true`) {
		t.Errorf("no retirement notice: %q", errOut.String())
	}
}

// serve's tunnel flags name any tunnel type a package provides, with its
// config as an opaque JSON object; the per-type flags are retired, each
// naming its replacement, and so is --tunnel static.
func TestServeTunnelFlags(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) (ServeRequest, string, int) {
		var got ServeRequest
		e := Env{Getenv: func(string) string { return "" }, Cwd: root, Serve: func(r ServeRequest) int { got = r; return 0 }}
		var out, errOut bytes.Buffer
		code := RunOperator("test", "abc", append([]string{"--data-dir", root, "serve", "--config", filepath.Join(root, "c.json")}, args...), e, &out, &errOut)
		return got, errOut.String(), code
	}
	r, errOut, code := run("--webhook-addr", "127.0.0.1:8787", "--tunnel", "tailscale-funnel", "--tunnel-config", `{"port": 443}`)
	if code != 0 || r.TunnelKind != "tailscale-funnel" || string(r.TunnelConfig) != `{"port": 443}` {
		t.Fatalf("an installed package's tunnel type: %d %s %+v", code, errOut, r)
	}
	if r, _, code := run("--public-url", "https://hooks.example.com"); code != 0 || r.PublicURL != "https://hooks.example.com" || r.TunnelKind != "" {
		t.Fatalf("--public-url alone: %d %+v", code, r)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--tunnel-domain", "hooks.ngrok.app"}, `--tunnel ngrok --tunnel-config '{"domain"`},
		{[]string{"--tunnel-name=hooks"}, `--tunnel cloudflared --tunnel-config`},
		{[]string{"--tunnel-hostname", "https://h.example.com"}, `"hostname"`},
		{[]string{"--tunnel-binary", "/opt/ngrok"}, `{"binary"`},
		{[]string{"--tunnel", "static", "--public-url", "https://h.example.com"}, "--public-url alone"},
		{[]string{"--tunnel", "Bad Type"}, "a tunnel type"},
		{[]string{"--tunnel-config", "{}"}, "give --tunnel too"},
		{[]string{"--tunnel", "ngrok", "--tunnel-config", `"x"`}, "a JSON object"},
		{[]string{"--tunnel", "ngrok", "--tunnel-config", "null"}, "a JSON object"},
		{[]string{"--tunnel", "ngrok", "--public-url", "https://h.example.com"}, "not both"},
	} {
		if _, errOut, code := run(c.args...); code == 0 || !strings.Contains(errOut, c.want) {
			t.Errorf("serve %v: %d %q, want it refused with %q", c.args, code, errOut, c.want)
		}
	}
}
