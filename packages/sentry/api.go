package sentry

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
	"strings"
	"time"
)

// DefaultAPIURL is Sentry's SaaS address; a self-hosted Sentry sets the
// connection's api_url.
const DefaultAPIURL = "https://sentry.io"

// ErrCredential is a missing or unusable auth token.
var ErrCredential = errors.New("sentry credential unavailable")

// APIError is a non-2xx answer from Sentry.
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("sentry answered %d", e.Status) }

// Client calls one organization's Sentry API with the connection's token.
type Client struct {
	base, org string
	token     func(context.Context) (string, error)
	http      *http.Client
}

// NewClient returns a client for org at base (DefaultAPIURL when empty).
func NewClient(base, org string, token func(context.Context) (string, error), hc *http.Client) *Client {
	if base == "" {
		base = DefaultAPIURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimSuffix(base, "/"), org: org, token: token, http: hc}
}

// Issue is the part of a Sentry issue the package reads.
type Issue struct {
	ID        string `json:"id"`
	ShortID   string `json:"shortId"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit"`
	Level     string `json:"level"`
	Status    string `json:"status"`
	Substatus string `json:"substatus"`
	Permalink string `json:"permalink"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	Count     string `json:"count"`
	UserCount int    `json:"userCount"`
	Project   struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"project"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (http.Header, error) {
	token, err := c.token(ctx)
	if err != nil || token == "" {
		return nil, ErrCredential
	}
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = c.base + path
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %w", ErrCredential, &APIError{Status: resp.StatusCode})
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Status: resp.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return nil, fmt.Errorf("sentry answer: %w", err)
		}
	}
	return resp.Header, nil
}

func (c *Client) issuePath(id string) string {
	return "/api/0/organizations/" + url.PathEscape(c.org) + "/issues/" + url.PathEscape(id) + "/"
}

// Issues lists one page of the organization's unresolved issues, from a
// cursor ("" for the first page), and the next page's cursor ("" when it is
// the last).
func (c *Client) Issues(ctx context.Context, cursor string) ([]Issue, string, error) {
	q := url.Values{"query": {"is:unresolved"}, "limit": {"100"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out []Issue
	h, err := c.do(ctx, http.MethodGet, "/api/0/organizations/"+url.PathEscape(c.org)+"/issues/?"+q.Encode(), nil, &out)
	if err != nil {
		return nil, "", err
	}
	return out, nextCursor(h.Get("Link")), nil
}

var linkPart = regexp.MustCompile(`rel="next"; results="(true|false)"; cursor="([^"]*)"`)

// nextCursor reads the next page's cursor from Sentry's Link header, ""
// when there are no more results.
func nextCursor(link string) string {
	m := linkPart.FindStringSubmatch(link)
	if m == nil || m[1] != "true" {
		return ""
	}
	return m[2]
}

// Issue reads one issue.
func (c *Client) Issue(ctx context.Context, id string) (Issue, error) {
	var out Issue
	_, err := c.do(ctx, http.MethodGet, c.issuePath(id), nil, &out)
	return out, err
}

// LatestEvent reads an issue's latest event as Sentry gives it.
func (c *Client) LatestEvent(ctx context.Context, issue string) (json.RawMessage, error) {
	var out json.RawMessage
	_, err := c.do(ctx, http.MethodGet, c.issuePath(issue)+"events/latest/", nil, &out)
	return out, err
}

// Committers reads an event's suspect committers.
func (c *Client) Committers(ctx context.Context, project, event string) (json.RawMessage, error) {
	var out json.RawMessage
	_, err := c.do(ctx, http.MethodGet, "/api/0/projects/"+url.PathEscape(c.org)+"/"+url.PathEscape(project)+"/events/"+url.PathEscape(event)+"/committers/", nil, &out)
	return out, err
}

// Comment posts a comment on an issue.
func (c *Client) Comment(ctx context.Context, issue, text string) error {
	_, err := c.do(ctx, http.MethodPost, c.issuePath(issue)+"comments/", map[string]string{"text": text}, nil)
	return err
}

// Resolve marks an issue resolved.
func (c *Client) Resolve(ctx context.Context, issue string) error {
	_, err := c.do(ctx, http.MethodPut, c.issuePath(issue), map[string]string{"status": "resolved"}, nil)
	return err
}
