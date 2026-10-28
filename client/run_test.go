package client

import (
	"testing"
	"time"
)

func TestRunActiveAddsTheOpenSegment(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 30, 0, time.UTC)
	r := Run{ActiveMS: 10000, Segments: []RunSegment{{StartedAt: "2026-10-28T09:00:00Z", EndedAt: "2026-10-28T09:00:10Z", Parked: true}, {StartedAt: "2026-10-28T10:00:00Z"}}}
	if got := r.Active(now); got != 40*time.Second {
		t.Fatalf("running: %s", got)
	}
	r.Segments[1].EndedAt = "2026-10-28T10:00:30Z"
	r.ActiveMS = 40000
	if got := r.Active(now.Add(time.Hour)); got != 40*time.Second {
		t.Fatalf("finished: %s", got)
	}
}
