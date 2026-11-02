// Package pagerdutytest is a fake of the parts of PagerDuty's REST API the
// pagerduty package uses: the incident list (offset paginated, as PagerDuty
// does), one incident, its notes and alerts, adding a note and changing an
// incident's status. It also signs V3 webhook deliveries the way PagerDuty
// does. Tests and the certification fixture use it; nothing in it talks to
// PagerDuty.
package pagerdutytest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Service is an incident's service.
type Service struct {
	ID      string `json:"id"`
	Summary string `json:"summary,omitempty"`
}

// Incident is a PagerDuty incident as the API gives it.
type Incident struct {
	ID                 string  `json:"id"`
	Type               string  `json:"type"`
	Number             int     `json:"incident_number"`
	Title              string  `json:"title"`
	Status             string  `json:"status"`
	Urgency            string  `json:"urgency,omitempty"`
	HTMLURL            string  `json:"html_url,omitempty"`
	CreatedAt          string  `json:"created_at,omitempty"`
	LastStatusChangeAt string  `json:"last_status_change_at,omitempty"`
	Service            Service `json:"service"`
}

// Note is a note added to an incident.
type Note struct {
	Incident, Content, From string
}

// Server is the fake. Its zero page size is 100.
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	tokens    map[string]bool
	incidents map[string]Incident
	alerts    map[string]json.RawMessage
	notes     []Note
	pageSize  int
	clock     int
}

// New starts a fake.
func New() *Server {
	s := &Server{tokens: map[string]bool{}, incidents: map[string]Incident{}, alerts: map[string]json.RawMessage{}, pageSize: 100}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// AddToken accepts an API token.
func (s *Server) AddToken(t string) { s.mu.Lock(); s.tokens[t] = true; s.mu.Unlock() }

// SetPageSize sets how many incidents a list page holds.
func (s *Server) SetPageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// Put adds or replaces an incident.
func (s *Server) Put(i Incident) {
	i.Type = "incident"
	s.mu.Lock()
	s.incidents[i.ID] = i
	s.mu.Unlock()
}

// Incident returns an incident as the fake holds it.
func (s *Server) Incident(id string) (Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.incidents[id]
	return i, ok
}

// SetAlerts sets an incident's alerts (PagerDuty's alerts JSON array).
func (s *Server) SetAlerts(incident string, alerts json.RawMessage) {
	s.mu.Lock()
	s.alerts[incident] = alerts
	s.mu.Unlock()
}

// Notes lists the notes added, in order.
func (s *Server) Notes() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Note(nil), s.notes...)
}

// Webhook is the body of a V3 webhook for an incident event type
// (incident.triggered, incident.acknowledged, incident.resolved, ...).
func (s *Server) Webhook(eventID, eventType, incident string) []byte {
	i, _ := s.Incident(incident)
	b, _ := json.Marshal(map[string]any{"event": map[string]any{
		"id": eventID, "event_type": eventType, "resource_type": "incident", "occurred_at": i.LastStatusChangeAt,
		"data": map[string]any{"id": i.ID, "type": "incident", "number": i.Number, "title": i.Title, "status": i.Status, "html_url": i.HTMLURL, "service": i.Service},
	}})
	return b
}

// Signature is PagerDuty's X-PagerDuty-Signature for a body.
func Signature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}

// Headers are the headers PagerDuty sends with a delivery.
func Headers(secret string, body []byte) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-PagerDuty-Signature", Signature(secret, body))
	h.Set("X-Webhook-Id", "wh-1")
	return h
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Token token=")
	if !s.tokens[token] {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "Unauthorized"}})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 || parts[0] != "incidents" {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.list(w, r)
	case len(parts) == 2 && r.Method == http.MethodGet:
		i, ok := s.incidents[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"incident": i})
	case len(parts) == 2 && r.Method == http.MethodPut:
		i, ok := s.incidents[parts[1]]
		var body struct {
			Incident struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"incident"`
		}
		if !ok || r.Header.Get("From") == "" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Incident.Type != "incident_reference" || body.Incident.Status == "" {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		if i.Status != body.Incident.Status {
			s.clock++
			i.Status, i.LastStatusChangeAt = body.Incident.Status, "2026-11-01T10:"+twoDigits(s.clock)+":00Z"
			s.incidents[i.ID] = i
		}
		writeJSON(w, map[string]any{"incident": i})
	case len(parts) == 3 && parts[2] == "notes" && r.Method == http.MethodGet:
		var list []map[string]any
		for _, n := range s.notes {
			if n.Incident == parts[1] {
				list = append(list, map[string]any{"content": n.Content, "user": map[string]string{"summary": n.From}, "created_at": "2026-11-01T10:00:00Z"})
			}
		}
		if list == nil {
			list = []map[string]any{}
		}
		writeJSON(w, map[string]any{"notes": list})
	case len(parts) == 3 && parts[2] == "notes" && r.Method == http.MethodPost:
		var body struct {
			Note struct {
				Content string `json:"content"`
			} `json:"note"`
		}
		_, ok := s.incidents[parts[1]]
		if !ok || r.Header.Get("From") == "" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Note.Content == "" {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		s.notes = append(s.notes, Note{Incident: parts[1], Content: body.Note.Content, From: r.Header.Get("From")})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"note": map[string]string{"id": strconv.Itoa(len(s.notes)), "content": body.Note.Content}})
	case len(parts) == 3 && parts[2] == "alerts" && r.Method == http.MethodGet:
		a, ok := s.alerts[parts[1]]
		if !ok {
			a = json.RawMessage(`[]`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"alerts":` + string(a) + `}`))
	default:
		http.NotFound(w, r)
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n % 60)
}

// list serves incidents with the requested statuses (and services), in ID
// order, offset paginated with PagerDuty's "more".
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	statuses, services := q["statuses[]"], q["service_ids[]"]
	var ids []string
	for id, i := range s.incidents {
		if (len(statuses) == 0 || slices.Contains(statuses, i.Status)) && (len(services) == 0 || slices.Contains(services, i.Service.ID)) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit := s.pageSize
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l < limit {
		limit = l
	}
	end := min(offset+limit, len(ids))
	page := []Incident{}
	if offset < len(ids) {
		for _, id := range ids[offset:end] {
			page = append(page, s.incidents[id])
		}
	}
	writeJSON(w, map[string]any{"incidents": page, "offset": offset, "limit": limit, "more": end < len(ids)})
}

func writeJSON(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	_ = json.NewEncoder(w).Encode(v)
}
