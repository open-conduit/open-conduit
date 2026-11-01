package protocol_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// A method a permission gates is enabled only when the manifest declares
// that permission, whatever surfaces it provides.
func TestPermissionsGateMethods(t *testing.T) {
	m := load(t, "ingress-support.json")
	m.Provides.ConfigCandidate = true
	gated := map[string]string{"telemetry.sink": protocol.PermissionTelemetrySink, "telemetry.capture": protocol.PermissionTelemetryCapture, "probe.list": protocol.PermissionProbes, "probe.run": protocol.PermissionProbes,
		"config.base": protocol.PermissionConfigRead, "config.edit": protocol.PermissionConfigRead, "config.validate": protocol.PermissionConfigRead, "config.trace": protocol.PermissionConfigRead, "config.candidate": protocol.PermissionConfigRead}
	for method, perm := range gated {
		if got, _ := protocol.Lookup(method); got.Permission != perm {
			t.Errorf("%s is gated by %q, want %q", method, got.Permission, perm)
		}
		if slices.Contains(m.Enabled(), method) {
			t.Errorf("%s is enabled without %s", method, perm)
		}
	}
	for _, p := range protocol.KnownPermissions {
		with := m
		with.Permissions = []string{p.Name}
		if err := with.Validate(); err != nil {
			t.Fatal(err)
		}
		for method, perm := range gated {
			if on := slices.Contains(with.Enabled(), method); on != (perm == p.Name) {
				t.Errorf("with %s: %s enabled %v", p.Name, method, on)
			}
		}
		if protocol.PermissionSummary(p.Name) == "" || !with.Allows(p.Name) {
			t.Errorf("%s: no summary or not allowed", p.Name)
		}
	}
}

// The host reads a package's declared settings and describe lines from an
// entry's config itself: declared fields of the declared type, defaults for
// absent ones, and nothing else.
func TestDeclaredSettingsAndDescribe(t *testing.T) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"team": "T1", "limit": 20, "ratio": 0.5, "channels": ["a", "b"], "verbose": "yes", "token": "secret",
		"jobs": [{"name": "daily", "when": "every day"}, {"name": "hourly", "when": 60}, {"when": "nameless"}, {"name": "bare"}]}`), &config); err != nil {
		t.Fatal(err)
	}
	decls := []protocol.SettingDecl{
		{Field: "team", Type: protocol.SettingString},
		{Field: "limit", Type: protocol.SettingNumber},
		{Field: "ratio", Type: protocol.SettingNumber},
		{Field: "channels", Type: protocol.SettingStrings},
		{Field: "verbose", Type: protocol.SettingBoolean},
		{Field: "manage", Type: protocol.SettingBoolean, Default: json.RawMessage(`true`)},
		{Field: "absent", Type: protocol.SettingString},
	}
	got := map[string]any{}
	for _, d := range decls {
		if v, ok := d.Value(config); ok {
			got[d.Field] = v
		}
	}
	want := map[string]any{"team": "T1", "limit": int64(20), "ratio": 0.5, "channels": []string{"a", "b"}, "manage": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings %v, want %v", got, want)
	}
	lines := protocol.DescribeDecl{List: "jobs", Name: "name", Detail: "when"}.Entries(config)
	wantLines := []protocol.DescribeEntry{{"daily", "every day"}, {"hourly", "60"}, {"bare", ""}}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("describe %v, want %v", lines, wantLines)
	}
}
