package devscope

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// TestName makes config identifiers of declared names.
func TestName(t *testing.T) {
	for in, want := range map[string]string{"notes": "notes", "acme.Board": "acme-board", "9lives": "x9lives", "a_b-c": "a_b-c"} {
		if got := Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGenerate writes one scope: the declared operations granted, an entry
// per type with its config and every secret, a route per handler then
// manual, a secret given by value through the environment and one given
// by reference as it is.
func TestGenerate(t *testing.T) {
	var m protocol.Manifest
	m.Provides.Operations = []protocol.OperationDecl{{Name: "weather.forecast"}}
	m.Provides.Extensions = []protocol.ExtensionDecl{{Type: "weather"}}
	m.Provides.Routes = []protocol.RouteDecl{{Handler: "weather"}}
	cfg, env, err := Generate([]protocol.Manifest{m}, Options{
		Agent:   "/bin/claude",
		Secrets: []Secret{{Name: "api_key", Value: "k"}, {Name: "token", Ref: "env:WEATHER_TOKEN"}},
		Entries: map[string]json.RawMessage{"weather": json.RawMessage(`{"service":"http://x"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != "OC_EXTENSIONTEST_SECRET_0=k" {
		t.Fatalf("env: %q", env)
	}
	var doc struct {
		Scopes []struct {
			ID       string `json:"id"`
			Bindings []struct {
				Operations []string       `json:"operations"`
				Config     map[string]any `json:"config"`
			} `json:"bindings"`
			Routes     []struct{ Handler string } `json:"routes"`
			Extensions []struct {
				Type        string          `json:"type"`
				SecretNames []string        `json:"secret_names"`
				Config      json.RawMessage `json:"config"`
			} `json:"extensions"`
			SecretRefs map[string]string `json:"secret_refs"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(cfg, &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.Scopes[0]
	var entry bytes.Buffer
	if len(s.Extensions) == 1 {
		_ = json.Compact(&entry, s.Extensions[0].Config)
	}
	if s.ID != ScopeID || strings.Join(s.Bindings[0].Operations, ",") != "read_context,wait,declare_outcome,weather.forecast" || s.Bindings[0].Config["binary"] != "/bin/claude" {
		t.Fatalf("binding: %+v", s.Bindings)
	}
	if len(s.Routes) != 2 || s.Routes[0].Handler != "weather" || s.Routes[1].Handler != "manual" {
		t.Fatalf("routes: %+v", s.Routes)
	}
	if len(s.Extensions) != 1 || entry.String() != `{"service":"http://x"}` || strings.Join(s.Extensions[0].SecretNames, ",") != "api_key,token" {
		t.Fatalf("extensions: %+v", s.Extensions)
	}
	if s.SecretRefs["api_key"] != "env:OC_EXTENSIONTEST_SECRET_0" || s.SecretRefs["token"] != "env:WEATHER_TOKEN" {
		t.Fatalf("secret refs: %v", s.SecretRefs)
	}

	if _, _, err := Generate([]protocol.Manifest{m}, Options{Entries: map[string]json.RawMessage{"nope": json.RawMessage(`{}`)}}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("an entry for an undeclared type: %v", err)
	}
}
