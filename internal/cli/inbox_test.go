package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// os inbox prints unacknowledged inbox entries in order as JSON lines and
// acknowledges each one, so a second read prints nothing.
func TestToolbeltInbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "consumed"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"000002-bbb.json": `{"id": "second"}`, "000001-aaa.json": `{"id": "first"}`, "000003-ccc.json": `{"id":"done"}`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "consumed", "ccc"), nil, 0o600)
	env := Env{Getenv: func(k string) string {
		if k == "OS_INBOX" {
			return dir
		}
		return ""
	}}
	var out, errOut bytes.Buffer
	if code := RunToolbelt("t", "c", []string{"inbox"}, env, nil, &out, &errOut); code != 0 {
		t.Fatalf("%d %s", code, errOut.String())
	}
	if out.String() != "{\"id\":\"first\"}\n{\"id\":\"second\"}\n" {
		t.Fatalf("inbox output %q", out.String())
	}
	for _, d := range []string{"aaa", "bbb"} {
		if _, err := os.Stat(filepath.Join(dir, "consumed", d)); err != nil {
			t.Fatalf("%s not acknowledged", d)
		}
	}
	out.Reset()
	if code := RunToolbelt("t", "c", []string{"inbox"}, env, nil, &out, &errOut); code != 0 || out.Len() != 0 {
		t.Fatalf("second read: %d %q", code, out.String())
	}
	env.Getenv = func(string) string { return "" }
	if code := RunToolbelt("t", "c", []string{"inbox"}, env, nil, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "OS_INBOX") {
		t.Fatalf("no inbox: %d %s", code, errOut.String())
	}
}
