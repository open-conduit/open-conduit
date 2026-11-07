package management

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// What a Runs page load costs the daemon as closed Work grows (#544): the
// reads its requests make, timed in their read transaction on a SQLite
// store. Each closed Work has a display, a key and four finished attempts,
// and 20 Work are open. OC_WORK_SIZES (comma-separated) picks the closed
// counts; the default is the #544 install's 420 and one larger.
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

// seededWork keeps each size's store for every benchmark in the run, since
// seeding takes a while; TestMain removes them.
var (
	seededWork    = map[int]*sqlite.Store{}
	seededWorkDir string
)

func TestMain(m *testing.M) {
	code := m.Run()
	for _, s := range seededWork {
		_ = s.Close()
	}
	if seededWorkDir != "" {
		_ = os.RemoveAll(seededWorkDir)
	}
	os.Exit(code)
}

func seedWork(b *testing.B, closed int) storage.Store {
	if s, ok := seededWork[closed]; ok {
		return s
	}
	ctx := context.Background()
	if seededWorkDir == "" {
		dir, err := os.MkdirTemp("", "oc-work-bench-")
		if err != nil {
			b.Fatal(err)
		}
		seededWorkDir = dir
	}
	store, err := sqlite.Open(ctx, filepath.Join(seededWorkDir, fmt.Sprintf("work-%d.db", closed)))
	if err != nil {
		b.Fatal(err)
	}
	seededWork[closed] = store
	now := time.Now()
	guard := storage.Guard{Now: now}
	if err = store.Transact(ctx, guard, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "ops", Name: "Operations"}, 0)
	}); err != nil {
		b.Fatal(err)
	}
	const batch = 500
	total := closed + 20
	for start := 0; start < total; start += batch {
		if err = store.Transact(ctx, guard, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, total); i++ {
				id := domain.ID(fmt.Sprintf("work/%06d", i))
				w := domain.Work{ID: id, Kind: "github.issue", ScopeID: "ops", State: domain.WorkReady,
					CorrelationKeys: []domain.CorrelationKey{{Source: "github.issue", ExternalID: fmt.Sprintf("org/repo#%d", i)}},
					Display:         &domain.WorkDisplay{Title: fmt.Sprintf("Issue %d about the build", i), Ref: fmt.Sprintf("#%d", i), Requester: "someone"}}
				if e := tx.PutWork(w, 0); e != nil {
					return e
				}
				if i >= closed {
					continue
				}
				for a := uint32(1); a <= benchAttempts; a++ {
					eid := domain.ID(fmt.Sprintf("evt/%06d/%d", i, a))
					key := w.CorrelationKeys[0]
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
					if w, e = tx.Work(id); e != nil {
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
	return store
}

// benchWorkRead times one read a Runs page makes, at each size.
func benchWorkRead(b *testing.B, read func(*Server, storage.Tx) error) {
	for _, n := range workSizes {
		b.Run(fmt.Sprintf("closed=%d", n), func(b *testing.B) {
			store := seedWork(b, n)
			s := &Server{o: Options{Store: store, Now: time.Now, CursorKey: []byte("key")}}
			b.ResetTimer()
			for range b.N {
				if err := storage.View(context.Background(), store, time.Now(), func(tx storage.Tx) error { return read(s, tx) }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchFilter(b *testing.B, values map[string]string, limit int) workFilter {
	f, err := parseWorkFilter(listQuery{limit: limit, filters: values})
	if err != nil {
		b.Fatal(err)
	}
	return f
}

const closedSet = "completed,failed,cancelled"

// GET /work/facets?state=…closed: the Runs page reads it at most every 30 s.
func BenchmarkWorkFacets(b *testing.B) {
	benchWorkRead(b, func(_ *Server, tx storage.Tx) error {
		_, err := facetsOf(tx, workFilter{q: listQuery{filters: map[string]string{"state": closedSet}}})
		return err
	})
}

// The Closed count on the default view: the closed list, limit 1.
func BenchmarkWorkClosedCount(b *testing.B) {
	benchWorkRead(b, func(s *Server, tx storage.Tx) error {
		_, err := s.queryWork(tx, benchFilter(b, map[string]string{"state": closedSet, "sort": "-updated"}, 1), nil)
		return err
	})
}

// The Closed view: the latest 50 closed Work with their runs.
func BenchmarkWorkClosedPage(b *testing.B) {
	benchWorkRead(b, func(s *Server, tx storage.Tx) error {
		_, err := s.queryWork(tx, benchFilter(b, map[string]string{"state": closedSet, "sort": "-updated"}, 50), map[string]bool{"runs": true})
		return err
	})
}

// A search on the Closed view, which reads each candidate's latest attempt.
func BenchmarkWorkClosedSearch(b *testing.B) {
	benchWorkRead(b, func(s *Server, tx storage.Tx) error {
		_, err := s.queryWork(tx, benchFilter(b, map[string]string{"state": closedSet, "sort": "-updated", "q": "build fix"}, 50), map[string]bool{"runs": true})
		return err
	})
}

// The open Work with their runs, as the world reads it (without the
// providers' actions and panels).
func BenchmarkWorkOpen(b *testing.B) {
	benchWorkRead(b, func(s *Server, tx storage.Tx) error {
		_, err := s.queryWork(tx, benchFilter(b, map[string]string{"state": "ready,running,waiting"}, 50), map[string]bool{"runs": true})
		return err
	})
}
