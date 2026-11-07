package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
)

// Provider health (issue #537): a provider call for actions or panels, or
// an annotator's call for a Work's annotations (issue #545), that times out is remembered across requests, so a provider that keeps timing
// out stops costing every page its timeout. While a call is known to be
// timing out, views skip it at once and contribute nothing from it; at most
// one probe at a time, off the request and within the provider timeout,
// asks it again once its retry time comes, backing off while it still
// times out. The first answer, from a probe or otherwise, clears it. Each
// change is a provider.* telemetry record, and doctor's providers check
// names every call timing out now.

const (
	// providerRetry is how long a call that timed out is skipped before a
	// probe asks it again; each probe that times out doubles it, up to
	// providerRetryMax.
	providerRetry    = 30 * time.Second
	providerRetryMax = 5 * time.Minute
)

// callHealth is one provider call that is timing out.
type callHealth struct {
	provider, call string
	since, last    time.Time
	timeouts       int
	retry          time.Duration
	retryAt        time.Time
	probing        bool
	// scope and work are the target of the latest timeout, for doctor and
	// the records' correlation.
	scope, work domain.ID
}

type providerHealth struct {
	mu    sync.Mutex
	calls map[string]*callHealth // provider + "\x00" + call
	// probes counts probes in flight (tests wait on it).
	probes sync.WaitGroup
}

func newProviderHealth() *providerHealth {
	return &providerHealth{calls: map[string]*callHealth{}}
}

// skip reports whether the call is known to be timing out, and if so
// whether this caller should probe it now (its retry time has come and no
// probe is in flight).
func (h *providerHealth) skip(key string, now time.Time) (skip, probe bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.calls[key]
	if !ok {
		return false, false
	}
	if c.probing || now.Before(c.retryAt) {
		return true, false
	}
	c.probing = true
	h.probes.Add(1)
	return true, true
}

// timedOut records a timeout of the call on a target, reporting the call's
// state and whether it was answering until now.
func (h *providerHealth) timedOut(key, provider, call string, scope, work domain.ID, now time.Time, probe bool) (callHealth, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.calls[key]
	if !ok {
		c = &callHealth{provider: provider, call: call, since: now, retry: providerRetry}
		h.calls[key] = c
	} else if probe {
		c.retry = min(2*c.retry, providerRetryMax)
	}
	if probe {
		c.probing = false
	}
	c.timeouts++
	c.last, c.retryAt, c.scope, c.work = now, now.Add(c.retry), scope, work
	return *c, !ok
}

// answered clears the call, reporting what it was if it was timing out.
func (h *providerHealth) answered(key string) (callHealth, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.calls[key]
	if ok {
		delete(h.calls, key)
		return *c, true
	}
	return callHealth{}, false
}

func (h *providerHealth) snapshot() []callHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]callHealth, 0, len(h.calls))
	for _, c := range h.calls {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].provider != out[j].provider {
			return out[i].provider < out[j].provider
		}
		return out[i].call < out[j].call
	})
	return out
}

// providerTimeout is the timeout of one call to the named provider.
func (s *Server) providerTimeout(provider string) <-chan time.Time {
	if s.o.ProviderTimeout != nil {
		return s.o.ProviderTimeout(provider)
	}
	return time.After(DefaultProviderTimeout)
}

// probe asks a call known to be timing out again, off the request: it is
// given up on at the provider timeout and nothing waits for it.
func probe[T any](s *Server, key, provider, name string, scope, work domain.ID, fn func(context.Context) (T, error)) {
	go func() {
		defer s.failing.probes.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := fn(ctx)
			done <- err
		}()
		select {
		case <-done:
			// An answer, even a refusal, is not a timeout: the call is
			// asked again from now on, and a refusal is handled per
			// request as before.
			s.recovered(key)
		case <-s.providerTimeout(provider):
			s.timedOut(key, provider, name, scope, work, true)
		}
	}()
}

func (s *Server) timedOut(key, provider, call string, scope, work domain.ID, probe bool) {
	c, first := s.failing.timedOut(key, provider, call, scope, work, s.o.Now(), probe)
	if first {
		s.observeProvider("provider.timing_out", c)
	}
}

func (s *Server) recovered(key string) {
	if c, ok := s.failing.answered(key); ok {
		s.observeProvider("provider.answering", c)
	}
}

// observeProvider reports a change in a provider call's health. The record
// is host-observed, like a reconcile scan: no Event triggered it, so it is
// anchored by the provider's name and correlated with the scope (and Work)
// whose view saw it.
func (s *Server) observeProvider(name string, c callHealth) {
	if s.o.Observer == nil {
		return
	}
	trace := digest("provider", c.provider, c.call, c.since.UTC().Format(time.RFC3339Nano))
	rec := observability.Record{Name: name, At: s.o.Now(), Correlation: observability.Correlation{ScopeID: c.scope, WorkID: c.work, TraceID: trace},
		Attributes: map[string]string{
			"provider.name":     c.provider,
			"provider.call":     c.call,
			"provider.since":    c.since.UTC().Format(time.RFC3339Nano),
			"provider.timeouts": strconv.Itoa(c.timeouts),
			"record_id":         digest(name, trace, strconv.Itoa(c.timeouts)),
		}}
	if name == "provider.timing_out" {
		rec.Attributes["provider.retry_at"] = c.retryAt.UTC().Format(time.RFC3339Nano)
	}
	if observability.Validate(rec) != nil {
		return
	}
	_ = s.o.Observer.Observe(context.Background(), rec)
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// providerCheck is doctor's providers check: every provider call for
// actions, panels or annotations that is timing out now, by provider and call.
func (s *Server) providerCheck() runtime.Check {
	calls := s.failing.snapshot()
	if len(calls) == 0 {
		return runtime.Check{Name: "providers", Status: runtime.CheckOK, Code: "ok", Detail: "every action provider and annotator answered its last call in time"}
	}
	check := runtime.Check{Name: "providers", Status: runtime.CheckWarn, Code: "provider_timing_out",
		Detail: "these extensions' calls time out, so views skip them and show none of their actions, panels or annotations until they answer again"}
	for _, c := range calls {
		item := fmt.Sprintf("%s %s: timing out since %s, %d timeout(s), last on scope %s", c.provider, c.call, c.since.UTC().Format(time.RFC3339), c.timeouts, c.scope)
		if c.work != "" {
			item += " Work " + string(c.work)
		}
		item += ", next asked from " + c.retryAt.UTC().Format(time.RFC3339)
		check.Items = append(check.Items, item)
	}
	return check
}
