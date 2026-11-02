// Package sentrytest is a fake of the parts of Sentry's web API the sentry
// package uses: an organization's issue list (paginated with Link headers,
// as Sentry does), one issue, its latest event, an event's suspect
// committers, issue comments and status updates. It also signs webhook
// deliveries the way Sentry does. Tests and the certification fixture use
// it; nothing in it talks to Sentry.
package sentrytest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Project is an issue's project.
type Project struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
}

// Issue is a Sentry issue as the API and the issue webhook give it.
type Issue struct {
	ID        string  `json:"id"`
	ShortID   string  `json:"shortId"`
	Title     string  `json:"title"`
	Culprit   string  `json:"culprit,omitempty"`
	Level     string  `json:"level,omitempty"`
	Status    string  `json:"status"`
	Substatus string  `json:"substatus,omitempty"`
	Permalink string  `json:"permalink,omitempty"`
	FirstSeen string  `json:"firstSeen,omitempty"`
	LastSeen  string  `json:"lastSeen,omitempty"`
	Count     string  `json:"count,omitempty"`
	UserCount int     `json:"userCount,omitempty"`
	Project   Project `json:"project"`
}

// Comment is a comment posted on an issue.
type Comment struct {
	Issue, Text string
}

// Server is the fake. Its zero page size is 100.
type Server struct {
	*httptest.Server
	Org string

	mu         sync.Mutex
	tokens     map[string]bool
	issues     map[string]Issue
	events     map[string]json.RawMessage // latest event by issue
	committers map[string]json.RawMessage // by event ID
	comments   []Comment
	pageSize   int
	fail       bool
}

// New starts a fake for one organization.
func New(org string) *Server {
	s := &Server{Org: org, tokens: map[string]bool{}, issues: map[string]Issue{}, events: map[string]json.RawMessage{}, committers: map[string]json.RawMessage{}, pageSize: 100}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// AddToken accepts a bearer token.
func (s *Server) AddToken(t string) { s.mu.Lock(); s.tokens[t] = true; s.mu.Unlock() }

// SetPageSize sets how many issues a list page holds.
func (s *Server) SetPageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// SetFailing makes every API call answer 500 until it is cleared.
func (s *Server) SetFailing(f bool) { s.mu.Lock(); s.fail = f; s.mu.Unlock() }

// Put adds or replaces an issue.
func (s *Server) Put(i Issue) { s.mu.Lock(); s.issues[i.ID] = i; s.mu.Unlock() }

// Issue returns an issue as the fake holds it.
func (s *Server) Issue(id string) (Issue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.issues[id]
	return i, ok
}

// SetLatestEvent sets an issue's latest event (Sentry's event JSON).
func (s *Server) SetLatestEvent(issue string, event json.RawMessage) {
	s.mu.Lock()
	s.events[issue] = event
	s.mu.Unlock()
}

// SetCommitters sets an event's suspect committers (Sentry's committers JSON).
func (s *Server) SetCommitters(event string, committers json.RawMessage) {
	s.mu.Lock()
	s.committers[event] = committers
	s.mu.Unlock()
}

// Comments lists the comments posted, in order.
func (s *Server) Comments() []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Comment(nil), s.comments...)
}

// IssueWebhook is the body of an issue webhook for an action (created,
// resolved, unresolved, ...).
func (s *Server) IssueWebhook(action, id string) []byte {
	i, _ := s.Issue(id)
	b, _ := json.Marshal(map[string]any{"action": action, "data": map[string]any{"issue": i}, "installation": map[string]string{"uuid": "inst-1"}})
	return b
}

// AlertWebhook is the body of an issue alert rule firing for an issue's
// event.
func (s *Server) AlertWebhook(rule, issue, event string) []byte {
	i, _ := s.Issue(issue)
	b, _ := json.Marshal(map[string]any{"action": "triggered", "data": map[string]any{
		"event":          map[string]any{"event_id": event, "issue_id": issue, "title": i.Title, "culprit": i.Culprit, "level": i.Level, "web_url": i.Permalink, "project": i.Project.ID},
		"triggered_rule": rule,
	}, "installation": map[string]string{"uuid": "inst-1"}})
	return b
}

