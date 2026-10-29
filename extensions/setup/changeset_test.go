package setup

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// A proposal names references, never values, in every secret_refs slot it
// reaches, and nothing the agent reads back or an Event carries holds
// reference text.
func TestReferencesAndRedaction(t *testing.T) {
	for name, c := range map[string]struct {
		pointer, value string
		ok             bool
	}{
		"a reference":           {"/scopes/web/secret_refs/token", `"keyring:github-token"`, true},
		"a literal":             {"/scopes/web/secret_refs/token", `"ghp_abc"`, false},
		"not a string":          {"/scopes/web/secret_refs/token", `{"x":1}`, false},
		"a whole secret_refs":   {"/scopes/web/secret_refs", `{"token":"env:GH_TOKEN"}`, true},
		"a literal in the refs": {"/scopes/web/secret_refs", `{"token":"plain"}`, false},
		"a nested scope":        {"/scopes/-", `{"id":"web","secret_refs":{"token":"plain"}}`, false},
		"a nested reference":    {"/scopes/-", `{"id":"web","secret_refs":{"token":"cmd:gh auth token"}}`, true},
		"no secrets at all":     {"/scopes/web/name", `"Web"`, true},
	} {
		err := checkReferences(c.pointer, json.RawMessage(c.value))
		var refusal *extension.OperationError
		if c.ok != (err == nil) || (err != nil && (!errors.As(err, &refusal) || refusal.Code != "not_a_reference")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct{ pointer, value, want string }{
		"a slot":        {"/scopes/web/secret_refs/token", `"keyring:x"`, `"[redacted]"`},
		"the refs":      {"/scopes/web/secret_refs", `{"token":"keyring:x"}`, `{"token":"[redacted]"}`},
		"nested":        {"/scopes/-", `{"id":"web","secret_refs":{"token":"keyring:x"}}`, `{"id":"web","secret_refs":{"token":"[redacted]"}}`},
		"nothing to do": {"/scopes/web/name", `"Web"`, `"Web"`},
	} {
		if got := string(redact(c.pointer, json.RawMessage(c.value))); got != c.want {
			t.Errorf("%s: %s", name, got)
		}
	}
}

func TestInFolder(t *testing.T) {
	cs := changeSet{Folders: []string{"web", "a/b"}}
	for pointer, want := range map[string]bool{"/scopes/web/name": true, "/scopes/a~1b/name": true, "/scopes/home/name": false, "/settings/x": false, "/scopes/-": false} {
		if inFolder(cs, pointer) != want {
			t.Errorf("%s: %v", pointer, !want)
		}
	}
}

// Approve all names the open changes it was listed for.
func TestRevision(t *testing.T) {
	a, b := revision([]change{{ID: "c1"}, {ID: "c2"}}), revision([]change{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}})
	if a == b || len(a) != 8 || a != revision([]change{{ID: "c1"}, {ID: "c2"}}) {
		t.Fatalf("revisions: %s %s", a, b)
	}
}
