package storage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// aborting fails its first n runs with err, then succeeds.
type aborting struct {
	n, runs int
	err     error
}

func (a *aborting) Transact(_ context.Context, _ storage.Guard, fn func(storage.Tx) error) error {
	a.runs++
	if a.runs <= a.n {
		return a.err
	}
	return fn(nil)
}

type abortingViewer struct{ *aborting }

func (a abortingViewer) View(ctx context.Context, fn func(storage.Tx) error) error {
	return a.Transact(ctx, storage.Guard{}, fn)
}

// TestRetryRerunsOnlyTransientAborts: a transient abort runs again until
// the policy's attempts run out, and nothing else does, not even a
// conflict; every abort but the last is observed as retried.
func TestRetryRerunsOnlyTransientAborts(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		n             int
		err           error
		runs          int
		ok            bool
		observedFinal bool
	}{
		{"transient then commit", 2, fmt.Errorf("busy: %w", storage.ErrTransient), 3, true, false},
		{"transient past the attempts", 9, storage.ErrTransient, 3, false, true},
		{"conflict is not retried", 9, storage.ErrConflict, 1, false, false},
	} {
		inner := &aborting{n: tc.n, err: tc.err}
		var observed []int
		final := false
		s := storage.Retry(inner, storage.RetryPolicy{Attempts: 3, Observe: func(n int, last bool, err error) {
			observed = append(observed, n)
			final = final || last
		}})
		called := 0
		err := s.Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, func(storage.Tx) error { called++; return nil })
		if (err == nil) != tc.ok || !tc.ok && !errors.Is(err, tc.err) {
			t.Fatalf("%s: got %v", tc.name, err)
		}
		if inner.runs != tc.runs {
			t.Fatalf("%s: %d runs, want %d", tc.name, inner.runs, tc.runs)
		}
		if final != tc.observedFinal {
			t.Fatalf("%s: observed final %v, want %v (%v)", tc.name, final, tc.observedFinal, observed)
		}
		if tc.ok && called != 1 {
			t.Fatalf("%s: the committing run called back %d times", tc.name, called)
		}
	}
}

// TestRetryKeepsAViewerAndStopsWithItsContext: Retry of a Viewer is a
// Viewer whose View retries too, Retry of a plain Store is not one, and a
// cancelled caller is not kept waiting out a backoff.
func TestRetryKeepsAViewerAndStopsWithItsContext(t *testing.T) {
	plain := storage.Retry(&aborting{}, storage.DefaultRetry)
	if _, ok := plain.(storage.Viewer); ok {
		t.Fatal("Retry of a plain Store is a Viewer")
	}
	inner := abortingViewer{&aborting{n: 1, err: storage.ErrTransient}}
	v, ok := storage.Retry(inner, storage.RetryPolicy{Attempts: 2}).(storage.Viewer)
	if !ok {
		t.Fatal("Retry of a Viewer is not a Viewer")
	}
	if err := v.View(context.Background(), func(storage.Tx) error { return nil }); err != nil || inner.runs != 2 {
		t.Fatalf("view: %v after %d runs", err, inner.runs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slow := &aborting{n: 9, err: storage.ErrTransient}
	s := storage.Retry(slow, storage.RetryPolicy{Attempts: 5, Backoff: func(int) time.Duration { return time.Hour }})
	if err := s.Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, func(storage.Tx) error { return nil }); !errors.Is(err, storage.ErrTransient) || slow.runs != 1 {
		t.Fatalf("cancelled: %v after %d runs", err, slow.runs)
	}
}
