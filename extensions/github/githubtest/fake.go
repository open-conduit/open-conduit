// Package githubtest is an in-memory GitHub REST fake for conformance and
// integration fixtures. It models the endpoints the extension uses, with
// ETag/304, Link pagination, primary/secondary rate limits, token and App
// authentication, and records every write. It is test support, never linked
// into production binaries.
package githubtest

import (
	"compress/gzip"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	// Association is the author's author_association (default OWNER).
	Association string
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
	// HeadRepo is the head's repository (default the fake's own; "-" for
	// a deleted fork, whose head repo is null).
	HeadRepo string
	// Mergeable is what a single PR's read and its webhook bodies say
	// (nil: GitHub has not worked it out). The list never carries it.
	Mergeable *bool
	// Behind is how many of its base branch's commits the head lacks, as
	// the compare endpoint reports it (0: up to date).
	Behind int
}

// Dispatch is one workflow_dispatch a caller started.
type Dispatch struct {
	ID       int64
	Workflow string
	Ref      string
	Inputs   map[string]any
	At       time.Time
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
	// protection and rulesets are the checks each branch requires;
	// refuseProtection and refuseRulesets make those reads an ordinary 403.
	protection, rulesets map[string][]string
	// mergeRules are what each base branch demands of a merge.
	mergeRules map[string]MergeRules
	// public makes the repository read as public.
	public                           bool
	refuseProtection, refuseRulesets bool
	statuses                         map[string][]Status
	tokens                           map[string]bool
	apps                             map[string]*rsa.PublicKey
	identities                       map[string]User   // writer by token or App ID
	installs                         map[string]string // installation token → app id
	scopes                           map[string]TokenScope
	writes                           []Write
	requests                         []string
	notMod                           int
	limitNext                        []limit
	failNext                         map[string]int
	loseNext                         map[string]int
	tokenTTL                         time.Duration
	// gitLag is how long after it is minted an installation token is
	// refused for push (SetGitPropagation); minted is when each was.
	gitLag     time.Duration
	minted     map[string]time.Time
	gitPushAsk int
	issued     int
	commits    map[string]time.Time
	contents   map[string][]byte // attachments branch path → content
	branches   map[string]bool
	onBranch   map[string]map[string]bool // branch → commits in its history
	// workflows are the workflow files that may be dispatched, and
	// dispatches those started.
	workflows  map[string]bool
	dispatches []Dispatch
	files      map[string]string // /files/<name> → redirect target
	fileReads  []Write
	fileData   map[string][]byte
	hooks      []*Hook
	perms      map[string]bool
	// collaborators are users' repository permissions by lower-case login
	// (none for a user not listed).
	collaborators map[string]string
	// resolved holds the review threads resolved through GraphQL, by the
	// ID of the inline comment that opened them; threadPage is how many
	// threads a reviewThreads page holds; refuseResolve makes resolving
	// a thread FORBIDDEN.
	resolved      map[int64]string
	threadPage    int
	refuseResolve bool
	// accepted is the X-Accepted-GitHub-Permissions header a refused
	// resolve or PR edit answers with ("" sends none); header is what the
	// request being answered adds to its response.
	accepted string
	header   http.Header
	// installPerms are the App installation's permissions, as
	// GET /repos/<repo>/installation reports them (none when nil).
	installPerms map[string]string
	// refusePings makes pinging a hook fail.
	refusePings bool
	// refusePullEdits makes editing a PR (PATCH and the draft mutations)
	// forbidden, as for a connection without Pull requests write.
	refusePullEdits bool
}

// Hook is a repository webhook.
type Hook struct {
	ID     int64
	URL    string
	Secret string
	Events []string
	Active bool
	// LastCode and LastMessage are the hook's last_response (none before
	// SetHookResponse); Pings counts the pings it was sent.
	LastCode    int
	LastMessage string
	Pings       int
}

// RefusePings makes pinging a hook answer 503.
func (s *Server) RefusePings(v bool) { s.mu.Lock(); s.refusePings = v; s.mu.Unlock() }

// SetHookResponse sets what every hook's last delivery answered, as GitHub
// reports it in last_response.
func (s *Server) SetHookResponse(code int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hooks {
		h.LastCode, h.LastMessage = code, message
	}
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

// SetCollaborator sets a user's permission on the repository (admin, write,
// read), as GET /repos/{repo}/collaborators/{login}/permission reports it.
func (s *Server) SetCollaborator(login, permission string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.collaborators == nil {
		s.collaborators = map[string]string{}
	}
	s.collaborators[strings.ToLower(login)] = permission
}

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
		apps: map[string]*rsa.PublicKey{}, identities: map[string]User{}, installs: map[string]string{}, minted: map[string]time.Time{}, scopes: map[string]TokenScope{}, failNext: map[string]int{}, loseNext: map[string]int{}, tokenTTL: time.Hour,
		commits: map[string]time.Time{}, contents: map[string][]byte{}, branches: map[string]bool{}, onBranch: map[string]map[string]bool{}, files: map[string]string{}, fileData: map[string][]byte{},
		protection: map[string][]string{}, rulesets: map[string][]string{}, resolved: map[int64]string{}, threadPage: 100}
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

// AppBot is the bot user of the App with id, as GET /app and GET
// /users/<login> report it: the slug app-<id>, the login <slug>[bot] and a
// user id of 9000000 plus the App's id.
func AppBot(id string) (slug, login string, userID int64) {
	n, _ := strconv.ParseInt(id, 10, 64)
	slug = "app-" + id
	return slug, slug + "[bot]", 9000000 + n
}

// SetIdentity sets who a token, or an App's installation tokens (by App
// ID), write as. Unset writers are open-conduit[bot].
func (s *Server) SetIdentity(tokenOrApp string, u User) {
	s.mu.Lock()
	s.identities[tokenOrApp] = u
	s.mu.Unlock()
}

