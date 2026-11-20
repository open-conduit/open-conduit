package conformance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// The cases in this file state guarantees a backend that serialises every
// write keeps without trying, and one with concurrent writers has to keep
// on purpose (#694): sequences become visible in sequence order, KV keys
// compare as bytes, and a transaction aborted under contention commits
// nothing and says so with ErrTransient.

// writerWait bounds how long a second writer may take to commit while a
// first holds its transaction open. A backend that serialises writers
// holds the second until the first ends, so for it this is a deadline that
// passes and the case moves on; one that runs writers together commits the
// second at once, and the case then reads as a waiting reader would.
const writerWait = 500 * time.Millisecond

// sequenceVisibility: a reader that has seen sequence n has seen every
// Event below n that will ever commit, so a waiting reader's cursor (a
// Wait's AfterSequence, an Events page) never skips an Event. A first
// writer accepts an Event and holds its transaction open; a second accepts
// another. If the second commits while the first is open, a reader then
// takes its cursor; once the first commits, the page after that cursor
// must hold every Event the reader had not seen.
func sequenceVisibility(s storage.Store) error {
	c := NewClock()
	ctx := context.Background()
	inside, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- transact(s, c, func(tx storage.Tx) error {
			if _, _, err := tx.AcceptEvent(event("first", c.Now())); err != nil {
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	select {
	case <-inside:
	case err := <-first:
		return fmt.Errorf("first writer ended early: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		second <- transact(s, c, func(tx storage.Tx) error {
			_, _, err := tx.AcceptEvent(event("second", c.Now()))
			return err
		})
	}()
	var cursor uint64
	seen := map[domain.ID]bool{}
	secondDone := false
	select {
	case err := <-second:
		secondDone = true
		if err == nil {
			err = storage.View(ctx, s, c.Now(), func(tx storage.Tx) error {
				latest, err := tx.LatestSequence()
				if err != nil {
					return err
				}
				events, err := tx.Events(0, 0)
				if err != nil {
					return err
				}
				for _, e := range events {
					if e.Sequence > latest {
						return fmt.Errorf("sequence: Events returned %d past LatestSequence %d", e.Sequence, latest)
					}
					seen[e.Event.ID] = true
				}
				cursor = latest
				return nil
			})
		}
		if err != nil {
			close(release)
			<-first
			return err
		}
	case <-time.After(writerWait):
	}
	close(release)
	if err := <-first; err != nil {
		return err
	}
	if !secondDone {
		if err := <-second; err != nil {
			return err
		}
	}
	return storage.View(ctx, s, c.Now(), func(tx storage.Tx) error {
		page, err := tx.Events(cursor, 0)
		if err != nil {
			return err
		}
		for _, e := range page {
			seen[e.Event.ID] = true
		}
		for _, id := range []domain.ID{"first", "second"} {
			if !seen[id] {
				e, _ := tx.Event(id)
				return fmt.Errorf("sequence: Event %s (sequence %d) became visible below a reader's cursor %d and was skipped", id, e.Sequence, cursor)
			}
		}
		latest, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		all, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		if len(all) != 2 || all[1].Sequence != latest || all[0].Sequence >= all[1].Sequence {
			return fmt.Errorf("sequence: %d Events ending at %d, LatestSequence %d", len(all), lastSequence(all), latest)
		}
		return nil
	})
}

func lastSequence(events []domain.AcceptedEvent) uint64 {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Sequence
}

// kvBytes: KV keys compare as bytes. A prefix matches byte for byte, so
// "%", "_" and "\" in it match only themselves (a backend that builds a
// LIKE pattern from the prefix must escape them), and List returns keys in
// byte order, not a collation's: upper case before lower, ASCII before
// multi-byte UTF-8, and case is never folded.
func kvBytes(s storage.Store) error {
	c := NewClock()
	const ns = "test.kv"
	keys := []string{"a%b", "a_b", "axb", "aXb", `a\b`, "a", "B", "Z", "~", "é", "ab"}
	if err := transact(s, c, func(tx storage.Tx) error {
		for _, k := range keys {
			if err := tx.Put(ns, k, []byte(k)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		for _, tc := range []struct {
			prefix string
			want   []string
		}{
			{"a%", []string{"a%b"}},
			{"a_", []string{"a_b"}},
			{`a\`, []string{`a\b`}},
			{"aX", []string{"aXb"}},
			{"A", nil},
			{"é", []string{"é"}},
			{"", []string{"B", "Z", "a", "a%b", "aXb", `a\b`, "a_b", "ab", "axb", "~", "é"}},
		} {
			rows, err := tx.List(ns, tc.prefix)
			if err != nil {
				return err
			}
			got := make([]string, len(rows))
			for i, r := range rows {
				got[i] = r.Key
				if string(r.Value) != r.Key {
					return fmt.Errorf("kv bytes: %q holds %q", r.Key, r.Value)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				what := "prefix matched a wildcard"
				if tc.prefix == "" {
					what = "keys not in byte order"
				} else if tc.prefix == "A" || tc.prefix == "aX" {
					what = "prefix folded case"
				}
				return fmt.Errorf("kv bytes: %s: List(%q) = %q, want %q", what, tc.prefix, got, tc.want)
			}
		}
		return nil
	})
}

// contenders is how many transactions transientAborts runs at once.
const contenders = 8

// transientAborts: transactions that contend for one record either commit
// or abort with ErrTransient having committed nothing, so storage.Retry
// rerunning them over reread state yields exactly one effect each. Each
// contender reads a KV counter, writes it plus one and accepts its own
// Event. The counter must end at the number of contenders (more means an
// abort had committed, fewer a lost update), every Event must be there
// once, and no contender may be refused with anything else: an abort
// reported as ErrConflict would end a healthy attempt's lease. A backend
// that serialises writers never aborts, and passes as it commits.
func transientAborts(s storage.Store) error {
	c := NewClock()
	const ns, key = "test.contention", "counter"
	if err := transact(s, c, func(tx storage.Tx) error { return tx.Put(ns, key, []byte("0")) }); err != nil {
		return err
	}
	aborts := 0
	var mu sync.Mutex
	retry := storage.Retry(s, storage.RetryPolicy{Attempts: 100, Observe: func(int, bool, error) {
		mu.Lock()
		aborts++
		mu.Unlock()
	}})
	start := make(chan struct{})
	results := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		go func(i int) {
			<-start
			results <- transact(retry, c, func(tx storage.Tx) error {
				b, err := tx.Get(ns, key)
				if err != nil {
					return err
				}
				n, err := strconv.Atoi(string(b))
				if err != nil {
					return err
				}
				if err := tx.Put(ns, key, []byte(strconv.Itoa(n+1))); err != nil {
					return err
				}
				_, _, err = tx.AcceptEvent(event(domain.ID(fmt.Sprint("contender-", i)), c.Now()))
				return err
			})
		}(i)
	}
	close(start)
	for i := 0; i < contenders; i++ {
		if err := <-results; err != nil {
			if errors.Is(err, storage.ErrConflict) {
				return fmt.Errorf("transient: contention was reported as a conflict, not ErrTransient: %v", err)
			}
			return fmt.Errorf("transient: a contender failed: %v", err)
		}
	}
	return transact(s, c, func(tx storage.Tx) error {
		b, err := tx.Get(ns, key)
		if err != nil {
			return err
		}
		if string(b) != strconv.Itoa(contenders) {
			what := "an update was lost"
			if n, _ := strconv.Atoi(string(b)); n > contenders {
				what = "an aborted transaction had committed"
			}
			return fmt.Errorf("transient: %s: counter %s after %d contenders and %d aborts", what, b, contenders, aborts)
		}
		if n := eventCount(tx); n != contenders {
			return fmt.Errorf("transient: %d Events after %d contenders", n, contenders)
		}
		return nil
	})
}
