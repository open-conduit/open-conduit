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

// maxBudgetTokens bounds the tokens Budgets remembers the installation of.
const maxBudgetTokens = 4096

// maxUsedBudgets bounds the budgets one client remembers calling on.
const maxUsedBudgets = 256

// Budgets is what GitHub's rate limits are counted against, shared by every
// client of one extension: an App's installation, whichever of its tokens made
// the call, or a plain token on its own. Credentials are held by digest only.
type Budgets struct {
	mu     sync.Mutex
	tokens map[string]budgetToken
	paused map[string]*RateLimitError
}

type budgetToken struct {
	budget  string
	expires time.Time
}

// NewBudgets returns an empty set of budgets.
func NewBudgets() *Budgets {
	return &Budgets{tokens: map[string]budgetToken{}, paused: map[string]*RateLimitError{}}
}

// installation records that token draws on installation id of the App API at
// api, until it expires.
func (b *Budgets) installation(token, api string, id int64, expires, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.tokens) >= maxBudgetTokens {
		for k, t := range b.tokens {
			if !now.Before(t.expires) {
				delete(b.tokens, k)
			}
		}
		if len(b.tokens) >= maxBudgetTokens {
			b.tokens = map[string]budgetToken{}
		}
	}
	b.tokens[tokenDigest(token)] = budgetToken{budget: "installation " + api + " " + strconv.FormatInt(id, 10), expires: expires}
}

// of is the budget a credential digest draws on.
func (b *Budgets) of(cred string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.tokens[cred]; ok {
		return t.budget
	}
	return cred
}

// transport sits under every call. GETs to the API are conditional, so an
// unchanged poll returns 304 and does not count against the rate limit. Once a
// budget is rate limited (primary reset or secondary Retry-After) calls on it
// are refused until the limit lifts, whichever of its tokens they carry.
type transport struct {
	base    http.RoundTripper
	apiHost string
	now     func() time.Time
	budgets *Budgets

	mu    sync.Mutex
	order *list.List
	cache map[string]*list.Element
	// used are the budgets this client has called on, for its doctor line.
	used map[string]bool
}

type cached struct {
	key    string
	etag   string
	status int
	header http.Header
	body   []byte
	// portable says etag is the digest revalidationTag computes, so the tag
	// for another token of the same budget can be worked out from body.
	portable bool
	weak     bool
}

func newTransport(base http.RoundTripper, apiHost string, now func() time.Time, budgets *Budgets) *transport {
	if base == nil {
		base = http.DefaultTransport
	}
	if budgets == nil {
		budgets = NewBudgets()
	}
	return &transport{base: base, apiHost: apiHost, now: now, budgets: budgets, order: list.New(), cache: map[string]*list.Element{}, used: map[string]bool{}}
}

// revalidationTag is the ETag GitHub gives body for this request's headers.
// GitHub's tag covers the Accept, Authorization and Cookie headers, so a
// rotated token gets a different tag for an unchanged response, and sending
// the old one is a full-price 200. GitHub doesn't document this: store only
// marks an answer portable when the tag it came with matches this digest, and
// a wrong guess costs what no guess would, a 200.
func revalidationTag(h http.Header, body []byte) string {
	sum := sha256.New()
	for _, name := range []string{"Accept", "Authorization", "Cookie"} {
		for _, v := range h.Values(name) {
			sum.Write([]byte(v))
			sum.Write([]byte{':'})
		}
	}
	sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil))
}

// etagFor is the tag to revalidate c with on req.
func (c *cached) etagFor(req *http.Request) string {
	if !c.portable {
		return c.etag
	}
	tag := `"` + revalidationTag(req.Header, c.body) + `"`
	if c.weak {
		tag = "W/" + tag
	}
	return tag
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

func credential(req *http.Request) string {
	auth := req.Header.Get("Authorization")
	if _, t, ok := strings.Cut(auth, " "); ok {
		auth = t
	}
	return tokenDigest(auth)
}

// revoking is a token's own revocation, which spends nothing worth saving and
// must still go out during a pause, or the token stays live until it expires.
func revoking(req *http.Request) bool {
	return req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/installation/token")
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	cred := t.budgets.of(credential(req))
	t.mu.Lock()
	if len(t.used) >= maxUsedBudgets && !t.used[cred] {
		// An App's JWTs each count as one, so this set must not grow forever.
		clear(t.used)
	}
	t.used[cred] = true
	t.mu.Unlock()
	if limit := t.pausedFor(cred); limit != nil && !revoking(req) {
		return nil, limit
	}
	key := cred + " " + req.URL.String()
	conditional := req.Method == http.MethodGet && req.URL.Host == t.apiHost && req.Header.Get("If-None-Match") == ""
	var hit *cached
	if conditional {
		if hit = t.lookup(key); hit != nil {
			etag := hit.etagFor(req)
			req = req.Clone(req.Context())
			req.Header.Set("If-None-Match", etag)
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
	tag, weak := strings.CutPrefix(etag, "W/")
	portable := tag == `"`+revalidationTag(req.Header, body)+`"`
	t.store(&cached{key: key, etag: etag, status: res.StatusCode, header: res.Header.Clone(), body: body, portable: portable, weak: weak})
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

func (t *transport) pausedFor(budget string) *RateLimitError {
	b := t.budgets
	b.mu.Lock()
	defer b.mu.Unlock()
	limit, ok := b.paused[budget]
	if !ok {
		return nil
	}
	if !t.now().Before(limit.Until) {
		delete(b.paused, budget)
		return nil
	}
	out := *limit
	return &out
}

func (t *transport) pause(budget string, limit *RateLimitError) {
	b := t.budgets
	b.mu.Lock()
	defer b.mu.Unlock()
	if prev, ok := b.paused[budget]; ok && t.now().Before(prev.Until) && !limit.Until.After(prev.Until) {
		return
	}
	out := *limit
	b.paused[budget] = &out
}

// PausedUntil reports the latest active pause on a budget this client has
// called on (for doctor).
func (t *transport) PausedUntil() (time.Time, bool) {
	t.mu.Lock()
	used := make([]string, 0, len(t.used))
	for k := range t.used {
		used = append(used, k)
	}
	t.mu.Unlock()
	var until time.Time
	for _, k := range used {
		if l := t.pausedFor(k); l != nil && l.Until.After(until) {
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
