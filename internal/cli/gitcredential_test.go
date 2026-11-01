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

// os git-credential get turns git's attributes into a github.git.credential
// call, without a request ID, and prints the credential as git reads it;
// a URL the host has nothing for prints nothing, a refusal is reported on
// stderr, and store and erase do nothing.
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
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case in["host"] == "github.com":
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
	if calls[0]["operation"] != "github.git.credential" || calls[0]["request_id"] != nil {
		t.Fatalf("call %v", calls[0])
	}
	if env := calls[0]["input"].(map[string]any); len(env) != 1 {
		t.Fatalf("input envelope %v", env)
	}
	if in := calls[0]["input"].(map[string]any)["data"].(map[string]any); len(in) != 3 || in["protocol"] != "https" || in["path"] != "octo/widgets.git" {
		t.Fatalf("input %v", in)
	}
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
