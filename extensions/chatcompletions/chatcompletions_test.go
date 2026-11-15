package chatcompletions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/chatcompletions"
)

// request is one request the fake endpoint received.
type request struct {
	Header   http.Header
	Body     map[string]json.RawMessage
	Messages []map[string]any
	Tools    []struct {
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
}

func (r request) toolNames() []string {
	var out []string
	for _, t := range r.Tools {
		out = append(out, t.Function.Name)
	}
	return out
}

// answer is the fake endpoint's reply to one request.
type answer struct {
	status int
	header map[string]string
	body   string
	// gate, when set, is waited on before answering.
	gate chan struct{}
}

// fakeEndpoint answers requests from a script, one answer each, and
// records them.
type fakeEndpoint struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	script []answer
	got    []request
	seen   chan struct{}
}

func newEndpoint(t *testing.T, script ...answer) *fakeEndpoint {
	f := &fakeEndpoint{t: t, script: script, seen: make(chan struct{}, 100)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req request
		req.Header = r.Header.Clone()
		_ = json.Unmarshal(raw, &req.Body)
		_ = json.Unmarshal(req.Body["messages"], &req.Messages)
		_ = json.Unmarshal(req.Body["tools"], &req.Tools)
		f.mu.Lock()
		f.got = append(f.got, req)
		var a answer
		if len(f.script) > 0 {
			a, f.script = f.script[0], f.script[1:]
		} else {
			a = answer{status: 500, body: `{"error":{"message":"script exhausted"}}`}
		}
		f.mu.Unlock()
		f.seen <- struct{}{}
		if a.gate != nil {
			select {
			case <-a.gate:
			case <-r.Context().Done():
				return
			}
		}
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		if a.status == 0 {
			a.status = 200
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEndpoint) url() string { return f.srv.URL + "/v1" }

func (f *fakeEndpoint) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// text is a reply with text only.
func text(s string) answer {
	b, _ := json.Marshal(s)
	return answer{body: fmt.Sprintf(`{"model":"fake-1","choices":[{"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`, b)}
}

// calls is a reply asking for tool calls, each "id name args" with args a
// JSON text the reply carries as a string.
func calls(cs ...[3]string) answer {
	var out []map[string]any
	for _, c := range cs {
		tc := map[string]any{"type": "function", "function": map[string]any{"name": c[1], "arguments": c[2]}}
		if c[0] != "" {
			tc["id"] = c[0]
		}
		out = append(out, tc)
	}
	raw, _ := json.Marshal(out)
	return answer{body: fmt.Sprintf(`{"model":"fake-1","choices":[{"message":{"role":"assistant","content":null,"tool_calls":%s},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4},"cost":0.001}}`, raw)}
}

// fakeToolbelt is the toolbelt: a guide and recorded calls.
type fakeToolbelt struct {
	mu     sync.Mutex
	guide  client.ToolbeltGuide
	calls  []client.ToolCall
	refuse map[string]error
}

func (b *fakeToolbelt) ToolbeltGuide(context.Context) (client.ToolbeltGuide, error) {
	return b.guide, nil
}
func (b *fakeToolbelt) ToolbeltCall(_ context.Context, c client.ToolCall) (client.ToolResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, c)
	if err := b.refuse[c.Operation]; err != nil {
		return client.ToolResult{}, err
	}
	switch c.Operation {
	case extension.Wait:
		return client.ToolResult{Wait: &client.Wait{ID: "wait/1"}}, nil
	case extension.DeclareOutcome:
		return client.ToolResult{}, nil
	case extension.ReadContext:
		return client.ToolResult{Context: &client.Payload{Schema: "ctx.v1", Data: json.RawMessage(`{"k":"v"}`)}}, nil
	}
	return client.ToolResult{Output: &client.Payload{Schema: "out.v1", Data: json.RawMessage(`{"note":"note a"}`)}}, nil
}

func (b *fakeToolbelt) recorded() []client.ToolCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

func defaultGuide() client.ToolbeltGuide {
	return client.ToolbeltGuide{
		Guidance:   []string{"Look notes up with notes.lookup."},
		Operations: []client.OperationDoc{{Name: "notes.lookup", Summary: "Look a note up.", Input: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)}, {Name: "github.issue.comment", Summary: "Comment."}},
		Granted:    []string{"read_context", "notes.lookup", "wait", "declare_outcome", "github.issue.comment", "notes.undescribed"},
	}
}

