// Package slacktest is an in-memory fake of the Slack Web API and Events API
// deliveries for tests. It implements only what the Slack extension calls:
// conversations.history, conversations.replies, chat.postMessage and
// auth.test, with cursor pagination, message metadata, edits, deletions
// (tombstones for parents with replies), 429 rate limits and injected faults.
// It is test support only; nothing in production imports it.
package slacktest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/extensions/slack"
)

// Server is the fake. URL is the Web API base (a connection's api_url).
type Server struct {
	URL  string
	Team string
	srv  *httptest.Server

	mu       sync.Mutex
	now      time.Time
	tokens   map[string]bool
	botUser  string
	botID    string
	channels map[string][]*slack.Message
	calls    map[string]int
	limits   map[string][2]int // method -> {retry-after seconds, remaining 429s}
	postMode string
	events   int

	noMeta    bool
	scopes    []string          // nil: the manifest's BotScopes
	members   map[string]string // channel ID -> name the bot is a member of
	appTokens map[string]bool
	sockets   []*socket
	connects  int
	envelopes int
	acks      map[string]bool
}

// New starts a fake workspace accepting the given bot tokens.
func New(team string, tokens ...string) *Server {
	s := &Server{Team: team, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), tokens: map[string]bool{}, botUser: "UBOT", botID: "BAPP",
		channels: map[string][]*slack.Message{}, calls: map[string]int{}, limits: map[string][2]int{}, appTokens: map[string]bool{}, acks: map[string]bool{}}
	for _, t := range tokens {
		s.tokens[t] = true
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL + "/api"
	return s
}

// SetScopes sets the bot token's granted scopes auth.test reports (nil: the
// manifest's).
func (s *Server) SetScopes(scopes []string) { s.mu.Lock(); s.scopes = scopes; s.mu.Unlock() }

// AddMember makes the bot a member of a channel (users.conversations).
func (s *Server) AddMember(channel, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members == nil {
		s.members = map[string]string{}
	}
	s.members[channel] = name
}

// Close stops the server and its sockets.
func (s *Server) Close() { s.SocketDrop(); s.srv.Close() }

// Now is the fake's clock (each new message advances it by a second).
func (s *Server) Now() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.now }

// Advance moves the fake's clock.
func (s *Server) Advance(d time.Duration) { s.mu.Lock(); s.now = s.now.Add(d); s.mu.Unlock() }

// SetClock sets the fake's clock.
func (s *Server) SetClock(t time.Time) { s.mu.Lock(); s.now = t.UTC(); s.mu.Unlock() }

// BotUser is the app's bot user ID.
func (s *Server) BotUser() string { return s.botUser }

// RevokeToken makes a token invalid.
func (s *Server) RevokeToken(t string) { s.mu.Lock(); delete(s.tokens, t); s.mu.Unlock() }

// AddToken accepts another token.
func (s *Server) AddToken(t string) { s.mu.Lock(); s.tokens[t] = true; s.mu.Unlock() }

// RateLimit answers the next n calls to method with HTTP 429 and Retry-After.
func (s *Server) RateLimit(method string, retryAfter, n int) {
	s.mu.Lock()
	s.limits[method] = [2]int{retryAfter, n}
	s.mu.Unlock()
}

// FailNextPost makes the next chat.postMessage misbehave: "lost" creates the
// message and then answers 500 (an ambiguous post); "refuse:<code>" answers
// ok:false without posting.
func (s *Server) FailNextPost(mode string) { s.mu.Lock(); s.postMode = mode; s.mu.Unlock() }

// WithholdMetadata makes listings omit message metadata, as for a token or
// workspace that does not return it.
func (s *Server) WithholdMetadata(on bool) { s.mu.Lock(); s.noMeta = on; s.mu.Unlock() }

