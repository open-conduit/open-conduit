package protocol_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// A package describes its own config types' fields (#494): the manifest
// accepts well-formed field declarations on every kind of type, and refuses
// malformed ones, each for its own reason.
func TestFieldDeclarations(t *testing.T) {
	ok := []protocol.FieldDecl{
		{Name: "service", Help: "the service's name", Required: true},
		{Name: "mode", Default: "fast", Options: []string{"fast", "slow"}},
		{Name: "watch", Default: "true"},
	}
	secret := protocol.FieldDecl{Name: "token", Help: "an API token", Secret: true}
	m := func(conn, entry, route, exec, res []protocol.FieldDecl) protocol.Manifest {
		return protocol.Manifest{Protocol: []int{1}, Name: "acme", Version: "1", Command: []string{"./acme"}, Provides: protocol.Provides{
			Connections: []protocol.TypeDecl{{Type: "acme", Fields: conn}},
			Extensions:  []protocol.ExtensionDecl{{Type: "acme", Fields: entry}},
			Routes:      []protocol.RouteDecl{{Handler: "acme", Fields: route}},
			Executors:   []protocol.ExecutorDecl{{Name: "acme", Capabilities: protocol.Capabilities{Toolbelt: "cli"}, Fields: exec}},
			Resources:   []protocol.ResourceDecl{{Type: "acme", Modes: []string{"shared"}, Fields: res}},
		}}
	}
	// A connection's secret_refs key may share a config field's name.
	conn := append([]protocol.FieldDecl{secret, {Name: "token", Help: "a config field of the same name"}}, ok...)
	if err := m(conn, ok, ok, ok, ok).Validate(); err != nil {
		t.Fatalf("well-formed fields: %v", err)
	}
	bad := map[string]protocol.FieldDecl{
		"must be a distinct field name":           {Name: "has space"},
		"help must be one line":                   {Name: "f", Help: "two\nlines"},
		"without surrounding spaces":              {Name: "f", Help: " padded"},
		"is required, so it has no default":       {Name: "f", Required: true, Default: "x"},
		"options must be distinct":                {Name: "f", Options: []string{"a", "a"}},
		"is not one of its options":               {Name: "f", Default: "c", Options: []string{"a", "b"}},
		"default must be one line":                {Name: "f", Default: "a\nb"},
		"only a connection type's fields are sec": {Name: "f", Secret: true},
	}
	bad["help must be one line"+" (long)"] = protocol.FieldDecl{Name: "f", Help: strings.Repeat("x", protocol.MaxFieldHelp+1)}
	for want, f := range bad {
		want, _, _ = strings.Cut(want, " (")
		fields := []protocol.FieldDecl{f}
		for kind, man := range map[string]protocol.Manifest{
			"entry":    m(nil, fields, nil, nil, nil),
			"route":    m(nil, nil, fields, nil, nil),
			"executor": m(nil, nil, nil, fields, nil),
			"resource": m(nil, nil, nil, nil, fields),
		} {
			err := man.Validate()
			if !errors.Is(err, protocol.ErrManifest) || !strings.Contains(err.Error(), want) {
				t.Errorf("%s field %+v: got %v, want %q", kind, f, err, want)
			}
		}
	}
	// The same name twice in one type is refused, on a connection too.
	dup := []protocol.FieldDecl{{Name: "f"}, {Name: "f"}}
	if err := m(dup, nil, nil, nil, nil).Validate(); err == nil || !strings.Contains(err.Error(), "distinct field name") {
		t.Errorf("duplicate connection field: %v", err)
	}
	if err := m([]protocol.FieldDecl{secret, secret}, nil, nil, nil, nil).Validate(); err == nil || !strings.Contains(err.Error(), "distinct field name") {
		t.Errorf("duplicate secret: %v", err)
	}
	// The manifest file carries them: a manifest with fields parses.
	raw := `{"protocol":[1],"name":"acme","version":"1","command":["./acme"],"provides":{"connections":[{"type":"acme","fields":[{"name":"token","help":"an API token","secret":true}]}]}}`
	parsed, err := protocol.ParseManifest([]byte(raw))
	if err != nil || len(parsed.Provides.Connections[0].Fields) != 1 || !parsed.Provides.Connections[0].Fields[0].Secret {
		t.Fatalf("parse: %+v %v", parsed.Provides.Connections, err)
	}
}
