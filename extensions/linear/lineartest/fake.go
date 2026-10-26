// Package lineartest is an in-memory fake of the parts of Linear's GraphQL
// API the Linear extension uses (test support only; no real service is ever
// called). It recognizes the extension's queries by their root fields.
package lineartest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is a workflow state.
type State struct {
	ID, Name, Type, Team string
}

// Issue is a fake issue.
type Issue struct {
	ID, Identifier, Title, Description string
	Team, Project, Priority            string
	State                              string // state id
	Labels                             []string
	CreatedAt, UpdatedAt               time.Time
	Creator                            string
	// History records state changes: from and to state ids.
	History []Change
}

// Change is one state change in an issue's history.
type Change struct {
	At       time.Time
	From, To string
}

// Comment is a fake comment.
type Comment struct {
	ID, Issue, Body, User string
	Bot                   bool
	CreatedAt, UpdatedAt  time.Time
}

// Fake is the fake workspace.
type Fake struct {
	mu       sync.Mutex
	Server   *httptest.Server
	Token    string
	Now      func() time.Time
	Issues   map[string]*Issue // by identifier
	Comments []*Comment
	States   []State
	Labels   map[string]string // id -> name
	Webhooks map[string]map[string]any
	// Calls counts each root operation.
	Calls map[string]int
	// RateLimited answers the next N calls with Linear's RATELIMITED error.
	RateLimited int
	// LoseCommentResponse creates the next comments but fails the answer.
	LoseCommentResponse int
	// Unauthorized answers the next N calls with 401.
	Unauthorized int
	seq          int
}

// New starts a fake with the given teams' standard workflow states.
func New(token string, teams ...string) *Fake {
	f := &Fake{Token: token, Now: time.Now, Issues: map[string]*Issue{}, Labels: map[string]string{}, Webhooks: map[string]map[string]any{}, Calls: map[string]int{}}
	for _, t := range teams {
		for _, s := range []struct{ name, typ string }{{"Backlog", "backlog"}, {"Todo", "unstarted"}, {"In Progress", "started"}, {"In Review", "started"}, {"Done", "completed"}, {"Canceled", "canceled"}} {
			f.States = append(f.States, State{ID: t + "-" + strings.ReplaceAll(strings.ToLower(s.name), " ", "-"), Name: s.name, Type: s.typ, Team: t})
		}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the GraphQL endpoint.
func (f *Fake) URL() string { return f.Server.URL + "/graphql" }

// Close stops the server.
func (f *Fake) Close() { f.Server.Close() }

func (f *Fake) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%04d", prefix, f.seq)
}

// StateID returns the id of a team's state by name.
func (f *Fake) StateID(team, name string) string {
	for _, s := range f.States {
		if s.Team == team && s.Name == name {
			return s.ID
		}
	}
	return ""
}

func (f *Fake) state(id string) State {
	for _, s := range f.States {
		if s.ID == id {
			return s
		}
	}
	return State{}
}

// AddIssue adds an issue in a team's state (by name).
func (f *Fake) AddIssue(team string, number int, title, state string, labels ...string) *Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now().UTC()
	i := &Issue{ID: f.next("issue"), Identifier: fmt.Sprintf("%s-%d", team, number), Title: title, Team: team, State: f.StateID(team, state),
		Labels: labels, CreatedAt: now.Add(time.Duration(number) * time.Millisecond), UpdatedAt: now, Creator: "Ada", Priority: "High"}
	for _, l := range labels {
		f.ensureLabel(l)
	}
	f.Issues[i.Identifier] = i
	return i
}

func (f *Fake) ensureLabel(name string) string {
	for id, n := range f.Labels {
		if strings.EqualFold(n, name) {
			return id
		}
	}
	id := f.next("label")
	f.Labels[id] = name
	return id
}

// Move changes an issue's state by name, recording history.
func (f *Fake) Move(identifier, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.Issues[identifier]
	to := f.StateID(i.Team, state)
	now := f.Now().UTC()
	i.History = append(i.History, Change{At: now, From: i.State, To: to})
	i.State, i.UpdatedAt = to, now
}

// AddComment adds a comment by a user.
func (f *Fake) AddComment(identifier, user, body string) *Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now().UTC()
	c := &Comment{ID: f.next("comment"), Issue: identifier, Body: body, User: user, CreatedAt: now, UpdatedAt: now}
	f.Comments = append(f.Comments, c)
	f.Issues[identifier].UpdatedAt = now
	return c
}

