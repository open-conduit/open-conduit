package human

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

func TestExtensionConfigureKeepsEachScopesFirstEntry(t *testing.T) {
	x := New()
	x.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{
		{ScopeID: "eng", Type: Type, Name: "decisions"},
		{ScopeID: "eng", Type: Type, Name: "later"},
		{ScopeID: "ops", Type: Type, Name: "broken", Config: json.RawMessage(`{"unknown":1}`)},
		{ScopeID: "web", Type: "other", Name: "x"},
	}})
	if e, ok := x.entry("eng"); !ok || e.Name != "decisions" {
		t.Fatalf("eng: %v %v", e, ok)
	}
	if _, ok := x.entry("ops"); ok {
		t.Fatal("an invalid first entry gives the scope none")
	}
	if _, ok := x.entry("web"); ok {
		t.Fatal("another type's entry is not a human entry")
	}
	if a := x.Actions("eng"); a["decisions"] == nil || len(a) != 1 {
		t.Fatalf("actions %v", a)
	}
	x.Configure(context.Background(), protocol.ConfigureParams{})
	if _, ok := x.entry("eng"); ok {
		t.Fatal("a later generation without the entry removes it")
	}
}
