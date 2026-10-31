package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// web passes its address and name, flag or environment, to the entrypoint's
// view, and refuses anything else.
func TestWebCommand(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "operator.token")
	if err := os.WriteFile(token, []byte(`{"token_id":"otok_x","token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OPEN_CONDUIT_WEB_HOST": "conduit.localhost"}
	var got []WebRequest
	run := func(args ...string) (string, int) {
		var out, errOut bytes.Buffer
		e := Env{Getenv: func(k string) string { return env[k] }, Cwd: root, Web: func(r WebRequest) error {
			got = append(got, r)
			return nil
		}}
		code := RunOperator("test", "abc", append([]string{"--token-file", token, "--socket", filepath.Join(root, "s.sock")}, args...), e, &out, &errOut)
		return errOut.String(), code
	}
	if errOut, code := run("web", "--addr", "127.0.0.1:9000"); code != 0 {
		t.Fatalf("web: %d %s", code, errOut)
	}
	if len(got) != 1 || got[0].Addr != "127.0.0.1:9000" || got[0].Host != "conduit.localhost" || got[0].Client == nil {
		t.Fatalf("request: %+v", got)
	}
	if errOut, code := run("web", "extra"); code != 2 || !strings.Contains(errOut, "web takes only") {
		t.Fatalf("extra argument: %d %s", code, errOut)
	}
	if errOut, code := run("web", "--port", "1"); code != 2 || !strings.Contains(errOut, "web takes only") {
		t.Fatalf("unknown flag: %d %s", code, errOut)
	}
}

// web url needs no daemon: it hands the entrypoint the sign-in file beside
// the operator token and asks only for the URLs.
func TestWebURL(t *testing.T) {
	root := t.TempDir()
	var got []WebRequest
	e := Env{Getenv: func(string) string { return "" }, Cwd: root, Web: func(r WebRequest) error {
		got = append(got, r)
		return nil
	}}
	var out, errOut bytes.Buffer
	if code := RunOperator("test", "abc", []string{"--data-dir", root, "web", "url", "--addr", "127.0.0.1:9000"}, e, &out, &errOut); code != 0 {
		t.Fatalf("web url: %d %s", code, errOut.String())
	}
	if len(got) != 1 || !got[0].URLOnly || got[0].Client != nil || got[0].Addr != "127.0.0.1:9000" || got[0].SignIn != filepath.Join(root, "private", "web.signin") {
		t.Fatalf("request: %+v", got)
	}
}

// serve --web, --web-addr or --web-host serve the view from the daemon,
// with the same sign-in file web url reads.
func TestServeWebFlags(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) ServeRequest {
		t.Helper()
		var got ServeRequest
		e := Env{Getenv: func(string) string { return "" }, Cwd: root, Serve: func(r ServeRequest) int { got = r; return 0 }}
		var out, errOut bytes.Buffer
		if code := RunOperator("test", "abc", append([]string{"--data-dir", root, "serve", "--config", filepath.Join(root, "c.json")}, args...), e, &out, &errOut); code != 0 {
			t.Fatalf("serve %v: %d %s", args, code, errOut.String())
		}
		return got
	}
	if r := run(); r.Web {
		t.Fatalf("web on by default: %+v", r)
	}
	if r := run("--web"); !r.Web || r.WebAddr != "" || r.WebSignIn != filepath.Join(root, "private", "web.signin") {
		t.Fatalf("--web: %+v", r)
	}
	if r := run("--web-addr", "127.0.0.1:9001", "--web-host", "c.localhost"); !r.Web || r.WebAddr != "127.0.0.1:9001" || r.WebHost != "c.localhost" {
		t.Fatalf("--web-addr: %+v", r)
	}
}
