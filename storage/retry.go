package storage

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// RetryPolicy is how Retry reruns a transaction that aborted with
// ErrTransient.
type RetryPolicy struct {
	// Attempts is how many times a transaction runs in all; below 1 is 1.
	Attempts int
	// Backoff is how long to wait before run n+1 after run n aborted
	// (n from 1). Nil waits nothing. The wait ends early with the
	// caller's context.
	Backoff func(n int) time.Duration
	// Observe, when set, hears each abort that is about to be retried
	// (n is the run that aborted) and each one that ran out of attempts
	// (final), so a host can report contention. It must not block.
	Observe func(n int, final bool, err error)
}

// DefaultRetry runs a transaction up to five times, waiting a jittered
// 5ms, 10ms, 20ms and 40ms between runs.
var DefaultRetry = RetryPolicy{Attempts: 5, Backoff: func(n int) time.Duration {
	step := 5 * time.Millisecond << (n - 1)
	return step/2 + rand.N(step/2+1)
}}

// Retry is s with one policy for transient aborts: a Transact or View that
// fails with ErrTransient runs again, callback and all, so the callback
// rereads the state and decides afresh. Every other error, ErrConflict
// included, is returned as it is. A callback run under Retry may therefore
// run more than once, and only the last run's transaction commits: it must
// keep to the Transact rule that it does nothing outside the transaction,
// set (not accumulate into) whatever it hands back to its caller, and
// decide any effect after the transaction from its last run alone.
// Retry keeps s a Viewer when it is one.
func Retry(s Store, p RetryPolicy) Store {
	r := retrying{inner: s, p: p}
	if v, ok := s.(Viewer); ok {
		return retryingViewer{r, v}
	}
	return r
}

type retrying struct {
	inner Store
	p     RetryPolicy
}

func (r retrying) Transact(ctx context.Context, g Guard, fn func(Tx) error) error {
	return r.p.run(ctx, func() error { return r.inner.Transact(ctx, g, fn) })
}

type retryingViewer struct {
	retrying
	v Viewer
}

func (r retryingViewer) View(ctx context.Context, fn func(Tx) error) error {
	return r.p.run(ctx, func() error { return r.v.View(ctx, fn) })
}

func (p RetryPolicy) run(ctx context.Context, once func() error) error {
	attempts := max(p.Attempts, 1)
	for n := 1; ; n++ {
		err := once()
		if err == nil || !errors.Is(err, ErrTransient) {
			return err
		}
		final := n >= attempts || ctx.Err() != nil
		if p.Observe != nil {
			p.Observe(n, final, err)
		}
		if final {
			return err
		}
		if p.Backoff != nil {
			if d := p.Backoff(n); d > 0 {
				t := time.NewTimer(d)
				select {
				case <-ctx.Done():
					t.Stop()
					return err
				case <-t.C:
				}
			}
		}
	}
}
