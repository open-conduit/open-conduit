package config

import (
	"context"
	"sync"
	"time"
)

// Default lifetimes of a SecretCache's entries.
const (
	DefaultSecretTTL        = time.Minute
	DefaultSecretFailureTTL = 10 * time.Second
)

// SecretCache holds what secret providers answered, in memory only, so a
// consumer that resolves on demand (an extension checking each webhook
// delivery's signature) asks the provider at most once per lifetime for a
// reference, whoever reaches the host. An entry is keyed by the consumer
// and the reference, lives TTL when the provider served a value and
// FailureTTL when it refused or failed, and every entry goes at Reset (a
// config apply). Concurrent resolutions of one key share one call to the
// provider. The zero value is ready to use, with the default lifetimes.
type SecretCache struct {
	TTL, FailureTTL time.Duration
	Now             func() time.Time

	mu      sync.Mutex
	epoch   uint64
	entries map[string]cached
	flight  map[string]*inflight
}

type cached struct {
	value, code string
	until       time.Time
}

type inflight struct {
	done        chan struct{}
	value, code string
}

func (c *SecretCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Reset drops every entry, and keeps a call already in flight from storing
// what it answers, so nothing resolved under an earlier config outlives it.
func (c *SecretCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.entries = nil
}

// do answers key from the cache, from a call in flight for it, or by
// running call once, detached from the caller's cancellation so one
// caller leaving doesn't fail the others. A caller whose ctx ends first
// gets cancelled.
func (c *SecretCache) do(ctx context.Context, key string, call func(context.Context) (string, string)) (string, string) {
	c.mu.Lock()
	now := c.now()
	if e, ok := c.entries[key]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.value, e.code
	}
	f, ok := c.flight[key]
	if !ok {
		f = &inflight{done: make(chan struct{})}
		if c.flight == nil {
			c.flight = map[string]*inflight{}
		}
		c.flight[key] = f
		epoch := c.epoch
		go func() {
			v, code := call(context.WithoutCancel(ctx))
			c.mu.Lock()
			defer c.mu.Unlock()
			f.value, f.code = v, code
			delete(c.flight, key)
			if epoch == c.epoch {
				ttl := c.TTL
				if ttl <= 0 {
					ttl = DefaultSecretTTL
				}
				if code != "" {
					if ttl = c.FailureTTL; ttl <= 0 {
						ttl = DefaultSecretFailureTTL
					}
				}
				if c.entries == nil {
					c.entries = map[string]cached{}
				}
				at := c.now()
				for k, e := range c.entries {
					if !at.Before(e.until) {
						delete(c.entries, k)
					}
				}
				c.entries[key] = cached{v, code, at.Add(ttl)}
			}
			close(f.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.value, f.code
	case <-ctx.Done():
		return "", "cancelled"
	}
}
