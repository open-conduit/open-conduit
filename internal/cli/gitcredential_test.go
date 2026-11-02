package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// os git-credential get asks the toolbelt which granted operations serve
// git.credential and calls each in turn with git's attributes, without a
// request ID, printing the first credential one answers as git reads it: the
// owning extension's operation answers whatever order it comes in, a URL no
// operation owns prints nothing, a refusal is reported on stderr only when
// no other operation answers, and store and erase do nothing.
func TestToolbeltGitCredential(t *testing.T) {
	dir, err := os.MkdirTemp("", "ocgc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var calls []map[string]any
	serving := `["acme.git.credential","github.git.credential"]`
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/toolbelt/serving" {
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("capability") != "git.credential" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"execution authority rejected"}}`)
				return
			}
			fmt.Fprintf(w, `{"data":{"operations":%s}}`, serving)
			return
		}
		var call map[string]any
		_ = json.NewDecoder(r.Body).Decode(&call)
		calls = append(calls, call)
		// The management handler decodes an extension operation's input
		// strictly as a payload, {"schema", "data"}; refuse anything else
		// as it does.
		in := map[string]any{}
		if raw, ok := call["input"].(map[string]any); ok {
			for k := range raw {
				if k != "schema" && k != "data" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"code":"malformed","message":"operation input is malformed or has unknown fields"}}`)
					return
				}
			}
			in, _ = raw["data"].(map[string]any)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"execution authority rejected"}}`)
		case call["operation"] == "acme.git.credential" && in["host"] == "git.acme.test":
			fmt.Fprint(w, `{"data":{"output":{"schema":"acme.v1","data":{"username":"acme","password":"acme_minted"}}}}`)
		case call["operation"] == "acme.git.credential" && in["host"] == "refusing.test":
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":{"code":"acme_static","message":"acme refuses"}}`)
		case call["operation"] == "github.git.credential" && in["host"] == "github.com":
			fmt.Fprint(w, `{"data":{"output":{"schema":"github.tool.v1","data":{"username":"x-access-token","password":"ghs_minted","expires_at":"2026-10-31T10:00:00Z"}}}}`)
		default:
			fmt.Fprint(w, `{"data":{"output":{"schema":"github.tool.v1","data":{}}}}`)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	token := "tok"
	env := Env{Getenv: func(k string) string {
		switch k {
		case "OS_ENDPOINT":
			return "unix://" + socket
		case "OS_TOKEN":
			return token
		}
		return ""
	}}
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunToolbelt("t", "c", append([]string{"git-credential"}, args...), env, strings.NewReader(stdin), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	code, out, _ := run("protocol=https\nhost=github.com\npath=octo/widgets.git\nusername=ignored\n\n", "get")
	if code != 0 || out != "username=x-access-token\npassword=ghs_minted\n" {
		t.Fatalf("get: %d %q", code, out)
	}
	// acme's operation comes first in the grant and owns another host.
	if len(calls) != 2 || calls[0]["operation"] != "acme.git.credential" || calls[1]["operation"] != "github.git.credential" || calls[1]["request_id"] != nil {
		t.Fatalf("calls %v", calls)
	}
	if env := calls[1]["input"].(map[string]any); len(env) != 1 {
		t.Fatalf("input envelope %v", env)
	}
	if in := calls[1]["input"].(map[string]any)["data"].(map[string]any); len(in) != 3 || in["protocol"] != "https" || in["path"] != "octo/widgets.git" {
		t.Fatalf("input %v", in)
	}
	calls = nil
	if code, out, _ := run("protocol=https\nhost=git.acme.test\n\n", "get"); code != 0 || out != "username=acme\npassword=acme_minted\n" || len(calls) != 1 {
		t.Fatalf("acme: %d %q %v", code, out, calls)
	}
	// One operation's refusal is reported only when no other answers.
	if code, out, errOut := run("protocol=https\nhost=refusing.test\n\n", "get"); code != 1 || out != "" || !strings.Contains(errOut, "acme refuses") {
		t.Fatalf("refused: %d %q %q", code, out, errOut)
	}
	serving = `[]`
	if code, out, _ := run("protocol=https\nhost=github.com\n\n", "get"); code != 0 || out != "" {
		t.Fatalf("nothing serves: %d %q", code, out)
	}
	serving = `["acme.git.credential","github.git.credential"]`
	if code, out, _ := run("protocol=https\nhost=example.com\n\n", "get"); code != 0 || out != "" {
		t.Fatalf("another host: %d %q", code, out)
	}
	for _, op := range []string{"store", "erase"} {
		n := len(calls)
		if code, out, _ := run("protocol=https\nhost=github.com\npassword=x\n\n", op); code != 0 || out != "" || len(calls) != n {
			t.Fatalf("%s: %d %q", op, code, out)
		}
	}
	token = "revoked"
	if code, out, errOut := run("protocol=https\nhost=github.com\n\n", "get"); code != 1 || out != "" || !strings.Contains(errOut, "no git credential") {
		t.Fatalf("revoked: %d %q %q", code, out, errOut)
	}
}
