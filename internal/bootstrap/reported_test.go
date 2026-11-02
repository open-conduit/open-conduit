package bootstrap

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
)

// TestPruneReported: per-record diagnostics nobody has reported for an hour
// are dropped, so the map doesn't keep every Event and Work the daemon ever
// had trouble with. A per-record key still being reported, and a
// connection-level key however old, stay (the negative controls).
func TestPruneReported(t *testing.T) {
	clock := conformance.NewClock()
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: clock.Now, reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	d.report("route", "event-old", "route_failed")
	d.report("start", "work-held", "capacity_concurrency")
	d.report("github", "conn", "restart_required")
	clock.Advance(50 * time.Minute)
	d.report("start", "work-held", "capacity_concurrency") // still held: reported each pass
	clock.Advance(20 * time.Minute)
	d.pruneReported()
	if got := d.reportedCode("route", "event-old"); got != "" {
		t.Fatalf("stale per-record diagnostic kept: %q", got)
	}
	if got := d.reportedCode("start", "work-held"); got != "capacity_concurrency" {
		t.Fatalf("live per-record diagnostic pruned: %q", got)
	}
	if got := d.reportedCode("github", "conn"); got != "restart_required" {
		t.Fatalf("connection diagnostic pruned: %q", got)
	}
	d.report("start", "work-held", "")
	if len(d.reported) != 1 || len(d.reportedAt) != 1 {
		t.Fatalf("an empty code should forget the key: %v %v", d.reported, d.reportedAt)
	}
}
