package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

// dev hands the entrypoint the package directory, each secret as a value
// or an env: reference, each entry's config and the agent, resolved
// against the working directory, and refuses malformed flags.
func TestDevCommand(t *testing.T) {
	root := t.TempDir()
	var got []DevRequest
	run := func(args ...string) (string, int) {
		var out, errOut bytes.Buffer
		e := Env{Getenv: func(string) string { return "" }, Cwd: root, Extensions: &ExtensionOps{Dev: func(_ context.Context, r DevRequest) error {
			got = append(got, r)
			return nil
		}}}
		code := RunOperator("test", "abc", append([]string{"dev"}, args...), e, &out, &errOut)
		return errOut.String(), code
	}
	if errOut, code := run("pkg", "--secret", "api_key=k", "--secret", "token=env:WEATHER_TOKEN", "--entry", `weather={"service":"x"}`, "--agent", "bin/claude", "--web-addr", "127.0.0.1:9100"); code != 0 {
		t.Fatalf("dev: %d %s", code, errOut)
	}
	r := got[0]
	if r.Dir != filepath.Join(root, "pkg") || r.Agent != filepath.Join(root, "bin/claude") || r.WebAddr != "127.0.0.1:9100" || r.Watch != 500*time.Millisecond {
		t.Fatalf("request: %+v", r)
	}
	if len(r.Secrets) != 2 || r.Secrets[0] != (DevSecret{Name: "api_key", Value: "k"}) || r.Secrets[1] != (DevSecret{Name: "token", Ref: "env:WEATHER_TOKEN"}) {
		t.Fatalf("secrets: %+v", r.Secrets)
	}
	if string(r.Entries["weather"]) != `{"service":"x"}` {
		t.Fatalf("entries: %s", r.Entries)
	}
	if _, code := run(); code != 0 || got[1].Dir != root {
		t.Fatalf("no DIR: %d %+v", code, got[1])
	}
	for _, bad := range [][]string{{"a", "b"}, {"--secret", "nameonly"}, {"--secret", "t=env:"}, {"--entry", "weather={"}, {"--entry", "={}"}} {
		if errOut, code := run(bad...); code != 2 {
			t.Errorf("%q: %d %s", bad, code, errOut)
		}
	}
}