// Methods lists every method called so far.
func (s *Server) Methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for m := range s.calls {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Calls reports how many times a method was called.
func (s *Server) Calls(method string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls[method] }

func (s *Server) tick() string {
	s.now = s.now.Add(time.Second)
	return slack.TS(s.now)
}

// Post adds a human top-level message and returns its ts.
func (s *Server) Post(channel, user, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tick()
	s.channels[channel] = append(s.channels[channel], &slack.Message{Type: "message", User: user, Text: text, TS: ts})
	return ts
}

// Reply adds a human reply in a thread and returns its ts.
func (s *Server) Reply(channel, thread, user, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tick()
	s.channels[channel] = append(s.channels[channel], &slack.Message{Type: "message", User: user, Text: text, TS: ts, ThreadTS: thread})
	return ts
}

// BotReply adds another integration's bot message in a thread.
func (s *Server) BotReply(channel, thread, botID, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tick()
	s.channels[channel] = append(s.channels[channel], &slack.Message{Type: "message", Subtype: "bot_message", BotID: botID, Text: text, TS: ts, ThreadTS: thread})
	return ts
}

// Join adds a channel_join message (not conversation content).
func (s *Server) Join(channel, user string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tick()
	s.channels[channel] = append(s.channels[channel], &slack.Message{Type: "message", Subtype: "channel_join", User: user, Text: "<@" + user + "> has joined the channel", TS: ts})
	return ts
}

func (s *Server) find(channel, ts string) *slack.Message {
	for _, m := range s.channels[channel] {
		if m.TS == ts {
			return m
		}
	}
	return nil
}

// Edit changes a message's text, as its author editing it.
func (s *Server) Edit(channel, ts, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(channel, ts)
	if m == nil {
		return
	}
	m.Text = text
	m.Edited = &struct {
		User string `json:"user,omitempty"`
		TS   string `json:"ts"`
	}{User: m.User, TS: s.tick()}
}

// Delete deletes a message: a parent with replies becomes a tombstone,
// anything else disappears. It returns the message as it was.
func (s *Server) Delete(channel, ts string) slack.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(channel, ts)
	if m == nil {
		return slack.Message{}
	}
	prev := *m
	if s.replies(channel, ts) > 0 && (m.ThreadTS == "" || m.ThreadTS == m.TS) {
		*m = slack.Message{Type: "message", Subtype: "tombstone", User: "USLACKBOT", Text: "This message was deleted.", TS: ts, ThreadTS: ts}
		return prev
	}
	out := s.channels[channel][:0]
	for _, x := range s.channels[channel] {
		if x.TS != ts {
			out = append(out, x)
		}
	}
	s.channels[channel] = out
	return prev
}

func (s *Server) replies(channel, thread string) int {
	n := 0
	for _, m := range s.channels[channel] {
		if m.ThreadTS == thread && m.TS != thread {
			n++
		}
	}
	return n
}

// Message returns a message's current state.
func (s *Server) Message(channel, ts string) (slack.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(channel, ts)
	if m == nil {
		return slack.Message{}, false
	}
	return s.present(channel, *m, true), true
}

// Thread returns a thread's messages, parent first.
func (s *Server) Thread(channel, thread string) []slack.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thread(channel, thread, true)
}

func (s *Server) thread(channel, thread string, meta bool) []slack.Message {
	var out []slack.Message
	for _, m := range s.channels[channel] {
		if m.TS == thread || m.ThreadTS == thread {
			out = append(out, s.present(channel, *m, meta))
		}
	}
	sort.Slice(out, func(i, j int) bool { return tsLess(out[i].TS, out[j].TS) })
	return out
}

// present adds the derived thread summary and drops metadata unless asked.
func (s *Server) present(channel string, m slack.Message, meta bool) slack.Message {
	if !meta || s.noMeta {
		m.Metadata = nil
	}
	if m.ThreadTS == "" || m.ThreadTS == m.TS {
		if n := s.replies(channel, m.TS); n > 0 {
			m.ThreadTS, m.ReplyCount = m.TS, n
			latest := ""
			for _, x := range s.channels[channel] {
				if x.ThreadTS == m.TS && x.TS != m.TS && (latest == "" || tsLess(latest, x.TS)) {
					latest = x.TS
				}
			}
			m.LatestReply = latest
		}
	}
	return m
}

