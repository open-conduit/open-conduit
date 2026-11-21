package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

var benchPayload = domain.Payload{Schema: "bench.v1", Data: []byte(`{"body":"` + strings.Repeat("x", 1000) + `"}`)}

func benchEvent(id string, at time.Time) domain.Event {
	return domain.Event{ID: domain.ID(id), Type: "bench.event", Source: "bench", DeliveryID: id, OccurredAt: at, ReceivedAt: at, Data: benchPayload}
}

// benchStore is a store holding rows Events of about 1 KiB, with its write
// transactions' waits collected.
func benchStore(b *testing.B, rows int) (*sqlite.Store, *benchWaits) {
	b.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { store.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	const batch = 1000
	for start := 0; start < rows; start += batch {
		if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, rows); i++ {
				if _, _, e := tx.AcceptEvent(benchEvent("seed-"+strconv.Itoa(i), now)); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	w := &benchWaits{}
	store.TimeTransactions(w.add)
	return store, w
}

// benchWaits collects how long transactions waited for the writer.
type benchWaits struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *benchWaits) add(x storage.TransactionTiming) {
	w.mu.Lock()
	w.d = append(w.d, x.Wait)
	w.mu.Unlock()
}

// report adds the p99 wait for the writer to the benchmark's result.
func (w *benchWaits) report(b *testing.B) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.d) == 0 {
		return
	}
	slices.Sort(w.d)
	b.ReportMetric(float64(w.d[min(len(w.d)*99/100, len(w.d)-1)].Microseconds()), "p99-wait-µs")
}

// BenchmarkTransactEmpty is the floor of a write: BEGIN IMMEDIATE and COMMIT
// at synchronous=FULL, with parallel callers queueing on the one connection.
func BenchmarkTransactEmpty(b *testing.B) {
	for _, rows := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			store, waits := benchStore(b, rows)
			guard := storage.Guard{Now: time.Unix(1_700_000_000, 0)}
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := store.Transact(context.Background(), guard, func(storage.Tx) error { return nil }); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			waits.report(b)
		})
	}
}

// BenchmarkAcceptEvent accepts one new Event per transaction.
func BenchmarkAcceptEvent(b *testing.B) {
	for _, rows := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			store, waits := benchStore(b, rows)
			now := time.Unix(1_700_000_000, 0).UTC()
			var next atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					id := "new-" + strconv.FormatInt(next.Add(1), 10)
					if err := store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
						_, _, e := tx.AcceptEvent(benchEvent(id, now))
						return e
					}); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			waits.report(b)
		})
	}
}

// BenchmarkListEvents reads a management page of 50 Events from the end of
// the log, through a View on the reader connections.
func BenchmarkListEvents(b *testing.B) {
	for _, rows := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			store, _ := benchStore(b, rows)
			after := uint64(rows - 50)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := store.View(context.Background(), func(tx storage.Tx) error {
						page, e := tx.Events(after, 50)
						if e == nil && len(page) != 50 {
							e = fmt.Errorf("read %d Events", len(page))
						}
						return e
					}); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
