package bootstrap

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
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
	inprocRegistry(&reg, []inproc.Extension{&installed{m: m}})
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
	if plain := reg.Extensions["chat-plain"]; plain.Settings != nil || plain.Describe != nil {
		t.Fatal("a type that declares nothing shows fields by name alone")
	}
}
