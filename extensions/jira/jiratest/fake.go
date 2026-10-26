// Package jiratest is an in-memory fake of the parts of Jira's REST API v2
// the Jira extension uses (test support only; no real service is ever
// called).
package jiratest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is a workflow status.
type Status struct {
	ID, Name, Category string // category: new, indeterminate, done
}

// Issue is a fake issue.
type Issue struct {
	Key, Summary, Description, Project, Priority, Reporter string
	Labels                                                 []string
	Status                                                 string // status id
	Created, Updated                                       time.Time
	Custom                                                 map[string]any
	History                                                []Change
}

// Change is a status change.
type Change struct {
	At       time.Time
	From, To string
}

// Comment is a fake comment.
type Comment struct {
	ID, Issue, Body, Author string
	App                     bool
	Created, Updated        time.Time
	Properties              map[string]json.RawMessage
}

// Fake is the fake site.
type Fake struct {
	mu       sync.Mutex
	Server   *httptest.Server
	Email    string
	Token    string
	Now      func() time.Time
	Statuses []Status
	Issues   map[string]*Issue
	Comments []*Comment
	Webhooks map[string]map[string]any
	Calls    map[string]int
	// NoEnhancedSearch answers the enhanced search with 404 (Data Center).
	NoEnhancedSearch bool
	// NotAdmin refuses the webhook admin API with 403.
	NotAdmin bool
	// LoseCommentResponse creates the next comments but answers 502.
	LoseCommentResponse int
	// TooMany answers the next N calls 429 with Retry-After.
	TooMany int
	seq     int
}

// New starts a fake site with standard statuses.
func New(email, token string) *Fake {
	f := &Fake{Email: email, Token: token, Now: time.Now, Issues: map[string]*Issue{}, Webhooks: map[string]map[string]any{}, Calls: map[string]int{},
		Statuses: []Status{{"1", "To Do", "new"}, {"3", "In Progress", "indeterminate"}, {"4", "In Review", "indeterminate"}, {"10", "Done", "done"}}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close stops the server.
func (f *Fake) Close() { f.Server.Close() }

func (f *Fake) status(id string) Status {
	for _, s := range f.Statuses {
		if s.ID == id || s.Name == id {
			return s
		}
	}
	return Status{}
}

// AddIssue adds an issue in a status (by name).
func (f *Fake) AddIssue(key, summary, status string, labels ...string) *Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now().UTC()
	project, num, _ := strings.Cut(key, "-")
	n, _ := strconv.Atoi(num)
	i := &Issue{Key: key, Summary: summary, Description: "Fix *this*", Project: project, Priority: "High", Reporter: "Ada",
		Labels: labels, Status: f.status(status).ID, Created: now.Add(time.Duration(n) * time.Millisecond), Updated: now, Custom: map[string]any{}}
	f.Issues[key] = i
	return i
}

// Move changes an issue's status by name, recording history.
func (f *Fake) Move(key, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.Issues[key]
	to := f.status(status).ID
	now := f.Now().UTC()
	i.History = append(i.History, Change{At: now, From: i.Status, To: to})
	i.Status, i.Updated = to, now
}

// AddComment adds a comment.
func (f *Fake) AddComment(key, author, body string) *Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now().UTC()
	f.seq++
	c := &Comment{ID: strconv.Itoa(10000 + f.seq), Issue: key, Body: body, Author: author, Created: now, Updated: now}
	f.Comments = append(f.Comments, c)
	f.Issues[key].Updated = now
	return c
}

// Snapshot returns an issue's labels, status name and comments.
func (f *Fake) Snapshot(key string) (labels []string, status string, comments []Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.Issues[key]
	labels = append(labels, i.Labels...)
	sort.Strings(labels)
	for _, c := range f.Comments {
		if c.Issue == key {
			comments = append(comments, *c)
		}
	}
	return labels, f.status(i.Status).Name, comments
}

const jiraTime = "2006-01-02T15:04:05.000-0700"

func (f *Fake) issueJSON(i *Issue) map[string]any {
	st := f.status(i.Status)
	fields := map[string]any{"summary": i.Summary, "description": i.Description, "labels": i.Labels,
		"status":  map[string]any{"id": st.ID, "name": st.Name, "statusCategory": map[string]any{"key": st.Category}},
		"created": i.Created.Format(jiraTime), "updated": i.Updated.Format(jiraTime),
		"reporter": map[string]any{"displayName": i.Reporter}, "project": map[string]any{"key": i.Project, "name": i.Project + " project"},
		"issuetype": map[string]any{"name": "Task"}, "priority": map[string]any{"name": i.Priority},
		"components": []any{map[string]any{"name": "Web"}}}
	for k, v := range i.Custom {
		fields[k] = v
	}
	return map[string]any{"key": i.Key, "fields": fields}
}

