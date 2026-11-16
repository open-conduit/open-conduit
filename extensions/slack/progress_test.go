package slack_test

import (
	"testing"

	"github.com/open-conduit/open-conduit/extensions/slack"
)

func TestProgressConfig(t *testing.T) {
	base := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `","channels":["` + chanA + `"]`
	for cfg, ok := range map[string]bool{
		base + `}`:                             true,
		base + `,"progress":{"enabled":true}}`: true,
		base + `,"progress":{"enabled":true,"stop":["slack:` + alice + `","slack:W0ENTERPRISE"]}}`:  true,
		base + `,"progress":{"stop":["slack:` + alice + `"]}}`:                                      false, // a Stop button on no message
		base + `,"progress":{"enabled":true,"stop":["` + alice + `"]}}`:                             false, // not slack:<user>
		base + `,"progress":{"enabled":true,"stop":["operator"]}}`:                                  false, // only Slack can vouch for a press
		base + `,"progress":{"enabled":true,"stop":["slack:C0NOTAUSER"]}}`:                          false,
		base + `,"progress":{"enabled":true,"stop":["slack:` + alice + `","slack:` + alice + `"]}}`: false,
	} {
		if _, err := slack.ParseExtension([]byte(cfg), nil); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok %v", cfg, err, ok)
		}
	}
	s, err := slack.ParseExtension([]byte(base+`,"progress":{"enabled":true,"stop":["slack:`+bob+`","slack:`+alice+`"]}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Progress || !s.MayStop(alice) || !s.MayStop(bob) || s.MayStop("UCAROL") || s.MayStop("") {
		t.Fatalf("settings: %+v", s)
	}
	if off := settings(t, ""); off.Progress || off.MayStop(alice) {
		t.Fatal("progress on without the setting")
	}
}
