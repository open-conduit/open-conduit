package config

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// sandboxRegistry registers entry type "stdio", whose entries must name a
// sandbox, "remote", which may not, and "either"; the "jail" provider
// refuses a policy that says "wide".
func sandboxRegistry() Registry {
	reg := registry()
	reg.Extensions["stdio"] = ExtensionType{Owner: "bridge", Sandbox: "required"}
	reg.Extensions["remote"] = ExtensionType{Owner: "bridge"}
	reg.Extensions["either"] = ExtensionType{Owner: "bridge", Sandbox: "optional"}
	reg.Sandboxes = map[string]func(json.RawMessage) error{"jail": func(raw json.RawMessage) error {
		if strings.Contains(string(raw), "wide") {
			return errors.New("the jail refuses a wide policy")
		}
		return nil
	}}
	return reg
}

func sandboxConfig(entry string) []byte {
	return []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"org","name":"org",
		"authorization":{"connections":[],"operations":["read_context"],"read_scopes":[]},
		"extensions":[` + entry + `],
		"bindings":[{"name":"agent","executor":"shell","requires":{"toolbelt":"cli"},"operations":["read_context"],"secret_names":[],"config":{}}]}]}`)
}

// An entry names a sandbox only when its type declares one, must when it
// requires one, and its provider checks the policy, as a binding's does.
func TestEntrySandbox(t *testing.T) {
	s := mustValidate(t, sandboxConfig(`{"name":"crm","type":"stdio","config":{},"secret_names":[],"sandbox":{"provider":"jail","config":{"network":["api.example.com"]}}}`), sandboxRegistry())
	x := s.Scopes["org"].Extensions[0]
	if x.Sandbox == nil || x.Sandbox.Provider != "jail" || string(x.Sandbox.Config) != `{"network":["api.example.com"]}` {
		t.Fatalf("entry sandbox = %+v", x.Sandbox)
	}
	mustValidate(t, sandboxConfig(`{"name":"crm","type":"either","config":{},"secret_names":[]}`), sandboxRegistry())
	mustValidate(t, sandboxConfig(`{"name":"crm","type":"remote","config":{},"secret_names":[]}`), sandboxRegistry())
	for _, c := range []struct{ name, entry, code string }{
		{"a required sandbox missing", `{"name":"crm","type":"stdio","config":{},"secret_names":[]}`, "sandbox_required"},
		{"a sandbox nothing would confine", `{"name":"crm","type":"remote","config":{},"secret_names":[],"sandbox":{"provider":"jail","config":{}}}`, "unsupported_sandbox"},
		{"an unknown provider", `{"name":"crm","type":"stdio","config":{},"secret_names":[],"sandbox":{"provider":"none","config":{}}}`, "unknown_registration"},
		{"a policy the provider refuses", `{"name":"crm","type":"stdio","config":{},"secret_names":[],"sandbox":{"provider":"jail","config":{"wide":true}}}`, "invalid_config"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(sandboxConfig(c.entry), sandboxRegistry())
			if err == nil {
				t.Fatal("accepted")
			}
			if got := codes(diagnostics(t, err)); !slices.Contains(got, c.code) {
				t.Fatalf("codes = %v, want %s", got, c.code)
			}
		})
	}
}
