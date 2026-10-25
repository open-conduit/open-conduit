// Package githubtest is an in-memory GitHub REST fake for conformance and
// integration fixtures. It models the endpoints the extension uses, with
// ETag/304, Link pagination, primary/secondary rate limits, token and App
// authentication, and records every write. It is test support, never linked
// into production binaries.
package githubtest

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type Issue struct {
	Number      int
	Title, Body string
	User        User
	Labels      []string
	State       string
	StateReason string
	Created     time.Time
	Updated     time.Time
	Closed      *time.Time
	// Pull marks the issue face of a pull request.
	Pull bool
}

type Pull struct {
	Number    int
	HeadRef   string
	HeadSHA   string
	BaseRef   string
	Draft     bool
	MergedAt  *time.Time
	Reviews   []Review
	CreatedAt time.Time
}

type Review struct {
	ID        int64
	User      User
	Body      string
	State     string
	Submitted time.Time
	CommitID  string
}

type Comment struct {
	ID       int64
	Number   int
	User     User
	Body     string
	Created  time.Time
	Updated  time.Time
	Inline   bool
	Path     string
	Line     int
	Reacts   map[string]int
	ReplyTo  int64
	CommitID string
}

type CheckRun struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	Started    time.Time
	Completed  *time.Time
	Details    string
}

type Status struct {
	ID      int64
	Context string
	State   string
	Target  string
	At      time.Time
}

type IssueEvent struct {
	ID      int64
	Event   string
	Created time.Time
}

// Write is one recorded mutation.
type Write struct {
	Method, Path, Token string
	Body                json.RawMessage
}

// Server is the fake. All exported fields are guarded by mu; use the methods.
type Server struct {
	*httptest.Server
	Repo string

	mu       sync.Mutex
	now      time.Time
	nextID   int64
	issues   map[int]*Issue
	pulls    map[int]*Pull
	comments []*Comment
	events   map[int][]IssueEvent
	checks   map[string][]CheckRun
	// refuseRuns makes check runs an ordinary 403 (a fine-grained token);
	// refuseActions does the same for Actions workflow runs.
	refuseRuns, refuseActions bool
	statuses                  map[string][]Status
	tokens                    map[string]bool
	apps                      map[string]*rsa.PublicKey
	installs                  map[string]string // installation token → app id
	writes                    []Write
	requests                  []string
	notMod                    int
	limitNext                 []limit
	failNext                  map[string]int
	loseNext                  map[string]int
	tokenTTL                  time.Duration
	issued                    int
	commits                   map[string]time.Time
	contents                  map[string][]byte // attachments branch path → content
	branches                  map[string]bool
	files                     map[string]string // /files/<name> → redirect target
	fileData                  map[string][]byte
	hooks                     []*Hook
	perms                     map[string]bool
}

// Hook is a repository webhook.
type Hook struct {
	ID     int64
	URL    string
	Secret string
	Events []string
	Active bool
}

// Hooks returns copies of the repository webhooks.
func (s *Server) Hooks() []Hook {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Hook, 0, len(s.hooks))
	for _, h := range s.hooks {
		out = append(out, *h)
	}
	return out
}

// SetPermissions makes GET /repos/{repo} report the caller's permissions.
func (s *Server) SetPermissions(p map[string]bool) { s.mu.Lock(); s.perms = p; s.mu.Unlock() }

type limit struct {
	status     int
	retryAfter int
	reset      time.Time
	message    string
}

// New starts a fake for repo owner/name with an accepted token.
func New(repo string, tokens ...string) *Server {
	s := &Server{Repo: repo, now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), nextID: 1000, issues: map[int]*Issue{}, pulls: map[int]*Pull{},
		events: map[int][]IssueEvent{}, checks: map[string][]CheckRun{}, statuses: map[string][]Status{}, tokens: map[string]bool{},
		apps: map[string]*rsa.PublicKey{}, installs: map[string]string{}, failNext: map[string]int{}, loseNext: map[string]int{}, tokenTTL: time.Hour,
		commits: map[string]time.Time{}, contents: map[string][]byte{}, branches: map[string]bool{}, files: map[string]string{}, fileData: map[string][]byte{}}
	for _, t := range tokens {
		s.tokens[t] = true
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// ---- clock and state mutation ----------------------------------------------

func (s *Server) Now() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.now }

// Advance moves the fake's clock (timestamps of subsequent mutations).
func (s *Server) Advance(d time.Duration) { s.mu.Lock(); s.now = s.now.Add(d); s.mu.Unlock() }

func (s *Server) id() int64 { s.nextID++; return s.nextID }

func (s *Server) tick() time.Time { s.now = s.now.Add(time.Second); return s.now }

// AddToken accepts a personal access token.
func (s *Server) AddToken(t string) { s.mu.Lock(); s.tokens[t] = true; s.mu.Unlock() }

// RevokeToken makes a token answer 401.
func (s *Server) RevokeToken(t string) { s.mu.Lock(); delete(s.tokens, t); s.mu.Unlock() }

