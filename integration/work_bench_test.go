package integration_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// What the Runs page's requests cost the daemon as closed Work grows
// (#544), timed through the management API on SQLite. Each closed Work has
// a display, a key and four finished attempts, and 20 Work are open.
// OC_WORK_SIZES (comma-separated) picks the closed counts; the default is
// the #544 install's 420 and one larger.
var workSizes = func() []int {
	sizes := []int{420, 5_000}
	if v := os.Getenv("OC_WORK_SIZES"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 {
				panic("OC_WORK_SIZES: " + f)
			}
			sizes = append(sizes, n)
		}
	}
	return sizes
}()

const benchAttempts = 4

// seedClosedWork writes closed Work, each with its attempts, and 20 open
// Work straight through storage into the harness's scope.
func seedClosedWork(b *testing.B, h *managementHarness, closed int) {
	ctx := context.Background()
	now := h.clock.Now()
	guard := storage.Guard{Now: now}
	const batch = 500
	total := closed + 20
	for start := 0; start < total; start += batch {
		if err := h.store.Transact(ctx, guard, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, total); i++ {
				id := domain.ID(fmt.Sprintf("work/bench-%06d", i))
				key := domain.CorrelationKey{Source: "github.issue", ExternalID: fmt.Sprintf("org/repo#%d", i)}
				w := domain.Work{ID: id, Kind: "github.issue", ScopeID: "ops", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{key},
					Display: &domain.WorkDisplay{Title: fmt.Sprintf("Issue %d about the build", i), Ref: fmt.Sprintf("#%d", i), Requester: "someone"}}
				if e := tx.PutWork(w, 0); e != nil {
					return e
				}
				if i >= closed {
					continue
				}
				for a := uint32(1); a <= benchAttempts; a++ {
					eid := domain.ID(fmt.Sprintf("evt/bench-%06d/%d", i, a))
					if _, _, e := runtime.AcceptInTx(tx, "ops", domain.Event{ID: eid, Type: "github.issue.comment", Source: "github", DeliveryID: string(eid), OccurredAt: now, ReceivedAt: now, CorrelationKey: &key}); e != nil {
						return e
					}
					xid := domain.ID(fmt.Sprintf("%s/attempt/%d", id, a))
					if e := tx.PutExecution(domain.Execution{ID: xid, WorkID: id, ExecutorBinding: "run", Attempt: a, State: domain.ExecutionPending, TriggerID: eid}, 0); e != nil {
						return e
					}
					if _, e := tx.Claim(xid, "owner", time.Minute); e != nil {
						return e
					}
					x, e := tx.Execution(xid)
					if e != nil {
						return e
					}
					x.State, x.Outcome = domain.ExecutionCompleted, &domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "Opened a PR for the fix"}
					if e = tx.PutExecution(x, x.Revision); e != nil {
						return e
					}
				}
				w, e := tx.Work(id)
				if e != nil {
					return e
				}
				w.State = domain.WorkCompleted
				if e = tx.PutWork(w, w.Revision); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// Each request a Runs page load can make, at each size. Run with
// go test -run '^$' -bench BenchmarkRunsPageReads ./integration.
func BenchmarkRunsPageReads(b *testing.B) {
	const closedStates = "completed,failed,cancelled"
	reads := []struct {
		name string
		read func(context.Context, *client.Client) error
	}{
		{"open with runs", func(ctx context.Context, c *client.Client) error {
			_, err := c.WorksWithRuns(ctx, url.Values{"state": {"ready,running,waiting"}, "limit": {"50"}}, false)
			return err
		}},
		{"facets", func(ctx context.Context, c *client.Client) error {
			_, err := c.WorkFacets(ctx, url.Values{"state": {closedStates}})
			return err
		}},
		{"closed count with a facet", func(ctx context.Context, c *client.Client) error {
			_, err := c.Works(ctx, url.Values{"state": {closedStates}, "source": {"github"}, "sort": {"-updated"}, "limit": {"1"}})
			return err
		}},
		{"closed page of 50", func(ctx context.Context, c *client.Client) error {
			_, err := c.WorksWithRuns(ctx, url.Values{"state": {closedStates}, "sort": {"-updated"}, "limit": {"50"}}, false)
			return err
		}},
		{"closed search", func(ctx context.Context, c *client.Client) error {
			_, err := c.WorksWithRuns(ctx, url.Values{"state": {closedStates}, "sort": {"-updated"}, "q": {"build fix"}, "limit": {"50"}}, false)
			return err
		}},
	}
	for _, n := range workSizes {
		b.Run(fmt.Sprintf("closed=%d", n), func(b *testing.B) {
			h := newManagementHarness(b)
			seedClosedWork(b, h, n)
			c := h.operatorClient()
			for _, r := range reads {
				b.Run(strings.ReplaceAll(r.name, " ", "_"), func(b *testing.B) {
					for range b.N {
						if err := r.read(context.Background(), c); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
