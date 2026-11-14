package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// The toolbelt's git transport (extension.GitTransport): git in a run
// reaches a repository through the host, which signs each request in with
// the credential an operation serving extension.CapabilityGitCredential
// answers, so the credential never enters the run. `os git-remote` opens a
// session for the repository (POST /toolbelt/git), which is where the
// operation, and any gate on it, is asked; then it relays git's smart HTTP
// requests through the session (GET for the advertisement, POST for an
// exchange, /toolbelt/git/{session}/{service}) and ends it (DELETE). The
// host forwards only git's own two services, only to the repository the
// session was opened for, and nowhere without a credential: it is not a
// proxy.

const (
	// gitSessionLife bounds a session when the credential gives no expiry
	// (and when it gives a later one). A session serves one git command.
	gitSessionLife = time.Hour
	// maxGitSessions bounds the sessions the host keeps at once, and
	// maxExecutionGitSessions those of one execution: a git command and its
	// submodules need a few, and a run that opens more evicts its own
	// oldest rather than filling the host's table for its neighbours.
	maxGitSessions          = 1024
	maxExecutionGitSessions = 8
	// gitUserAgent is the agent the host's requests carry: git's hosts
	// serve their smart protocol to git.
	gitUserAgent = "git/open-conduit"
)

// GitCredentials is what the git transport asks for an execution's
// credential through: the toolbelt (runtime.Toolbelt).
type GitCredentials interface {
	Serving(ctx context.Context, token string, execution domain.ID, capability string) ([]string, error)
	CallRequest(ctx context.Context, token, requestID string, c extension.ToolCall) (extension.ToolResult, error)
}

// Handler serves the transport's routes (/v1/toolbelt/git...) for one
// execution's socket over belt, as the management server's execution
// handler does, for a host that serves the toolbelt some other way.
func (g *GitTransport) Handler(execution domain.ID, belt GitCredentials) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := func() error {
			parts, ok := split(r.URL.EscapedPath())
			if !ok || !toolbeltPath(parts) || parts[1] != "git" {
				return fail(404, "not_found", "unknown endpoint")
			}
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				return fail(401, "unauthenticated", "missing or invalid bearer token")
			}
			c := &call{ctx: context.WithValue(r.Context(), executionKey{}, execution), r: r, w: w, who: principal{token: token}, params: parts[2:]}
			var status int
			var data any
			var err error
			switch {
			case len(parts) == 2 && r.Method == http.MethodPost:
				if c.body, err = io.ReadAll(io.LimitReader(r.Body, maxBody)); err != nil {
					return fail(400, "malformed", "request body unreadable")
				}
				status, data, err = g.openSession(c, belt)
			case len(parts) == 3 && r.Method == http.MethodDelete:
				status, data, err = g.endSession(c)
			case len(parts) == 4 && (r.Method == http.MethodGet || r.Method == http.MethodPost):
				status, data, err = g.relay(c, belt)
			default:
				return fail(405, "method_not_allowed", "method not allowed")
			}
			if err != nil {
				return err
			}
			if !c.written {
				writeJSON(w, status, map[string]any{"data": data})
			}
			return nil
		}()
		if err != nil {
			api := mapError(err)
			writeJSON(w, api.status, map[string]any{"error": map[string]any{"code": api.code, "message": api.message}})
		}
	})
}

// GitTransport is the host's side of the toolbelt's git transport: the open
// sessions and the client that reaches the repositories. The daemon keeps
// one across config generations, so a reload doesn't cut a push short.
type GitTransport struct {
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	sessions map[string]*gitSession
}

// gitSession is one git command's way to its repository.
type gitSession struct {
	execution          domain.ID
	repo               *url.URL
	username, password string
	opened, expires    time.Time
}