func (f *Fake) commentJSON(c *Comment, props bool) map[string]any {
	acct := "atlassian"
	if c.App {
		acct = "app"
	}
	out := map[string]any{"id": c.ID, "body": c.Body, "created": c.Created.Format(jiraTime), "updated": c.Updated.Format(jiraTime),
		"author": map[string]any{"accountId": "acc-" + strings.ToLower(c.Author), "displayName": c.Author, "accountType": acct}}
	if props {
		var ps []any
		for k, v := range c.Properties {
			ps = append(ps, map[string]any{"key": k, "value": v})
		}
		out["properties"] = ps
	}
	return out
}

var (
	reIssue       = regexp.MustCompile(`^/rest/api/2/issue/([A-Z][A-Z0-9_]*-\d+)$`)
	reComments    = regexp.MustCompile(`^/rest/api/2/issue/([A-Z][A-Z0-9_]*-\d+)/comment$`)
	reComment     = regexp.MustCompile(`^/rest/api/2/issue/([A-Z][A-Z0-9_]*-\d+)/comment/(\d+)$`)
	reTransitions = regexp.MustCompile(`^/rest/api/2/issue/([A-Z][A-Z0-9_]*-\d+)/transitions$`)
	reHook        = regexp.MustCompile(`^/rest/webhooks/1.0/webhook/(.+)$`)
	reProject     = regexp.MustCompile(`project (?:= "([A-Z0-9_]+)"|in \(([^)]*)\))`)
	reUpdated     = regexp.MustCompile(`updated >= -(\d+)m`)
)

