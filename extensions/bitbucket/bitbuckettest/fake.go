// Package bitbuckettest is an in-memory fake of the parts of Bitbucket
// Cloud's 2.0 REST API the Bitbucket extension uses (test support only; no
// real service is ever called).
package bitbuckettest

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

// PR is a fake pull request.
type PR struct {
	ID                 int
	Title, Description string
	State              string // OPEN, MERGED, DECLINED
	Branch, Base, SHA  string
	Author             string
	Created, Updated   time.Time
}

// Comment is a fake PR comment.
type Comment struct {
	ID, PR           int
	Body, User       string
	App              bool
	Path             string
	Line, Parent     int
	Created, Updated time.Time
}

// Status is a fake commit status.
type Status struct {
	SHA, Key, Name, State, URL string
	Updated                    time.Time
}

// Fake is the fake repository host.
type Fake struct {
	mu       sync.Mutex
	Server   *httptest.Server
	Repo     string
	User     string
	Token    string
	Now      func() time.Time
	PRs      map[int]*PR
	Comments []*Comment
	Statuses []*Status
	Hooks    map[string]map[string]any
	Calls    map[string]int
	PageSize int
	// LoseCommentResponse creates the next comments but answers 502.
	LoseCommentResponse int
	seq                 int
}

// New starts a fake for one repository ("workspace/slug").
func New(repo, user, token string) *Fake {
	f := &Fake{Repo: repo, User: user, Token: token, Now: time.Now, PRs: map[int]*PR{}, Hooks: map[string]map[string]any{}, Calls: map[string]int{}, PageSize: 50}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the API base.
func (f *Fake) URL() string { return f.Server.URL + "/2.0" }

// Close stops the server.
func (f *Fake) Close() { f.Server.Close() }

// AddPR opens a PR.
func (f *Fake) AddPR(id int, title, branch, sha string) *PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now().UTC()
	p := &PR{ID: id, Title: title, State: "OPEN", Branch: branch, Base: "main", SHA: sha, Author: "Ada", Created: now.Add(time.Duration(id) * time.Millisecond), Updated: now}
	f.PRs[id] = p
	return p
}

// SetState changes a PR's state.
func (f *Fake) SetState(id int, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PRs[id].State, f.PRs[id].Updated = state, f.Now().UTC()
}

// AddComment adds a PR comment.
func (f *Fake) AddComment(pr int, user, body string) *Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	now := f.Now().UTC()
	c := &Comment{ID: 500 + f.seq, PR: pr, Body: body, User: user, Created: now, Updated: now}
	f.Comments = append(f.Comments, c)
	return c
}

// SetStatus sets a commit status.
func (f *Fake) SetStatus(sha, key, state, url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.Statuses {
		if s.SHA == sha && s.Key == key {
			s.State, s.URL, s.Updated = state, url, f.Now().UTC()
			return
		}
	}
	f.Statuses = append(f.Statuses, &Status{SHA: sha, Key: key, Name: key, State: state, URL: url, Updated: f.Now().UTC()})
}

// PRComments returns a PR's comments.
func (f *Fake) PRComments(pr int) []Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Comment
	for _, c := range f.Comments {
		if c.PR == pr {
			out = append(out, *c)
		}
	}
	return out
}

func (f *Fake) prJSON(p *PR) map[string]any {
	return map[string]any{"id": p.ID, "title": p.Title, "description": p.Description, "state": p.State,
		"author": map[string]any{"display_name": p.Author}, "created_on": p.Created.Format(time.RFC3339Nano), "updated_on": p.Updated.Format(time.RFC3339Nano),
		"source":      map[string]any{"branch": map[string]any{"name": p.Branch}, "commit": map[string]any{"hash": p.SHA}},
		"destination": map[string]any{"branch": map[string]any{"name": p.Base}},
		"links":       map[string]any{"html": map[string]any{"href": "https://bitbucket.org/" + f.Repo + "/pull-requests/" + strconv.Itoa(p.ID)}}}
}

func (f *Fake) commentJSON(c *Comment) map[string]any {
	typ := "user"
	if c.App {
		typ = "app_user"
	}
	out := map[string]any{"id": c.ID, "content": map[string]any{"raw": c.Body}, "user": map[string]any{"display_name": c.User, "type": typ},
		"created_on": c.Created.Format(time.RFC3339Nano), "updated_on": c.Updated.Format(time.RFC3339Nano),
		"links": map[string]any{"html": map[string]any{"href": fmt.Sprintf("https://bitbucket.org/%s/pull-requests/%d#comment-%d", f.Repo, c.PR, c.ID)}}}
	if c.Path != "" {
		out["inline"] = map[string]any{"path": c.Path, "to": c.Line}
	}
	if c.Parent > 0 {
		out["parent"] = map[string]any{"id": c.Parent}
	}
	return out
}

func (f *Fake) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *Fake) page(w http.ResponseWriter, r *http.Request, values []any) {
	start, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if start < 1 {
		start = 1
	}
	size := f.PageSize
	from := (start - 1) * size
	to := min(from+size, len(values))
	out := map[string]any{"values": values[min(from, len(values)):to]}
	if to < len(values) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(start+1))
		out["next"] = f.URL() + strings.TrimPrefix(r.URL.Path, "/2.0") + "?" + q.Encode()
	}
	f.write(w, 200, out)
}

