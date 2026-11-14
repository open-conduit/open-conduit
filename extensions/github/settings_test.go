package github

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// The entry's settings are its repository and policy, with defaults, and
// never the webhook secret's name.
func TestExtensionSettings(t *testing.T) {
	got := ExtensionSettings(json.RawMessage(`{"namespace":"github/o/r","scope":"eng","repo":"o/r","webhook_secret":"hook","base_branch":"dev"}`))
	want := []extension.Setting{{Name: "repo", Value: "o/r"}, {Name: "track_issues", Value: true}, {Name: "base_branch", Value: "dev"}, {Name: "manage_webhook", Value: false}, {Name: "track_pull_requests", Value: false}, {Name: "acknowledge_issues", Value: false}, {Name: "admit_authors", Value: "OWNER, MEMBER, COLLABORATOR"}, {Name: "admit_forks", Value: false}, {Name: "closed_horizon_hours", Value: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings: %+v", got)
	}
	got = ExtensionSettings(json.RawMessage(`{"namespace":"github/o/r","scope":"eng","repo":"o/r","workflows":["release.yml","release.yml","ci.yaml"]}`))
	if last := got[len(got)-1]; last != (extension.Setting{Name: "workflows", Value: "release.yml, ci.yaml"}) {
		t.Fatalf("workflows setting: %+v", last)
	}
	for _, bad := range []string{`"../release.yml"`, `"release"`, `""`, `"a/b.yml"`} {
		if _, err := ParseExtension(json.RawMessage(`{"namespace":"github/o/r","scope":"eng","repo":"o/r","workflows":[`+bad+`]}`), nil); err == nil {
			t.Fatalf("workflows %s accepted", bad)
		}
	}
	if ExtensionSettings(json.RawMessage(`{"repo":1}`)) != nil {
		t.Fatal("settings of a rejected config")
	}
}
