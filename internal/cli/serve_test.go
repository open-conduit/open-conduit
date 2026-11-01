package cli

import (
	"bytes"
	"path/filepath"
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
