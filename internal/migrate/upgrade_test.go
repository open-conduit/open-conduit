package migrate

import (
	"strings"
	"testing"
)

// Upgrade renames only the renamed keys of the right entry type, keeping
// every other byte, and leaves an entry that sets both names to the operator.
func TestUpgradeRenamesSettingsInPlace(t *testing.T) {
	src := `{
  "version": 1,
  "scopes": [
    {"id": "eng", "extensions": [
      {"name": "ph", "type": "posthog", "config": {"turns": true, "introspection": false, "nested": {"introspection": 1}}},
      {"name": "other", "type": "schedule", "config": {"introspection": true}}
    ]},
    {"id": "ops", "extensions": [
      {"type": "posthog", "name": "both", "config": {"introspection": true, "telemetry_events": false}},
      {"type": "posthog", "name": "new", "config": {"telemetry_events": false}}
    ]}
  ]
}
`
	renames := Renames{"posthog": {"introspection": "telemetry_events"}}
	res, err := Upgrade([]byte(src), renames)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(src, `"turns": true, "introspection": false`, `"turns": true, "telemetry_events": false`, 1)
	if string(res.Config) != want {
		t.Fatalf("config:\n%s", res.Config)
	}
	if len(res.Items) != 2 || res.Items[0].Level != Changed || res.Items[0].Target != "/scopes/0/extensions/0/config/telemetry_events" ||
		res.Items[1].Level != Action || !strings.Contains(res.Items[1].Path, "both") {
		t.Fatalf("items %+v", res.Items)
	}
	if strings.Join(res.Scopes, ",") != "eng,ops" {
		t.Fatal(res.Scopes)
	}
	// Nothing to rename: the same bytes back.
	res, err = Upgrade([]byte(want), renames)
	if err != nil || string(res.Config) != want || len(res.Items) != 1 {
		t.Fatalf("second pass: %v %+v", err, res.Items)
	}
	if _, err := Upgrade([]byte("version: 1"), renames); err == nil {
		t.Fatal("not JSON")
	}
}