func (f *Fake) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *Fake) search(jql string) []*Issue {
	var projects []string
	if m := reProject.FindStringSubmatch(jql); m != nil {
		if m[1] != "" {
			projects = []string{m[1]}
		} else {
			for _, p := range strings.Split(m[2], ",") {
				projects = append(projects, strings.Trim(strings.TrimSpace(p), `"`))
			}
		}
	}
	since := time.Time{}
	if m := reUpdated.FindStringSubmatch(jql); m != nil {
		n, _ := strconv.Atoi(m[1])
		since = f.Now().Add(-time.Duration(n) * time.Minute)
	}
	var out []*Issue
	for _, i := range f.Issues {
		in := false
		for _, p := range projects {
			in = in || i.Project == p
		}
		if in && (f.status(i.Status).Category != "done" || !i.Updated.Before(since)) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.Before(out[b].Created) })
	return out
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.TooMany > 0 {
		f.TooMany--
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(f.Email+":"+f.Token))
	if f.Email == "" {
		want = "Bearer " + f.Token
	}
	if r.Header.Get("Authorization") != want {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	p := r.URL.Path
	switch {
	case p == "/rest/api/2/myself":
		f.write(w, 200, map[string]any{"displayName": "Fake Owner"})
	case p == "/rest/api/2/status":
		var out []any
		for _, s := range f.Statuses {
			out = append(out, map[string]any{"id": s.ID, "name": s.Name, "statusCategory": map[string]any{"key": s.Category}})
		}
		f.write(w, 200, out)
	case p == "/rest/api/2/search/jql" && r.Method == http.MethodPost:
		f.Calls["search/jql"]++
		if f.NoEnhancedSearch {
			f.write(w, 404, nil)
			return
		}
		list := f.search(fmt.Sprint(body["jql"]))
		max := int(body["maxResults"].(float64))
		start := 0
		if tok, ok := body["nextPageToken"].(string); ok {
			start, _ = strconv.Atoi(strings.TrimPrefix(tok, "tok"))
		}
		end := min(start+max, len(list))
		var issues []any
		for _, i := range list[min(start, len(list)):end] {
			issues = append(issues, f.issueJSON(i))
		}
		out := map[string]any{"issues": issues, "isLast": end >= len(list)}
		if end < len(list) {
			out["nextPageToken"] = fmt.Sprintf("tok%d", end)
		}
		f.write(w, 200, out)
	case p == "/rest/api/2/search" && r.Method == http.MethodPost:
		f.Calls["search"]++
		list := f.search(fmt.Sprint(body["jql"]))
		max := int(body["maxResults"].(float64))
		start := int(body["startAt"].(float64))
		end := min(start+max, len(list))
		var issues []any
		for _, i := range list[min(start, len(list)):end] {
			issues = append(issues, f.issueJSON(i))
		}
		f.write(w, 200, map[string]any{"issues": issues, "total": len(list)})
	case p == "/rest/api/2/issue" && r.Method == http.MethodPost:
		f.Calls["create"]++
		fields, _ := body["fields"].(map[string]any)
		project, _ := fields["project"].(map[string]any)
		n := 100
		for range f.Issues {
			n++
		}
		now := f.Now().UTC()
		i := &Issue{Key: fmt.Sprintf("%s-%d", project["key"], n), Summary: fmt.Sprint(fields["summary"]), Description: fmt.Sprint(fields["description"]),
			Project: fmt.Sprint(project["key"]), Status: "1", Created: now, Updated: now, Reporter: "Agent", Custom: map[string]any{}}
		if ls, ok := fields["labels"].([]any); ok {
			for _, l := range ls {
				i.Labels = append(i.Labels, fmt.Sprint(l))
			}
		}
		f.Issues[i.Key] = i
		f.write(w, 201, map[string]any{"key": i.Key})
	case reComment.MatchString(p):
		m := reComment.FindStringSubmatch(p)
		for _, c := range f.Comments {
			if c.Issue == m[1] && c.ID == m[2] {
				f.write(w, 200, f.commentJSON(c, true))
				return
			}
		}
		f.write(w, 404, nil)
	case reComments.MatchString(p) && r.Method == http.MethodPost:
		f.Calls["comment"]++
		key := reComments.FindStringSubmatch(p)[1]
		if f.Issues[key] == nil {
			f.write(w, 404, nil)
			return
		}
		now := f.Now().UTC()
		f.seq++
		c := &Comment{ID: strconv.Itoa(10000 + f.seq), Issue: key, Body: fmt.Sprint(body["body"]), Author: "Agent", Created: now, Updated: now, Properties: map[string]json.RawMessage{}}
		if ps, ok := body["properties"].([]any); ok {
			for _, x := range ps {
				prop, _ := x.(map[string]any)
				raw, _ := json.Marshal(prop["value"])
				c.Properties[fmt.Sprint(prop["key"])] = raw
			}
		}
		f.Comments = append(f.Comments, c)
		if f.LoseCommentResponse > 0 {
			f.LoseCommentResponse--
			f.write(w, 502, nil)
			return
		}
		f.write(w, 201, f.commentJSON(c, false))
	case reComments.MatchString(p):
		key := reComments.FindStringSubmatch(p)[1]
		if f.Issues[key] == nil {
			f.write(w, 404, nil)
			return
		}
		var list []any
		for _, c := range f.Comments {
			if c.Issue == key {
				list = append(list, f.commentJSON(c, r.URL.Query().Get("expand") == "properties"))
			}
		}
		f.write(w, 200, map[string]any{"comments": list, "total": len(list)})
	case reTransitions.MatchString(p):
		key := reTransitions.FindStringSubmatch(p)[1]
		i := f.Issues[key]
		if i == nil {
			f.write(w, 404, nil)
			return
		}
		if r.Method == http.MethodPost {
			f.Calls["transition"]++
			tr, _ := body["transition"].(map[string]any)
			to := strings.TrimPrefix(fmt.Sprint(tr["id"]), "t")
			i.History = append(i.History, Change{At: f.Now().UTC(), From: i.Status, To: to})
			i.Status, i.Updated = to, f.Now().UTC()
			f.write(w, 204, nil)
			return
		}
		var list []any
		for _, s := range f.Statuses {
			if s.ID != i.Status {
				list = append(list, map[string]any{"id": "t" + s.ID, "name": "Move to " + s.Name, "to": map[string]any{"name": s.Name}})
			}
		}
		f.write(w, 200, map[string]any{"transitions": list})
	case reIssue.MatchString(p):
		key := reIssue.FindStringSubmatch(p)[1]
		i := f.Issues[key]
		if i == nil {
			f.write(w, 404, nil)
			return
		}
		if r.Method == http.MethodPut {
			f.Calls["labels"]++
			update, _ := body["update"].(map[string]any)
			ops, _ := update["labels"].([]any)
			for _, x := range ops {
				op, _ := x.(map[string]any)
				if a, ok := op["add"].(string); ok {
					found := false
					for _, l := range i.Labels {
						found = found || l == a
					}
					if !found {
						i.Labels = append(i.Labels, a)
					}
				}
				if rm, ok := op["remove"].(string); ok {
					var keep []string
					for _, l := range i.Labels {
						if l != rm {
							keep = append(keep, l)
						}
					}
					i.Labels = keep
				}
			}
			f.write(w, 204, nil)
			return
		}
		out := f.issueJSON(i)
		if r.URL.Query().Get("expand") == "changelog" {
			var hs []any
			for _, h := range i.History {
				hs = append(hs, map[string]any{"created": h.At.Format(jiraTime), "items": []any{map[string]any{"field": "status", "from": h.From, "to": h.To}}})
			}
			out["changelog"] = map[string]any{"histories": hs}
		}
		f.write(w, 200, out)
	case p == "/rest/webhooks/1.0/webhook" && r.Method == http.MethodPost:
		if f.NotAdmin {
			f.write(w, 403, nil)
			return
		}
		f.seq++
		id := strconv.Itoa(f.seq)
		f.Webhooks[id] = body
		f.write(w, 201, map[string]any{"self": f.Server.URL + "/rest/webhooks/1.0/webhook/" + id})
	case reHook.MatchString(p) && r.Method == http.MethodPut:
		if f.NotAdmin {
			f.write(w, 403, nil)
			return
		}
		id := reHook.FindStringSubmatch(p)[1]
		if _, ok := f.Webhooks[id]; !ok {
			f.write(w, 404, nil)
			return
		}
		f.Webhooks[id] = body
		f.write(w, 200, map[string]any{"self": f.Server.URL + "/rest/webhooks/1.0/webhook/" + id})
	default:
		f.write(w, 404, nil)
	}
}
