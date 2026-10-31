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

// os mcp's initialize answers instructions made of the base sentence and the
// guidance the toolbelt serves for the execution's granted operations; when
// the toolbelt answers none, or not at all, the base sentence alone.
func TestMCPInstructionsCarryGuidance(t *testing.T) {
	dir, err := os.MkdirTemp("", "ocmcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	answer := `{"data":{"guidance":["Note things with fixture.note.","Ask with fixture.ask."]}}`
	status := http.StatusOK
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" || r.URL.Path != "/v1/toolbelt/guidance" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"unknown endpoint"}}`)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, answer)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	env := Env{Getenv: func(k string) string {
		switch k {
		case "OS_ENDPOINT":
			return "unix://" + socket
		case "OS_TOKEN":
			return "tok"
		}
		return ""
	}}
	instructions := func() string {
		t.Helper()
		var out, errOut bytes.Buffer
		in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
		if code := RunToolbelt("t", "c", []string{"mcp"}, env, strings.NewReader(in), &out, &errOut); code != 0 {
			t.Fatalf("mcp exited %d: %s", code, errOut.String())
		}
		var resp struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("response %q: %v", out.String(), err)
		}
		return resp.Result.Instructions
	}
	want := mcpBaseInstructions + "\n\nNote things with fixture.note.\n\nAsk with fixture.ask."
	if got := instructions(); got != want {
		t.Fatalf("instructions = %q", got)
	}
	answer = `{"data":{"guidance":[]}}`
	if got := instructions(); got != mcpBaseInstructions {
		t.Fatalf("instructions without guidance = %q", got)
	}
	status, answer = http.StatusNotFound, `{"error":{"code":"not_found","message":"unknown endpoint"}}`
	if got := instructions(); got != mcpBaseInstructions {
		t.Fatalf("instructions from an older daemon = %q", got)
	}
}
