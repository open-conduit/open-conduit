package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SecretFunc resolves a connection secret by name (a key of the connection's
// secret_refs). It must never log or return the reference.
type SecretFunc func(ctx context.Context, name string) (string, error)

// ErrCredential is returned for a missing, invalid or revoked bot token. It
// never carries the reference or the value.
var ErrCredential = errors.New("slack credential unavailable")

// RateLimitError means Slack rate limited a Web API method for this app; calls
// to that method are refused locally until Until, without reaching the network.
type RateLimitError struct {
	Method string
	Until  time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("slack rate limit on %s until %s", e.Method, e.Until.UTC().Format(time.RFC3339))
}

// APIError is a Slack Web API refusal (`ok: false`). Code is Slack's error
// name, sanitized; the response body is never retained.
type APIError struct{ Method, Code string }

func (e *APIError) Error() string { return "slack " + e.Method + ": " + e.Code }

// StatusError is a non-2xx HTTP answer that was not a rate limit.
type StatusError struct {
	Method string
	Status int
}

func (e *StatusError) Error() string { return fmt.Sprintf("slack %s: http %d", e.Method, e.Status) }

// IsCode reports whether err is a Slack refusal with that error name.
func IsCode(err error, code string) bool {
	var a *APIError
	return errors.As(err, &a) && a.Code == code
}

var safeCode = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ClientOptions tune the client.
type ClientOptions struct {
	HTTP *http.Client
	Now  func() time.Time
}

// Client calls the Slack Web API with the connection's bot token. A method
// Slack rate limits (HTTP 429 with Retry-After) is paused for this client;
// its calls then fail locally with RateLimitError until the limit lifts.
type Client struct {
	api     string
	secrets SecretFunc
	http    *http.Client
	now     func() time.Time

	mu     sync.Mutex
	tokens map[string]string // by secret name
	paused map[string]time.Time
}

// NewClient binds connection config to a secret resolver.
func NewClient(c ConnectionConfig, secrets SecretFunc, o ClientOptions) (*Client, error) {
	if secrets == nil || c.APIURL == "" {
		return nil, errors.New("slack client: config and secrets are required")
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{api: c.APIURL, secrets: secrets, http: o.HTTP, now: o.Now, tokens: map[string]string{}, paused: map[string]time.Time{}}, nil
}

func (c *Client) bearer(ctx context.Context, name string, refresh bool) (string, error) {
	c.mu.Lock()
	if t := c.tokens[name]; !refresh && t != "" {
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	t, err := c.secrets(ctx, name)
	if err != nil || strings.TrimSpace(t) == "" {
		return "", ErrCredential
	}
	t = strings.TrimSpace(t)
	c.mu.Lock()
	c.tokens[name] = t
	c.mu.Unlock()
	return t, nil
}

func (c *Client) pausedFor(method string) *RateLimitError {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.paused[method]
	if !ok {
		return nil
	}
	if !c.now().Before(until) {
		delete(c.paused, method)
		return nil
	}
	return &RateLimitError{Method: method, Until: until}
}

func (c *Client) pause(method string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.paused[method]; ok && !until.After(prev) {
		return
	}
	c.paused[method] = until
}

// PausedUntil reports the latest active method pause (for doctor).
func (c *Client) PausedUntil() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var until time.Time
	for _, u := range c.paused {
		if c.now().Before(u) && u.After(until) {
			until = u
		}
	}
	return until, !until.IsZero()
}

// credentialCodes are Slack refusals meaning the token cannot be used.
var credentialCodes = map[string]bool{"invalid_auth": true, "not_authed": true, "token_revoked": true, "token_expired": true, "account_inactive": true}

// call invokes a Web API method. Reads send form fields; writes send JSON.
// A credential refusal re-resolves the token once, so a rotated reference
// takes effect without a restart.
func (c *Client) call(ctx context.Context, method string, form url.Values, body any, out any) error {
	return c.callWith(ctx, SecretBotToken, method, form, body, out)
}

func (c *Client) callWith(ctx context.Context, secret, method string, form url.Values, body any, out any) error {
	if limit := c.pausedFor(method); limit != nil {
		return limit
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.bearer(ctx, secret, attempt > 0)
		if err != nil {
			return err
		}
		var req *http.Request
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				return err
			}
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/"+method, bytes.NewReader(data))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		} else {
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/"+method, strings.NewReader(form.Encode()))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := c.http.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if err != nil {
			return err
		}
		if res.StatusCode == http.StatusTooManyRequests {
			wait := time.Minute
			if secs, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
			until := c.now().Add(wait)
			c.pause(method, until)
			return &RateLimitError{Method: method, Until: until}
		}
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return &StatusError{Method: method, Status: res.StatusCode}
		}
		var envelope struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return &StatusError{Method: method, Status: res.StatusCode}
		}
		if !envelope.OK {
			code := envelope.Error
			if !safeCode.MatchString(code) {
				code = "unknown"
			}
			if code == "ratelimited" {
				until := c.now().Add(time.Minute)
				c.pause(method, until)
				return &RateLimitError{Method: method, Until: until}
			}
			if credentialCodes[code] {
				if attempt == 0 && (code == "invalid_auth" || code == "token_expired") {
					continue
				}
				return ErrCredential
			}
			return &APIError{Method: method, Code: code}
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
	return ErrCredential
}