// Snapshot returns an issue's labels, state name and comments.
func (f *Fake) Snapshot(identifier string) (labels []string, state string, comments []Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.Issues[identifier]
	labels = append(labels, i.Labels...)
	sort.Strings(labels)
	for _, c := range f.Comments {
		if c.Issue == identifier {
			comments = append(comments, *c)
		}
	}
	return labels, f.state(i.State).Name, comments
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unauthorized > 0 {
		f.Unauthorized--
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if auth := r.Header.Get("Authorization"); auth != f.Token && auth != "Bearer "+f.Token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.RateLimited > 0 {
		f.RateLimited--
		w.Header().Set("X-RateLimit-Requests-Reset", fmt.Sprint(f.Now().Add(30*time.Second).UnixMilli()))
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "Rate limit exceeded", "extensions": map[string]any{"code": "RATELIMITED"}}}})
		return
	}
	data, errMsg := f.dispatch(req.Query, req.Variables)
	w.Header().Set("Content-Type", "application/json")
	if errMsg != "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []any{map[string]any{"message": errMsg}}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func str(v any) string { s, _ := v.(string); return s }

func strs(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, str(x))
		}
	}
	return out
}

func (f *Fake) user(name string) any {
	if name == "" {
		return nil
	}
	return map[string]any{"id": "u-" + strings.ToLower(name), "name": name, "displayName": name, "email": strings.ToLower(name) + "@example.com"}
}

func (f *Fake) issueJSON(i *Issue) map[string]any {
	st := f.state(i.State)
	var labels []any
	for _, l := range i.Labels {
		labels = append(labels, map[string]any{"id": f.ensureLabel(l), "name": l})
	}
	return map[string]any{"id": i.ID, "identifier": i.Identifier, "title": i.Title, "description": i.Description,
		"url": "https://linear.app/fake/issue/" + i.Identifier, "createdAt": i.CreatedAt, "updatedAt": i.UpdatedAt,
		"priorityLabel": i.Priority, "estimate": nil, "state": map[string]any{"id": st.ID, "name": st.Name, "type": st.Type},
		"team": map[string]any{"key": i.Team, "name": i.Team + " team"}, "project": nil, "cycle": nil, "assignee": nil,
		"creator": f.user(i.Creator), "labels": map[string]any{"nodes": labels}, "parent": nil}
}

func (f *Fake) commentJSON(c *Comment) map[string]any {
	out := map[string]any{"id": c.ID, "body": c.Body, "url": "https://linear.app/fake/issue/" + c.Issue + "#comment-" + c.ID,
		"createdAt": c.CreatedAt, "updatedAt": c.UpdatedAt, "user": f.user(c.User), "botActor": nil,
		"issue": map[string]any{"identifier": c.Issue}}
	if c.Bot {
		out["user"], out["botActor"] = nil, map[string]any{"name": c.User}
	}
	return out
}

func (f *Fake) find(id string) *Issue {
	if i, ok := f.Issues[strings.ToUpper(id)]; ok {
		return i
	}
	for _, i := range f.Issues {
		if i.ID == id {
			return i
		}
	}
	return nil
}

func closed(t string) bool { return t == "completed" || t == "canceled" || t == "duplicate" }

