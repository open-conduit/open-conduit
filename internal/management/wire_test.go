package management

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// The API shows a Work's display and a Wait's description as they were
// stored, with a timed Wait's due time.
func TestWireDisplayAndWaitDescription(t *testing.T) {
	w := domain.Work{ID: "work/1", Kind: "fixture.work", Display: &domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1", Links: []domain.Link{{Label: "Issue #1", URL: "https://tracker.example/issues/1"}}}}
	b, _ := json.Marshal(toWork(w))
	var got map[string]json.RawMessage
	_ = json.Unmarshal(b, &got)
	if string(got["display"]) != `{"title":"Fix the example widget","ref":"#1","links":[{"label":"Issue #1","url":"https://tracker.example/issues/1"}]}` {
		t.Fatalf("work display %s", got["display"])
	}
	w.Display = nil
	b, _ = json.Marshal(toWork(w))
	got = nil
	_ = json.Unmarshal(b, &got)
	if _, ok := got["display"]; ok {
		t.Fatal("Work without a display shows one")
	}
	due := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	described := toWait(domain.Wait{ID: "wait/1", Description: "checks on PR #2", Progress: "3 of 7 checks finished", TimeoutTimerID: "t"}, due)
	if described.Description != "checks on PR #2" || described.Progress != "3 of 7 checks finished" || described.DueAt != "2026-10-25T09:00:00Z" {
		t.Fatalf("described wait %+v", described)
	}
	// Core describes a timed Wait when it is declared, not here.
	if timed := toWait(domain.Wait{ID: "wait/2", TimeoutTimerID: "t"}, due); timed.Description != "" || timed.DueAt != "2026-10-25T09:00:00Z" {
		t.Fatalf("timed wait %+v", timed)
	}
	if plain := toWait(domain.Wait{ID: "wait/3"}, time.Time{}); plain.Description != "" || plain.DueAt != "" {
		t.Fatalf("plain wait %+v", plain)
	}
}