// AgentReplies returns the thread messages carrying an agent marker.
func (s *Server) AgentReplies(channel, thread string) []slack.Message {
	var out []slack.Message
	for _, m := range s.Thread(channel, thread) {
		if m.Metadata != nil && m.Metadata.EventType == slack.MarkerEventType {
			out = append(out, m)
		}
	}
	return out
}

func tsLess(a, b string) bool {
	ta, _ := slack.TSTime(a)
	tb, _ := slack.TSTime(b)
	return ta.Before(tb)
}

func inWindow(ts, oldest, latest string) bool {
	if oldest != "" && tsLess(ts, oldest) {
		return false
	}
	if latest != "" && tsLess(latest, ts) {
		return false
	}
	return true
}

type apiAnswer map[string]any

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		s.serveSocket(w, r)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	s.mu.Lock()
	s.calls[method]++
	if l, ok := s.limits[method]; ok && l[1] > 0 {
		l[1]--
		s.limits[method] = l
		s.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(l[0]))
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if method == "apps.connections.open" {
		answer := s.openConnection(r)
		s.mu.Unlock()
		writeJSON(w, answer)
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.tokens[auth] {
		s.mu.Unlock()
		writeJSON(w, apiAnswer{"ok": false, "error": "invalid_auth"})
		return
	}
	var answer apiAnswer
	status := http.StatusOK
	switch method {
	case "auth.test":
		answer = apiAnswer{"ok": true, "team_id": s.Team, "team": "Fake", "user_id": s.botUser, "bot_id": s.botID}
		scopes := s.scopes
		if scopes == nil {
			scopes = slack.BotScopes
		}
		w.Header().Set("X-OAuth-Scopes", strings.Join(scopes, ","))
	case "users.conversations":
		ids := make([]string, 0, len(s.members))
		for id := range s.members {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		list := []map[string]any{}
		for _, id := range ids {
			list = append(list, map[string]any{"id": id, "name": s.members[id], "is_private": strings.HasPrefix(id, "G")})
		}
		answer = apiAnswer{"ok": true, "channels": list, "response_metadata": map[string]any{"next_cursor": ""}}
	case "conversations.history":
		_ = r.ParseForm()
		answer = s.history(r)
	case "conversations.replies":
		_ = r.ParseForm()
		answer = s.repliesAnswer(r)
	case "chat.postMessage":
		answer, status = s.post(r)
	default:
		answer = apiAnswer{"ok": false, "error": "unknown_method"}
	}
	s.mu.Unlock()
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, answer)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func offset(cursor string) int {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(string(raw), "offset:"))
	return n
}

func page(all []slack.Message, cursor, limitText string) apiAnswer {
	limit, err := strconv.Atoi(limitText)
	if err != nil || limit <= 0 {
		limit = 100
	}
	start := offset(cursor)
	if start > len(all) {
		start = len(all)
	}
	end := start + limit
	more := end < len(all)
	if !more {
		end = len(all)
	}
	answer := apiAnswer{"ok": true, "messages": all[start:end], "has_more": more}
	if more {
		answer["response_metadata"] = map[string]string{"next_cursor": base64.StdEncoding.EncodeToString([]byte("offset:" + strconv.Itoa(end)))}
	}
	return answer
}

func (s *Server) history(r *http.Request) apiAnswer {
	channel := r.Form.Get("channel")
	if _, ok := s.channels[channel]; !ok {
		return apiAnswer{"ok": false, "error": "channel_not_found"}
	}
	meta := r.Form.Get("include_all_metadata") == "true"
	var top []slack.Message
	for _, m := range s.channels[channel] {
		if m.ThreadTS != "" && m.ThreadTS != m.TS && m.Subtype != "thread_broadcast" {
			continue
		}
		if !inWindow(m.TS, r.Form.Get("oldest"), r.Form.Get("latest")) {
			continue
		}
		top = append(top, s.present(channel, *m, meta))
	}
	sort.Slice(top, func(i, j int) bool { return tsLess(top[j].TS, top[i].TS) })
	return page(top, r.Form.Get("cursor"), r.Form.Get("limit"))
}

