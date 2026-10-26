package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli"
)

const editDoc = `{
  "version": 1,
  "connections": [
    {"id": "linear", "type": "linear", "config": {}, "secret_refs": {"token": "keyring:linear-token"}}
  ],
  "scopes": [
    {"id": "eng", "extensions": [{"name": "eng", "type": "linear", "config": {"namespace": "linear/eng", "scope": "eng", "teams": ["ENG"]}}]}
  ]
}`

func editKeys(m tea.Model, keys ...string) tea.Model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m, _ = m.Update(msg)
	}
	return m
}

func moveTo(t *testing.T, m tea.Model, ptr string) tea.Model {
	t.Helper()
	em := m.(EditModel)
	for i, e := range em.entries {
		if e.Pointer == ptr {
			em.cursor = i
			return em
		}
	}
	t.Fatalf("no entry %s", ptr)
	return m
}

func TestConfigEditor(t *testing.T) {
	doc, err := cli.ParseConfigDoc([]byte(editDoc))
	if err != nil {
		t.Fatal(err)
	}
	var saved []byte
	invalid := false
	catalog := []cli.Adapter{{Type: "linear", Title: "Linear",
		Connection: []cli.Field{{Name: "api_url", Default: "https://api.linear.app/graphql"}, {Name: "agent_account", Help: "a user whose comments count as an agent's"}},
		Secrets:    []cli.Field{{Name: "token", Required: true}},
		Entry:      []cli.Field{{Name: "namespace", Required: true}, {Name: "teams", Required: true, Help: "team keys"}, {Name: "states", Default: "unstarted, started"}}}}
	req := cli.EditRequest{Path: "/tmp/config.json", Doc: doc, Catalog: catalog,
		Validate: func(b []byte) ([]client.Detail, error) {
			if invalid {
				return []client.Detail{{Path: "/scopes/0", Code: "invalid", Message: "bad"}}, nil
			}
			return nil, nil
		},
		Save: func(b []byte) error { saved = b; return nil }}
	var m tea.Model = NewEditor(req)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	// The entry's documented fields show, marking those present.
	m = moveTo(t, m, "/scopes/0/extensions/0/config/teams")
	view := m.View()
	for _, want := range []string{"Linear entry config", "✓ teams (required) — team keys", "  states (default unstarted, started)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q:\n%s", want, view)
		}
	}
	// Edit a value.
	m = editKeys(m, "e")
	m = editKeys(m, "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace")
	m = editKeys(m, `["ENG","OPS"]`, "enter")
	// Add a field beside it.
	m = moveTo(t, m, "/scopes/0/extensions/0/config/namespace")
	m = editKeys(m, "a", "states", "enter", `["Backlog"]`, "enter")
	// Secret refs are redacted and must stay references.
	m = moveTo(t, m, "/connections/0/secret_refs/token")
	if !strings.Contains(m.View(), `token: "[redacted]"`) || strings.Contains(m.View(), "keyring:linear-token") {
		t.Fatalf("secret reference shown:\n%s", m.View())
	}
	m = editKeys(m, "e", `"plain-secret"`, "enter")
	if em := m.(EditModel); len(em.errs) == 0 || em.mode != valueEdit {
		t.Fatalf("a secret value was accepted as a reference: %+v %v %q", em.errs, em.mode, em.notice)
	}
	m = editKeys(m, "esc")
	// An invalid document is never saved.
	invalid = true
	m = editKeys(m, "s")
	if saved != nil || !strings.Contains(m.View(), "/scopes/0  invalid  bad") {
		t.Fatalf("invalid save:\n%s", m.View())
	}
	// Quit asks once when there are unsaved changes.
	m = editKeys(m, "q")
	if m.(EditModel).quit {
		t.Fatal("quit with unsaved changes did not ask")
	}
	invalid = false
	m = editKeys(m, "s")
	if !m.(EditModel).Saved() || !strings.Contains(string(saved), `"teams": [`) || !strings.Contains(string(saved), `"OPS"`) || !strings.Contains(string(saved), `"states": [`) {
		t.Fatalf("saved:\n%s", saved)
	}
	if !strings.Contains(string(saved), `"keyring:linear-token"`) {
		t.Fatal("the saved document lost the real reference")
	}
	// Delete, then quit without saving asks twice.
	m = moveTo(t, m, "/scopes/0/extensions/0/config/states")
	m = editKeys(m, "d", "q", "q")
	if !m.(EditModel).quit {
		t.Fatal("second q quits")
	}
}
