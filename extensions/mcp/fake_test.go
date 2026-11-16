package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The test binary is also the fake MCP server: run with fakeServerEnv set,
// it serves MCP over stdin and stdout. Its tools are read from the file
// fakeToolsEnv names on every tools/list, so a test changes them by
// rewriting it, and each tools/call it serves is appended to the file
// fakeCallsEnv names.
const (
	fakeServerEnv = "MCP_FAKE_SERVER"
	fakeToolsEnv  = "MCP_FAKE_TOOLS"
	fakeCallsEnv  = "MCP_FAKE_CALLS"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) != "" {
		serveStdio(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func serveStdio(in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		// Ask the client something first, as a server may: the bridge
		// must answer it and carry on.
		if m.Method == "tools/call" {
			fmt.Fprintln(out, `{"jsonrpc":"2.0","id":"s1","method":"roots/list"}`)
		}
		b, _ := json.Marshal(answer(m, os.Getenv(fakeToolsEnv), os.Getenv(fakeCallsEnv)))
		fmt.Fprintln(out, string(b))
	}
}

// answer is the fake server's response to one request. The tools are
// listed one to a page, so a client must follow the cursor.
func answer(m message, toolsFile, callsFile string) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	switch m.Method {
	case "initialize":
		reply["result"] = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fake", "version": "1"}}
	case "tools/list":
		var tools []ServerTool
		raw, _ := os.ReadFile(toolsFile)
		_ = json.Unmarshal(raw, &tools)
		var p struct{ Cursor string }
		_ = json.Unmarshal(m.Params, &p)
		i, _ := strconv.Atoi(p.Cursor)
		page := map[string]any{"tools": []ServerTool{}}
		if i < len(tools) {
			page["tools"] = tools[i : i+1]
			if i+1 < len(tools) {
				page["nextCursor"] = strconv.Itoa(i + 1)
			}
		}
		reply["result"] = page
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f, _ := os.OpenFile(callsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintln(f, p.Name)
		f.Close()
		switch p.Name {
		case "fail":
			reply["result"] = map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": "no such customer"}}}
		case "reject":
			reply["error"] = map[string]any{"code": -32602, "message": "bad arguments"}
		default:
			wd, _ := os.Getwd()
			text, _ := json.Marshal(map[string]any{"args": p.Arguments, "dir": wd, "token": os.Getenv("CRM_TOKEN"), "jailed": os.Getenv("JAILED"), "leaked": os.Getenv("DAEMON_SECRET")})
			reply["result"] = map[string]any{"content": []any{map[string]string{"type": "text", "text": string(text)}}, "structuredContent": map[string]any{"ok": true}}
		}
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "unknown method"}
	}
	return reply
}

// httpServer serves the fake over streamable HTTP: it requires the bearer
// token, assigns a session at initialize and requires it after, and
// answers tools/call as an event stream.
type httpServer struct {
	toolsFile, callsFile, token string
	mu                          sync.Mutex
	sessions                    int
	headers                     []http.Header
}

func (s *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.headers = append(s.headers, r.Header.Clone())
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodDelete {
		return
	}
	var m message
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if m.Method == "initialize" {
		s.mu.Lock()
		s.sessions++
		w.Header().Set("Mcp-Session-Id", "session-"+strconv.Itoa(s.sessions))
		s.mu.Unlock()
	} else if !strings.HasPrefix(r.Header.Get("Mcp-Session-Id"), "session-") {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	if len(m.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	b, _ := json.Marshal(answer(m, s.toolsFile, s.callsFile))
	if m.Method == "tools/call" {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// writeTools writes the fake's tools.
func writeTools(t *testing.T, path string, tools ...ServerTool) {
	t.Helper()
	b, _ := json.Marshal(tools)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// calls are the tools the fake has served, in order.
func calls(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func tempFile(t *testing.T, name string) string { return filepath.Join(t.TempDir(), name) }