// AddApp registers a GitHub App public key; its installation covers Repo.
func (s *Server) AddApp(id string, key *rsa.PublicKey) { s.mu.Lock(); s.apps[id] = key; s.mu.Unlock() }

// SetTokenTTL sets installation token lifetime.
func (s *Server) SetTokenTTL(d time.Duration) { s.mu.Lock(); s.tokenTTL = d; s.mu.Unlock() }

// InstallationTokensIssued counts minted installation tokens.
func (s *Server) InstallationTokensIssued() int { s.mu.Lock(); defer s.mu.Unlock(); return s.issued }

// OpenIssue creates an issue and returns its number.
func (s *Server) OpenIssue(title, body string, user User, labels ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.issues) + 1
	t := s.tick()
	s.issues[n] = &Issue{Number: n, Title: title, Body: body, User: user, Labels: append([]string(nil), labels...), State: "open", Created: t, Updated: t}
	return n
}

func (s *Server) issue(n int) *Issue {
	i, ok := s.issues[n]
	if !ok {
		panic(fmt.Sprintf("githubtest: no issue %d", n))
	}
	return i
}

// CloseIssue closes an issue (reason completed|not_planned).
func (s *Server) CloseIssue(n int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.issue(n)
	t := s.tick()
	i.State, i.StateReason, i.Closed, i.Updated = "closed", reason, &t, t
	s.events[n] = append(s.events[n], IssueEvent{ID: s.id(), Event: "closed", Created: t})
}

// ReopenIssue reopens an issue and returns the reopened event ID.
func (s *Server) ReopenIssue(n int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.issue(n)
	t := s.tick()
	i.State, i.StateReason, i.Closed, i.Updated = "open", "reopened", nil, t
	id := s.id()
	s.events[n] = append(s.events[n], IssueEvent{ID: id, Event: "reopened", Created: t})
	return id
}

// SetLabels replaces an issue's labels (an external edit).
func (s *Server) SetLabels(n int, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.issue(n)
	i.Labels, i.Updated = append([]string(nil), labels...), s.tick()
}

// Labels reports an issue's labels.
func (s *Server) Labels(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.issue(n).Labels...)
	sort.Strings(out)
	return out
}

// Issue returns a copy of an issue.
func (s *Server) Issue(n int) Issue { s.mu.Lock(); defer s.mu.Unlock(); return *s.issue(n) }

// OpenPull creates a pull request (sharing the issue number space).
func (s *Server) OpenPull(title, body, head, sha string, user User) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.issues) + 1
	t := s.tick()
	s.issues[n] = &Issue{Number: n, Title: title, Body: body, User: user, State: "open", Created: t, Updated: t, Pull: true}
	s.pulls[n] = &Pull{Number: n, HeadRef: head, HeadSHA: sha, BaseRef: "main", CreatedAt: t}
	if _, ok := s.commits[sha]; !ok {
		s.commits[sha] = t
	}
	return n
}

// CommitAt records a commit's committer date (default: when first seen).
func (s *Server) CommitAt(sha string, at time.Time) { s.mu.Lock(); s.commits[sha] = at; s.mu.Unlock() }

// Content returns an uploaded attachment's content.
func (s *Server) Content(path string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.contents[path]
	return b, ok
}

// ServeFile makes GET /files/<name> serve data to an authenticated reader,
// or redirect to target when it is set (a signed storage link elsewhere).
func (s *Server) ServeFile(name string, data []byte, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileData[name], s.files[name] = data, target
}

// Pull returns a copy of a pull request's state.
func (s *Server) Pull(n int) Pull { s.mu.Lock(); defer s.mu.Unlock(); return *s.pulls[n] }

// PushPull moves a PR head.
func (s *Server) PushPull(n int, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].HeadSHA = sha
	s.issue(n).Updated = s.tick()
	if _, ok := s.commits[sha]; !ok {
		s.commits[sha] = s.now
	}
}

// MergePull merges a PR.
func (s *Server) MergePull(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tick()
	i := s.issue(n)
	i.State, i.Closed, i.Updated = "closed", &t, t
	s.pulls[n].MergedAt = &t
}

// AddComment adds a conversation comment and returns its ID.
func (s *Server) AddComment(n int, user User, body string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addComment(n, user, body, false, "", 0)
}

func (s *Server) addComment(n int, user User, body string, inline bool, path string, line int) int64 {
	t := s.tick()
	c := &Comment{ID: s.id(), Number: n, User: user, Body: body, Created: t, Updated: t, Inline: inline, Path: path, Line: line, Reacts: map[string]int{}}
	s.comments = append(s.comments, c)
	s.issue(n).Updated = t
	return c.ID
}

// EditComment edits a comment body.
func (s *Server) EditComment(id int64, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.comments {
		if c.ID == id {
			c.Body, c.Updated = body, s.tick()
		}
	}
}

// AddReviewComment adds an inline review comment.
func (s *Server) AddReviewComment(n int, user User, body, path string, line int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addComment(n, user, body, true, path, line)
}

