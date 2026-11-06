package management

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
)

type recorder struct {
	mu      sync.Mutex
	records []observability.Record
}

func (r *recorder) Observe(_ context.Context, rec observability.Record) error {
	if err := observability.Validate(rec); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.records {
		out = append(out, rec.Name)
	}
	return out
}

// flaky is a provider call that hangs until told to answer. While it
// hangs, its timeout fires once the call is in it; while it answers, never.
// No test waits on real time.
type flaky struct {
	hung    atomic.Bool
	calls   atomic.Int32
	entered chan struct{}
}

func newFlaky() *flaky { return &flaky{entered: make(chan struct{}, 16)} }

func (f *flaky) call(ctx context.Context) ([]extension.Action, error) {
	f.calls.Add(1)
	if f.hung.Load() {
		f.entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []extension.Action{{ID: "go", Label: "Go"}}, nil
}

func (f *flaky) timeout(string) <-chan time.Time {
	if !f.hung.Load() {
		return nil
	}
	c := make(chan time.Time)
	go func() {
		<-f.entered
		close(c)
	}()
	return c
}

// A provider call that times out is skipped by later requests, without
// being made, until its retry time; then one probe off the request asks it
// again, backing off while it still times out, and the first answer clears
// it. Doctor names the call while it times out, and each change is a
// provider.* record.
func TestProviderTimeoutsAreRememberedAcrossRequests(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var clock sync.Mutex
	at := func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	advance := func(d time.Duration) { clock.Lock(); defer clock.Unlock(); now = now.Add(d) }
	f := newFlaky()
	f.hung.Store(true)
	rec := &recorder{}
	s := &Server{o: Options{Now: at, ProviderTimeout: f.timeout, ViewDeadline: func() <-chan time.Time { return nil }, Observer: rec}, failing: newProviderHealth()}
	w := domain.Work{ID: "w1", ScopeID: "ops"}
	target := extension.ActionTarget{Scope: domain.Scope{ID: "ops"}, Work: &w}
	ask := func() bool {
		v, cancel := s.viewer(context.Background())
		defer cancel()
		_, ok := providerCall(v, target, "assist", "panels", f.call)
		return ok
	}

	if ask() || f.calls.Load() != 1 {
		t.Fatalf("first call: %d calls", f.calls.Load())
	}
	check := s.providerCheck()
	if check.Status != runtime.CheckWarn || check.Code != "provider_timing_out" || len(check.Items) != 1 || !strings.Contains(check.Items[0], "assist panels") || !strings.Contains(check.Items[0], "Work w1") {
		t.Fatalf("doctor while timing out: %+v", check)
	}
	if got := rec.names(); len(got) != 1 || got[0] != "provider.timing_out" || rec.records[0].Attributes["provider.name"] != "assist" || rec.records[0].Correlation.ScopeID != "ops" {
		t.Fatalf("records: %+v", rec.records)
	}

	// Before its retry time, another request does not ask it.
	advance(providerRetry - time.Second)
	if ask() || f.calls.Load() != 1 {
		t.Fatalf("asked again before the retry: %d calls", f.calls.Load())
	}

	// At the retry time, one probe asks it, off the request; it still
	// hangs, so the retry doubles.
	advance(time.Second)
	if ask() {
		t.Fatal("a request waited on a probe")
	}
	s.failing.probes.Wait()
	if f.calls.Load() != 2 {
		t.Fatalf("probe: %d calls", f.calls.Load())
	}
	c := s.failing.snapshot()
	if len(c) != 1 || c[0].timeouts != 2 || !c[0].retryAt.Equal(at().Add(2*providerRetry)) {
		t.Fatalf("after a probe timed out: %+v", c)
	}
	if got := rec.names(); len(got) != 1 {
		t.Fatalf("a probe that still times out is no new record: %v", got)
	}

	// Once it answers, the next probe clears it and requests ask it again.
	f.hung.Store(false)
	advance(2 * providerRetry)
	if ask() {
		t.Fatal("the probing request used the probe's answer")
	}
	s.failing.probes.Wait()
	if len(s.failing.snapshot()) != 0 || s.providerCheck().Status != runtime.CheckOK {
		t.Fatalf("still failing after an answer: %+v", s.failing.snapshot())
	}
	if got := rec.names(); len(got) != 2 || got[1] != "provider.answering" {
		t.Fatalf("records: %v", got)
	}
	if !ask() || f.calls.Load() != 4 {
		t.Fatalf("after recovery: %d calls", f.calls.Load())
	}
}

// The retry backs off to its cap, and only one probe is out at a time.
func TestProviderProbeBacksOffAndRunsAlone(t *testing.T) {
	h := newProviderHealth()
	now := time.Unix(0, 0)
	h.timedOut("k", "p", "actions", "ops", "", now, false)
	for range 10 {
		now = h.calls["k"].retryAt
		if skip, probe := h.skip("k", now); !skip || !probe {
			t.Fatal("no probe at the retry time")
		}
		if skip, probe := h.skip("k", now); !skip || probe {
			t.Fatal("a second probe while one is out")
		}
		h.timedOut("k", "p", "actions", "ops", "", now, true)
		h.probes.Done()
	}
	if got := h.calls["k"].retry; got != providerRetryMax {
		t.Fatalf("retry %v, want the cap %v", got, providerRetryMax)
	}
}
