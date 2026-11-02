package schedule

import (
	"encoding/json"
	"testing"
)

// The extension reads the code host's delivery payload from the event log by
// schema, so a finished check suite's conclusion must survive the decoding,
// or a job's on.conclusions filter never matches.
func TestDeliveryPayloadCarriesTheConclusion(t *testing.T) {
	data := []byte(`{"event":"check_suite","action":"completed","delivery":"d1","ref":"refs/heads/main","after":"abc1234","conclusion":"failure"}`)
	var p deliveryPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Conclusion != "failure" || p.After != "abc1234" || p.Ref != "refs/heads/main" {
		t.Fatalf("decoded %+v", p)
	}
}