// AddReview submits a review.
func (s *Server) AddReview(n int, user User, state, body string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pulls[n]
	r := Review{ID: s.id(), User: user, Body: body, State: state, Submitted: s.tick(), CommitID: p.HeadSHA}
	p.Reviews = append(p.Reviews, r)
	return r.ID
}

// SetCheck adds or updates a named check run on sha.
func (s *Server) SetCheck(sha, name, status, conclusion string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tick()
	for i := range s.checks[sha] {
		r := &s.checks[sha][i]
		if r.Name == name {
			r.Status, r.Conclusion = status, conclusion
			if status == "completed" {
				r.Completed = &t
			}
			return
		}
	}
	r := CheckRun{ID: s.id(), Name: name, Status: status, Conclusion: conclusion, Started: t}
	if status == "completed" {
		r.Completed = &t
	}
	s.checks[sha] = append(s.checks[sha], r)
}

// SetStatus sets a commit status context.
func (s *Server) SetStatus(sha, context, state, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tick()
	for i := range s.statuses[sha] {
		if s.statuses[sha][i].Context == context {
			s.statuses[sha][i].State, s.statuses[sha][i].Target, s.statuses[sha][i].At = state, target, t
			return
		}
	}
	s.statuses[sha] = append(s.statuses[sha], Status{ID: s.id(), Context: context, State: state, Target: target, At: t})
}

// Comments returns conversation comments on an issue.
func (s *Server) Comments(n int) []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Comment
	for _, c := range s.comments {
		if c.Number == n && !c.Inline {
			out = append(out, *c)
		}
	}
	return out
}

// InlineComments returns a PR's inline review comments (and replies).
func (s *Server) InlineComments(n int) []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Comment
	for _, c := range s.comments {
		if c.Number == n && c.Inline {
			out = append(out, *c)
		}
	}
	return out
}

// Reactions returns a comment's reaction counts.
func (s *Server) Reactions(id int64) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.comments {
		if c.ID == id {
			out := map[string]int{}
			for k, v := range c.Reacts {
				out[k] = v
			}
			return out
		}
	}
	return nil
}

// Writes returns recorded mutations.
func (s *Server) Writes() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Write(nil), s.writes...)
}

// Requests returns "METHOD path?query" for every request served.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// NotModified counts conditional 304 answers.
func (s *Server) NotModified() int { s.mu.Lock(); defer s.mu.Unlock(); return s.notMod }

// RateLimitPrimary makes the next request answer a primary limit until reset.
func (s *Server) RateLimitPrimary(reset time.Time) {
	s.mu.Lock()
	s.limitNext = append(s.limitNext, limit{status: http.StatusForbidden, reset: reset, message: "API rate limit exceeded"})
	s.mu.Unlock()
}

// RateLimitSecondary makes the next request answer a secondary limit.
func (s *Server) RateLimitSecondary(retryAfter int) {
	s.mu.Lock()
	s.limitNext = append(s.limitNext, limit{status: http.StatusForbidden, retryAfter: retryAfter, message: "You have exceeded a secondary rate limit"})
	s.mu.Unlock()
}

// RefuseCheckRuns makes every check-runs read an ordinary 403, as GitHub
// answers a fine-grained personal access token (which cannot hold the Checks
// permission). With actions false, Actions workflow runs are refused too;
// otherwise they serve the same runs as workflow runs.
func (s *Server) RefuseCheckRuns(actions bool) {
	s.mu.Lock()
	s.refuseRuns, s.refuseActions = true, !actions
	s.mu.Unlock()
}

// Forbid makes the next request an ordinary 403 (no access).
func (s *Server) Forbid() {
	s.mu.Lock()
	s.limitNext = append(s.limitNext, limit{status: http.StatusForbidden, message: "Resource not accessible by integration"})
	s.mu.Unlock()
}

// LoseNextResponse performs the next count matching requests but answers 502,
// as when a write succeeds and its response is lost in transit.
func (s *Server) LoseNextResponse(prefix string, count int) {
	s.mu.Lock()
	s.loseNext[prefix] += count
	s.mu.Unlock()
}

// FailNext makes the next count requests whose path has prefix answer 502.
func (s *Server) FailNext(prefix string, count int) {
	s.mu.Lock()
	s.failNext[prefix] += count
	s.mu.Unlock()
}

// ---- webhook payloads --------------------------------------------------------

// Sign returns headers for a signed delivery.
func Sign(secret, event, delivery string, body []byte) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", delivery)
	h.Set("Content-Type", "application/json")
	return h
}

func (s *Server) repository() map[string]any { return map[string]any{"full_name": s.Repo} }

// IssuesHook is an `issues` delivery body for the issue's current state.
func (s *Server) IssuesHook(action string, n int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return mustJSON(map[string]any{"action": action, "repository": s.repository(), "issue": s.issueJSON(s.issue(n)), "sender": User{Login: "someone", Type: "User"}})
}

