package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension/inproc"
)

// The host keeps config text behind handles and hands out only redacted
// copies: an edit's text and the value it replaced carry no reference text,
// while the text behind the handle (what the operator applies) keeps it.
func TestConfigHostRedactsBehindHandles(t *testing.T) {
	h := configHost{handles: &configHandles{}}
	base := []byte(`{"connections": [{"id": "gh", "type": "github", "secret_refs": {"token": "keyring:gh", "unset": false}}],
  "scopes": [{"id": "web", "name": "Web", "secret_refs": {"slack": "env:SLACK_TOKEN"}}]}`)
	handle := h.handles.put(base)
	if again := h.handles.put(base); again != handle {
		t.Fatalf("the same text got a new handle: %s, %s", handle, again)
	}
	ctx := context.Background()
	steps := []struct {
		op, pointer, value, before string
	}{
		{"replace", "/scopes/web/name", `"Website"`, `"Web"`},
		{"add", "/scopes/web/secret_refs/new", `"cmd:pass show new"`, ``},
		{"replace", "/connections/gh/secret_refs/token", `"keyring:gh2"`, `"[redacted]"`},
		{"replace", "/connections/gh/secret_refs", `{"token": "keyring:gh3"}`, `{"token":"[redacted]","unset":false}`},
		{"replace", "/connections/gh", `{"id": "gh", "type": "github"}`, `{"id":"gh","secret_refs":{"token":"[redacted]"},"type":"github"}`},
	}
	for _, s := range steps {
		e, err := h.Edit(ctx, handle, s.op, s.pointer, json.RawMessage(s.value))
		if err != nil {
			t.Fatalf("%s %s: %v", s.op, s.pointer, err)
		}
		if string(e.Before) != s.before {
			t.Errorf("%s: before %s, want %s", s.pointer, e.Before, s.before)
		}
		for _, ref := range []string{"keyring:", "env:", "cmd:"} {
			if strings.Contains(string(e.Text), ref) || strings.Contains(string(e.Before), ref) {
				t.Fatalf("%s: reference text reached the extension: %s %s", s.pointer, e.Text, e.Before)
			}
		}
		handle = e.Handle
	}
	full, ok := h.handles.get(handle)
	if !ok || !strings.Contains(string(full), "Website") || !strings.Contains(string(full), "cmd:pass show new") || !strings.Contains(string(full), "env:SLACK_TOKEN") {
		t.Fatalf("the held candidate lost its text: %s", full)
	}
	if _, err := h.Edit(ctx, "cfg_unknown", "add", "/scopes/-", json.RawMessage(`{}`)); !errors.Is(err, inproc.ErrConfigHandle) {
		t.Fatalf("an unknown handle: %v", err)
	}
	if _, _, err := h.Validate(ctx, "cfg_unknown"); !errors.Is(err, inproc.ErrConfigHandle) {
		t.Fatalf("validate an unknown handle: %v", err)
	}
}

// The host holds a bounded number of texts, dropping the least recently
// used.
func TestConfigHandlesAreBounded(t *testing.T) {
	var h configHandles
	first := h.put([]byte(`{"n": 0}`))
	for i := 1; i <= maxConfigHandles; i++ {
		h.put([]byte(`{"n": ` + strings.Repeat("1", i) + `}`))
		if i == maxConfigHandles/2 {
			h.get(first)
		}
	}
	if _, ok := h.get(first); !ok {
		t.Fatal("a recently used handle was dropped")
	}
	if len(h.byID) != maxConfigHandles || len(h.byText) != maxConfigHandles || len(h.order) != maxConfigHandles {
		t.Fatalf("holding %d/%d/%d texts", len(h.byID), len(h.byText), len(h.order))
	}
}

// add is RFC 6902 add: at an array index it inserts before the element
// there and replaces nothing, so it has no before; an element named by id
// or name already exists and is refused rather than overwritten.
func TestConfigHostAddInsertsIntoArrays(t *testing.T) {
	h := configHost{handles: &configHandles{}}
	base := []byte(`{"scopes": [{"id": "assist", "routes": [{"name": "threads", "handler": "slack"}]}]}`)
	ctx := context.Background()
	handle := h.handles.put(base)

	e, err := h.Edit(ctx, handle, "add", "/scopes/assist/routes/0", json.RawMessage(`{"name": "inbox-check", "handler": "schedule"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Before) != 0 {
		t.Fatalf("an insert replaced %s", e.Before)
	}
	e, err = h.Edit(ctx, e.Handle, "add", "/scopes/assist/routes/2", json.RawMessage(`{"name": "last"}`))
	if err != nil {
		t.Fatalf("add at the array's length: %v", err)
	}
	var doc struct {
		Scopes []struct {
			Routes []struct{ Name string } `json:"routes"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(e.Text, &doc); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range doc.Scopes[0].Routes {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "inbox-check,threads,last" {
		t.Fatalf("routes %v, want inbox-check,threads,last", names)
	}

	for _, p := range []string{"/scopes/assist/routes/threads", "/scopes/assist/routes/4", "/scopes/assist/routes/nope"} {
		if _, err := h.Edit(ctx, e.Handle, "add", p, json.RawMessage(`{"name": "x"}`)); err == nil {
			t.Errorf("add at %s was not refused", p)
		}
	}
	e, err = h.Edit(ctx, e.Handle, "replace", "/scopes/assist/routes/threads", json.RawMessage(`{"name": "threads", "handler": "slack2"}`))
	if err != nil || !strings.Contains(string(e.Before), `"slack"`) {
		t.Fatalf("replace by name: %v, before %s", err, e.Before)
	}
}
