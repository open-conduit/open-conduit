package management

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

func TestRunWireCarriesSegments(t *testing.T) {
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	parked, resumed := start.Add(10*time.Second), start.Add(time.Hour)
	r := runtime.Run{Execution: domain.Execution{ID: "w/attempt/1", State: domain.ExecutionRunning, StartedAt: &start},
		Segments:   []runtime.Segment{{StartedAt: start, EndedAt: &parked, Parked: true}, {StartedAt: resumed, WakeEventID: "wake-1"}},
		ActiveTime: 10 * time.Second}
	data, err := json.Marshal(toRun(r))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Segments []map[string]any `json:"segments"`
		ActiveMS int64            `json:"active_ms"`
	}
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ActiveMS != 10000 || len(got.Segments) != 2 {
		t.Fatalf("run wire: %s", data)
	}
	first, second := got.Segments[0], got.Segments[1]
	if first["started_at"] != "2026-10-28T09:00:00Z" || first["ended_at"] != "2026-10-28T09:00:10Z" || first["parked"] != true || first["wake_event_id"] != nil {
		t.Fatalf("first segment: %v", first)
	}
	if second["started_at"] != "2026-10-28T10:00:00Z" || second["ended_at"] != nil || second["parked"] != nil || second["wake_event_id"] != "wake-1" {
		t.Fatalf("open segment: %v", second)
	}
}
