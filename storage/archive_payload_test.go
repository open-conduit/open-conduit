package storage

import (
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

// TestNoPayloadSurvivesEncoding keeps a record without a payload the same
// record after a JSON round trip, which turns it into the text "null". An
// empty object is a payload, and is the negative control.
func TestNoPayloadSurvivesEncoding(t *testing.T) {
	record := domain.AcceptedEvent{Sequence: 1, Event: domain.Event{ID: "e"}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.AcceptedEvent
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !SameAcceptedEvent(record, decoded) {
		t.Fatalf("a record without a payload changed in encoding: %q", decoded.Event.Data.Data)
	}
	other := record
	other.Event.Data.Data = json.RawMessage(`{}`)
	if SameAcceptedEvent(record, other) {
		t.Fatal("negative control: an empty object counted as no payload")
	}
}
