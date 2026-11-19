package protocol_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// A request's wording and settle time cross the wire as they are
// (issue #762), and a request without them stays without.
func TestRequestWordingRoundTrips(t *testing.T) {
	for _, r := range []extension.Request{
		{ID: "ask/1", Label: "Approve or refuse deploy.run", State: extension.RequestRefused, By: "slack:U1", SettledAt: time.Date(2026, 11, 19, 7, 53, 0, 0, time.UTC),
			Wording: &extension.Wording{Wants: "deploy to prod", Fields: []extension.WordingField{{Name: "Env", Value: "prod"}}, Input: "{}", Note: "Escalated.",
				Choices: []extension.ChoiceWording{{Choice: "approve", Label: "Approve", Tone: extension.ToneAffirm, Confirm: "Sure?"}}}},
		{ID: "ask/2", Label: "Answer the agent's question", State: extension.RequestOpen},
	} {
		data, err := json.Marshal(protocol.FromRequest(r))
		if err != nil {
			t.Fatal(err)
		}
		var back protocol.Request
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if got := back.Extension(); !reflect.DeepEqual(got, r) {
			t.Fatalf("round trip:\n got %+v\nwant %+v\n%s", got, r, data)
		}
	}
}