// writer is the account a write signed in with token is recorded as.
func (s *Server) writer(token string) User {
	if app, ok := s.installs[token]; ok {
		token = app
	}
	if u, ok := s.identities[token]; ok {
		return u
	}
	return User{Login: "open-conduit[bot]", Type: "Bot"}
}

// TokenScope is what an installation token was minted for: the
// repositories (names, without the owner) and permissions the request
// asked for. A token minted without either covers the whole installation.
type TokenScope struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// Scope reports what an installation token was minted for, and whether it
// is still valid (minted and not revoked).
func (s *Server) Scope(token string) (TokenScope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.installs[token]
	return s.scopes[token], ok
}

// TokenApp is the App (by ID) that minted an installation token still
// valid, or false.
func (s *Server) TokenApp(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.installs[token]
	return app, ok
}

// gitAllowed reports whether token may read (or, with push, write) repo's
// git data: an accepted token, or a valid installation token whose scope
// covers repo, with contents: write for a push, and for a push no longer
// propagating (SetGitPropagation).
func (s *Server) gitAllowed(token, repo string, push bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gitAllowedLocked(token, repo, push)
}

// gitAllowedLocked is gitAllowed with s.mu held.
func (s *Server) gitAllowedLocked(token, repo string, push bool) bool {
	if s.tokens[token] {
		return true
	}
	if _, ok := s.installs[token]; !ok {
		return false
	}
	if push && s.now.Before(s.minted[token].Add(s.gitLag)) {
		return false
	}
	scope, scoped := s.scopes[token]
	if !scoped {
		return strings.EqualFold(repo, s.Repo)
	}
	_, name, _ := strings.Cut(repo, "/")
	owner, _, _ := strings.Cut(s.Repo, "/")
	if !strings.HasPrefix(strings.ToLower(repo), strings.ToLower(owner)+"/") || !slices.ContainsFunc(scope.Repositories, func(r string) bool { return strings.EqualFold(r, name) }) {
		return false
	}
	if scope.Permissions == nil {
		return true
	}
	perm := scope.Permissions["contents"]
	return perm == "write" || !push && perm == "read"
}

// Handler is the fake's API handler, for serving it somewhere other than
// its own test server.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// GitHandler serves git's smart HTTP for the bare repositories under root
// (<owner>/<name>.git) with git's own upload-pack and receive-pack, as
// github.com would: HTTP basic auth whose password is a token gitAllowed
// accepts for the repository, a push needing contents: write. Anything
// else is a 401.
func (s *Server) GitHandler(root, git string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := reGitPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		service := m[2]
		if service == "info/refs" {
			service = r.URL.Query().Get("service")
		}
		if service != "git-upload-pack" && service != "git-receive-pack" {
			http.Error(w, "unsupported service", http.StatusForbidden)
			return
		}
		_, token, ok := r.BasicAuth()
		if !ok || !s.gitAllowed(token, m[1], service == "git-receive-pack") {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "authentication failed", http.StatusUnauthorized)
			return
		}
		dir := filepath.Join(root, filepath.FromSlash(m[1])+".git")
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			http.NotFound(w, r)
			return
		}
		args := []string{strings.TrimPrefix(service, "git-"), "--stateless-rpc"}
		if m[2] == "info/refs" {
			args = append(args, "--advertise-refs")
		}
		cmd := exec.CommandContext(r.Context(), git, append(args, dir)...)
		if m[2] == "info/refs" {
			w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
			w.Header().Set("Cache-Control", "no-cache")
			line := "# service=" + service + "\n"
			fmt.Fprintf(w, "%04x%s0000", len(line)+4, line)
		} else {
			body := io.Reader(r.Body)
			if r.Header.Get("Content-Encoding") == "gzip" {
				z, err := gzip.NewReader(r.Body)
				if err != nil {
					http.Error(w, "bad body", http.StatusBadRequest)
					return
				}
				body = z
			}
			cmd.Stdin = body
			w.Header().Set("Content-Type", "application/x-"+service+"-result")
		}
		cmd.Stdout = w
		_ = cmd.Run()
	})
}