// NewGitTransport makes the git transport. rt reaches the repositories
// (http.DefaultTransport when nil); redirects are never followed, so a
// request and its credential go only to the repository's own host.
func NewGitTransport(rt http.RoundTripper, now func() time.Time) *GitTransport {
	if rt == nil {
		rt = http.DefaultTransport
	}
	if now == nil {
		now = time.Now
	}
	return &GitTransport{now: now, sessions: map[string]*gitSession{},
		client: &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// gitServices are the services the transport carries: git's fetch and push.
var gitServices = map[string]bool{"git-upload-pack": true, "git-receive-pack": true}

// gitRepository reads the repository URL a session is opened for: https,
// a host, a path, and nothing else (no user, query or fragment).
func gitRepository(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, fail(400, "bad_git_url", "the git transport takes an https repository URL with no user, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	if strings.Trim(u.Path, "/") == "" || strings.Contains(u.Path, "/../") || strings.HasSuffix(u.Path, "/..") || strings.Contains(u.Path, "/./") {
		return nil, fail(400, "bad_git_url", "the git transport takes a repository URL with a path")
	}
	return u, nil
}

// gitSessionOpen opens a session for git's command on a repository: each
// operation the execution is granted that serves git.credential is asked
// in turn, as the host's own call, until one gives a credential. A gate
// that holds the sign-in parks the run and the request is refused with
// what it said. A repository none of them gives a credential for is
// refused with git_no_credential, so the client reaches it directly.
func (s *Server) gitSessionOpen(c *call) (int, any, error) {
	return s.o.Git.openSession(c, s.o.Toolbelt)
}

func (g *GitTransport) openSession(c *call, belt GitCredentials) (int, any, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	repo, err := gitRepository(in.URL)
	if err != nil {
		return 0, nil, err
	}
	exec := scopedExecution(c.ctx)
	ops, err := belt.Serving(c.ctx, c.who.token, exec, extension.CapabilityGitCredential)
	if err != nil {
		return 0, nil, err
	}
	data, _ := json.Marshal(map[string]string{"protocol": "https", "host": repo.Host, "path": strings.TrimPrefix(repo.Path, "/")})
	var refused error
	for _, op := range ops {
		result, err := belt.CallRequest(c.ctx, c.who.token, "", extension.ToolCall{Operation: op, ExecutionID: exec, Capability: extension.CapabilityGitCredential, Input: domain.Payload{Data: data}})
		if err != nil {
			// Another operation may still own the repository; report the
			// refusal only if none does.
			refused = err
			continue
		}
		if result.Wait != nil {
			// A gate parked the run on the sign-in (a person's approval):
			// git gets nothing, and the agent is told what the gate said.
			var out struct{ Next string }
			if len(result.Output.Data) > 0 {
				_ = json.Unmarshal(result.Output.Data, &out)
			}
			why := out.Next
			if why == "" {
				why = "it waits for " + result.Wait.Description + "; end your turn now, and once resumed run the same git command again"
			}
			return 0, nil, fail(409, "git_sign_in_held", why)
		}
		var cred struct {
			Username, Password string
			ExpiresAt          string `json:"expires_at"`
		}
		if len(result.Output.Data) > 0 {
			_ = json.Unmarshal(result.Output.Data, &cred)
		}
		if cred.Password == "" {
			continue
		}
		id, expires, err := g.open(gitSession{execution: exec, repo: repo, username: cred.Username, password: cred.Password}, cred.ExpiresAt)
		if err != nil {
			return 0, nil, err
		}
		return 200, map[string]any{"session": id, "expires_at": ts(expires)}, nil
	}
	if refused != nil {
		return 0, nil, refused
	}
	return 0, nil, fail(403, "git_no_credential", "no operation this run is granted gives git a credential for "+repo.Host+repo.Path)
}

// open keeps a session, until the credential's expiry (RFC 3339, "" for
// none) or gitSessionLife, whichever comes first.
func (g *GitTransport) open(sess gitSession, expiresAt string) (string, time.Time, error) {
	now := g.now()
	sess.opened, sess.expires = now, now.Add(gitSessionLife)
	// An expiry already past (a clock that differs from the host's) says
	// nothing the repository won't: it refuses a token that has expired.
	if t, err := time.Parse(time.RFC3339, expiresAt); err == nil && t.After(now) && t.Before(sess.expires) {
		sess.expires = t
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	id := "gs_" + hex.EncodeToString(b)
	g.mu.Lock()
	defer g.mu.Unlock()
	oldest, held := "", 0
	for k, v := range g.sessions {
		if !now.Before(v.expires) {
			delete(g.sessions, k)
			continue
		}
		if v.execution == sess.execution {
			held++
			if oldest == "" || v.opened.Before(g.sessions[oldest].opened) {
				oldest = k
			}
		}
	}
	if held >= maxExecutionGitSessions {
		delete(g.sessions, oldest)
	}
	if len(g.sessions) >= maxGitSessions {
		return "", time.Time{}, fail(503, "git_sessions_exhausted", "the host has too many git commands open; run the git command again shortly")
	}
	g.sessions[id] = &sess
	return id, sess.expires, nil
}

// Reconcile drops the sessions of every execution live doesn't report
// running, so an ended run's sessions don't wait out their expiry.
func (g *GitTransport) Reconcile(live func([]domain.ID) (map[domain.ID]bool, error)) error {
	g.mu.Lock()
	seen := map[domain.ID]bool{}
	var ids []domain.ID
	for _, v := range g.sessions {
		if !seen[v.execution] {
			seen[v.execution] = true
			ids = append(ids, v.execution)
		}
	}
	g.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	running, err := live(ids)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range g.sessions {
		if !running[v.execution] {
			delete(g.sessions, k)
		}
	}
	return nil
}

// session is the execution's open session id.
func (g *GitTransport) session(id string, exec domain.ID) (gitSession, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	sess, ok := g.sessions[id]
	if !ok || sess.execution != exec {
		return gitSession{}, false
	}
	if !g.now().Before(sess.expires) {
		delete(g.sessions, id)
		return gitSession{}, false
	}
	return *sess, true
}

// gitSessionEnd ends a session; ending one that is gone succeeds.
func (s *Server) gitSessionEnd(c *call) (int, any, error) { return s.o.Git.endSession(c) }

func (g *GitTransport) endSession(c *call) (int, any, error) {
	exec := scopedExecution(c.ctx)
	g.mu.Lock()
	if sess, ok := g.sessions[c.params[0]]; ok && sess.execution == exec {
		delete(g.sessions, c.params[0])
	}
	g.mu.Unlock()
	return 200, map[string]any{}, nil
}

// gitRelay forwards one of git's requests through a session to its
// repository, signed in, and streams the answer back: GET is the service's
// advertisement (info/refs), POST an exchange with the request's body. The
// execution's token must still be live and granted an operation serving
// git.credential: a run whose authority ended or narrowed reaches nothing.
func (s *Server) gitRelay(c *call) (int, any, error) { return s.o.Git.relay(c, s.o.Toolbelt) }

func (g *GitTransport) relay(c *call, belt GitCredentials) (int, any, error) {
	exec := scopedExecution(c.ctx)
	sess, ok := g.session(c.params[0], exec)
	if !ok {
		return 0, nil, fail(404, "git_session_unknown", "the git session is unknown or has ended; run the git command again")
	}
	service := c.params[1]
	if !gitServices[service] {
		return 0, nil, fail(404, "not_found", "the git transport carries only git-upload-pack and git-receive-pack")
	}
	ops, err := belt.Serving(c.ctx, c.who.token, exec, extension.CapabilityGitCredential)
	if err != nil {
		return 0, nil, err
	}
	if len(ops) == 0 {
		return 0, nil, fail(403, "forbidden", "execution authority rejected")
	}
	target := *sess.repo
	var body io.Reader
	if c.r.Method == http.MethodGet {
		target.Path += "/info/refs"
		target.RawQuery = url.Values{"service": {service}}.Encode()
	} else {
		target.Path += "/" + service
		body = c.r.Body
	}
	req, err := http.NewRequestWithContext(c.ctx, c.r.Method, target.String(), body)
	if err != nil {
		return 0, nil, err
	}
	if body != nil && c.r.ContentLength > 0 {
		req.ContentLength = c.r.ContentLength
	}
	req.SetBasicAuth(sess.username, sess.password)
	req.Header.Set("User-Agent", gitUserAgent)
	if p := c.r.Header.Get("Git-Protocol"); p != "" && gitProtocolValue(p) {
		req.Header.Set("Git-Protocol", p)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-"+service+"-request")
		req.Header.Set("Accept", "application/x-"+service+"-result")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		if c.ctx.Err() != nil {
			return 0, nil, c.ctx.Err()
		}
		return 0, nil, fail(502, "git_unreachable", fmt.Sprintf("the host could not reach %s: %v", sess.repo.Host, errors.Unwrap(err)))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fail(502, "git_upstream", fmt.Sprintf("%s answered %s for %s", sess.repo.Host, resp.Status, strings.TrimPrefix(sess.repo.Path, "/")))
	}
	c.written = true
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.w.Header().Set("Content-Type", ct)
	}
	c.w.Header().Set("Cache-Control", "no-store")
	c.w.WriteHeader(http.StatusOK)
	flush := func() {}
	if f, ok := c.w.(http.Flusher); ok {
		flush = f.Flush
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := c.w.Write(buf[:n]); werr != nil {
				return 0, nil, nil
			}
			flush()
		}
		if err != nil {
			return 0, nil, nil
		}
	}
}

// gitProtocolValue reports whether a Git-Protocol header is git's own
// shape: key=value pairs of printable characters, separated by colons.
func gitProtocolValue(v string) bool {
	if len(v) > 256 {
		return false
	}
	for _, r := range v {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
