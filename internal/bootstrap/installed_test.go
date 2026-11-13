package bootstrap

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
)

// A package's entry type shows the operator the settings and describe
// lines its manifest declares, read from the entry's config by the host:
// offline, with no process.
func TestInstalledEntrySettingsAndDescribe(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "chat", Version: "1", Command: []string{"./chat"}, Provides: protocol.Provides{
		Extensions: []protocol.ExtensionDecl{
			{Type: "chat", Settings: []protocol.SettingDecl{{Field: "team", Type: protocol.SettingString}, {Field: "manage", Type: protocol.SettingBoolean, Default: json.RawMessage(`true`)}},
				Describe: &protocol.DescribeDecl{List: "channels", Name: "id", Detail: "purpose"}},
			{Type: "chat-plain"},
		},
	}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := config.Registry{}
	packageRegistry(&reg, []*installed{{m: m}})
	entry := json.RawMessage(`{"team": "T1", "token_ref": "keyring:chat", "channels": [{"id": "C1", "purpose": "support"}]}`)
	typ := reg.Extensions["chat"]
	if typ.Settings == nil || typ.Describe == nil {
		t.Fatal("the declared settings and describe did not reach the registry")
	}
	if got, want := typ.Settings(entry), []extension.Setting{{Name: "team", Value: "T1"}, {Name: "manage", Value: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settings %v, want %v", got, want)
	}
	if got, want := typ.Describe(entry), []config.Entry{{Name: "C1", Detail: "support"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("describe %v, want %v", got, want)
	}
	// A type that declares nothing shows fields by name alone, and no lines
	// until the package describes a config it validated.
	if plain := reg.Extensions["chat-plain"]; plain.Settings != nil || plain.Describe != nil && plain.Describe(entry) != nil {
		t.Fatal("a type that declares nothing shows fields by name alone")
	}
}

// A package's run-once Work kind keeps RunOnce in the host's policy.
func TestInstalledRunOnceWorkKind(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1", Command: []string{"./acme"}, Provides: protocol.Provides{
		Routes: []protocol.RouteDecl{{Handler: "acme"}}, WorkKinds: []protocol.WorkKindDecl{{Kind: "acme.fix", RunOnce: true}, {Kind: "acme.job", Standing: true}}}}
	kinds := map[string]workKind{}
	for _, k := range (&installed{m: m}).WorkKinds() {
		kinds[k.Kind] = k
	}
	if !kinds["acme.fix"].RunOnce || kinds["acme.job"].RunOnce || !kinds["acme.job"].Standing {
		t.Fatalf("work kinds %+v", kinds)
	}
}

// A package's configurable route handler says which kinds of child Work a
// route binds through what it answered when it validated the route's
// config; before any validation, it binds none.
func TestInstalledRouteWorkKinds(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1", Command: []string{"./acme"}, Provides: protocol.Provides{
		Routes: []protocol.RouteDecl{{Handler: "acme", Config: true}, {Handler: "acme-plain"}}}}
	reg := config.Registry{}
	packageRegistry(&reg, []*installed{{m: m}})
	kinds := reg.HandlerWorkKinds["acme"]
	if kinds == nil {
		t.Fatal("a configurable route handler can't say which kinds it binds")
	}
	if got := kinds(json.RawMessage(`{"kinds":["acme.task.x"]}`)); len(got) != 0 {
		t.Fatalf("kinds before the package validated the config: %v", got)
	}
	if reg.HandlerWorkKinds["acme-plain"] != nil {
		t.Fatal("a route handler without config binds kinds")
	}
}
