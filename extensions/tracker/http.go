package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxResponseBytes bounds a service response.
const MaxResponseBytes = 16 << 20

// Response is one service answer. Body is for the Service's own parsing and
// must never reach an error, a log or an agent.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Credential resolves a secret lazily and caches it until Invalidate (after a
// 401), so a rotated reference takes effect without a restart.
type Credential struct {
	Name    string
	Secrets SecretFunc

	mu    sync.Mutex
	value string
}

// Get returns the cached secret, resolving it when needed.
func (c *Credential) Get(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != "" {
		return c.value, nil
	}
	if c.Secrets == nil {
		return "", ErrCredential
	}
	v, err := c.Secrets(ctx, c.Name)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", ErrCredential
	}
	c.value = strings.TrimSpace(v)
	return c.value, nil
}

// Invalidate drops the cached secret.
func (c *Credential) Invalidate() {
	c.mu.Lock()
	c.value = ""
	c.mu.Unlock()
}

// JSONClient performs JSON requests. Auth sets the request's credentials; a
// 401 invalidates them (OnUnauthorized) and retries once with a fresh
// resolution. A primary rate limit pauses the client until it lifts: calls
// then fail locally with RateLimitError.
type JSONClient struct {
	HTTP           *http.Client
	Auth           func(ctx context.Context, r *http.Request) error
	OnUnauthorized func()
	Now            func() time.Time
	// UserAgent is sent with every request.
	UserAgent string

	mu     sync.Mutex
	paused time.Time
}

func (c *JSONClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Pause records a rate limit lifting at until.
func (c *JSONClient) Pause(until time.Time) {
	c.mu.Lock()
	if until.After(c.paused) {
		c.paused = until
	}
	c.mu.Unlock()
}

// Raw performs one request and returns the answer whatever its status
// (after the 401 retry). Transport failures and rate limits are errors.
func (c *JSONClient) Raw(ctx context.Context, method, url string, in any) (Response, error) {
	c.mu.Lock()
	paused := c.paused
	c.mu.Unlock()
	if c.now().Before(paused) {
		return Response{}, &RateLimitError{Until: paused}
	}
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return Response{}, err
		}
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return Response{}, errors.New("tracker: invalid request")
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
		}
		if c.Auth != nil {
			if err := c.Auth(ctx, req); err != nil {
				return Response{}, err
			}
		}
		client := c.HTTP
		if client == nil {
			client = &http.Client{Timeout: 30 * time.Second}
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return Response{}, ctx.Err()
			}
			return Response{}, errors.New("tracker: service unreachable")
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
		resp.Body.Close()
		if err != nil {
			return Response{}, errors.New("tracker: response unreadable")
		}
		if resp.StatusCode == http.StatusUnauthorized {
			if c.OnUnauthorized != nil {
				c.OnUnauthorized()
			}
			if attempt == 0 {
				continue
			}
			return Response{}, ErrCredential
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			until := c.now().Add(time.Minute)
			if s := resp.Header.Get("Retry-After"); s != "" {
				if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs >= 0 && secs < 86400 {
					until = c.now().Add(time.Duration(secs) * time.Second)
				}
			}
			c.Pause(until)
			return Response{}, &RateLimitError{Until: until}
		}
		return Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
	}
}

// Do performs a request expecting 2xx and decodes the JSON answer into out.
// 404 is ErrNotFound; other statuses are StatusError.
func (c *JSONClient) Do(ctx context.Context, method, url string, in, out any) (http.Header, error) {
	resp, err := c.Raw(ctx, method, url, in)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.Status == http.StatusNotFound || resp.Status == http.StatusGone:
		return resp.Header, ErrNotFound
	case resp.Status < 200 || resp.Status > 299:
		return resp.Header, &StatusError{Status: resp.Status}
	}
	if out != nil && len(bytes.TrimSpace(resp.Body)) > 0 {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp.Header, errors.New("tracker: response is not the expected JSON")
		}
	}
	return resp.Header, nil
}

// PausedUntil reports a rate limit in force.
func (c *JSONClient) PausedUntil() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused, c.now().Before(c.paused)
}
