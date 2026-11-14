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

// `os call --output data` prints the output's data alone on one line, for
// a program that reads it (an AWS credential_process); a call a gate held
// prints the gate's answer on stderr and exits 3, so the program fails
// rather than reading the answer as its credentials.
func TestCallOutputData(t *testing.T) {
	dir, err := os.MkdirTemp("", "occd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket, tokenFile := filepath.Join(dir, "s"), filepath.Join(dir, "token")
	if err = os.WriteFile(tokenFile, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		inputs = append(inputs, string(call.Input))
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(call.Input), "write") {
			fmt.Fprint(w, `{"data":{"wait":{"id":"w1"},"output":{"schema":"human.gate.v1","data":{"state":"approval_required","message":"a person decides"}}}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"output":{"schema":"aws.credentials.v1","data":{"Version": 1, "AccessKeyId": "ASIA1"}}}}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	env := Env{Getenv: func(k string) string {
		switch k {
		case "OS_ENDPOINT":
			return "unix://" + socket
		case "OS_TOKEN_FILE":
			return tokenFile
		}
		return ""
	}}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunToolbelt("t", "c", args, env, nil, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	code, out, errOut := run("call", "aws.credentials", "--output", "data", "--data", `{"role":"read"}`)
	if code != 0 || out != `{"Version":1,"AccessKeyId":"ASIA1"}`+"\n" || errOut != "" {
		t.Fatalf("exit %d: %q %q", code, out, errOut)
	}
	if inputs[0] != `{"data":{"role":"read"}}` {
		t.Fatalf("input %s", inputs[0])
	}
	code, out, errOut = run("call", "aws.credentials", "--output", "data", "--data", `{"role":"write"}`)
	if code != heldExit || out != "" || !strings.Contains(errOut, "a person decides") {
		t.Fatalf("held call: exit %d: %q %q", code, out, errOut)
	}
	// Control: without --output, the same call prints the whole payload.
	if code, out, _ = run("call", "aws.credentials", "--data", `{"role":"read"}`); code != 0 || !strings.Contains(out, `"schema": "aws.credentials.v1"`) {
		t.Fatalf("default output: exit %d: %q", code, out)
	}
	if code, _, _ = run("call", "aws.credentials", "--output", "yaml"); code == 0 {
		t.Fatal("accepted --output yaml")
	}
}
