package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// TestDaemonUsageFeedSurvivesKill follows the usage feed the way a collector
// does, keeping one number: a record written before a SIGKILL is readable
// after the restart, and the cursor sees every record exactly once.
func TestDaemonUsageFeedSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	d := newDrill(t)
	d.start("")
	root, err := d.submit("drill.event")
	if err != nil {
		t.Fatal(err)
	}
	w := d.waitState("waiting")
	var cursor uint64
	seen := map[string]int{}
	follow := func() int {
		t.Helper()
		n := 0
		for {
			page, err := d.c.Events(ctx, url.Values{"type": {domain.TypeExecutionUsage}, "after_sequence": {strconv.FormatUint(cursor, 10)}, "limit": {"200"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) == 0 {
				return n
			}
			for _, e := range page.Items {
				var r domain.UsageRecord
				if err := json.Unmarshal(e.Data.Data, &r); err != nil {
					t.Fatal(err)
				}
				if r.Amounts["minutes"] <= 0 || r.Work == "" || r.Execution == "" {
					t.Fatalf("usage record: %+v", r)
				}
				seen[e.ID]++
				n++
				if cursor, err = strconv.ParseUint(e.Sequence, 10, 64); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	d.until("the parked turn's record", 20*time.Second, func() bool { return follow() > 0 || len(seen) > 0 })
	before := len(seen)
	d.kill()
	d.start("")
	if got := follow(); got != 0 {
		t.Fatalf("the cursor read %d records again after the restart", got)
	}
	all, err := d.c.Events(ctx, url.Values{"type": {domain.TypeExecutionUsage}, "limit": {"200"}})
	if err != nil || len(all.Items) != before {
		t.Fatalf("records after the restart: %d, want %d (%v)", len(all.Items), before, err)
	}
	if err = d.wake(w); err != nil {
		t.Fatal(err)
	}
	d.verify(root, "woken by drill.reply", false)
	d.until("the finished turn's record", 20*time.Second, func() bool { follow(); return len(seen) > before })
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("record %s seen %d times", id, n)
		}
	}
}