var reGitPath = regexp.MustCompile(`^/([^/]+/[^/]+?)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

// SetGitPropagation makes the git service refuse an installation token for
// push (403, "Permission to <repo>.git denied") until d after it was minted
// by the fake's clock, as GitHub can while it propagates a new token.
func (s *Server) SetGitPropagation(d time.Duration) { s.mu.Lock(); s.gitLag = d; s.mu.Unlock() }

// GitPushChecks counts the receive-pack advertisements asked for: the
// first request of a git push.
func (s *Server) GitPushChecks() int { s.mu.Lock(); defer s.mu.Unlock(); return s.gitPushAsk }

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

// SetAssociation sets an issue's or PR's author_association.
func (s *Server) SetAssociation(n int, association string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issue(n).Association = association
}

// SetHeadRepo sets a PR's head repository ("-" for a deleted fork).
func (s *Server) SetHeadRepo(n int, repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].HeadRepo = repo
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

// ServeFile makes GET <any prefix>/files/<name> serve data to an
// authenticated reader, or redirect to target when it is set (a signed
// storage link elsewhere). Under /<owner>/<repo>/files/<name> a scoped
// installation token must cover that repository.
func (s *Server) ServeFile(name string, data []byte, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileData[name], s.files[name] = data, target
}

// FileReads lists the GETs of files ServeFile serves, with the token each
// was sent with, in order.
func (s *Server) FileReads() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.fileReads)
}

// readAllowed reports whether token may read files under repo: an
// accepted token, an installation token minted for the whole
// installation, or one whose scope names repo (under the fake's owner).
func (s *Server) readAllowed(token, repo string) bool {
	if s.tokens[token] {
		return true
	}
	scope, scoped := s.scopes[token]
	if !scoped || scope.Repositories == nil {
		return true
	}
	owner, name, _ := strings.Cut(repo, "/")
	fakeOwner, _, _ := strings.Cut(s.Repo, "/")
	return strings.EqualFold(owner, fakeOwner) && slices.ContainsFunc(scope.Repositories, func(r string) bool { return strings.EqualFold(r, name) })
}

// ForcePushPull rewrites a PR's history to end at sha. Like GitHub, it
// moves every review's commit_id to the new head, because the commit the
// review was submitted on is no longer in the PR.
func (s *Server) ForcePushPull(n int, sha string) {
	s.PushPull(n, sha)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.pulls[n].Reviews {
		s.pulls[n].Reviews[i].CommitID = sha
	}
}

// Pull returns a copy of a pull request's state.
func (s *Server) Pull(n int) Pull { s.mu.Lock(); defer s.mu.Unlock(); return *s.pulls[n] }

// SetBase retargets a PR to another base branch.
func (s *Server) SetBase(n int, base string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].BaseRef = base
	s.issue(n).Updated = s.tick()
}

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

// SetDraft marks a PR a draft, or ready for review.
func (s *Server) SetDraft(n int, draft bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].Draft = draft
	s.issue(n).Updated = s.tick()
}

// SetMergeable sets what GitHub says about whether a PR merges cleanly
// (false: it has conflicts; nil: not worked out yet).
func (s *Server) SetMergeable(n int, m *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].Mergeable = m
}

// SetBehind sets how many of its base's commits a PR's head lacks.
func (s *Server) SetBehind(n, by int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[n].Behind = by
}

// MergeRules are what a base branch's protection or rulesets demand of a
// merge, as GitHub enforces them: the merge call refuses a PR that breaks
// one, and a single PR's read says so in mergeable_state. The zero value
// demands nothing beyond an open PR, not a draft, without conflicts.
type MergeRules struct {
	// Approvals is how many approving reviews are required; with any, a
	// reviewer still requesting changes blocks the merge too.
	Approvals int
	// Checks are the check runs that must have succeeded on the head.
	Checks []string
	// UpToDate requires the head to hold every commit of its base
	// (strict_required_status_checks_policy).
	UpToDate bool
	// Methods are the merge methods allowed; empty allows all.
	Methods []string
}

// SetMergeRules sets what branch demands of a merge into it.
func (s *Server) SetMergeRules(branch string, r MergeRules) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mergeRules == nil {
		s.mergeRules = map[string]MergeRules{}
	}
	s.mergeRules[branch] = r
}

// mergeState is GitHub's mergeable_state for PR p under its base's rules,
// with the message its merge call refuses with when it isn't clean.
func (s *Server) mergeState(p *Pull) (state, message string) {
	r := s.mergeRules[p.BaseRef]
	switch {
	case p.Mergeable != nil && !*p.Mergeable:
		return "dirty", "Pull Request is not mergeable"
	case p.Draft:
		return "draft", "Pull Request is still a draft"
	}
	if r.Approvals > 0 {
		standing := map[string]string{}
		for _, rv := range p.Reviews {
			switch strings.ToUpper(rv.State) {
			case "APPROVED", "CHANGES_REQUESTED":
				standing[rv.User.Login] = strings.ToUpper(rv.State)
			case "DISMISSED":
				delete(standing, rv.User.Login)
			}
		}
		approved := 0
		for _, st := range standing {
			if st == "CHANGES_REQUESTED" {
				return "blocked", "Changes must be made through a pull request. Review changes requested."
			}
			approved++
		}
		if approved < r.Approvals {
			return "blocked", fmt.Sprintf("At least %d approving review is required by reviewers with write access.", r.Approvals)
		}
	}
	for _, name := range r.Checks {
		ok := false
		for _, c := range s.checks[p.HeadSHA] {
			if c.Name == name {
				ok = c.Status == "completed" && c.Conclusion == "success"
			}
		}
		if !ok {
			return "blocked", fmt.Sprintf("Required status check %q is expected.", name)
		}
	}
	if r.UpToDate && p.Behind > 0 {
		return "behind", "Head branch is out of date. Review and try the merge again."
	}
	return "clean", ""
}

// AddWorkflow lets a workflow file be dispatched.
func (s *Server) AddWorkflow(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workflows == nil {
		s.workflows = map[string]bool{}
	}
	s.workflows[name] = true
}

// Dispatches are the workflow runs callers started, oldest first.
func (s *Server) Dispatches() []Dispatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Dispatch(nil), s.dispatches...)
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
	if p := s.pulls[n]; inline && p != nil {
		c.CommitID = p.HeadSHA
	}
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

// AddReviewReply replies in the inline review thread holding parent.
func (s *Server) AddReviewReply(n int, user User, parent int64, body string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.comment(parent)
	id := s.addComment(n, user, body, true, p.Path, p.Line)
	s.comment(id).ReplyTo = parent
	return id
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

// Reviews returns a PR's submitted reviews.
func (s *Server) Reviews(n int) []Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Review(nil), s.pulls[n].Reviews...)
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

// RerunCheck re-runs a named check on sha, as GitHub does: a new run with
// its own ID, in progress, in place of the finished one.
func (s *Server) RerunCheck(sha, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.checks[sha] {
		if s.checks[sha][i].Name == name {
			s.checks[sha][i] = CheckRun{ID: s.id(), Name: name, Status: "in_progress", Started: s.tick()}
		}
	}
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

// RequireChecks makes branch protection on branch require contexts (the
// branch read's protection.required_status_checks).
func (s *Server) RequireChecks(branch string, contexts ...string) {
	s.mu.Lock()
	s.protection[branch] = append([]string(nil), contexts...)
	s.mu.Unlock()
}

// SetPublic makes the repository read as public (default private).
func (s *Server) SetPublic(public bool) { s.mu.Lock(); s.public = public; s.mu.Unlock() }

// RequireRulesetChecks makes a ruleset on branch require contexts (a
// required_status_checks rule from the branch rules read).
func (s *Server) RequireRulesetChecks(branch string, contexts ...string) {
	s.mu.Lock()
	s.rulesets[branch] = append([]string(nil), contexts...)
	s.mu.Unlock()
}

// RefuseRequiredChecks makes branch reads (protection) and branch rules
// reads (rulesets) an ordinary 403, as GitHub answers an identity without
// the permission or a plan without rulesets.
func (s *Server) RefuseRequiredChecks(protection, rulesets bool) {
	s.mu.Lock()
	s.refuseProtection, s.refuseRulesets = protection, rulesets
	s.mu.Unlock()
}

// ThreadResolvedBy is who resolved the review thread an inline comment
// opened or replied in ("" while it is unresolved).
func (s *Server) ThreadResolvedBy(commentID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolved[s.threadRoot(commentID)]
}

// ResolveThread resolves the thread an inline comment is in, as login.
func (s *Server) ResolveThread(commentID int64, login string) {
	s.mu.Lock()
	s.resolved[s.threadRoot(commentID)] = login
	s.mu.Unlock()
}

// SetThreadPage sets how many review threads a GraphQL page holds.
func (s *Server) SetThreadPage(n int) { s.mu.Lock(); s.threadPage = n; s.mu.Unlock() }

// RefuseThreadResolution makes resolving a review thread FORBIDDEN, as for
// a connection without Contents write or Pull requests write.
func (s *Server) RefuseThreadResolution() { s.mu.Lock(); s.refuseResolve = true; s.mu.Unlock() }

// SetAcceptedPermissions makes refused thread resolutions and PR edits
// answer with an X-Accepted-GitHub-Permissions header of v
// ("contents=write"), as GitHub does when it names what the call needs.
func (s *Server) SetAcceptedPermissions(v string) { s.mu.Lock(); s.accepted = v; s.mu.Unlock() }

// SetInstallationPermissions sets the App installation's permissions on
// the repository ({"pull_requests": "write", "contents": "read"}). Once
// set, a scoped installation token asking for more is refused with 422,
// as GitHub refuses it.
func (s *Server) SetInstallationPermissions(p map[string]string) {
	s.mu.Lock()
	s.installPerms = p
	s.mu.Unlock()
}

// refusedHeader adds the accepted-permissions header, if set, to the
// response being answered.
func (s *Server) refusedHeader() {
	if s.accepted != "" {
		s.header = http.Header{"X-Accepted-Github-Permissions": {s.accepted}}
	}
}

// RefusePullEdits makes editing a PR's title, body or draft state
// forbidden, as for a connection without Pull requests write.
func (s *Server) RefusePullEdits() { s.mu.Lock(); s.refusePullEdits = true; s.mu.Unlock() }

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
	pr["mergeable"] = s.pulls[n].Mergeable
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

// CommitOnBranch puts sha in branch's history, as the compare endpoint
// reads it.
func (s *Server) CommitOnBranch(branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onBranch[branch] == nil {
		s.onBranch[branch] = map[string]bool{}
	}
	s.onBranch[branch][sha] = true
}

// CheckSuiteHook is a completed `check_suite` delivery body for a commit on
// a branch, ending in conclusion.
func (s *Server) CheckSuiteHook(branch, sha, conclusion string) []byte {
	return mustJSON(map[string]any{"action": "completed", "repository": s.repository(),
		"check_suite": map[string]any{"head_sha": sha, "head_branch": branch, "status": "completed", "conclusion": conclusion, "pull_requests": []any{}}})
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
		"author_association": association(i.Association),
		"state":              i.State, "state_reason": reason, "created_at": ts(i.Created), "updated_at": ts(i.Updated), "closed_at": tp(i.Closed),
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
	var headRepo any = map[string]any{"full_name": s.Repo}
	switch p.HeadRepo {
	case "":
	case "-":
		headRepo = nil
	default:
		headRepo = map[string]any{"full_name": p.HeadRepo}
	}
	return map[string]any{"number": n, "node_id": "PR_" + strconv.Itoa(n), "title": i.Title, "body": i.Body, "html_url": s.web(n), "state": i.State, "draft": p.Draft,
		"merged_at": tp(p.MergedAt), "closed_at": tp(i.Closed), "created_at": ts(i.Created), "updated_at": ts(i.Updated), "user": i.User,
		"author_association": association(i.Association), "labels": labels,
		"head": map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "repo": headRepo}, "base": map[string]any{"ref": p.BaseRef, "repo": map[string]any{"full_name": s.Repo}}}
}

// association defaults an author_association to OWNER.
func association(a string) string {
	if a == "" {
		return "OWNER"
	}
	return a
}

// findComment is the comment with id, or nil.
func (s *Server) findComment(id int64) *Comment {
	for _, c := range s.comments {
		if c.ID == id {
			return c
		}
	}
	return nil
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
		// Like GitHub, commit_id and line follow the PR's head: once a push
		// moves past the comment's commit, it is outdated and line is null.
		// original_commit_id and original_line never change.
		head := s.pulls[c.Number].HeadSHA
		m["path"], m["commit_id"], m["original_commit_id"], m["original_line"] = c.Path, head, c.CommitID, c.Line
		if c.CommitID == "" || c.CommitID == head {
			m["line"] = c.Line
		} else {
			m["line"] = nil
		}
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
	// reFile is a file ServeFile serves: /files/<name>,
	// /user-attachments/files/<name> or /<owner>/<repo>/files/<name>.
	reGitRefs = regexp.MustCompile(`^/([^/]+/[^/]+)\.git/info/refs$`)
	reFile    = regexp.MustCompile(`^/(?:(user-attachments|[^/]+/[^/]+)/)?files/(.+)$`)
)

// gitReceivePack answers git's push advertisement for repo, signed in with
// Basic auth as git does: 401 without a token it knows, 403 for one that
// can't push to repo (not its repository, no contents: write, or still
// propagating), else 200. s.mu is held.
func (s *Server) gitReceivePack(w http.ResponseWriter, repo string, r *http.Request) {
	s.gitPushAsk++
	_, tok, _ := r.BasicAuth()
	switch s.pushStatus(tok, repo) {
	case http.StatusUnauthorized:
		http.Error(w, "Invalid username or token.", http.StatusUnauthorized)
	case http.StatusForbidden:
		http.Error(w, "Permission to "+repo+".git denied.", http.StatusForbidden)
	default:
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = io.WriteString(w, "001f# service=git-receive-pack\n0000")
	}
}

// GitPushable reports whether the git service would take tok for a push
// to the fake's repository now, without counting a check.
func (s *Server) GitPushable(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushStatus(tok, s.Repo) == http.StatusOK
}

// pushStatus is the git service's answer to a push to repo signed in with
// tok: 401 for a token it doesn't know, 403 for one gitAllowed refuses the
// push. s.mu is held.
func (s *Server) pushStatus(tok, repo string) int {
	if _, install := s.installs[tok]; tok == "" || !s.tokens[tok] && !install {
		return http.StatusUnauthorized
	}
	if !s.gitAllowedLocked(tok, repo, true) {
		return http.StatusForbidden
	}
	return http.StatusOK
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	auth := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "token ")
	if reInstall.MatchString(r.URL.Path) || reAccessToken.MatchString(r.URL.Path) || r.URL.Path == "/app" {
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
	if m := reGitRefs.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet && r.URL.Query().Get("service") == "git-receive-pack" {
		s.gitReceivePack(w, m[1], r)
		return
	}
	token, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if m := reFile.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet {
		name := m[2]
		s.fileReads = append(s.fileReads, Write{Method: r.Method, Path: r.URL.Path, Token: token})
		// Under /<owner>/<repo>/ a scoped token reads only the
		// repositories it was minted for, as GitHub's would.
		if repo := m[1]; repo != "" && repo != "user-attachments" && !s.readAllowed(token, repo) {
			http.NotFound(w, r)
			return
		}
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
	s.header = nil
	status, out := s.route(r, token, body)
	for k, v := range s.header {
		w.Header()[k] = v
	}
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

// baseBranch reports whether name is a branch the fake knows as a base:
// main, any PR's base, or a branch given required checks.
func (s *Server) baseBranch(name string) bool {
	if name == "main" {
		return true
	}
	if _, ok := s.protection[name]; ok {
		return true
	}
	for _, p := range s.pulls {
		if p.BaseRef == name {
			return true
		}
	}
	return false
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
	rePullMerge    = regexp.MustCompile(`^/pulls/([0-9]+)/merge$`)
	rePullUpdate   = regexp.MustCompile(`^/pulls/([0-9]+)/update-branch$`)
	reDispatch     = regexp.MustCompile(`^/actions/workflows/([^/]+)/dispatches$`)
	reWorkflowRuns = regexp.MustCompile(`^/actions/workflows/([^/]+)/runs$`)
	reCheckRuns    = regexp.MustCompile(`^/commits/([^/]+)/check-runs$`)
	reStatus       = regexp.MustCompile(`^/commits/([^/]+)/status$`)
	reCommitPulls  = regexp.MustCompile(`^/commits/([^/]+)/pulls$`)
	reCommit       = regexp.MustCompile(`^/commits/([^/]+)$`)
	rePullComments = regexp.MustCompile(`^/pulls/([0-9]+)/comments$`)
	reReply        = regexp.MustCompile(`^/pulls/([0-9]+)/comments/([0-9]+)/replies$`)
)

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// permLevel orders permission levels: write includes read, admin both.
var permLevel = map[string]int{"read": 1, "write": 2, "admin": 3}

func (s *Server) route(r *http.Request, token string, body json.RawMessage) (int, any) {
	if r.URL.Path == "/graphql" && r.Method == http.MethodPost {
		return s.graphql(token, body)
	}
	if m := reAccessToken.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodPost {
		var scope TokenScope
		scoped := len(body) > 0 && json.Unmarshal(body, &scope) == nil && (scope.Repositories != nil || scope.Permissions != nil)
		// GitHub refuses a token with more than the installation holds.
		if scoped && s.installPerms != nil {
			for name, level := range scope.Permissions {
				if permLevel[s.installPerms[name]] < permLevel[level] {
					return http.StatusUnprocessableEntity, map[string]any{"message": "The permissions requested are not granted to this installation."}
				}
			}
		}
		s.issued++
		tok := fmt.Sprintf("ghs_%s_%d", strings.TrimPrefix(token, "app:"), s.issued)
		s.installs[tok] = strings.TrimPrefix(token, "app:")
		s.minted[tok] = s.now
		if scoped {
			s.scopes[tok] = scope
		}
		return http.StatusCreated, map[string]any{"token": tok, "expires_at": ts(s.now.Add(s.tokenTTL))}
	}
	if r.URL.Path == "/installation/token" && r.Method == http.MethodDelete {
		if _, ok := s.installs[token]; !ok {
			return 0, nil
		}
		delete(s.installs, token)
		delete(s.scopes, token)
		return http.StatusNoContent, nil
	}
	if r.URL.Path == "/app" && r.Method == http.MethodGet {
		id := strings.TrimPrefix(token, "app:")
		slug, _, _ := AppBot(id)
		n, _ := strconv.ParseInt(id, 10, 64)
		return http.StatusOK, map[string]any{"id": n, "slug": slug, "name": slug}
	}
	if login, ok := strings.CutPrefix(r.URL.Path, "/users/"); ok && r.Method == http.MethodGet {
		for id := range s.apps {
			if _, bot, uid := AppBot(id); bot == login {
				return http.StatusOK, map[string]any{"login": bot, "id": uid, "type": "Bot"}
			}
		}
		return 0, nil
	}
	if m := reInstall.FindStringSubmatch(r.URL.Path); m != nil {
		if !strings.EqualFold(m[1], s.Repo) {
			return 0, nil
		}
		out := map[string]any{"id": 77}
		if s.installPerms != nil {
			out["permissions"] = s.installPerms
		}
		return http.StatusOK, out
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
		out := map[string]any{"full_name": s.Repo, "default_branch": "main", "private": !s.public}
		if s.perms != nil {
			out["permissions"] = s.perms
		}
		return http.StatusOK, out
	case get && strings.HasPrefix(rest, "/collaborators/") && strings.HasSuffix(rest, "/permission"):
		login := strings.TrimSuffix(strings.TrimPrefix(rest, "/collaborators/"), "/permission")
		perm, ok := s.collaborators[strings.ToLower(login)]
		if !ok {
			perm = "none"
		}
		return http.StatusOK, map[string]any{"permission": perm, "user": map[string]any{"login": login}}
	case get && rest == "/hooks":
		list := []map[string]any{}
		for _, h := range s.hooks {
			list = append(list, map[string]any{"id": h.ID, "active": h.Active, "events": h.Events, "config": map[string]any{"url": h.URL, "content_type": "json"}})
		}
		return http.StatusOK, paged(r, list)
	case (get || r.Method == http.MethodPost && strings.HasSuffix(rest, "/pings")) && strings.HasPrefix(rest, "/hooks/"):
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "/hooks/"), "/pings")
		for _, h := range s.hooks {
			if strconv.FormatInt(h.ID, 10) != id {
				continue
			}
			if !get && s.refusePings {
				return http.StatusServiceUnavailable, map[string]any{"message": "unavailable"}
			}
			if !get {
				h.Pings++
				return http.StatusNoContent, nil
			}
			last := map[string]any{"code": nil, "status": "unused", "message": nil}
			if h.LastCode != 0 {
				last = map[string]any{"code": h.LastCode, "status": "active", "message": h.LastMessage}
			}
			return http.StatusOK, map[string]any{"id": h.ID, "active": h.Active, "events": h.Events, "config": map[string]any{"url": h.URL, "content_type": "json"}, "last_response": last}
		}
		return http.StatusNotFound, map[string]any{"message": "Not Found"}
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
		from := since(r)
		for _, n := range s.listed(q) {
			i := s.issues[n]
			if i.Updated.Before(from) {
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
		s.issues[n] = &Issue{Number: n, Title: req.Title, Body: req.Body, User: s.writer(token), Labels: req.Labels, State: "open", Created: t, Updated: t}
		return http.StatusCreated, s.issueJSON(s.issues[n])
	case get && rest == "/pulls":
		owner, _, _ := strings.Cut(s.Repo, "/")
		var list []map[string]any
		for _, n := range s.listed(q) {
			if !s.issues[n].Pull {
				continue
			}
			if head := q.Get("head"); head != "" && head != owner+":"+s.pulls[n].HeadRef {
				continue
			}
			if base := q.Get("base"); base != "" && base != s.pulls[n].BaseRef {
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
		s.issues[n] = &Issue{Number: n, Title: req.Title, Body: req.Body, User: s.writer(token), State: "open", Created: t, Updated: t, Pull: true}
		s.pulls[n] = &Pull{Number: n, HeadRef: req.Head, HeadSHA: sha, BaseRef: req.Base, Draft: req.Draft, CreatedAt: t}
		s.commits[sha] = t
		return http.StatusCreated, s.pullJSON(n)
	case get && strings.HasPrefix(rest, "/compare/"):
		branch, sha, ok := strings.Cut(strings.TrimPrefix(rest, "/compare/"), "...")
		commits, known := s.onBranch[branch]
		if ok && !commits[sha] {
			// A PR's head against its base: ahead of it, or diverged by
			// the commits the head lacks.
			for _, p := range s.pulls {
				if p.HeadSHA == sha && p.BaseRef == branch && p.MergedAt == nil {
					if p.Behind > 0 {
						return http.StatusOK, map[string]any{"status": "diverged", "ahead_by": 1, "behind_by": p.Behind}
					}
					return http.StatusOK, map[string]any{"status": "ahead", "ahead_by": 1, "behind_by": 0}
				}
			}
		}
		if !ok || !known {
			return 0, nil
		}
		if commits[sha] {
			return http.StatusOK, map[string]any{"status": "behind"}
		}
		return http.StatusOK, map[string]any{"status": "diverged"}
	case get && strings.HasPrefix(rest, "/branches/"):
		name := strings.TrimPrefix(rest, "/branches/")
		if s.branches[name] {
			return http.StatusOK, map[string]any{"name": name}
		}
		if !s.baseBranch(name) {
			return 0, nil
		}
		if s.refuseProtection {
			return http.StatusForbidden, map[string]any{"message": "Resource not accessible by personal access token"}
		}
		required := s.protection[name]
		checks := []map[string]any{}
		for _, c := range required {
			checks = append(checks, map[string]any{"context": c, "app_id": nil})
		}
		return http.StatusOK, map[string]any{"name": name, "protected": len(required) > 0, "protection": map[string]any{"enabled": len(required) > 0,
			"required_status_checks": map[string]any{"enforcement_level": "non_admins", "contexts": append([]string{}, required...), "checks": checks}}}
	case get && strings.HasPrefix(rest, "/rules/branches/"):
		if s.refuseRulesets {
			return http.StatusForbidden, map[string]any{"message": "Upgrade to GitHub Pro or make this repository public to enable this feature."}
		}
		list := []map[string]any{}
		if required := s.rulesets[strings.TrimPrefix(rest, "/rules/branches/")]; len(required) > 0 {
			checks := []map[string]any{}
			for _, c := range required {
				checks = append(checks, map[string]any{"context": c})
			}
			list = append(list, map[string]any{"type": "deletion", "ruleset_id": 1},
				map[string]any{"type": "required_status_checks", "ruleset_id": 1, "parameters": map[string]any{"strict_required_status_checks_policy": false, "required_status_checks": checks}})
		}
		return http.StatusOK, paged(r, list)
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
	if x := rePullMerge.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPut {
		n := atoi(x[1])
		p, ok := s.pulls[n]
		if !ok {
			return 0, nil
		}
		var req struct {
			SHA         string `json:"sha"`
			MergeMethod string `json:"merge_method"`
		}
		_ = json.Unmarshal(body, &req)
		switch {
		case p.MergedAt != nil || s.issue(n).State != "open":
			return http.StatusMethodNotAllowed, map[string]any{"message": "Pull Request is not mergeable"}
		case req.SHA != "" && req.SHA != p.HeadSHA:
			return http.StatusConflict, map[string]any{"message": "Head branch was modified. Review and try the merge again."}
		}
		if methods := s.mergeRules[p.BaseRef].Methods; req.MergeMethod != "" && len(methods) > 0 && !slices.Contains(methods, req.MergeMethod) {
			return http.StatusMethodNotAllowed, map[string]any{"message": map[string]string{"merge": "Merge commits", "squash": "Squash merges", "rebase": "Rebase merges"}[req.MergeMethod] + " are not allowed on this repository."}
		}
		if state, msg := s.mergeState(p); state != "clean" {
			return http.StatusMethodNotAllowed, map[string]any{"message": msg}
		}
		t := s.tick()
		i := s.issue(n)
		i.State, i.Closed, i.Updated = "closed", &t, t
		p.MergedAt = &t
		return http.StatusOK, map[string]any{"merged": true, "sha": "merge-" + p.HeadSHA, "message": "Pull Request successfully merged"}
	}
	if x := rePullUpdate.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPut {
		n := atoi(x[1])
		p, ok := s.pulls[n]
		if !ok {
			return 0, nil
		}
		var req struct {
			Expected string `json:"expected_head_sha"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Expected != "" && req.Expected != p.HeadSHA {
			return http.StatusUnprocessableEntity, map[string]any{"message": "expected head sha didn't match current head ref."}
		}
		if p.Mergeable != nil && !*p.Mergeable {
			return http.StatusUnprocessableEntity, map[string]any{"message": "merge conflict between base and head"}
		}
		p.HeadSHA = p.HeadSHA + "-updated"
		p.Behind = 0
		s.issue(n).Updated = s.tick()
		s.commits[p.HeadSHA] = s.now
		return http.StatusAccepted, map[string]any{"message": "Updating pull request branch."}
	}
	if x := reDispatch.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPost {
		if !s.workflows[x[1]] {
			return http.StatusNotFound, map[string]any{"message": "Not Found"}
		}
		var req struct {
			Ref    string         `json:"ref"`
			Inputs map[string]any `json:"inputs"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Ref == "" {
			return http.StatusUnprocessableEntity, map[string]any{"message": "Invalid request: ref is required"}
		}
		s.dispatches = append(s.dispatches, Dispatch{ID: s.id(), Workflow: x[1], Ref: req.Ref, Inputs: req.Inputs, At: s.tick()})
		return http.StatusNoContent, nil
	}
	if x := reWorkflowRuns.FindStringSubmatch(rest); x != nil && get {
		if !s.workflows[x[1]] {
			return http.StatusNotFound, map[string]any{"message": "Not Found"}
		}
		from, _ := time.Parse(time.RFC3339, strings.TrimPrefix(q.Get("created"), ">="))
		runs := []map[string]any{}
		for _, d := range s.dispatches {
			if d.Workflow == x[1] && (q.Get("branch") == "" || q.Get("branch") == d.Ref) && !d.At.Before(from) {
				runs = append(runs, map[string]any{"id": d.ID, "event": "workflow_dispatch", "head_branch": d.Ref, "created_at": ts(d.At),
					"html_url": s.URL + "/" + s.Repo + "/actions/runs/" + strconv.FormatInt(d.ID, 10)})
			}
		}
		return http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": runs}
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
			id := s.addComment(n, s.writer(token), req.Body, false, "", 0)
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
	if x := rePull.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPatch {
		n := atoi(x[1])
		if _, ok := s.pulls[n]; !ok {
			return 0, nil
		}
		if s.refusePullEdits {
			s.refusedHeader()
			return http.StatusForbidden, map[string]any{"message": "Resource not accessible by integration"}
		}
		var req struct {
			Title *string `json:"title"`
			Body  *string `json:"body"`
		}
		_ = json.Unmarshal(body, &req)
		i := s.issues[n]
		if req.Title != nil {
			i.Title = *req.Title
		}
		if req.Body != nil {
			i.Body = *req.Body
		}
		i.Updated = s.tick()
		return http.StatusOK, s.pullJSON(n)
	}
	if x := rePull.FindStringSubmatch(rest); x != nil && get {
		if _, ok := s.pulls[atoi(x[1])]; ok {
			pr := s.pullJSON(atoi(x[1]))
			pr["merged"] = s.pulls[atoi(x[1])].MergedAt != nil
			pr["mergeable"] = s.pulls[atoi(x[1])].Mergeable
			pr["mergeable_state"], _ = s.mergeState(s.pulls[atoi(x[1])])
			if s.pulls[atoi(x[1])].Mergeable == nil && pr["mergeable_state"] == "clean" {
				pr["mergeable_state"] = "unknown"
			}
			return http.StatusOK, pr
		}
		return 0, nil
	}
	if x := rePullReviews.FindStringSubmatch(rest); x != nil && r.Method == http.MethodPost {
		p, ok := s.pulls[atoi(x[1])]
		if !ok {
			return 0, nil
		}
		var req struct {
			Event    string `json:"event"`
			Body     string `json:"body"`
			Comments []struct {
				Path string `json:"path"`
				Line int    `json:"line"`
				Body string `json:"body"`
			} `json:"comments"`
		}
		_ = json.Unmarshal(body, &req)
		writer := s.writer(token)
		states := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}
		state, known := states[req.Event]
		if !known {
			return http.StatusUnprocessableEntity, map[string]any{"message": "Unprocessable Entity", "errors": []string{"Event is not included in the list"}}
		}
		if req.Event != "COMMENT" && s.issue(p.Number).User.Login == writer.Login {
			verb := map[string]string{"APPROVE": "approve", "REQUEST_CHANGES": "request changes on"}[req.Event]
			return http.StatusUnprocessableEntity, map[string]any{"message": "Unprocessable Entity", "errors": []string{"Review Can not " + verb + " your own pull request"}}
		}
		for _, c := range req.Comments {
			if c.Line <= 0 || c.Line > 1000 {
				return http.StatusUnprocessableEntity, map[string]any{"message": "Unprocessable Entity", "errors": []string{"Line could not be resolved"}}
			}
		}
		rv := Review{ID: s.id(), User: writer, Body: req.Body, State: state, Submitted: s.tick(), CommitID: p.HeadSHA}
		p.Reviews = append(p.Reviews, rv)
		for _, c := range req.Comments {
			s.addComment(p.Number, writer, c.Body, true, c.Path, c.Line)
		}
		return http.StatusOK, reviewJSON(rv, s.web(p.Number))
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
		id := s.addComment(n, s.writer(token), req.Body, true, p.Path, p.Line)
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

// listed is the issue numbers a repository listing returns, filtered by its
// state (GitHub's default is open) and ordered by its sort and direction
// (created or updated; descending by default, as GitHub does).
func (s *Server) listed(q url.Values) []int {
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var ns []int
	for _, n := range s.sortedIssues() {
		if state == "all" || s.issues[n].State == state {
			ns = append(ns, n)
		}
	}
	at := func(n int) time.Time {
		if q.Get("sort") == "updated" {
			return s.issues[n].Updated
		}
		return s.issues[n].Created
	}
	asc := q.Get("direction") == "asc"
	sort.SliceStable(ns, func(a, b int) bool {
		ta, tb := at(ns[a]), at(ns[b])
		if ta.Equal(tb) {
			return asc == (ns[a] < ns[b])
		}
		return asc == ta.Before(tb)
	})
	return ns
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

// threadRoot is the inline comment that opened the thread id is in.
func (s *Server) threadRoot(id int64) int64 {
	for range len(s.comments) {
		c := s.findComment(id)
		if c == nil || c.ReplyTo == 0 {
			return id
		}
		id = c.ReplyTo
	}
	return id
}

// graphql answers the GraphQL calls the extension makes: a PR's review
// threads, a page at a time, resolveReviewThread, and the draft mutations
// markPullRequestReadyForReview and convertPullRequestToDraft. Errors come back as
// GitHub sends them, in a 200's errors.
func (s *Server) graphql(token string, body json.RawMessage) (int, any) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if json.Unmarshal(body, &req) != nil {
		return http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"}
	}
	fail := func(kind, msg string) (int, any) {
		return http.StatusOK, map[string]any{"data": nil, "errors": []map[string]any{{"type": kind, "message": msg}}}
	}
	str := func(k string) string { v, _ := req.Variables[k].(string); return v }
	switch {
	case strings.Contains(req.Query, "resolveReviewThread"):
		root, err := strconv.ParseInt(strings.TrimPrefix(str("thread"), "PRRT_"), 10, 64)
		if c := s.findComment(root); err != nil || c == nil || !c.Inline || c.ReplyTo != 0 {
			return fail("NOT_FOUND", "Could not resolve to a node with the global id of '"+str("thread")+"'")
		}
		if s.refuseResolve {
			s.refusedHeader()
			return fail("FORBIDDEN", "Resource not accessible by integration")
		}
		s.resolved[root] = s.writer(token).Login
		return http.StatusOK, map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"id": str("thread"), "isResolved": true}}}}
	case strings.Contains(req.Query, "markPullRequestReadyForReview"), strings.Contains(req.Query, "convertPullRequestToDraft"):
		n, err := strconv.Atoi(strings.TrimPrefix(str("pr"), "PR_"))
		p, ok := s.pulls[n]
		if err != nil || !ok {
			return fail("NOT_FOUND", "Could not resolve to a node with the global id of '"+str("pr")+"'")
		}
		if s.refusePullEdits {
			s.refusedHeader()
			return fail("FORBIDDEN", "Resource not accessible by integration")
		}
		p.Draft = strings.Contains(req.Query, "convertPullRequestToDraft")
		s.issues[n].Updated = s.tick()
		key := "markPullRequestReadyForReview"
		if p.Draft {
			key = "convertPullRequestToDraft"
		}
		return http.StatusOK, map[string]any{"data": map[string]any{key: map[string]any{"pullRequest": map[string]any{"id": str("pr"), "isDraft": p.Draft}}}}
	case strings.Contains(req.Query, "reviewThreads"):
		if !strings.EqualFold(str("owner")+"/"+str("name"), s.Repo) {
			return fail("NOT_FOUND", "Could not resolve to a Repository")
		}
		n, _ := req.Variables["number"].(float64)
		if _, ok := s.pulls[int(n)]; !ok {
			return fail("NOT_FOUND", "Could not resolve to a PullRequest")
		}
		var threads []map[string]any
		for _, c := range s.comments {
			if c.Number != int(n) || !c.Inline || c.ReplyTo != 0 {
				continue
			}
			ids := []map[string]any{}
			for _, x := range s.comments {
				if x.Inline && s.threadRoot(x.ID) == c.ID {
					ids = append(ids, map[string]any{"databaseId": x.ID})
				}
			}
			_, done := s.resolved[c.ID]
			threads = append(threads, map[string]any{"id": "PRRT_" + strconv.FormatInt(c.ID, 10), "isResolved": done, "comments": map[string]any{"nodes": ids}})
		}
		from, _ := strconv.Atoi(str("after"))
		to := min(from+s.threadPage, len(threads))
		page := []map[string]any{}
		if from < len(threads) {
			page = threads[from:to]
		}
		info := map[string]any{"hasNextPage": to < len(threads), "endCursor": strconv.Itoa(to)}
		return http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"nodes": page, "pageInfo": info}}}}}
	}
	return fail("INVALID", "the fake does not model this query")
}
