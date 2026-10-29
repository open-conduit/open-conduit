package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBindingGranted(t *testing.T) {
	b := Binding{Name: "agent", Config: Payload{Schema: "x", Data: json.RawMessage(`{"tools":[],"model":"opus"}`)},
		Grants: []BindingGrant{{Name: "shell", Config: json.RawMessage(`{"tools":["Bash"]}`)}, {Name: "web", Config: json.RawMessage(`{"tools":["WebFetch"],"strict":false}`)}, {Name: "ops", Operations: []string{"emit"}}}}
	for _, c := range []struct {
		names, applied []string
		want           map[string]any
	}{
		{nil, nil, map[string]any{"tools": []any{}, "model": "opus"}},
		{[]string{"shell"}, []string{"shell"}, map[string]any{"tools": []any{"Bash"}, "model": "opus"}},
		// Later grants in the binding's order win, whatever order the Work names them in.
		{[]string{"web", "shell", "ops", "gone"}, []string{"shell", "web", "ops"}, map[string]any{"tools": []any{"WebFetch"}, "model": "opus", "strict": false}},
	} {
		got, applied := b.Granted(c.names)
		var cfg map[string]any
		if err := json.Unmarshal(got.Config.Data, &cfg); err != nil || !reflect.DeepEqual(cfg, c.want) || !reflect.DeepEqual(applied, c.applied) || got.Config.Schema != "x" {
			t.Errorf("%v: %s %v", c.names, got.Config.Data, applied)
		}
	}
	if string(b.Config.Data) != `{"tools":[],"model":"opus"}` {
		t.Fatal("Granted changed the binding it was called on")
	}
}