// Signature is Sentry's Sentry-Hook-Signature for a body.
func Signature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Headers are the headers Sentry sends with a delivery of a resource.
func Headers(secret, resource, requestID string, body []byte) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Sentry-Hook-Resource", resource)
	h.Set("Sentry-Hook-Timestamp", "1730000000")
	h.Set("Sentry-Hook-Signature", Signature(secret, body))
	h.Set("Request-ID", requestID)
	return h
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		http.Error(w, `{"detail":"internal"}`, http.StatusInternalServerError)
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.tokens[token] {
		http.Error(w, `{"detail":"Invalid token"}`, http.StatusUnauthorized)
		return
	}
	prefix := "/api/0/organizations/" + s.Org + "/issues/"
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		if s.committersFor(w, r) {
			return
		}
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		s.list(w, r)
	case len(parts) == 1 && r.Method == http.MethodGet:
		i, ok := s.issues[parts[0]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, i)
	case len(parts) == 1 && r.Method == http.MethodPut:
		i, ok := s.issues[parts[0]]
		var body struct {
			Status string `json:"status"`
		}
		if !ok || json.NewDecoder(r.Body).Decode(&body) != nil || body.Status == "" {
			http.Error(w, `{"detail":"bad request"}`, http.StatusBadRequest)
			return
		}
		i.Status, i.Substatus = body.Status, ""
		s.issues[i.ID] = i
		writeJSON(w, map[string]string{"status": i.Status})
	case len(parts) == 3 && parts[1] == "events" && parts[2] == "latest" && r.Method == http.MethodGet:
		e, ok := s.events[parts[0]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(e)
	case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		if _, ok := s.issues[parts[0]]; !ok || json.NewDecoder(r.Body).Decode(&body) != nil || body.Text == "" {
			http.Error(w, `{"detail":"bad request"}`, http.StatusBadRequest)
			return
		}
		s.comments = append(s.comments, Comment{Issue: parts[0], Text: body.Text})
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": strconv.Itoa(len(s.comments)), "data": map[string]string{"text": body.Text}})
	default:
		http.NotFound(w, r)
	}
}

// committersFor serves /api/0/projects/{org}/{project}/events/{id}/committers/.
func (s *Server) committersFor(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 8 || parts[2] != "projects" || parts[3] != s.Org || parts[5] != "events" || parts[7] != "committers" {
		return false
	}
	c, ok := s.committers[parts[6]]
	if !ok {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(c)
	return true
}

// list serves unresolved issues in ID order, a page at a time, with
// Sentry's Link header (cursor "0:<offset>:0").
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("query"); q != "is:unresolved" {
		http.Error(w, `{"detail":"unsupported query"}`, http.StatusBadRequest)
		return
	}
	var ids []string
	for id, i := range s.issues {
		if i.Status == "unresolved" {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(a, b int) bool {
		x, _ := strconv.Atoi(ids[a])
		y, _ := strconv.Atoi(ids[b])
		return x < y
	})
	offset := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		f := strings.Split(c, ":")
		if len(f) != 3 {
			http.Error(w, `{"detail":"bad cursor"}`, http.StatusBadRequest)
			return
		}
		offset, _ = strconv.Atoi(f[1])
	}
	end := min(offset+s.pageSize, len(ids))
	page := []Issue{}
	if offset < len(ids) {
		for _, id := range ids[offset:end] {
			page = append(page, s.issues[id])
		}
	}
	more := end < len(ids)
	base := s.URL + r.URL.Path
	w.Header().Set("Link", fmt.Sprintf(`<%s?cursor=0:%d:1>; rel="previous"; results="%t"; cursor="0:%d:1", <%s?cursor=0:%d:0>; rel="next"; results="%t"; cursor="0:%d:0"`,
		base, offset, offset > 0, offset, base, end, more, end))
	writeJSON(w, page)
}

func writeJSON(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	_ = json.NewEncoder(w).Encode(v)
}