// Metadata is Slack message metadata. An agent reply carries the
// discoverable marker here (MarkerEventType), invisible to readers.
type Metadata struct {
	EventType    string          `json:"event_type"`
	EventPayload json.RawMessage `json:"event_payload,omitempty"`
}

// File is a shared file's identity.
type File struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Message is a Slack message as the Web API and the Events API present it.
type Message struct {
	Type        string `json:"type,omitempty"`
	Subtype     string `json:"subtype,omitempty"`
	User        string `json:"user,omitempty"`
	BotID       string `json:"bot_id,omitempty"`
	Text        string `json:"text,omitempty"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	ReplyCount  int    `json:"reply_count,omitempty"`
	LatestReply string `json:"latest_reply,omitempty"`
	Edited      *struct {
		User string `json:"user,omitempty"`
		TS   string `json:"ts"`
	} `json:"edited,omitempty"`
	Files    []File    `json:"files,omitempty"`
	Metadata *Metadata `json:"metadata,omitempty"`
}

type listing struct {
	Messages         []Message `json:"messages"`
	HasMore          bool      `json:"has_more"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// History reads one page of a channel's top-level messages in [oldest,
// latest], newest first, with message metadata. next is empty on the last page.
func (c *Client) History(ctx context.Context, channel, oldest, latest, cursor string, limit int) ([]Message, string, error) {
	form := url.Values{"channel": {channel}, "inclusive": {"true"}, "include_all_metadata": {"true"}, "limit": {strconv.Itoa(limit)}}
	if oldest != "" {
		form.Set("oldest", oldest)
	}
	if latest != "" {
		form.Set("latest", latest)
	}
	if cursor != "" {
		form.Set("cursor", cursor)
	}
	var out listing
	if err := c.call(ctx, "conversations.history", form, nil, &out); err != nil {
		return nil, "", err
	}
	next := out.ResponseMetadata.NextCursor
	if !out.HasMore {
		next = ""
	}
	return out.Messages, next, nil
}

// Replies reads one page of a thread (its parent first), with metadata.
func (c *Client) Replies(ctx context.Context, channel, thread, oldest, latest, cursor string, limit int) ([]Message, string, error) {
	form := url.Values{"channel": {channel}, "ts": {thread}, "inclusive": {"true"}, "include_all_metadata": {"true"}, "limit": {strconv.Itoa(limit)}}
	if oldest != "" {
		form.Set("oldest", oldest)
	}
	if latest != "" {
		form.Set("latest", latest)
	}
	if cursor != "" {
		form.Set("cursor", cursor)
	}
	var out listing
	if err := c.call(ctx, "conversations.replies", form, nil, &out); err != nil {
		return nil, "", err
	}
	next := out.ResponseMetadata.NextCursor
	if !out.HasMore {
		next = ""
	}
	return out.Messages, next, nil
}

// maxThreadPages bounds one thread read (200 messages a page).
const maxThreadPages = 50

// ErrThreadTooLong means a thread exceeded the bounded read.
var ErrThreadTooLong = errors.New("slack thread exceeds the bounded read")

// Thread reads a whole thread up to latest (inclusive), following cursors.
// It returns the number of API calls made. An unfinished read is an error,
// never a short success.
func (c *Client) Thread(ctx context.Context, channel, thread, oldest, latest string) ([]Message, int, error) {
	var out []Message
	cursor := ""
	for calls := 1; calls <= maxThreadPages; calls++ {
		page, next, err := c.Replies(ctx, channel, thread, oldest, latest, cursor, 200)
		if err != nil {
			return nil, calls, err
		}
		out = append(out, page...)
		if next == "" {
			return out, calls, nil
		}
		cursor = next
	}
	return nil, maxThreadPages, ErrThreadTooLong
}

// Post is chat.postMessage into a thread with metadata.
type Post struct {
	Channel   string    `json:"channel"`
	ThreadTS  string    `json:"thread_ts,omitempty"`
	Text      string    `json:"text"`
	Broadcast bool      `json:"reply_broadcast,omitempty"`
	Metadata  *Metadata `json:"metadata,omitempty"`
	// Links are not unfurled: a reply never creates follow-up previews.
	UnfurlLinks bool `json:"unfurl_links"`
	UnfurlMedia bool `json:"unfurl_media"`
}

// PostMessage posts and returns the new message's ts.
func (c *Client) PostMessage(ctx context.Context, p Post) (string, error) {
	var out struct {
		TS string `json:"ts"`
	}
	if err := c.call(ctx, "chat.postMessage", nil, p, &out); err != nil {
		return "", err
	}
	if out.TS == "" {
		return "", &APIError{Method: "chat.postMessage", Code: "no_ts"}
	}
	return out.TS, nil
}

// Identity is what auth.test reports for the bot token.
type Identity struct {
	Team   string `json:"team_id"`
	User   string `json:"user_id"`
	BotID  string `json:"bot_id"`
	TeamNm string `json:"team"`
}

// OpenConnection asks for a Socket Mode WebSocket URL with the app-level
// token (apps.connections.open, scope connections:write).
func (c *Client) OpenConnection(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.callWith(ctx, SecretAppToken, "apps.connections.open", url.Values{}, nil, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", &APIError{Method: "apps.connections.open", Code: "no_url"}
	}
	return out.URL, nil
}

// AuthTest verifies the token (doctor's connection test).
func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	var out Identity
	err := c.call(ctx, "auth.test", url.Values{}, nil, &out)
	return out, err
}
