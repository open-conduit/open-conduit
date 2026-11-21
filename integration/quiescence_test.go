package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// TestDaemonQuiescence runs the real daemon: idle, it answers quiescent with
// a time to be awake by and what its start cost, and the retention duty's due
// time survives a restart rather than starting again from zero.
func TestDaemonQuiescence(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	d := newDrill(t)
	d.start("")
	var q client.Quiescence
	quiet := func() bool {
		var err error
		q, err = d.c.Quiescence(context.Background())
		return err == nil && q.Quiescent && q.Startup != nil
	}
	d.until("quiescent", 30*time.Second, quiet)
	if q.Until == nil || q.Next == "" || !q.Until.After(time.Now()) {
		t.Fatalf("quiescent with no time to wake: %+v", q)
	}
	if q.Startup.FirstPassMS < q.Startup.ListeningMS {
		t.Fatalf("startup: %+v", q.Startup)
	}
	retention := func() time.Time {
		for _, o := range q.Obligations {
			if o.Duty == "retention" {
				return o.Due
			}
		}
		t.Fatalf("no retention obligation: %+v", q.Obligations)
		return time.Time{}
	}
	due := retention()
	began := q.Startup.At
	d.stop()
	d.start("")
	d.until("quiescent after restart", 30*time.Second, func() bool { return quiet() && q.Startup.At.After(began) })
	if got := retention(); !got.Equal(due) {
		t.Fatalf("retention due %v after a restart, was %v", got, due)
	}
}
