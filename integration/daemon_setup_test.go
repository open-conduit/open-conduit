package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest"
)

// setupBinding is a setup session's agent: Claude Code (binary, keeping
// its state under root) with its built-in tools off and only the operations
// a session may have, offering a shell the operator may turn on for one
// session.
func setupBinding(root, binary string) map[string]any {
	return map[string]any{"name": "agent", "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp", "inject": true, "resume": true},
		"operations": []string{"read_context", "wait", "declare_outcome", "setup.say", "setup.ask"}, "secret_names": []any{},
		"config": map[string]any{"binary": binary, "playbook": "-", "builtin_tools": []string{}, "model": "opus", "models": []string{"opus", "sonnet"}, "cold_turns": "resume",
			"env": map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(root, "claude-config")}},
		"grants": []any{map[string]any{"name": "shell", "label": "A shell", "config": map[string]any{"builtin_tools": []string{"Bash"}}}}}
}

// setupDaemonConfig is a scope with a `setup` entry and route bound to
// binding.
func setupDaemonConfig(binding map[string]any) []byte {
	ops := binding["operations"]
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "setup", "name": "Setup",
			"authorization": map[string]any{"connections": []string{}, "operations": ops, "read_scopes": []any{}},
			"bindings":      []any{binding},
			"routes": []any{map[string]any{"name": "setup", "handler": "setup", "binding": "agent", "active_policy": "inject",
				"unsupported_fallback": "queue", "error_fallback": "queue", "config": map[string]any{}}},
			"extensions":  []any{map[string]any{"name": "setup", "type": "setup", "secret_names": []any{}, "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonSetupSession drives a setup session through the composed
// daemon: the operator starts it from the scope on the model they chose,
// the agent's message and question arrive as typed Events on its Work, a
// message sent while it is parked waits for the answer, and the answer
// resumes it with both. The shell the operator turned on is in every turn.
// An agent with a shell for every session is refused.
func TestDaemonSetupSession(t *testing.T) {
	fake := t.TempDir()
	// The first turn says hello and asks a question; the next keeps what
	// it was resumed with and finishes.
	claude, err := extensiontest.FakeClaude(fake,
		extensiontest.Turn{State: "help", Calls: []extensiontest.Call{
			{Operation: "setup.say", RequestID: "hello", Input: map[string]any{"text": "Claude Code is installed and signed in."}},
			{Operation: "setup.ask", RequestID: "repo", Input: map[string]any{"question": "Which repository?", "choices": []string{"octo/widgets", "octo/gadgets"}}},
		}},
		extensiontest.Turn{State: "finished"},
	)
	if err != nil {
		t.Fatal(err)
	}
	d := extensiontest.Daemon(t, extensiontest.Options{Config: setupDaemonConfig(setupBinding(fake, claude.Path)), Packages: []string{setupPackage(t)}})
	c := d.Client()
	ctx := context.Background()
	for name, b := range map[string]map[string]any{
		"a shell": {"name": "agent", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"},
			"secret_names": []any{}, "config": map[string]any{"command": "true"}},
		"built-in tools on": func() map[string]any {
			b := setupBinding(fake, claude.Path)
			delete(b["config"].(map[string]any), "builtin_tools")
			return b
		}(),
	} {
		if _, err := c.ValidateConfig(ctx, setupDaemonConfig(b)); err == nil || !strings.Contains(err.Error(), "binding_refused") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if cfg, err := c.Config(ctx); err != nil || len(cfg.Scopes) != 1 || len(cfg.Scopes[0].Bindings) != 1 || len(cfg.Scopes[0].Bindings[0].Grants) != 1 || cfg.Scopes[0].Bindings[0].Grants[0].Label != "A shell" {
		t.Fatalf("the config view's grants: %+v %v", cfg, err)
	}
	scope := client.ActionTarget{ScopeID: "setup"}
	ids := func(target client.ActionTarget) string {
		acts, _ := c.Actions(ctx, target)
		var out []string
		for _, a := range acts {
			if a.Provider == "setup" {
				out = append(out, a.ID)
			}
		}
		return strings.Join(out, ",")
	}
	if got := ids(scope); got != "start" {
		t.Fatalf("scope actions: %q", got)
	}
	// The setup extension declares its page: a session of setup.session
	// work, started by the start action through the setup route, whose
	// binding offers the shell grant (above).
	pages, err := c.Pages(ctx)
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages: %+v %v", pages, err)
	}
	if p := pages[0]; p.Extension != "setup" || p.ID != "setup" || p.Title != "Setup" || p.WorkKind != "setup.session" || p.Start != "start" || p.Route != "setup" {
		t.Fatalf("the setup page: %+v", p)
	}
	if _, err := c.InvokeAction(ctx, scope, "setup", "start", "sonnet +web", "bad-start"); err == nil || !strings.Contains(err.Error(), "+shell") {
		t.Fatalf("a grant the binding doesn't offer: %v", err)
	}
	res, err := c.InvokeAction(ctx, scope, "setup", "start", "sonnet +shell", "start")
	if err != nil || res.Event.Type != "setup.start" || !strings.HasPrefix(res.Event.RoutedWorkID, "setup/setup/") {
		t.Fatalf("start: %+v %v", res, err)
	}
	session := client.ActionTarget{WorkID: res.Event.RoutedWorkID}
	work := func() client.Work {
		w, _ := c.Work(ctx, session.WorkID)
		return w
	}
	if w := work(); w.Kind != "setup.session" || w.Display == nil || w.Display.Title != "Setup session" || w.Context == nil || string(w.Context.Data) != `{"model":"sonnet","grants":["shell"]}` {
		t.Fatalf("session: %+v", w)
	}
	if got := ids(scope); got != "" {
		t.Fatalf("a second start is offered while the session is open: %q", got)
	}
	d.Until("the agent parked on its question", 30*time.Second, func() bool { return work().State == "waiting" && ids(session) == "answer,message" })

	events := func(typ string) []client.Event {
		page, _ := c.Events(ctx, url.Values{"work_id": {session.WorkID}, "type": {typ}})
		return page.Items
	}
	said, asked := events("setup.message"), events("setup.question")
	if len(said) != 1 || !strings.Contains(string(said[0].Data.Data), "signed in") || said[0].CorrelationKey != nil {
		t.Fatalf("said: %+v", said)
	}
	if len(asked) != 1 || !strings.Contains(string(asked[0].Data.Data), `"choices":["octo/widgets","octo/gadgets"]`) {
		t.Fatalf("asked: %+v", asked)
	}
	// The session draws itself as panels: a conversation of the agent's
	// message, then its question with the choices (the answer action,
	// above, answers it), and the model and access the operator chose.
	panels, err := c.Panels(ctx, session)
	if err != nil || len(panels) != 2 || panels[0].ID != "conversation" || panels[1].ID != "session" {
		t.Fatalf("panels: %s %v", mustJSONString(panels), err)
	}
	var messages []client.PanelMessage
	for _, s := range panels[0].Sections {
		messages = append(messages, s.Messages...)
	}
	if len(messages) != 2 || messages[0].From != "agent" || !strings.Contains(messages[0].Text, "signed in") {
		t.Fatalf("the conversation: %s", mustJSONString(messages))
	}
	if q := messages[1]; q.Label != "Assistant asks" || q.Text != "Which repository?" || strings.Join(q.Choices, ",") != "octo/widgets,octo/gadgets" || q.Chosen != "" {
		t.Fatalf("the question: %s", mustJSONString(q))
	}
	if got := mustJSONString(panels[1].Sections); !strings.Contains(got, `{"name":"Model","value":"sonnet"}`) || !strings.Contains(got, `{"name":"Access","value":"A shell"}`) {
		t.Fatalf("the session's panel: %s", got)
	}

	if _, err = c.InvokeAction(ctx, session, "setup", "message", "I use the work account.", "msg"); err != nil {
		t.Fatalf("message: %v", err)
	}
	if w := work(); w.State != "waiting" {
		t.Fatalf("a message woke the question: %s", w.State)
	}
	// Choices are suggestions, so only a blank answer is refused.
	if _, err = c.InvokeAction(ctx, session, "setup", "answer", "   ", "bad"); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("invalid answer: %v", err)
	}
	if _, err = c.InvokeAction(ctx, session, "setup", "answer", "2", "answer"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	d.Until("the session finished", 30*time.Second, func() bool { return work().State == "completed" })
	turns := claude.Turns()
	if len(turns) != 2 {
		t.Fatalf("turns: %+v", turns)
	}
	if resumed := turns[1].Input; !strings.Contains(resumed, "answered your question") || !strings.Contains(resumed, "octo/gadgets") || !strings.Contains(resumed, "work account") {
		t.Fatalf("resumed with: %s", resumed)
	}
	for _, turn := range turns {
		if line := strings.Join(turn.Args, " "); !strings.Contains(line, "--model sonnet") || !strings.Contains(line, " --tools Bash") {
			t.Fatalf("a turn ran with: %s", line)
		}
	}
	if got := ids(scope); got != "start" {
		t.Fatalf("scope actions once the session ended: %q", got)
	}
}

// TestDaemonDefaultConfiguration: with no config file and no stored
// generation the daemon starts on the default configuration, a setup scope,
// without writing the file; doctor says so and the web view can start a
// session. The first apply writes the file, and a restart keeps it.
func TestDaemonDefaultConfiguration(t *testing.T) {
	d := newDrill(t)
	if err := os.Remove(d.config); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	cfg, err := d.c.Config(ctx)
	if err != nil || cfg.Generation != "1" || len(cfg.Scopes) != 1 || cfg.Scopes[0].ID != "setup" {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if _, err = os.Stat(d.config); !os.IsNotExist(err) {
		t.Fatalf("the default configuration wrote the file: %v", err)
	}
	code := func() string {
		doc, err := d.c.Doctor(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Checks {
			if c.Name == "config" {
				return c.Status + "/" + c.Code
			}
		}
		return ""
	}
	if got := code(); got != "info/default_config" {
		t.Fatalf("doctor config: %s", got)
	}
	acts, err := d.c.Actions(ctx, client.ActionTarget{ScopeID: "setup"})
	if err != nil || len(acts) != 1 || acts[0].ID != "start" || strings.Join(acts[0].Choices, ",") != "opus,sonnet" {
		t.Fatalf("actions: %+v %v", acts, err)
	}

	d.kill()
	d.start("")
	if got := code(); got != "info/default_config" {
		t.Fatalf("doctor config after a restart: %s", got)
	}
	// The session's agent never runs here.
	binary := filepath.Join(d.root, "claude")
	if err = os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	next := setupDaemonConfig(setupBinding(d.root, binary))
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(d.config); err != nil {
		t.Fatalf("apply wrote no file: %v", err)
	}
	if got := code(); got != "ok/current" {
		t.Fatalf("doctor config after apply: %s", got)
	}
}
