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