// CommentHook is an `issue_comment` delivery body.
func (s *Server) CommentHook(action string, id int64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.comment(id)
	return mustJSON(map[string]any{"action": action, "repository": s.repository(), "issue": s.issueJSON(s.issue(c.Number)), "comment": s.commentJSON(c)})
}

// PullHook is a `pull_request` delivery body.
func (s *Server) PullHook(action string, n int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.pullJSON(n)
	pr["merged"] = s.pulls[n].MergedAt != nil
	return mustJSON(map[string]any{"action": action, "repository": s.repository(), "pull_request": pr, "number": n})
}

// ReviewHook is a `pull_request_review` delivery body (lowercase state, as sent).
func (s *Server) ReviewHook(action string, n int, id int64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.pulls[n].Reviews {
		if r.ID == id {
			rj := reviewJSON(r, s.web(n))
			rj["state"] = strings.ToLower(r.State)
			return mustJSON(map[string]any{"action": action, "repository": s.repository(), "pull_request": s.pullJSON(n), "review": rj})
		}
	}
	panic("githubtest: no review")
}

// ReviewCommentHook is a `pull_request_review_comment` delivery body.
func (s *Server) ReviewCommentHook(action string, id int64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.comment(id)
	return mustJSON(map[string]any{"action": action, "repository": s.repository(), "pull_request": s.pullJSON(c.Number), "comment": s.commentJSON(c)})
}

// CheckRunHook is a completed `check_run` delivery body.
func (s *Server) CheckRunHook(sha string, prs ...int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []map[string]any
	for _, n := range prs {
		list = append(list, map[string]any{"number": n, "head": map[string]any{"sha": sha}})
	}
	return mustJSON(map[string]any{"action": "completed", "repository": s.repository(), "check_run": map[string]any{"head_sha": sha, "status": "completed", "pull_requests": list}})
}

// StatusHook is a `status` delivery body.
func (s *Server) StatusHook(sha, state string) []byte {
	return mustJSON(map[string]any{"repository": s.repository(), "sha": sha, "state": state})
}

