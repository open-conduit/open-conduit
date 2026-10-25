package github

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cacheSize bounds the GET answers kept for conditional requests.
const cacheSize = 4096

// RateLimitError means GitHub rate limited the credential; calls made with it
// are refused locally until Until, without reaching the network.
type RateLimitError struct {
	Until     time.Time
	Secondary bool
}

func (e *RateLimitError) Error() string {
	kind := "primary"
	if e.Secondary {
		kind = "secondary"
	}
	return fmt.Sprintf("github %s rate limit until %s", kind, e.Until.UTC().Format(time.RFC3339))
}

// transport sits under every call. GETs to the API are conditional, so an
// unchanged poll returns 304 and does not count against the rate limit. Once a
// credential is rate limited (primary reset or secondary Retry-After) its calls
// are refused until the limit lifts. Credentials are keyed by digest only.
type transport struct {
	base    http.RoundTripper
	apiHost string
	now     func() time.Time

	mu     sync.Mutex
	order  *list.List
	cache  map[string]*list.Element
	paused map[string]*RateLimitError
}

type cached struct {
	key    string
	etag   string
	status int
	header http.Header
	body   []byte
}

func newTransport(base http.RoundTripper, apiHost string, now func() time.Time) *transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, apiHost: apiHost, now: now, order: list.New(), cache: map[string]*list.Element{}, paused: map[string]*RateLimitError{}}
}

func credential(req *http.Request) string {
	auth := req.Header.Get("Authorization")
	if _, t, ok := strings.Cut(auth, " "); ok {
		auth = t
	}
	sum := sha256.Sum256([]byte(auth))
	return hex.EncodeToString(sum[:16])
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	cred := credential(req)
	if limit := t.pausedFor(cred); limit != nil {
		return nil, limit
	}
	key := cred + " " + req.URL.String()
	conditional := req.Method == http.MethodGet && req.URL.Host == t.apiHost && req.Header.Get("If-None-Match") == ""
	var hit *cached
	if conditional {
		if hit = t.lookup(key); hit != nil {
			req = req.Clone(req.Context())
			req.Header.Set("If-None-Match", hit.etag)
		}
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if limit := t.rateLimited(res); limit != nil {
		res.Body.Close()
		t.pause(cred, limit)
		return nil, limit
	}
	if !conditional {
		return res, nil
	}
	if res.StatusCode == http.StatusNotModified && hit != nil {
		res.Body.Close()
		return hit.response(req), nil
	}
	etag := res.Header.Get("ETag")
	if res.StatusCode != http.StatusOK || etag == "" {
		return res, nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	t.store(&cached{key: key, etag: etag, status: res.StatusCode, header: res.Header.Clone(), body: body})
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

func (c *cached) response(req *http.Request) *http.Response {
	return &http.Response{Status: strconv.Itoa(c.status) + " " + http.StatusText(c.status), StatusCode: c.status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: c.header.Clone(),
		Body: io.NopCloser(bytes.NewReader(c.body)), ContentLength: int64(len(c.body)), Request: req}
}

func (t *transport) lookup(key string) *cached {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.cache[key]
	if !ok {
		return nil
	}
	t.order.MoveToFront(el)
	return el.Value.(*cached)
}

func (t *transport) store(c *cached) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.cache[c.key]; ok {
		el.Value = c
		t.order.MoveToFront(el)
		return
	}
	t.cache[c.key] = t.order.PushFront(c)
	for t.order.Len() > cacheSize {
		last := t.order.Back()
		t.order.Remove(last)
		delete(t.cache, last.Value.(*cached).key)
	}
}

func (t *transport) pausedFor(cred string) *RateLimitError {
	t.mu.Lock()
	defer t.mu.Unlock()
	limit, ok := t.paused[cred]
	if !ok {
		return nil
	}
	if !t.now().Before(limit.Until) {
		delete(t.paused, cred)
		return nil
	}
	out := *limit
	return &out
}

func (t *transport) pause(cred string, limit *RateLimitError) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.paused[cred]; ok && t.now().Before(prev.Until) && !limit.Until.After(prev.Until) {
		return
	}
	out := *limit
	t.paused[cred] = &out
}

// PausedUntil reports the latest active pause across credentials (for doctor).
func (t *transport) PausedUntil() (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var until time.Time
	for _, l := range t.paused {
		if t.now().Before(l.Until) && l.Until.After(until) {
			until = l.Until
		}
	}
	return until, !until.IsZero()
}

// rateLimited recognizes GitHub's primary and secondary rate limits. A 403
// that is neither (no access, say) stays an ordinary error with its body.
func (t *transport) rateLimited(res *http.Response) *RateLimitError {
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	now := t.now()
	var until time.Time
	secondary := false
	if s := res.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			until, secondary = now.Add(time.Duration(secs)*time.Second), true
		}
	}
	if until.IsZero() && res.Header.Get("X-Ratelimit-Remaining") == "0" {
		if secs, err := strconv.ParseInt(res.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			until = time.Unix(secs, 0)
		}
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(data))
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &body)
	if until.IsZero() && strings.Contains(strings.ToLower(body.Message), "rate limit") {
		// GitHub's advice when a secondary limit gives no duration: wait a minute.
		until, secondary = now.Add(time.Minute), true
	}
	if until.IsZero() {
		return nil
	}
	if !until.After(now) {
		until = now.Add(time.Second)
	}
	return &RateLimitError{Until: until, Secondary: secondary}
}