func (f *Fake) dispatch(q string, v map[string]any) (any, string) {
	root := func(name string) bool { return strings.Contains(q, name) }
	switch {
	case root("commentCreate("):
		f.Calls["commentCreate"]++
		input, _ := v["input"].(map[string]any)
		i := f.find(str(input["issueId"]))
		if i == nil {
			return nil, "Entity not found"
		}
		now := f.Now().UTC()
		c := &Comment{ID: f.next("comment"), Issue: i.Identifier, Body: str(input["body"]), User: "Agent", CreatedAt: now, UpdatedAt: now}
		f.Comments = append(f.Comments, c)
		if f.LoseCommentResponse > 0 {
			f.LoseCommentResponse--
			return nil, "internal error"
		}
		return map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": c.ID, "url": "https://linear.app/fake/c/" + c.ID}}}, ""
	case root("issueCreate("):
		f.Calls["issueCreate"]++
		input, _ := v["input"].(map[string]any)
		team := ""
		for _, s := range f.States {
			if "team-"+s.Team == str(input["teamId"]) {
				team = s.Team
			}
		}
		n := 1
		for _, i := range f.Issues {
			if i.Team == team {
				n++
			}
		}
		now := f.Now().UTC()
		i := &Issue{ID: f.next("issue"), Identifier: fmt.Sprintf("%s-%d", team, 100+n), Title: str(input["title"]), Description: str(input["description"]),
			Team: team, State: f.StateID(team, "Todo"), CreatedAt: now, UpdatedAt: now, Creator: "Agent"}
		for _, id := range strs(input["labelIds"]) {
			i.Labels = append(i.Labels, f.Labels[id])
		}
		f.Issues[i.Identifier] = i
		return map[string]any{"issueCreate": map[string]any{"issue": f.issueJSON(i)}}, ""
	case root("issueLabelCreate("):
		f.Calls["issueLabelCreate"]++
		input, _ := v["input"].(map[string]any)
		f.ensureLabel(str(input["name"]))
		return map[string]any{"issueLabelCreate": map[string]any{"success": true}}, ""
	case root("issueUpdate("):
		f.Calls["issueUpdate"]++
		i := f.find(str(v["id"]))
		if i == nil {
			return nil, "Entity not found"
		}
		input, _ := v["input"].(map[string]any)
		if ids, ok := input["labelIds"]; ok {
			i.Labels = nil
			for _, id := range strs(ids) {
				i.Labels = append(i.Labels, f.Labels[id])
			}
		}
		if s := str(input["stateId"]); s != "" {
			i.History = append(i.History, Change{At: f.Now().UTC(), From: i.State, To: s})
			i.State = s
		}
		i.UpdatedAt = f.Now().UTC()
		return map[string]any{"issueUpdate": map[string]any{"success": true}}, ""
	case root("webhookUpdate("):
		f.Calls["webhookUpdate"]++
		id := str(v["id"])
		if _, ok := f.Webhooks[id]; !ok {
			return nil, "Webhook not found"
		}
		f.Webhooks[id], _ = v["input"].(map[string]any)
		return map[string]any{"webhookUpdate": map[string]any{"success": true}}, ""
	case root("webhookCreate("):
		f.Calls["webhookCreate"]++
		id := f.next("hook")
		f.Webhooks[id], _ = v["input"].(map[string]any)
		return map[string]any{"webhookCreate": map[string]any{"success": true, "webhook": map[string]any{"id": id}}}, ""
	case root("viewer"):
		f.Calls["viewer"]++
		return map[string]any{"viewer": map[string]any{"name": "Fake Owner", "email": "owner@example.com"}}, ""
	case root("workflowStates("):
		f.Calls["workflowStates"]++
		var nodes []any
		teams := strs(v["teams"])
		for _, s := range f.States {
			for _, t := range teams {
				if s.Team == t {
					nodes = append(nodes, map[string]any{"id": s.ID, "name": s.Name, "type": s.Type, "team": map[string]any{"key": s.Team}})
				}
			}
		}
		return map[string]any{"workflowStates": map[string]any{"nodes": nodes}}, ""
	case root("issueLabels("):
		f.Calls["issueLabels"]++
		var nodes []any
		ids := make([]string, 0, len(f.Labels))
		for id := range f.Labels {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			nodes = append(nodes, map[string]any{"id": id, "name": f.Labels[id], "team": nil})
		}
		return map[string]any{"issueLabels": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}}, ""
	case root("teams("):
		f.Calls["teams"]++
		return map[string]any{"teams": map[string]any{"nodes": []any{map[string]any{"id": "team-" + str(v["key"])}}}}, ""
	case root("comment(id"):
		f.Calls["comment"]++
		for _, c := range f.Comments {
			if c.ID == str(v["id"]) {
				return map[string]any{"comment": f.commentJSON(c)}, ""
			}
		}
		return nil, "Entity not found"
	case root("history("):
		f.Calls["history"]++
		i := f.find(str(v["id"]))
		if i == nil {
			return nil, "Entity not found"
		}
		var nodes []any
		for _, h := range i.History {
			nodes = append(nodes, map[string]any{"createdAt": h.At, "fromState": map[string]any{"type": f.state(h.From).Type}, "toState": map[string]any{"type": f.state(h.To).Type}})
		}
		return map[string]any{"issue": map[string]any{"history": map[string]any{"nodes": nodes}}}, ""
	case root("comments("):
		f.Calls["comments"]++
		i := f.find(str(v["id"]))
		if i == nil {
			return nil, "Entity not found"
		}
		var nodes []any
		for _, c := range f.Comments {
			if c.Issue == i.Identifier {
				nodes = append(nodes, f.commentJSON(c))
			}
		}
		return map[string]any{"issue": map[string]any{"comments": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}}}, ""
	case root("issues("):
		f.Calls["issues"]++
		teams := strs(v["teams"])
		since, _ := time.Parse(time.RFC3339, str(v["since"]))
		var list []*Issue
		for _, i := range f.Issues {
			in := false
			for _, t := range teams {
				in = in || i.Team == t
			}
			if in && (!closed(f.state(i.State).Type) || !i.UpdatedAt.Before(since)) {
				list = append(list, i)
			}
		}
		sort.Slice(list, func(a, b int) bool { return list[a].CreatedAt.Before(list[b].CreatedAt) })
		first := 50
		if n, ok := v["first"].(float64); ok && n > 0 {
			first = int(n)
		}
		start := 0
		if after := str(v["after"]); after != "" {
			fmt.Sscanf(after, "c%d", &start)
		}
		end := start + first
		if end > len(list) {
			end = len(list)
		}
		var nodes []any
		for _, i := range list[min(start, len(list)):end] {
			nodes = append(nodes, f.issueJSON(i))
		}
		return map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": end < len(list), "endCursor": fmt.Sprintf("c%d", end)}}}, ""
	case root("issue(id"):
		f.Calls["issue"]++
		i := f.find(str(v["id"]))
		if i == nil {
			return nil, "Entity not found"
		}
		return map[string]any{"issue": f.issueJSON(i)}, ""
	}
	return nil, "unknown query"
}