type harness struct {
	t     *testing.T
	e     *chatcompletions.Executor
	ep    *fakeEndpoint
	belt  *fakeToolbelt
	state string
	n     int
}

func newHarness(t *testing.T, script ...answer) *harness {
	h := &harness{t: t, ep: newEndpoint(t, script...), belt: &fakeToolbelt{guide: defaultGuide()}, state: t.TempDir()}
	e, err := chatcompletions.New(chatcompletions.Options{StateDir: h.state, Dial: func(endpoint, token string) (chatcompletions.Toolbelt, error) {
		if endpoint != "unix:///toolbelt.sock" || token != "tok" {
			t.Errorf("dialled %q %q", endpoint, token)
		}
		return h.belt, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.e = e
	return h
}

func (h *harness) request(config map[string]any, inputs ...domain.Event) extension.RunRequest {
	h.n++
	if config == nil {
		config = map[string]any{}
	}
	if config["base_url"] == nil {
		config["base_url"] = h.ep.url()
	}
	if config["model"] == nil {
		config["model"] = "fake-model"
	}
	raw, _ := json.Marshal(config)
	if len(inputs) == 0 {
		inputs = []domain.Event{event(fmt.Sprintf("in-%d", h.n), "drill.start", `{"text":"hello"}`)}
	}
	return extension.RunRequest{
		Work:      domain.Work{ID: "work/one", ScopeID: "scope", CorrelationKeys: []domain.CorrelationKey{{Source: "github", ExternalID: "issue/42"}}},
		Execution: domain.Execution{ID: domain.ID(fmt.Sprintf("exec-%d", h.n)), WorkID: "work/one", Attempt: uint32(h.n), Lease: domain.Lease{Epoch: 1}},
		Trigger:   inputs[0],
		Inputs:    inputs,
		Context:   extension.ExecutionContext{Config: domain.Payload{Data: raw}, ToolbeltEndpoint: "unix:///toolbelt.sock", ToolbeltToken: "tok", Secrets: map[string]string{"router": "sk-secret"}},
		PersistRecovery: func(domain.Payload) error {
			return nil
		},
	}
}

func event(id, typ, data string) domain.Event {
	return domain.Event{ID: domain.ID(id), Type: typ, Source: "drill", DeliveryID: id, Data: domain.Payload{Schema: "drill.v1", Data: json.RawMessage(data)}, OccurredAt: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
}

func (h *harness) run(r extension.RunRequest) extension.Result {
	h.t.Helper()
	handle, err := h.e.Run(context.Background(), r)
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := handle.Wait(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	if err := extension.ValidateResult(res); err != nil {
		h.t.Fatal(err)
	}
	return res
}

func resultData(t *testing.T, r extension.Result) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(r.Outcome.Data.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConformance(t *testing.T) {
	ep := newEndpoint(t, text("one"), text("two"))
	raw, _ := json.Marshal(map[string]any{"base_url": ep.url(), "model": "m"})
	conformance.RunExecutor(t, func() extension.Executor {
		e, err := chatcompletions.New(chatcompletions.Options{StateDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}, extension.RunRequest{Work: domain.Work{ID: "w"}, Execution: domain.Execution{ID: "x", Lease: domain.Lease{Epoch: 1}}, Context: extension.ExecutionContext{Config: domain.Payload{Data: raw}}})
}

func TestInjectConformance(t *testing.T) {
	gate := make(chan struct{})
	ep := newEndpoint(t, answer{body: text("first").body, gate: gate}, text("took it in"))
	raw, _ := json.Marshal(map[string]any{"base_url": ep.url(), "model": "m"})
	e, err := chatcompletions.New(chatcompletions.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	conformance.RunInjectExecutor(t, func() conformance.InjectFixture {
		return conformance.InjectFixture{Executor: e,
			Request: extension.RunRequest{Work: domain.Work{ID: "w"}, Execution: domain.Execution{ID: "x", Lease: domain.Lease{Epoch: 1}}, Context: extension.ExecutionContext{Config: domain.Payload{Data: raw}}},
			Ready:   func() error { <-ep.seen; return nil },
			Finish:  func() error { close(gate); return nil }}
	})
	got := ep.requests()
	if len(got) != 2 || !strings.Contains(fmt.Sprint(got[1].Messages[len(got[1].Messages)-1]["content"]), "## Arrived during this turn") {
		t.Fatalf("the injected event was not folded into a second request: %+v", got)
	}
}

// TestToolLoop drives a turn: the tools are the granted operations (named
// for the provider, typed from their inputs) and builtins; a call goes
// through the toolbelt with its input as the payload's data and a stable
// request ID; declare_outcome ends the turn with its status as the turn's.
func TestToolLoop(t *testing.T) {
	h := newHarness(t,
		calls([3]string{"c1", "notes_lookup", `{"id":"a"}`}),
		calls([3]string{"c2", "declare_outcome", `{"kind":"handoff","summary":"looked it up","status":"review","result":"note a"}`}))
	res := h.run(h.request(map[string]any{"api_key_secret": "router", "headers": map[string]string{"HTTP-Referer": "https://example.com"}, "request": map[string]any{"temperature": 0.2}}))
	if res.Outcome.Kind != domain.OutcomeHandoff || res.Outcome.Summary != "looked it up" || res.Outcome.Data.Schema != domain.TurnSchema {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	d := resultData(t, res)
	if d["status"] != "review" || d["result"] != "note a" || d["reason"] != "declared" || d["tool_calls"] != float64(2) || len(d["rounds"].([]any)) != 2 {
		t.Fatalf("data: %v", d)
	}
	if *res.Usage.InputTokens != 40 || *res.Usage.OutputTokens != 10 || *res.Usage.CachedTokens != 8 || res.Usage.Currency != "USD" || *res.Usage.Cost < 0.0019 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	got := h.ep.requests()
	first := got[0]
	if names := first.toolNames(); strings.Join(names, ",") != "read_context,wait,declare_outcome,notes_lookup,github_issue_comment,notes_undescribed" {
		t.Fatalf("tools: %v", names)
	}
	if first.Header.Get("Authorization") != "Bearer sk-secret" || first.Header.Get("HTTP-Referer") != "https://example.com" || string(first.Body["temperature"]) != "0.2" || string(first.Body["model"]) != `"fake-model"` {
		t.Fatalf("request: %v %v", first.Header, first.Body)
	}
	if first.Messages[0]["role"] != "system" || !strings.Contains(first.Messages[0]["content"].(string), "Look notes up with notes.lookup.") {
		t.Fatalf("system message: %v", first.Messages[0])
	}
	if !strings.Contains(first.Messages[1]["content"].(string), "## Already waiting") {
		t.Fatalf("first message: %v", first.Messages[1])
	}
	if tool := got[1].Messages[len(got[1].Messages)-1]; tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != `{"note":"note a"}` {
		t.Fatalf("tool result: %v", tool)
	}
	belt := h.belt.recorded()
	if len(belt) != 2 || belt[0].Operation != "notes.lookup" || string(belt[0].Input) != `{"data":{"id":"a"}}` || !strings.HasPrefix(belt[0].RequestID, "chat-") {
		t.Fatalf("toolbelt calls: %+v", belt)
	}
	if belt[1].Operation != "declare_outcome" || string(belt[1].Input) != `{"kind":"handoff","summary":"looked it up"}` {
		t.Fatalf("declare: %+v", belt[1])
	}
}

// TestOperationsNarrowing offers only the operations the binding names,
// plus wait and declare_outcome.
func TestOperationsNarrowing(t *testing.T) {
	h := newHarness(t, calls([3]string{"c", "declare_outcome", `{"kind":"completed","summary":"ok"}`}))
	h.run(h.request(map[string]any{"operations": []string{"github.*"}}))
	if names := h.ep.requests()[0].toolNames(); strings.Join(names, ",") != "wait,declare_outcome,github_issue_comment" {
		t.Fatalf("tools: %v", names)
	}
}

// TestProviderQuirks: malformed argument JSON is answered as an error the
// model can correct, a call without an ID gets one, arguments sent as an
// object are read, and of parallel calls those after a wait are answered
// as not run.
func TestProviderQuirks(t *testing.T) {
	objectArgs := answer{body: `{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"notes_lookup","arguments":{"id":"b"}}}]}}]}`}
	h := newHarness(t,
		calls([3]string{"c1", "notes_lookup", `{"id":`}),
		objectArgs,
		calls([3]string{"", "read_context", ``}, [3]string{"", "wait", `{"match":{"types":["drill.more"]},"description":"more"}`}, [3]string{"", "notes_lookup", `{"id":"c"}`}))
	res := h.run(h.request(nil))
	if res.Outcome.Kind != domain.OutcomeHandoff || res.Outcome.Summary != "more" || resultData(t, res)["reason"] != "wait" {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	got := h.ep.requests()
	if bad := got[1].Messages[len(got[1].Messages)-1]; !strings.Contains(bad["content"].(string), "not a JSON object") {
		t.Fatalf("malformed arguments: %v", bad)
	}
	belt := h.belt.recorded()
	ops := []string{}
	for _, c := range belt {
		ops = append(ops, c.Operation)
	}
	if strings.Join(ops, ",") != "notes.lookup,read_context,wait" || string(belt[0].Input) != `{"data":{"id":"b"}}` {
		t.Fatalf("calls: %+v", belt)
	}
	// The conversation kept answers every call by a unique ID.
	th := thread(t, h.state)
	ids := map[string]bool{}
	for _, m := range th {
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || ids[tc.ID] {
				t.Fatalf("tool call IDs: %+v", th)
			}
			ids[tc.ID] = true
		}
	}
	last := th[len(th)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, "not run") {
		t.Fatalf("the call after wait: %+v", last)
	}
}

func thread(t *testing.T, state string) []chatcompletions.Message {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(state, "work", "*.json"))
	if len(matches) != 1 {
		t.Fatalf("threads: %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var th struct {
		Messages []chatcompletions.Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &th); err != nil {
		t.Fatal(err)
	}
	return th.Messages
}

// TestRateLimitRetries ends the attempt with a retry hint at the
// provider's Retry-After, and the next attempt carries the open turn on
// without showing its inputs again.
func TestRateLimitRetries(t *testing.T) {
	h := newHarness(t,
		calls([3]string{"c1", "notes_lookup", `{"id":"a"}`}),
		answer{status: 429, header: map[string]string{"Retry-After": "30"}, body: `{"error":{"message":"slow down"}}`},
		calls([3]string{"c2", "declare_outcome", `{"kind":"completed","summary":"done"}`}))
	r := h.request(nil)
	res := h.run(r)
	if res.Outcome.Kind != domain.OutcomeFailed || res.RetryHint == nil || res.RetryHint.ResetAt == nil || time.Until(*res.RetryHint.ResetAt) < 20*time.Second || res.Permanent {
		t.Fatalf("rate limit: %+v %+v", res.Outcome, res.RetryHint)
	}
	if d := resultData(t, res); d["reason"] != "rate_limit" {
		t.Fatalf("data: %v", d)
	}
	// The retry: the same inputs, as the runtime presents them again.
	r2 := h.request(nil, r.Inputs...)
	res = h.run(r2)
	if res.Outcome.Kind != domain.OutcomeCompleted || resultData(t, res)["kind"] != "continue" {
		t.Fatalf("retry: %+v %s", res.Outcome, res.Outcome.Data.Data)
	}
	got := h.ep.requests()
	if len(got[2].Messages) != len(got[1].Messages) {
		t.Fatalf("the retry changed the conversation: %d then %d messages", len(got[1].Messages), len(got[2].Messages))
	}
	if n := len(h.belt.recorded()); n != 2 {
		t.Fatalf("toolbelt calls: %d, want 2 (none repeated)", n)
	}
}

// TestFailuresClassified: a rate limit, or an outage that says when to
// come back, is a limit with a retry hint; another server error is an
// ordinary failure, retried on the runtime's delays; bad
// credentials won't and are permanent; a context-length refusal starts the
// next turn on a fresh conversation from the note.
func TestFailuresClassified(t *testing.T) {
	for _, c := range []struct {
		name      string
		a         answer
		reason    string
		retry     bool
		permanent bool
	}{
		{"server", answer{status: 503, body: `oops`}, "provider_unavailable", false, false},
		{"server with retry-after", answer{status: 503, header: map[string]string{"Retry-After": "60"}, body: `busy`}, "provider_unavailable", true, false},
		{"auth", answer{status: 401, body: `{"error":{"message":"bad key"}}`}, "auth", false, true},
		{"model", answer{status: 404, body: `{"error":{"message":"no such model"}}`}, "not_found", false, true},
		{"error in a 200", answer{body: `{"error":{"code":429,"message":"upstream rate limited"}}`}, "rate_limit", true, false},
		{"no choices", answer{body: `{"choices":[]}`}, "malformed_reply", false, false},
		{"context", answer{status: 400, body: `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`}, "context_limit", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.a)
			res := h.run(h.request(nil))
			d := resultData(t, res)
			if res.Outcome.Kind != domain.OutcomeFailed || d["reason"] != c.reason || (res.RetryHint != nil) != c.retry || res.Permanent != c.permanent {
				t.Fatalf("%+v %+v %v", res.Outcome, res.RetryHint, d)
			}
			if strings.Contains(string(res.Outcome.Data.Data), "sk-secret") {
				t.Fatal("the result holds the key")
			}
		})
	}
}

// TestWaitAndResume parks the turn on wait, and the wake replays the
// conversation with the events since.
func TestWaitAndResume(t *testing.T) {
	h := newHarness(t,
		calls([3]string{"c1", "wait", `{"match":{"types":["drill.more"]},"description":"a reply"}`}),
		calls([3]string{"c2", "declare_outcome", `{"kind":"completed","summary":"done"}`}))
	res := h.run(h.request(map[string]any{"playbook": "triage", "plugin_dir": playbooks(t)}))
	if res.Outcome.Kind != domain.OutcomeHandoff || res.Outcome.Summary != "a reply" {
		t.Fatalf("wait: %+v", res.Outcome)
	}
	r := h.request(map[string]any{"playbook": "triage", "plugin_dir": playbooks(t)}, event("more", "drill.more", `{"text":"here it is"}`))
	handle, err := h.e.Resume(context.Background(), r, domain.Payload{})
	if err != nil {
		t.Fatal(err)
	}
	if res, err = handle.Wait(context.Background()); err != nil || res.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("resume: %+v %v", res, err)
	}
	got := h.ep.requests()
	first, second := got[0].Messages, got[1].Messages
	if !strings.HasPrefix(first[1]["content"].(string), "Triage the issue.\n\nARGUMENTS: 42\n\n## Already waiting") {
		t.Fatalf("first turn: %q", first[1]["content"])
	}
	last := second[len(second)-1]["content"].(string)
	if len(second) != len(first)+3 || !strings.HasPrefix(last, "ARGUMENTS: 42 events\n\n## Since your last turn") || !strings.Contains(last, "here it is") {
		t.Fatalf("resume replayed %d messages after %d: %q", len(second), len(first), last)
	}
}

func playbooks(t *testing.T) string {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills", "triage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "triage", "SKILL.md"), []byte("---\nname: triage\n---\nTriage the issue.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestNudgeThenNoOutcome reminds a model that replied without ending its
// turn once, then fails the attempt.
func TestNudgeThenNoOutcome(t *testing.T) {
	h := newHarness(t, text("I think it's fine"), text("still fine"))
	res := h.run(h.request(nil))
	if res.Outcome.Kind != domain.OutcomeFailed || resultData(t, res)["reason"] != "no_outcome" {
		t.Fatalf("%+v", res.Outcome)
	}
	got := h.ep.requests()
	if len(got) != 2 || !strings.Contains(got[1].Messages[len(got[1].Messages)-1]["content"].(string), "declare_outcome") {
		t.Fatalf("no nudge: %+v", got)
	}
}

// TestHandoffOnSize starts a fresh conversation from the last turn's note
// once its last request carried more than handoff_tokens.
func TestHandoffOnSize(t *testing.T) {
	h := newHarness(t,
		calls([3]string{"c1", "declare_outcome", `{"kind":"handoff","summary":"halfway through the list"}`}),
		calls([3]string{"c2", "declare_outcome", `{"kind":"completed","summary":"done"}`}))
	cfg := map[string]any{"handoff_tokens": 10}
	h.run(h.request(cfg))
	res := h.run(h.request(cfg))
	if d := resultData(t, res); d["kind"] != "handoff" || d["handoff_reason"] != "size" {
		t.Fatalf("data: %v", d)
	}
	msgs := h.ep.requests()[1].Messages
	if len(msgs) != 2 || !strings.Contains(msgs[1]["content"].(string), "## Handoff from your last session\n\nhalfway through the list") {
		t.Fatalf("fresh conversation: %v", msgs)
	}
}

// TestInterrupt cancels the request in flight.
func TestInterrupt(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, answer{body: text("late").body, gate: gate})
	r := h.request(nil)
	handle, err := h.e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	<-h.ep.seen
	if err := h.e.Interrupt(context.Background(), r.Execution.ID); err != nil {
		t.Fatal(err)
	}
	res, err := handle.Wait(context.Background())
	if err != nil || res.Outcome.Kind != domain.OutcomeFailed || resultData(t, res)["reason"] != "interrupted" {
		t.Fatalf("%+v %v", res, err)
	}
	if err := h.e.Inject(context.Background(), r.Execution.ID, event("late", "x", `{}`)); !errors.Is(err, extension.ErrNotAccepting) {
		t.Fatalf("inject after the end: %v", err)
	}
}

// TestStop ends a running attempt; a newer epoch refuses an older stop.
func TestStop(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, answer{body: text("late").body, gate: gate})
	r := h.request(nil)
	r.Execution.Lease.Epoch = 3
	handle, err := h.e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	<-h.ep.seen
	if err := h.e.Stop(context.Background(), r.Execution.ID, 2); !errors.Is(err, chatcompletions.ErrSuperseded) {
		t.Fatalf("older stop: %v", err)
	}
	if err := h.e.Stop(context.Background(), r.Execution.ID, 3); err != nil {
		t.Fatal(err)
	}
	if res, _ := handle.Wait(context.Background()); resultData(t, res)["reason"] != "stopped" {
		t.Fatalf("%+v", res.Outcome)
	}
	if err := h.e.Stop(context.Background(), "unknown", 1); err != nil {
		t.Fatalf("stop of nothing: %v", err)
	}
	if err := h.e.Forget(context.Background(), r.Execution.ID); err != nil {
		t.Fatal(err)
	}
}

// TestLostCallsFinished: an attempt that ended between a reply asking for
// calls and their results leaves the next one to make them, with the same
// request IDs, before anything else.
func TestLostCallsFinished(t *testing.T) {
	h := newHarness(t,
		calls([3]string{"c1", "notes_lookup", `{"id":"a"}`}),
		calls([3]string{"c2", "declare_outcome", `{"kind":"completed","summary":"done"}`}))
	h.belt.refuse = map[string]error{"notes.lookup": context.Canceled}
	// The first call's answer is lost: make it look so by removing it.
	res := h.run(h.request(nil))
	_ = res
	path, _ := filepath.Glob(filepath.Join(h.state, "work", "*.json"))
	raw, _ := os.ReadFile(path[0])
	var th map[string]json.RawMessage
	_ = json.Unmarshal(raw, &th)
	var msgs []json.RawMessage
	_ = json.Unmarshal(th["messages"], &msgs)
	// Keep the user message and the reply asking for c1, as if the attempt
	// ended before c1's result was saved; the turn is still open.
	cut, _ := json.Marshal(msgs[:2])
	th["messages"], th["open"] = cut, json.RawMessage(`true`)
	raw, _ = json.Marshal(th)
	if err := os.WriteFile(path[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.belt.refuse = nil
	h.ep.mu.Lock()
	h.ep.script = []answer{calls([3]string{"c2", "declare_outcome", `{"kind":"completed","summary":"done"}`})}
	h.ep.mu.Unlock()
	before := h.belt.recorded()
	res = h.run(h.request(nil))
	if res.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("%+v %s", res.Outcome, res.Outcome.Data.Data)
	}
	after := h.belt.recorded()[len(before):]
	if len(after) < 1 || after[0].Operation != "notes.lookup" || after[0].RequestID != before[0].RequestID {
		t.Fatalf("the lost call was not made again with its request ID: %+v then %+v", before, after)
	}
}

func TestConfig(t *testing.T) {
	ok := `{"base_url":"https://openrouter.ai/api/v1","model":"m"}`
	if _, err := chatcompletions.ParseConfig([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{}`,
		`{"base_url":"ftp://x","model":"m"}`,
		`{"base_url":"https://u:p@x/v1","model":"m"}`,
		`{"base_url":"https://x/v1"}`,
		`{"base_url":"https://x/v1","model":"m","headers":{"Authorization":"Bearer x"}}`,
		`{"base_url":"https://x/v1","model":"m","headers":{"X-Api-Key":"x"}}`,
		`{"base_url":"https://x/v1","model":"m","request":{"messages":[]}}`,
		`{"base_url":"https://x/v1","model":"m","operations":["Bad Name"]}`,
		`{"base_url":"https://x/v1","model":"m","max_rounds":-1}`,
		`{"base_url":"https://x/v1","model":"m","unknown":1}`,
	} {
		if _, err := chatcompletions.ParseConfig([]byte(bad)); !errors.Is(err, chatcompletions.ErrConfig) {
			t.Errorf("%s accepted", bad)
		}
	}
	if err := chatcompletions.ValidateConfig(json.RawMessage(`{"base_url":"https://x/v1","model":"m","api_key_secret":"k"}`), nil); err == nil {
		t.Error("an unselected api_key_secret accepted")
	}
	// A header carrying a selected secret is refused at run time.
	h := newHarness(t)
	res := h.run(h.request(map[string]any{"headers": map[string]string{"X-Title": "sk-secret"}}))
	if !res.Permanent || resultData(t, res)["reason"] != "invalid_config" || len(h.ep.requests()) != 0 {
		t.Fatalf("%+v", res)
	}
}
