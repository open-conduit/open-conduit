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
	want := []extension.Setting{{Name: "repo", Value: "o/r"}, {Name: "track_issues", Value: true}, {Name: "base_branch", Value: "dev"}, {Name: "manage_webhook", Value: false}, {Name: "closed_horizon_hours", Value: 168}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings: %+v", got)
	}
	if ExtensionSettings(json.RawMessage(`{"repo":1}`)) != nil {
		t.Fatal("settings of a rejected config")
	}
}