func (s *Server) repliesAnswer(r *http.Request) apiAnswer {
	channel, thread := r.Form.Get("channel"), r.Form.Get("ts")
	if _, ok := s.channels[channel]; !ok {
		return apiAnswer{"ok": false, "error": "channel_not_found"}
	}
	if s.find(channel, thread) == nil {
		return apiAnswer{"ok": false, "error": "thread_not_found"}
	}
	var msgs []slack.Message
	for _, m := range s.thread(channel, thread, r.Form.Get("include_all_metadata") == "true") {
		if m.TS == thread || inWindow(m.TS, r.Form.Get("oldest"), r.Form.Get("latest")) {
			msgs = append(msgs, m)
		}
	}
	return page(msgs, r.Form.Get("cursor"), r.Form.Get("limit"))
}

func (s *Server) post(r *http.Request) (apiAnswer, int) {
	var p slack.Post
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || strings.TrimSpace(p.Text) == "" {
		return apiAnswer{"ok": false, "error": "no_text"}, http.StatusOK
	}
	if _, ok := s.channels[p.Channel]; !ok {
		return apiAnswer{"ok": false, "error": "channel_not_found"}, http.StatusOK
	}
	mode := s.postMode
	s.postMode = ""
	if code, ok := strings.CutPrefix(mode, "refuse:"); ok {
		return apiAnswer{"ok": false, "error": code}, http.StatusOK
	}
	ts := s.tick()
	m := &slack.Message{Type: "message", User: s.botUser, BotID: s.botID, Text: p.Text, TS: ts, ThreadTS: p.ThreadTS, Metadata: p.Metadata}
	if p.Broadcast {
		m.Subtype = "thread_broadcast"
	}
	s.channels[p.Channel] = append(s.channels[p.Channel], m)
	if mode == "lost" {
		return nil, http.StatusInternalServerError
	}
	return apiAnswer{"ok": true, "channel": p.Channel, "ts": ts}, http.StatusOK
}

// ---- Events API deliveries -------------------------------------------------

func (s *Server) callback(event any) []byte {
	s.events++
	body, _ := json.Marshal(map[string]any{"token": "unused", "team_id": s.Team, "api_app_id": "AAPP", "type": "event_callback",
		"event_id": fmt.Sprintf("Ev%08d", s.events), "event_time": s.now.Unix(), "event": event})
	return body
}

// MessageEvent is the Events API callback for a message's current state, as
// Slack pushes a new message (without the derived thread summary).
func (s *Server) MessageEvent(channel, ts string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(channel, ts)
	if m == nil {
		return nil
	}
	e := map[string]any{}
	raw, _ := json.Marshal(*m)
	_ = json.Unmarshal(raw, &e)
	e["type"], e["channel"], e["channel_type"] = "message", channel, "channel"
	return s.callback(e)
}

// ChangedEvent is the message_changed callback for a message's current state.
func (s *Server) ChangedEvent(channel, ts string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(channel, ts)
	if m == nil {
		return nil
	}
	return s.callback(map[string]any{"type": "message", "subtype": "message_changed", "channel": channel, "hidden": true, "ts": s.tick(), "message": *m})
}

// DeletedEvent is the message_deleted callback for a message Delete removed.
func (s *Server) DeletedEvent(channel string, previous slack.Message) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callback(map[string]any{"type": "message", "subtype": "message_deleted", "channel": channel, "hidden": true, "ts": s.tick(), "deleted_ts": previous.TS, "previous_message": previous})
}

// Headers signs an Events API body as Slack would at time at.
func Headers(secret string, body []byte, at time.Time) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Slack-Request-Timestamp", ts)
	h.Set("X-Slack-Signature", slack.Sign(secret, ts, body))
	return h
}
