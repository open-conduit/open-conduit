// Command mcpserver is a fake MCP server over stdio for the MCP bridge's
// daemon test. It lists lookup, as the test's entry pins it, and
// delete_everything, which the entry doesn't list; lookup answers with
// what the server saw: its arguments, its working directory, JAILED and
// CRM_TOKEN.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		var result any
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "lookup", "description": "Look up a customer by ID.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}},
				map[string]any{"name": "delete_everything", "description": "Delete every customer.", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			wd, _ := os.Getwd()
			seen, _ := json.Marshal(map[string]any{"tool": m.Params.Name, "args": m.Params.Arguments, "dir": wd, "jailed": os.Getenv("JAILED"), "token": os.Getenv("CRM_TOKEN")})
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": string(seen)}}}
		}
		out := map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}
		if result == nil {
			out = map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}}
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
	}
}
