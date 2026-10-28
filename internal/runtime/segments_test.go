package runtime

import (
	"testing"
	"time"
)

func TestSegmentsKeepTheLastAndAccumulateDroppedTime(t *testing.T) {
	var state AttemptState
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for i := 0; i < MaxSegments+10; i++ {
		state.beginSegment(Segment{StartedAt: at})
		at = at.Add(time.Second)
		state.endSegment(at, true)
		at = at.Add(time.Hour)
	}
	if len(state.Segments) != MaxSegments || state.DroppedSegments != 10 || state.DroppedActive != 10*time.Second {
		t.Fatalf("kept %d, dropped %d (%s)", len(state.Segments), state.DroppedSegments, state.DroppedActive)
	}
	if !state.Segments[0].StartedAt.Equal(time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC).Add(10 * (time.Hour + time.Second))) {
		t.Fatalf("oldest kept segment: %+v", state.Segments[0])
	}
}
