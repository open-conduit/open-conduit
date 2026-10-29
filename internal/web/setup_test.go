package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

func setupFake(t *testing.T) (*fake, *Server) {
	t.Helper()
	f, s, _ := setup(t)
	f.work = map[string]client.Work{}
	f.config = &client.Config{Generation: "1", Scopes: []client.ConfigScope{
		{ID: "setup", Name: "Setup", Enabled: true, Extensions: []client.ConfigExtension{{Name: "setup", Type: "setup"}}},
		{ID: "repo", Name: "Repo", Enabled: true, Extensions: []client.ConfigExtension{{Name: "github", Type: "github"}}},
	}}
	return f, s
}

func payloadOf(schema string, v any) *client.Payload {
	data, _ := json.Marshal(v)
	return &client.Payload{Schema: schema, Data: data}
}

func TestSetupOffersTheStartActionWithItsModels(t *testing.T) {
	f, s := setupFake(t)
	f.actions["setup"] = []client.Action{{Provider: "setup", ID: "start", Label: "Start a setup session", Prompt: "the model the agent uses", Choices: []string{"opus", "sonnet"}}}
	body := do(s.Handler(addr), "GET", "/setup", nil, s).Body.String()
	for _, want := range []string{`name="scope" value="setup"`, `name="action" value="start"`, `value="opus" required checked`, `value="sonnet" required>`, "Start a setup session", `name="back" value="/setup?starting=1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if body := do(s.Handler(addr), "GET", "/setup?starting=1", nil, s).Body.String(); !strings.Contains(body, "Starting the session") || strings.Contains(body, `value="start"`) {
		t.Fatalf("a session just started is offered again:\n%s", body)
	}
}

func TestSetupWithoutASetupScopeFallsBackToTheTerminal(t *testing.T) {
	f, s := setupFake(t)
	f.config.Scopes = f.config.Scopes[1:]
	body := do(s.Handler(addr), "GET", "/setup", nil, s).Body.String()
	if !strings.Contains(body, "open-conduit init --with claude") || !strings.Contains(body, "has none") {
		t.Fatalf("no terminal fallback:\n%s", body)
	}
}

func TestSetupDrawsTheSessionAsAConversation(t *testing.T) {
	f, s := setupFake(t)
	id := "setup/setup/abc"
	f.work[id] = client.Work{ID: id, Kind: "setup.session", ScopeID: "setup", State: "waiting", Revision: "3", Context: payloadOf("setup.session.v1", map[string]string{"model": "sonnet"}), CreatedAt: at0(0), UpdatedAt: at0(0)}
	f.events = []client.Event{
		{ID: "e4", Sequence: "12", WorkID: id, Type: "setup.question", OccurredAt: at0(0), Data: payloadOf("setup.question.v1", map[string]any{"question_id": "q1", "text": "Which repository?", "choices": []string{"open-conduit", "dotfiles"}})},
		{ID: "e1", Sequence: "9", RoutedWorkID: id, Type: "setup.start", OccurredAt: at0(0), Data: payloadOf("setup.start.v1", map[string]string{"model": "sonnet"})},
		{ID: "e2", Sequence: "10", WorkID: id, Type: "setup.message", OccurredAt: at0(0), Data: payloadOf("setup.message.v1", map[string]string{"role": "agent", "text": "Claude Code is **signed in**."})},
		{ID: "e3", Sequence: "11", RoutedWorkID: id, Type: "setup.message", OccurredAt: at0(0), Data: payloadOf("setup.message.v1", map[string]string{"role": "operator", "text": "<b>hello</b>"})},
	}
	f.actions[id] = []client.Action{{Provider: "setup", ID: "message", Label: "Send the agent a message", Prompt: "your message"}, {Provider: "setup", ID: "answer", Label: "Answer the agent's question", Prompt: "a choice", Choices: []string{"open-conduit", "dotfiles"}}}
	body := do(s.Handler(addr), "GET", "/setup", nil, s).Body.String()
	signed, hello, asked := strings.Index(body, "<strong>signed in</strong>"), strings.Index(body, "&lt;b&gt;hello&lt;/b&gt;"), strings.Index(body, "Which repository?")
	if signed < 0 || hello < signed || asked < hello {
		t.Fatalf("conversation out of order or unescaped (%d %d %d):\n%s", signed, hello, asked, body)
	}
	for _, want := range []string{`name="work" value="setup/setup/abc"`, `name="action" value="answer"`, `<button class="choice" name="input" value="dotfiles">`, `name="action" value="message"`, "it reads this after you answer", "Setup · sonnet", "End session",
		`<textarea name="input" rows="1"`, `data-grow`, `/static/setup.js`, `data-live="setup-convo"`, `data-live="setup-bench"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="start"`) {
		t.Fatal("start offered beside an open session")
	}
	// The message box is outside every live region, so typing in it never
	// holds back the conversation's redraw.
	if strings.Contains(body, `data-live="setup"`) {
		t.Fatal("the whole page is one live region beside an open session")
	}

	// Answered, the question shows the choice taken and offers nothing.
	f.events = append(f.events, client.Event{ID: "e5", Sequence: "13", RoutedWorkID: id, Type: "setup.answer", OccurredAt: at0(0), Data: payloadOf("setup.answer.v1", map[string]string{"question_id": "q1", "answer": "dotfiles"})})
	f.actions[id] = f.actions[id][:1]
	w := f.work[id]
	w.State = "running"
	f.work[id] = w
	s.cachedWorld(t.Context(), true)
	body = do(s.Handler(addr), "GET", "/setup", nil, s).Body.String()
	if !strings.Contains(body, `class="choice is-on" disabled>dotfiles`) || strings.Contains(body, `value="answer"`) || !strings.Contains(body, "Waiting for an agent slot") && !strings.Contains(body, "The assistant is working") {
		t.Fatalf("answered question:\n%s", body)
	}
}

func TestSetupMsgsSkipOtherEvents(t *testing.T) {
	msgs := setupMsgs([]client.Event{
		{Sequence: "2", Type: "setup.proposal", Data: payloadOf("x", map[string]string{"text": "later"})},
		{Sequence: "1", Type: "setup.message", Data: payloadOf("setup.message.v1", map[string]string{"role": "agent", "text": "hi"})},
		{Sequence: "3", Type: "setup.message"},
	})
	if len(msgs) != 1 || msgs[0].Text != "hi" || msgs[0].Operator {
		t.Fatalf("msgs %+v", msgs)
	}
}

// TestSetupStartOffersTheBindingsGrants: the start form offers each grant
// the setup binding offers as a checkbox, the choices it sends reach the
// action as one input, and the session shows what was turned on.
func TestSetupStartOffersTheBindingsGrants(t *testing.T) {
	f, s := setupFake(t)
	f.config.Scopes[0].Routes = []client.ConfigRoute{{Route: client.Route{Name: "setup", Handler: "setup", Binding: "agent"}}}
	f.config.Scopes[0].Bindings = []client.ConfigBinding{{Name: "agent", Executor: "claude-code", Grants: []client.ConfigGrant{{Name: "shell", Label: "A shell on this machine"}, {Name: "connectors"}}}}
	f.actions["setup"] = []client.Action{{Provider: "setup", ID: "start", Label: "Start a setup session", Prompt: "the model the agent uses", Choices: []string{"opus", "sonnet"}}}
	body := do(s.Handler(addr), "GET", "/setup", nil, s).Body.String()
	for _, want := range []string{`<input type="checkbox" name="input" value="+shell"><span>A shell on this machine</span>`, `value="+connectors"><span>connectors</span>`, "for this session only"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	form := url.Values{"csrf": {s.csrf}, "scope": {"setup"}, "provider": {"setup"}, "action": {"start"}, "input": {"sonnet", "+shell"}, "back": {"/setup?starting=1"}}
	do(s.Handler(addr), "POST", "/actions", form, s)
	if len(f.invoked) != 1 || f.invoked[0].in != "sonnet +shell" {
		t.Fatalf("invoked %+v", f.invoked)
	}

	id := "setup/setup/abc"
	f.work[id] = client.Work{ID: id, Kind: "setup.session", ScopeID: "setup", State: "running", Revision: "1", Context: payloadOf("setup.session.v1", map[string]any{"model": "sonnet", "grants": []string{"shell", "gone"}}), CreatedAt: at0(0), UpdatedAt: at0(0)}
	s.cachedWorld(t.Context(), true)
	body = do(s.Handler(addr), "GET", "/setup", nil, s).Body.String()
	if !strings.Contains(body, "<b>Access</b><span>A shell on this machine; gone</span>") || strings.Contains(body, "It has no shell") {
		t.Fatalf("session access:\n%s", body)
	}
}
