package pagerduty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is PagerDuty's REST API (api.eu.pagerduty.com for an EU
// account, set as the connection's api_url).
const DefaultAPIURL = "https://api.pagerduty.com"

// ErrCredential is a missing or unusable API token.
var ErrCredential = errors.New("pagerduty credential unavailable")

// APIError is a non-2xx answer from PagerDuty.
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("pagerduty answered %d", e.Status) }

// Client calls PagerDuty's REST API with the connection's token. From is
// the PagerDuty user's email that writes are made as.
type Client struct {
	base, from string
	token      func(context.Context) (string, error)
	http       *http.Client
}

// NewClient returns a client for base (DefaultAPIURL when empty).
func NewClient(base, from string, token func(context.Context) (string, error), hc *http.Client) *Client {
	if base == "" {
		base = DefaultAPIURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimSuffix(base, "/"), from: from, token: token, http: hc}
}

// Incident is the part of a PagerDuty incident the package reads.
type Incident struct {
	ID                 string `json:"id"`
	Number             int    `json:"incident_number"`
	Title              string `json:"title"`
	Status             string `json:"status"`
	Urgency            string `json:"urgency"`
	HTMLURL            string `json:"html_url"`
	CreatedAt          string `json:"created_at"`
	LastStatusChangeAt string `json:"last_status_change_at"`
	Service            struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"service"`
}

// ErrNoFrom is a write with no From address configured.
var ErrNoFrom = errors.New("pagerduty connection has no from address")

func (c *Client) do(ctx context.Context, method, path string, write bool, body, out any) error {
	token, err := c.token(ctx)
	if err != nil || token == "" {
		return ErrCredential
	}
	if write && c.from == "" {
		return ErrNoFrom
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token token="+token)
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if write {
		req.Header.Set("From", c.from)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %w", ErrCredential, &APIError{Status: resp.StatusCode})
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return fmt.Errorf("pagerduty answer: %w", err)
		}
	}
	return nil
}

// PageSize is how many incidents a list page asks for.
const PageSize = 100

// Incidents lists one page of open (triggered or acknowledged) incidents,
// of the services when given, from an offset, and the next page's offset
// (-1 on the last page).
func (c *Client) Incidents(ctx context.Context, services []string, offset int) ([]Incident, int, error) {
	q := url.Values{"statuses[]": {"triggered", "acknowledged"}, "limit": {strconv.Itoa(PageSize)}, "offset": {strconv.Itoa(offset)}}
	for _, s := range services {
		q.Add("service_ids[]", s)
	}
	var out struct {
		Incidents []Incident `json:"incidents"`
		More      bool       `json:"more"`
	}
	if err := c.do(ctx, http.MethodGet, "/incidents?"+q.Encode(), false, nil, &out); err != nil {
		return nil, 0, err
	}
	next := -1
	if out.More && len(out.Incidents) > 0 {
		next = offset + len(out.Incidents)
	}
	return out.Incidents, next, nil
}

func incidentPath(id string) string { return "/incidents/" + url.PathEscape(id) }

// Incident reads one incident.
func (c *Client) Incident(ctx context.Context, id string) (Incident, error) {
	var out struct {
		Incident Incident `json:"incident"`
	}
	err := c.do(ctx, http.MethodGet, incidentPath(id), false, nil, &out)
	return out.Incident, err
}

// Note is an incident note.
type Note struct {
	Content string `json:"content"`
	By      string `json:"by,omitempty"`
	At      string `json:"at,omitempty"`
}

// Notes reads an incident's notes.
func (c *Client) Notes(ctx context.Context, id string) ([]Note, error) {
	var out struct {
		Notes []struct {
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
			User      struct {
				Summary string `json:"summary"`
			} `json:"user"`
		} `json:"notes"`
	}
	if err := c.do(ctx, http.MethodGet, incidentPath(id)+"/notes", false, nil, &out); err != nil {
		return nil, err
	}
	notes := []Note{}
	for _, n := range out.Notes {
		notes = append(notes, Note{Content: n.Content, By: n.User.Summary, At: n.CreatedAt})
	}
	return notes, nil
}

// Alert is one of an incident's alerts.
type Alert struct {
	ID      string          `json:"id"`
	Summary string          `json:"summary"`
	Status  string          `json:"status,omitempty"`
	At      string          `json:"created_at,omitempty"`
	URL     string          `json:"html_url,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// MaxAlertDetails bounds the details an alert returns.
const MaxAlertDetails = 4096

// Alerts reads an incident's alerts, with each alert's details (what the
// monitoring tool sent, such as a Sentry issue link) when they fit.
func (c *Client) Alerts(ctx context.Context, id string) ([]Alert, error) {
	var out struct {
		Alerts []struct {
			ID        string `json:"id"`
			Summary   string `json:"summary"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
			HTMLURL   string `json:"html_url"`
			Body      struct {
				Details json.RawMessage `json:"details"`
			} `json:"body"`
		} `json:"alerts"`
	}
	if err := c.do(ctx, http.MethodGet, incidentPath(id)+"/alerts", false, nil, &out); err != nil {
		return nil, err
	}
	alerts := []Alert{}
	for _, a := range out.Alerts {
		x := Alert{ID: a.ID, Summary: a.Summary, Status: a.Status, At: a.CreatedAt, URL: a.HTMLURL}
		if len(a.Body.Details) > 0 && len(a.Body.Details) <= MaxAlertDetails && string(a.Body.Details) != "null" {
			x.Details = a.Body.Details
		}
		alerts = append(alerts, x)
	}
	return alerts, nil
}

// AddNote adds a note to an incident.
func (c *Client) AddNote(ctx context.Context, id, content string) error {
	return c.do(ctx, http.MethodPost, incidentPath(id)+"/notes", true, map[string]any{"note": map[string]string{"content": content}}, nil)
}

// Acknowledge acknowledges an incident.
func (c *Client) Acknowledge(ctx context.Context, id string) (Incident, error) {
	var out struct {
		Incident Incident `json:"incident"`
	}
	err := c.do(ctx, http.MethodPut, incidentPath(id), true, map[string]any{"incident": map[string]string{"type": "incident_reference", "status": "acknowledged"}}, &out)
	return out.Incident, err
}