var (
	branchQ  = regexp.MustCompile(`source\.branch\.name="([^"]*)"`)
	descQ    = regexp.MustCompile(`description ~ "([^"]*)"`)
	updatedQ = regexp.MustCompile(`updated_on >= (\S+)`)
)

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(f.User+":"+f.Token)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	base := "/2.0/repositories/" + f.Repo
	p := r.URL.Path
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case p == "/2.0/user":
		f.write(w, 200, map[string]any{"display_name": "Fake Owner"})
	case p == base:
		f.write(w, 200, map[string]any{"mainbranch": map[string]any{"name": "main"}})
	case p == base+"/pullrequests" && r.Method == http.MethodPost:
		f.Calls["create"]++
		src, _ := body["source"].(map[string]any)
		dst, _ := body["destination"].(map[string]any)
		id := len(f.PRs) + 1
		for f.PRs[id] != nil {
			id++
		}
		now := f.Now().UTC()
		pr := &PR{ID: id, Title: fmt.Sprint(body["title"]), Description: fmt.Sprint(body["description"]), State: "OPEN",
			Branch: fmt.Sprint(src["branch"].(map[string]any)["name"]), Base: fmt.Sprint(dst["branch"].(map[string]any)["name"]),
			SHA: "abc" + strconv.Itoa(id), Author: "Agent", Created: now, Updated: now}
		f.PRs[id] = pr
		f.write(w, 201, f.prJSON(pr))
	case p == base+"/pullrequests":
		f.Calls["list"]++
		q := r.URL.Query().Get("q")
		states := r.URL.Query()["state"]
		var list []*PR
		for _, pr := range f.PRs {
			ok := len(states) == 0
			for _, s := range states {
				ok = ok || s == pr.State
			}
			if m := branchQ.FindStringSubmatch(q); m != nil && pr.Branch != m[1] {
				ok = false
			}
			if m := descQ.FindStringSubmatch(q); m != nil && !strings.Contains(pr.Description, m[1]) {
				ok = false
			}
			if m := updatedQ.FindStringSubmatch(q); m != nil {
				since, _ := time.Parse("2006-01-02T15:04:05Z", m[1])
				if pr.State != "OPEN" && pr.Updated.Before(since) {
					ok = false
				}
			}
			if ok {
				list = append(list, pr)
			}
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Created.Before(list[b].Created) })
		var values []any
		for _, pr := range list {
			values = append(values, f.prJSON(pr))
		}
		f.page(w, r, values)
	case strings.HasPrefix(p, base+"/pullrequests/"):
		rest := strings.Split(strings.TrimPrefix(p, base+"/pullrequests/"), "/")
		id, _ := strconv.Atoi(rest[0])
		pr := f.PRs[id]
		if pr == nil {
			f.write(w, 404, nil)
			return
		}
		switch {
		case len(rest) == 1:
			f.write(w, 200, f.prJSON(pr))
		case len(rest) == 2 && rest[1] == "comments" && r.Method == http.MethodPost:
			f.Calls["comment"]++
			f.seq++
			now := f.Now().UTC()
			content, _ := body["content"].(map[string]any)
			c := &Comment{ID: 500 + f.seq, PR: id, Body: fmt.Sprint(content["raw"]), User: "Agent", Created: now, Updated: now}
			if parent, ok := body["parent"].(map[string]any); ok {
				c.Parent = int(parent["id"].(float64))
			}
			f.Comments = append(f.Comments, c)
			if f.LoseCommentResponse > 0 {
				f.LoseCommentResponse--
				f.write(w, 502, nil)
				return
			}
			f.write(w, 201, f.commentJSON(c))
		case len(rest) == 2 && rest[1] == "comments":
			var values []any
			for _, c := range f.Comments {
				if c.PR == id {
					values = append(values, f.commentJSON(c))
				}
			}
			f.page(w, r, values)
		case len(rest) == 3 && rest[1] == "comments":
			cid, _ := strconv.Atoi(rest[2])
			for _, c := range f.Comments {
				if c.PR == id && c.ID == cid {
					f.write(w, 200, f.commentJSON(c))
					return
				}
			}
			f.write(w, 404, nil)
		default:
			f.write(w, 404, nil)
		}
	case strings.HasPrefix(p, base+"/commit/") && strings.HasSuffix(p, "/statuses"):
		sha := strings.TrimSuffix(strings.TrimPrefix(p, base+"/commit/"), "/statuses")
		var values []any
		for _, s := range f.Statuses {
			if s.SHA == sha {
				values = append(values, map[string]any{"key": s.Key, "name": s.Name, "state": s.State, "url": s.URL, "updated_on": s.Updated.Format(time.RFC3339Nano)})
			}
		}
		f.page(w, r, values)
	case p == base+"/hooks" && r.Method == http.MethodPost:
		f.seq++
		id := fmt.Sprintf("{hook-%d}", f.seq)
		f.Hooks[id] = body
		f.write(w, 201, map[string]any{"uuid": id})
	case strings.HasPrefix(p, base+"/hooks/") && r.Method == http.MethodPut:
		id := strings.TrimPrefix(p, base+"/hooks/")
		if _, ok := f.Hooks[id]; !ok {
			f.write(w, 404, nil)
			return
		}
		f.Hooks[id] = body
		f.write(w, 200, map[string]any{"uuid": id})
	default:
		f.write(w, 404, nil)
	}
}
