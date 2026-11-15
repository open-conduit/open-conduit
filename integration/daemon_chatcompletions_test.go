package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest"
)

// chatRequest is one request the fake Chat Completions endpoint received.
type chatRequest struct {
	auth     string
	tools    []string
	messages []map[string]any
}

// TestDaemonChatCompletionsExecution runs the composed daemon with the
// Chat Completions executor installed as a package (extensions/
// chatcompletions, served by open-conduit extension-run chat-completions)
// beside the notes example package, against a fake endpoint. One turn: the
// tools are listed from the grant (the notes package's operation and the
// granted builtins); a call goes through the toolbelt to the package; wait
// parks the Work; the wake resumes the conversation with the history
// replayed; a 429 ends that attempt with a retry hint, from which the
// runtime schedules its retry; and the retry, taken at once with the
// operator's retry control, carries the turn on to its declared outcome.
func TestDaemonChatCompletionsExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon with installed packages")
	}
	bins := packageBinaries(t)
	notesPkg, _ := assemble(t, t.TempDir(), bins["notes-extension"])
	chatPkg, _ := hostPackage(t, t.TempDir(), "chat-completions")

	var mu sync.Mutex
	var got []chatRequest
	replies := []string{
		toolCalls(`{"id":"c1","type":"function","function":{"name":"notes_lookup","arguments":"{\"id\":\"missing\"}"}}`),
		toolCalls(`{"id":"c2","type":"function","function":{"name":"wait","arguments":"{\"match\":{\"types\":[\"drill.more\"],\"keys\":[{\"source\":\"drill\",\"external_id\":\"one\"}]},\"description\":\"more from drill\"}"}}`),
		"429",
		toolCalls(`{"id":"c3","type":"function","function":{"name":"declare_outcome","arguments":"{\"kind\":\"completed\",\"summary\":\"all done\"}"}}`),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		req := chatRequest{auth: r.Header.Get("Authorization"), messages: body.Messages}
		for _, tool := range body.Tools {
			req.tools = append(req.tools, tool.Function.Name)
		}
		mu.Lock()
		got = append(got, req)
		reply := `{"error":{"message":"script exhausted"}}`
		if len(replies) > 0 {
			reply, replies = replies[0], replies[1:]
		}
		mu.Unlock()
		if reply == "429" {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	requests := func() []chatRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}

	service := filepath.Join(t.TempDir(), "notes.json")
	if err := os.WriteFile(service, []byte(`{"notes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := []string{"read_context", "declare_outcome", "wait", "notes.lookup"}
	cfg, _ := json.Marshal(map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "drill", "name": "Drill",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "agent", "executor": "chat-completions", "requires": map[string]any{"toolbelt": "sdk", "inject": true, "resume": true}, "operations": ops, "secret_names": []any{"router"},
				"config": map[string]any{"base_url": srv.URL + "/v1", "model": "fake-model", "api_key_secret": "router"}}},
			"routes":      []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "inject", "config": map[string]any{}}},
			"extensions":  []any{map[string]any{"name": "notes", "type": "notes", "config": map[string]any{"service": service, "scope": "drill"}, "secret_names": []any{}}},
			"secret_refs": map[string]any{"router": "env:CHAT_DRILL_KEY"},
		}},
	})
	d := extensiontest.Daemon(t, extensiontest.Package(notesPkg), extensiontest.Package(chatPkg), extensiontest.Options{Config: cfg, Env: []string{"CHAT_DRILL_KEY=sk-drill"}})
	c := d.Client()
	submit := func(typ, delivery string) {
		t.Helper()
		if _, err := c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: "drill", Type: typ, DeliveryID: delivery, CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-"+delivery); err != nil {
			t.Fatal(err)
		}
	}
	work := func() client.Work {
		t.Helper()
		page, err := c.Works(context.Background(), url.Values{"scope_id": {"drill"}})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("work: %+v %v", page.Items, err)
		}
		return page.Items[0]
	}

	submit("drill.start", "d1")
	d.Until("the turn parked on its wait", extensiontest.Deadline, func() bool { return len(requests()) == 2 && work().State == "waiting" })
	first := requests()
	if names := strings.Join(first[0].tools, ","); names != "read_context,wait,declare_outcome,notes_lookup" {
		t.Fatalf("tools: %s", names)
	}
	if first[0].auth != "Bearer sk-drill" || !strings.Contains(fmt.Sprint(first[0].messages[1]["content"]), "drill.start") {
		t.Fatalf("first request: %+v", first[0])
	}
	// The lookup reached the package through the toolbelt, which has no
	// such note.
	if tool := first[1].messages[len(first[1].messages)-1]; tool["role"] != "tool" || !strings.Contains(fmt.Sprint(tool["content"]), "missing") {
		t.Fatalf("the lookup's result: %v", tool)
	}

	// The wake resumes the conversation; its first request is refused as
	// rate limited, which ends the attempt with a retry hint: the runtime
	// schedules the retry at the provider's reset, with its margin.
	submit("drill.more", "d2")
	var limited client.Run
	d.Until("the rate-limited attempt's retry scheduled", extensiontest.Deadline, func() bool {
		runs, err := c.Runs(context.Background(), url.Values{"work_id": {work().ID}})
		if err != nil || len(runs.Items) == 0 {
			return false
		}
		limited = runs.Items[0]
		return limited.State == "failed" && limited.Retry != nil && limited.Retry.EligibleAt != ""
	})
	if limited.Outcome == nil || limited.Outcome.Data == nil || !strings.Contains(string(limited.Outcome.Data.Data), `"reason":"rate_limit"`) || limited.Retry.FailureCount != 1 {
		t.Fatalf("the rate-limited attempt: %+v %+v", limited.Outcome, limited.Retry)
	}
	if at, err := time.Parse(time.RFC3339, limited.Retry.EligibleAt); err != nil || time.Until(at) < time.Minute {
		t.Fatalf("the retry is due at %q, want the reset plus the runtime's margin", limited.Retry.EligibleAt)
	}
	// Rather than wait for it, the operator retries now; the new attempt
	// carries the open turn on to its declared outcome.
	if _, err := c.RetryWork(context.Background(), work().ID, work().Revision, "drill", "retry-1"); err != nil {
		t.Fatal(err)
	}
	w := d.WaitWork(work().ID, "completed")
	all := requests()
	if len(all) != 4 {
		t.Fatalf("%d requests, want 4", len(all))
	}
	woken, retried := all[2].messages, all[3].messages
	since := fmt.Sprint(woken[len(woken)-1]["content"])
	if len(woken) != len(first[1].messages)+3 || !strings.Contains(since, "## Since your last turn") || !strings.Contains(since, "drill.more") {
		t.Fatalf("the wake did not replay the conversation: %d messages after %d, last %q", len(woken), len(first[1].messages), since)
	}
	// The retry carries the same conversation on: what the wake showed is
	// not shown again, and the one new input, the operator's retry, is.
	if len(retried) != len(woken)+1 || !strings.Contains(fmt.Sprint(retried[len(retried)-1]["content"]), "core.manual.retry") || strings.Count(fmt.Sprint(retried), "drill.more") != strings.Count(fmt.Sprint(woken), "drill.more") {
		t.Fatalf("the retry changed the conversation: %d messages, then %d: %v", len(woken), len(retried), retried[len(retried)-1])
	}

	runs, err := c.Runs(context.Background(), url.Values{"work_id": {w.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var retried429, completed bool
	for _, r := range runs.Items {
		if r.Outcome == nil || r.Outcome.Data == nil {
			continue
		}
		var data struct {
			Reason string `json:"reason"`
			Rounds []any  `json:"rounds"`
		}
		_ = json.Unmarshal(r.Outcome.Data.Data, &data)
		retried429 = retried429 || r.Outcome.Kind == "failed" && data.Reason == "rate_limit"
		completed = completed || r.Outcome.Kind == "completed" && data.Reason == "declared" && len(data.Rounds) == 1 && r.Usage != nil && r.Usage.InputTokens != nil
	}
	if !retried429 || !completed {
		raw, _ := json.Marshal(runs.Items)
		t.Fatalf("runs: %s", raw)
	}
}

// toolCalls is a Chat Completions reply asking for the tool calls given
// as JSON objects.
func toolCalls(calls ...string) string {
	return `{"model":"fake-model","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[` + strings.Join(calls, ",") + `]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
}