// PushHook is a `push` delivery body.
func (s *Server) PushHook(ref, before, after string, files ...string) []byte {
	return mustJSON(map[string]any{"repository": s.repository(), "ref": ref, "before": before, "after": after, "commits": []map[string]any{{"added": files}}})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- JSON representations -----------------------------------------------------

func (s *Server) web(n int) string { return s.URL + "/web/" + s.Repo + "/issues/" + strconv.Itoa(n) }

func tp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (s *Server) issueJSON(i *Issue) map[string]any {
	labels := []map[string]string{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	var reason any
	if i.StateReason != "" {
		reason = i.StateReason
	}
	m := map[string]any{"number": i.Number, "title": i.Title, "body": i.Body, "html_url": s.web(i.Number), "user": i.User, "labels": labels,
		"state": i.State, "state_reason": reason, "created_at": ts(i.Created), "updated_at": ts(i.Updated), "closed_at": tp(i.Closed),
		"assignees": []User{}, "comments": s.countComments(i.Number), "milestone": nil}
	if i.Pull {
		m["pull_request"] = map[string]any{"url": s.URL + "/repos/" + s.Repo + "/pulls/" + strconv.Itoa(i.Number), "merged_at": tp(s.pulls[i.Number].MergedAt)}
	}
	return m
}

func (s *Server) countComments(n int) int {
	c := 0
	for _, x := range s.comments {
		if x.Number == n && !x.Inline {
			c++
		}
	}
	return c
}

func (s *Server) pullJSON(n int) map[string]any {
	i, p := s.issue(n), s.pulls[n]
	labels := []map[string]string{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]any{"number": n, "title": i.Title, "body": i.Body, "html_url": s.web(n), "state": i.State, "draft": p.Draft,
		"merged_at": tp(p.MergedAt), "closed_at": tp(i.Closed), "created_at": ts(i.Created), "updated_at": ts(i.Updated), "user": i.User,
		"labels": labels, "head": map[string]string{"ref": p.HeadRef, "sha": p.HeadSHA}, "base": map[string]string{"ref": p.BaseRef}}
}

func (s *Server) comment(id int64) *Comment {
	for _, c := range s.comments {
		if c.ID == id {
			return c
		}
	}
	panic("githubtest: no comment")
}

func (s *Server) commentJSON(c *Comment) map[string]any {
	m := map[string]any{"id": c.ID, "user": c.User, "body": c.Body, "html_url": s.web(c.Number) + "#issuecomment-" + strconv.FormatInt(c.ID, 10),
		"created_at": ts(c.Created), "updated_at": ts(c.Updated), "author_association": "OWNER"}
	if c.Inline {
		m["pull_request_url"] = s.URL + "/repos/" + s.Repo + "/pulls/" + strconv.Itoa(c.Number)
		m["path"], m["line"], m["commit_id"] = c.Path, c.Line, s.pulls[c.Number].HeadSHA
		if c.ReplyTo != 0 {
			m["in_reply_to_id"] = c.ReplyTo
		}
	} else {
		m["issue_url"] = s.URL + "/repos/" + s.Repo + "/issues/" + strconv.Itoa(c.Number)
	}
	reactions := map[string]any{"total_count": 0}
	total := 0
	for k, v := range c.Reacts {
		reactions[k] = v
		total += v
	}
	reactions["total_count"] = total
	m["reactions"] = reactions
	return m
}

func reviewJSON(r Review, web string) map[string]any {
	return map[string]any{"id": r.ID, "user": r.User, "body": r.Body, "state": r.State, "submitted_at": ts(r.Submitted), "html_url": web + "#pullrequestreview-" + strconv.FormatInt(r.ID, 10), "commit_id": r.CommitID}
}

// ---- HTTP ---------------------------------------------------------------------

var (
	reInstall     = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/installation$`)
	reAccessToken = regexp.MustCompile(`^/app/installations/([0-9]+)/access_tokens$`)
)

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	auth := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "token ")
	if reInstall.MatchString(r.URL.Path) || reAccessToken.MatchString(r.URL.Path) {
		app, ok := s.verifyJWT(auth)
		if !ok {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return "", false
		}
		return "app:" + app, true
	}
	if s.tokens[auth] {
		return auth, true
	}
	if _, ok := s.installs[auth]; ok {
		return auth, true
	}
	http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	return "", false
}

func (s *Server) verifyJWT(tok string) (string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", false
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(claimsJSON, &claims) != nil {
		return "", false
	}
	key, ok := s.apps[claims.Iss]
	if !ok {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return "", false
	}
	return claims.Iss, true
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	if len(s.limitNext) > 0 {
		l := s.limitNext[0]
		s.limitNext = s.limitNext[1:]
		if l.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(l.retryAfter))
		}
		if !l.reset.IsZero() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(l.reset.Unix(), 10))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(l.status)
		fmt.Fprintf(w, `{"message":%q}`, l.message)
		return
	}
	for prefix, n := range s.failNext {
		if n > 0 && strings.HasPrefix(r.URL.Path, prefix) {
			s.failNext[prefix] = n - 1
			http.Error(w, `{"message":"Bad gateway"}`, http.StatusBadGateway)
			return
		}
	}
	token, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if name, found := strings.CutPrefix(r.URL.Path, "/files/"); found && r.Method == http.MethodGet {
		if target := s.files[name]; target != "" {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		if data, ok := s.fileData[name]; ok {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
		return
	}
	var body json.RawMessage
	if r.Body != nil && r.Method != http.MethodGet {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if r.Method != http.MethodGet {
		s.writes = append(s.writes, Write{Method: r.Method, Path: r.URL.Path, Token: token, Body: body})
	}
	status, out := s.route(r, token, body)
	for prefix, n := range s.loseNext {
		if n > 0 && strings.HasPrefix(r.URL.Path, prefix) {
			s.loseNext[prefix] = n - 1
			http.Error(w, `{"message":"Bad gateway"}`, http.StatusBadGateway)
			return
		}
	}
	if status == 0 {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	data := mustJSON(out)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		sum := sha256.Sum256(data)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			s.notMod++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if link := s.link(r, out); link != "" {
			w.Header().Set("Link", link)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (s *Server) link(r *http.Request, out any) string {
	q := r.URL.Query()
	if q.Get("page") == "" {
		return ""
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per == 0 {
		per = 30
	}
	// The handler already sliced the page; it marks more pages by length.
	if items, ok := out.([]any); ok && len(items) >= per {
		page, _ := strconv.Atoi(q.Get("page"))
		q.Set("page", strconv.Itoa(page+1))
		u := *r.URL
		u.RawQuery = q.Encode()
		return fmt.Sprintf(`<%s%s>; rel="next"`, s.URL, u.RequestURI())
	}
	return ""
}

func paged[T any](r *http.Request, all []T) []any {
	q := r.URL.Query()
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 30
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	out := []any{}
	for i := (page - 1) * per; i < len(all) && i < page*per; i++ {
		out = append(out, all[i])
	}
	return out
}

func since(r *http.Request) time.Time {
	t, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	return t
}

var (
	reRepo         = regexp.MustCompile(`^/repos/([^/]+/[^/]+)(/.*)?$`)
	reIssue        = regexp.MustCompile(`^/issues/([0-9]+)$`)
	reIssueEvents  = regexp.MustCompile(`^/issues/([0-9]+)/events$`)
	reIssueComms   = regexp.MustCompile(`^/issues/([0-9]+)/comments$`)
	reIssueLabels  = regexp.MustCompile(`^/issues/([0-9]+)/labels$`)
	reIssueLabel   = regexp.MustCompile(`^/issues/([0-9]+)/labels/(.+)$`)
	reComment      = regexp.MustCompile(`^/issues/comments/([0-9]+)$`)
	reCommentReact = regexp.MustCompile(`^/(issues|pulls)/comments/([0-9]+)/reactions$`)
	rePull         = regexp.MustCompile(`^/pulls/([0-9]+)$`)
	rePullReviews  = regexp.MustCompile(`^/pulls/([0-9]+)/reviews$`)
	reCheckRuns    = regexp.MustCompile(`^/commits/([^/]+)/check-runs$`)
	reStatus       = regexp.MustCompile(`^/commits/([^/]+)/status$`)
	reCommitPulls  = regexp.MustCompile(`^/commits/([^/]+)/pulls$`)
	reCommit       = regexp.MustCompile(`^/commits/([^/]+)$`)
	rePullComments = regexp.MustCompile(`^/pulls/([0-9]+)/comments$`)
	reReply        = regexp.MustCompile(`^/pulls/([0-9]+)/comments/([0-9]+)/replies$`)
)

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func (s *Server) route(r *http.Request, token string, body json.RawMessage) (int, any) {
	if m := reAccessToken.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodPost {
		s.issued++
		tok := fmt.Sprintf("ghs_%s_%d", strings.TrimPrefix(token, "app:"), s.issued)
		s.installs[tok] = strings.TrimPrefix(token, "app:")
		return http.StatusCreated, map[string]any{"token": tok, "expires_at": ts(s.now.Add(s.tokenTTL))}
	}
	if m := reInstall.FindStringSubmatch(r.URL.Path); m != nil {
		if !strings.EqualFold(m[1], s.Repo) {
			return 0, nil
		}
		return http.StatusOK, map[string]any{"id": 77}
	}
	m := reRepo.FindStringSubmatch(r.URL.Path)
	if m == nil || !strings.EqualFold(m[1], s.Repo) {
		return 0, nil
	}
	rest, _ := url.PathUnescape(m[2])
	get := r.Method == http.MethodGet
	q := r.URL.Query()
	switch {
	case get && rest == "":
		out := map[string]any{"full_name": s.Repo, "default_branch": "main"}
		if s.perms != nil {
			out["permissions"] = s.perms
		}
		return http.StatusOK, out
	case get && rest == "/hooks":
		list := []map[string]any{}
		for _, h := range s.hooks {
			list = append(list, map[string]any{"id": h.ID, "active": h.Active, "events": h.Events, "config": map[string]any{"url": h.URL, "content_type": "json"}})
		}
		return http.StatusOK, paged(r, list)
	case (r.Method == http.MethodPost && rest == "/hooks") || (r.Method == http.MethodPatch && strings.HasPrefix(rest, "/hooks/")):
		var req struct {
			Active bool     `json:"active"`
			Events []string `json:"events"`
			Config struct {
				URL    string `json:"url"`
				Secret string `json:"secret"`
			} `json:"config"`
		}
		if json.Unmarshal(body, &req) != nil || req.Config.URL == "" {
			return http.StatusUnprocessableEntity, map[string]any{"message": "invalid hook"}
		}
		if r.Method == http.MethodPost {
			h := &Hook{ID: s.id(), URL: req.Config.URL, Secret: req.Config.Secret, Events: req.Events, Active: req.Active}
			s.hooks = append(s.hooks, h)
			return http.StatusCreated, map[string]any{"id": h.ID}
		}
		for _, h := range s.hooks {
			if strconv.FormatInt(h.ID, 10) == strings.TrimPrefix(rest, "/hooks/") {
				h.URL, h.Secret, h.Events, h.Active = req.Config.URL, req.Config.Secret, req.Events, req.Active
				return http.StatusOK, map[string]any{"id": h.ID}
			}
		}
		return http.StatusNotFound, map[string]any{"message": "Not Found"}
	case get && rest == "/issues":
		var list []map[string]any
		for _, n := range s.sortedIssues() {
			i := s.issues[n]
			if st := q.Get("state"); st != "" && st != "all" && i.State != st {
				continue
			}
			list = append(list, s.issueJSON(i))
		}
		return http.StatusOK, paged(r, list)
	case r.Method == http.MethodPost && rest == "/issues":
		var req struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		_ = json.Unmarshal(body, &req)
		n := len(s.issues) + 1
		t := s.tick()
		s.issues[n] = &Issue{Number: n, Title: req.Title, Body: req.Body, User: User{Login: "open-conduit[bot]", Type: "Bot"}, Labels: req.Labels, State: "open", Created: t, Updated: t}
		return http.StatusCreated, s.issueJSON(s.issues[n])
	case get && rest == "/pulls":
		owner, _, _ := strings.Cut(s.Repo, "/")
		var list []map[string]any
		for _, n := range s.sortedIssues() {
			if !s.issues[n].Pull {
				continue
			}
			if st := q.Get("state"); st == "open" && s.issues[n].State != "open" {
				continue
			}
			if head := q.Get("head"); head != "" && head != owner+":"+s.pulls[n].HeadRef {
				continue
			}
			list = append(list, s.pullJSON(n))
		}
		return http.StatusOK, paged(r, list)
	case r.Method == http.MethodPost && rest == "/pulls":
		var req struct {
			Title, Body, Head, Base string
			Draft                   bool
		}
		_ = json.Unmarshal(body, &req)
		for _, n := range s.sortedIssues() {
			if p, ok := s.pulls[n]; ok && p.HeadRef == req.Head && s.issues[n].State == "open" {
				return http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed: A pull request already exists"}
			}
		}
		n := len(s.issues) + 1
		t := s.tick()
		sha := fmt.Sprintf("sha-%s-%d", strings.ReplaceAll(req.Head, "/", "-"), n)
		s.issues[n] = &Issue{Number: n, Title: req.Title, Body: req.Body, User: User{Login: "open-conduit[bot]", Type: "Bot"}, State: "open", Created: t, Updated: t, Pull: true}
		s.pulls[n] = &Pull{Number: n, HeadRef: req.Head, HeadSHA: sha, BaseRef: req.Base, Draft: req.Draft, CreatedAt: t}
		s.commits[sha] = t
		return http.StatusCreated, s.pullJSON(n)
	case get && strings.HasPrefix(rest, "/branches/"):
		if s.branches[strings.TrimPrefix(rest, "/branches/")] {
			return http.StatusOK, map[string]any{"name": strings.TrimPrefix(rest, "/branches/")}
		}
		return 0, nil
	case r.Method == http.MethodPost && rest == "/git/trees":
		return http.StatusCreated, map[string]any{"sha": "tree-1"}
	case r.Method == http.MethodPost && rest == "/git/commits":
		return http.StatusCreated, map[string]any{"sha": "commit-1"}
	case r.Method == http.MethodPost && rest == "/git/refs":
		var req struct {
			Ref string `json:"ref"`
		}
		_ = json.Unmarshal(body, &req)
		name := strings.TrimPrefix(req.Ref, "refs/heads/")
		if s.branches[name] {
			return http.StatusUnprocessableEntity, map[string]any{"message": "Reference already exists"}
		}
		s.branches[name] = true
		return http.StatusCreated, map[string]any{"ref": req.Ref}
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "/contents/"):
		path := strings.TrimPrefix(rest, "/contents/")
		if _, exists := s.contents[path]; exists {
			return http.StatusUnprocessableEntity, map[string]any{"message": "Invalid request. \"sha\" wasn't supplied."}
		}
		var req struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &req)
		data, _ := base64.StdEncoding.DecodeString(req.Content)
		s.contents[path] = data
		return http.StatusCreated, map[string]any{"content": map[string]any{"path": path}}
	case get && rest == "/issues/comments", get && rest == "/pulls/comments":
		inline, from := rest == "/pulls/comments", since(r)
		var list []map[string]any
		for _, c := range s.comments {
			if c.Inline == inline && !c.Updated.Before(from) {
				list = append(list, s.commentJSON(c))
			}
		}
		return http.StatusOK, paged(r, list)
	}
	if x := reIssue.FindStringSubmatch(rest); x != nil && get {
		if i, ok := s.issues[atoi(x[1])]; ok {
			return http.StatusOK, s.issueJSON(i)
		}
		return 0, nil
	}
	if x := reIssueEvents.FindStringSubmatch(rest); x != nil && get {
		var list []map[string]any
		for _, e := range s.events[atoi(x[1])] {
			list = append(list, map[string]any{"id": e.ID, "event": e.Event, "created_at": ts(e.Created)})
		}
		return http.StatusOK, paged(r, list)
	}
	if x := reIssueComms.FindStringSubmatch(rest); x != nil {
		n := atoi(x[1])
		if _, ok := s.issues[n]; !ok {
			return 0, nil
		}
		if r.Method == http.MethodPost {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(body, &req)
			login := "open-conduit[bot]"
			id := s.addComment(n, User{Login: login, Type: "Bot"}, req.Body, false, "", 0)
			return http.StatusCreated, s.commentJSON(s.comment(id))
		}
		from := since(r)
		var list []map[string]any
		for _, c := range s.comments {
			if c.Number == n && !c.Inline && !c.Updated.Before(from) {
				list = append(list, s.commentJSON(c))
			}
		}
		return http.StatusOK, paged(r, list)
	}
	if x := reIssueLabels.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPost {
		i := s.issues[atoi(x[1])]
		var req struct {
			Labels []string `json:"labels"`
		}
		_ = json.Unmarshal(body, &req)
		for _, l := range req.Labels {
			if !contains(i.Labels, l) {
				i.Labels = append(i.Labels, l)
			}
		}
		i.Updated = s.tick()
		return http.StatusOK, i.Labels
	}
	if x := reIssueLabel.FindStringSubmatch(rest); x != nil && r.Method == http.MethodDelete {
		i := s.issues[atoi(x[1])]
		for k, l := range i.Labels {
			if strings.EqualFold(l, x[2]) {
				i.Labels = append(i.Labels[:k], i.Labels[k+1:]...)
				i.Updated = s.tick()
				return http.StatusOK, i.Labels
			}
		}
		return 0, nil
	}
	if x := reComment.FindStringSubmatch(rest); x != nil && get {
		for _, c := range s.comments {
			if c.ID == int64(atoi(x[1])) {
				return http.StatusOK, s.commentJSON(c)
			}
		}
		return 0, nil
	}
	if x := reCommentReact.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPost {
		var req struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &req)
		for _, c := range s.comments {
			if c.ID == int64(atoi(x[2])) && c.Inline == (x[1] == "pulls") {
				if c.Reacts[req.Content] == 0 {
					c.Reacts[req.Content] = 1
					return http.StatusCreated, map[string]any{"content": req.Content}
				}
				return http.StatusOK, map[string]any{"content": req.Content}
			}
		}
		return 0, nil
	}
	if x := rePull.FindStringSubmatch(rest); x != nil && get {
		if _, ok := s.pulls[atoi(x[1])]; ok {
			pr := s.pullJSON(atoi(x[1]))
			pr["merged"] = s.pulls[atoi(x[1])].MergedAt != nil
			return http.StatusOK, pr
		}
		return 0, nil
	}
	if x := rePullReviews.FindStringSubmatch(rest); x != nil && get {
		p, ok := s.pulls[atoi(x[1])]
		if !ok {
			return 0, nil
		}
		var list []map[string]any
		for _, rv := range p.Reviews {
			list = append(list, reviewJSON(rv, s.web(p.Number)))
		}
		return http.StatusOK, paged(r, list)
	}
	if rest == "/actions/runs" && get {
		if s.refuseActions {
			return http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"}
		}
		runs := []map[string]any{}
		for _, c := range s.checks[r.URL.Query().Get("head_sha")] {
			var conclusion any
			if c.Conclusion != "" {
				conclusion = c.Conclusion
			}
			runs = append(runs, map[string]any{"id": c.ID, "name": c.Name, "status": c.Status, "conclusion": conclusion, "html_url": s.URL + "/actions/runs/" + strconv.FormatInt(c.ID, 10),
				"run_started_at": ts(c.Started), "updated_at": ts(c.Started)})
		}
		return http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": runs}
	}
	if x := reCheckRuns.FindStringSubmatch(rest); x != nil && get && s.refuseRuns {
		return http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"}
	}
	if x := reCheckRuns.FindStringSubmatch(rest); x != nil && get {
		var runs []map[string]any
		for _, c := range s.checks[x[1]] {
			var conclusion any
			if c.Conclusion != "" {
				conclusion = c.Conclusion
			}
			runs = append(runs, map[string]any{"id": c.ID, "name": c.Name, "status": c.Status, "conclusion": conclusion, "html_url": s.URL + "/runs/" + strconv.FormatInt(c.ID, 10),
				"details_url": c.Details, "started_at": ts(c.Started), "completed_at": tp(c.Completed)})
		}
		if runs == nil {
			runs = []map[string]any{}
		}
		return http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs}
	}
	if x := reStatus.FindStringSubmatch(rest); x != nil && get {
		list := []map[string]any{}
		for _, st := range s.statuses[x[1]] {
			list = append(list, map[string]any{"id": st.ID, "context": st.Context, "state": st.State, "target_url": st.Target, "created_at": ts(st.At), "updated_at": ts(st.At)})
		}
		return http.StatusOK, map[string]any{"state": "pending", "statuses": list}
	}
	if x := reCommit.FindStringSubmatch(rest); x != nil && get {
		at, ok := s.commits[x[1]]
		if !ok {
			return 0, nil
		}
		return http.StatusOK, map[string]any{"sha": x[1], "commit": map[string]any{"committer": map[string]any{"date": ts(at)}}}
	}
	if x := rePullComments.FindStringSubmatch(rest); x != nil {
		n := atoi(x[1])
		if _, ok := s.pulls[n]; !ok {
			return 0, nil
		}
		var list []map[string]any
		for _, c := range s.comments {
			if c.Number == n && c.Inline {
				list = append(list, s.commentJSON(c))
			}
		}
		return http.StatusOK, paged(r, list)
	}
	if x := reReply.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPost {
		n, parent := atoi(x[1]), int64(atoi(x[2]))
		var p *Comment
		for _, c := range s.comments {
			if c.ID == parent && c.Inline && c.Number == n {
				p = c
			}
		}
		if p == nil {
			return 0, nil
		}
		var req struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &req)
		id := s.addComment(n, User{Login: "open-conduit[bot]", Type: "Bot"}, req.Body, true, p.Path, p.Line)
		s.comment(id).ReplyTo = parent
		return http.StatusCreated, s.commentJSON(s.comment(id))
	}
	if x := reCommitPulls.FindStringSubmatch(rest); x != nil && get {
		var list []map[string]any
		for _, n := range s.sortedIssues() {
			if p, ok := s.pulls[n]; ok && p.HeadSHA == x[1] {
				list = append(list, s.pullJSON(n))
			}
		}
		return http.StatusOK, paged(r, list)
	}
	return 0, nil
}

func (s *Server) sortedIssues() []int {
	var ns []int
	for n := range s.issues {
		ns = append(ns, n)
	}
	sort.Ints(ns)
	return ns
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}
