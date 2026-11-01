package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// StatusError is a non-2xx API answer. Message is GitHub's short "message"
// field only (never the request, credentials or full body).
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("github: HTTP %d", e.Status)
	}
	return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Message)
}

// IsStatus reports whether err is a StatusError with that HTTP status.
func IsStatus(err error, status int) bool {
	var s *StatusError
	return errors.As(err, &s) && s.Status == status
}

// ClientOptions are test/host seams.
type ClientOptions struct {
	// HTTP supplies the base round tripper/timeout (default http.DefaultClient).
	HTTP *http.Client
	Now  func() time.Time
}

// Client is one GitHub connection. Reads use the sign-in identity; writes use
// the optional bot App so people can see who acted. All GETs are conditional
// and every call honours the shared rate-limit pause.
type Client struct {
	api, web, host string
	http           *http.Client
	base           *http.Client // ClientOptions.HTTP, which writer clients share
	tr             *transport
	now            func() time.Time
	who            atomic.Pointer[signIn] // swapped whole by Reconfigure
}

// signIn is who a client acts as: the reader, the writer (the bot App, or
// the reader again) and whether git works as the bot.
type signIn struct {
	read, write tokenSource
	gitBot      bool
	writer      string // Writer
}

// NewClient builds a client from validated connection config. No secret is
// resolved until the first call.
func NewClient(cfg ConnectionConfig, secrets SecretFunc, o ClientOptions) (*Client, error) {
	u, err := url.Parse(cfg.APIURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("github: invalid api_url")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	base := o.HTTP
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	c := &Client{api: cfg.APIURL, base: o.HTTP, now: o.Now}
	c.tr = newTransport(base.Transport, u.Host, o.Now)
	c.http = &http.Client{Transport: c.tr, Timeout: base.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if u.Host == "api.github.com" {
		c.web, c.host = "https://github.com", "github.com"
	} else {
		c.web, c.host = u.Scheme+"://"+u.Host, u.Host
	}
	who, err := c.signIn(cfg, secrets)
	if err != nil {
		return nil, err
	}
	c.who.Store(who)
	return c, nil
}

func (c *Client) signIn(cfg ConnectionConfig, secrets SecretFunc) (*signIn, error) {
	who := &signIn{gitBot: cfg.Git == GitBot}
	switch cfg.Auth {
	case "token", "":
		who.read = &staticToken{secrets: secrets, name: SecretToken}
	case "app":
		who.read = newAppToken(cfg.AppID, secrets, SecretAppPrivateKey, c)
	default:
		return nil, errors.New("github: unknown auth")
	}
	who.write, who.writer = who.read, "the connection's token"
	if cfg.Auth == "app" {
		who.writer = "App " + cfg.AppID
	}
	if cfg.BotAppID != "" {
		who.write, who.writer = newAppToken(cfg.BotAppID, secrets, SecretBotPrivateKey, c), "bot App "+cfg.BotAppID
	}
	return who, nil
}

// Writer names, from the connection config the client was last built or
// reconfigured from, who its writes go out as: "bot App <id>", "App <id>" or
// "the connection's token". Doctor compares a running entry's client with
// one built from the current config, to catch the two drifting apart.
func (c *Client) Writer() string { return c.who.Load().writer }

// ErrReconfigure is Reconfigure's refusal of a connection on another GitHub
// (api_url) than the client's: the client's host can't change in place.
var ErrReconfigure = errors.New("github: api_url changed; restart required")

// Reconfigure makes the client act as the connection's current sign-in, bot
// App and git setting, dropping every token it cached, so a changed
// connection takes effect on the next call. Calls already in flight finish as
// before. A connection on another api_url is refused with ErrReconfigure.
func (c *Client) Reconfigure(cfg ConnectionConfig, secrets SecretFunc) error {
	if !strings.EqualFold(strings.TrimRight(cfg.APIURL, "/"), strings.TrimRight(c.api, "/")) {
		return ErrReconfigure
	}
	who, err := c.signIn(cfg, secrets)
	if err != nil {
		return err
	}
	c.who.Store(who)
	return nil
}

// Host is the web host (github.com or an Enterprise host).
func (c *Client) Host() string { return c.host }

// ErrStaticToken is GitToken's refusal on a connection that signs in with
// a token rather than a GitHub App: there is nothing to mint.
var ErrStaticToken = errors.New("github: the connection signs in with a token, not an App")

// GitToken mints a token git can push to repo with: a GitHub App
// installation token limited to that one repository and contents: write,
// which expires within the hour. It comes from the bot App when there is
// one (writes are the bot's), else the sign-in App. The caller owns it and
// revokes it with RevokeToken when it is done.
func (c *Client) GitToken(ctx context.Context, repo string) (string, time.Time, error) {
	who := c.who.Load()
	app, ok := who.write.(*appToken)
	if !ok {
		if app, ok = who.read.(*appToken); !ok {
			return "", time.Time{}, ErrStaticToken
		}
	}
	return app.scoped(ctx, repo)
}

// GitIdentity is who git commits as for a GitHub App: its bot user's login
// and the noreply address GitHub links to that user, so the commits show
// the bot's name and avatar.
type GitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// GitIdentity is the identity of the App GitToken mints from: the bot App
// when there is one, else the sign-in App. The App's slug comes from GET
// /app with its JWT and its bot user's id from GET /users/<slug>[bot]; both
// are looked up once.
func (c *Client) GitIdentity(ctx context.Context, repo string) (GitIdentity, error) {
	who := c.who.Load()
	app, ok := who.write.(*appToken)
	if !ok {
		if app, ok = who.read.(*appToken); !ok {
			return GitIdentity{}, ErrStaticToken
		}
	}
	return app.identity(ctx, repo)
}

// GitBot reports whether git works as the App's bot in the runs of this
// connection's repositories (connection git "bot").
func (c *Client) GitBot() bool { return c.who.Load().gitBot }

// RevokeToken revokes an installation token GitToken minted, signed in as
// the token itself. A token GitHub no longer knows is already revoked.
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	_, err := c.raw(ctx, "Bearer "+token, http.MethodDelete, "/installation/token", nil, nil)
	if IsStatus(err, http.StatusUnauthorized) {
		return nil
	}
	return err
}

// hasBot reports whether writes go out as a bot App rather than the reader.
func (c *Client) hasBot() bool {
	who := c.who.Load()
	return who.write != who.read
}

// PausedUntil reports an active rate-limit pause.
func (c *Client) PausedUntil() (time.Time, bool) { return c.tr.PausedUntil() }

// call makes one API call as the reader (or writer) of repo, signing in again
// once after a 401.
func (c *Client) call(ctx context.Context, write bool, repo, method, path string, body, out any) (*http.Response, error) {
	who := c.who.Load()
	src := who.read
	if write {
		src = who.write
	}
	var res *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var tok string
		if tok, err = src.token(ctx, repo); err != nil {
			return nil, err
		}
		res, err = c.raw(ctx, "Bearer "+tok, method, path, body, out)
		if attempt == 0 && IsStatus(err, http.StatusUnauthorized) {
			src.invalidate(repo)
			continue
		}
		break
	}
	return res, err
}

// raw performs one request with an explicit Authorization header value.
func (c *Client) raw(ctx context.Context, authorization, method, path string, body, out any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	target := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		target = c.api + path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "open-conduit")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		var limit *RateLimitError
		if errors.As(err, &limit) {
			return nil, limit
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("github: %s %s: transport failure", method, redactPath(path))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return res, fmt.Errorf("github: reading response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var e struct {
			Message string            `json:"message"`
			Errors  []json.RawMessage `json:"errors"`
		}
		_ = json.Unmarshal(data, &e)
		// A 422 says why in errors, as strings ("Can not approve your own
		// pull request") or objects with a message.
		parts := []string{e.Message}
		for _, raw := range e.Errors {
			var text string
			var obj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &text) != nil && json.Unmarshal(raw, &obj) == nil {
				text = obj.Message
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
		msg := strings.Join(strings.Fields(strings.Join(parts, ": ")), " ")
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return res, &StatusError{Status: res.StatusCode, Message: msg}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return res, fmt.Errorf("github: decoding %s: %w", redactPath(path), err)
		}
	}
	return res, nil
}

func redactPath(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// page reads one page of a list endpoint and reports whether another follows.
func page[T any](ctx context.Context, c *Client, repo, path string, n, perPage int) ([]T, bool, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var items []T
	res, err := c.call(ctx, false, repo, http.MethodGet, fmt.Sprintf("%s%sper_page=%d&page=%d", path, sep, perPage, n), nil, &items)
	if err != nil {
		return nil, false, err
	}
	more := len(items) >= perPage
	if res != nil {
		if link := res.Header.Get("Link"); link != "" {
			more = nextLink.MatchString(link)
		}
	}
	return items, more, nil
}

// all reads every page (bounded) of a list endpoint. An unfinished listing is
// an error, never a short successful answer.
func all[T any](ctx context.Context, c *Client, repo, path string) ([]T, error) {
	var out []T
	for n := 1; n <= 100; n++ {
		items, more, err := page[T](ctx, c, repo, path, n, 100)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if !more {
			return out, nil
		}
	}
	return nil, errors.New("github: listing exceeds the page bound")
}

// ---- API shapes -----------------------------------------------------------

type rawUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type rawLabel struct {
	Name string `json:"name"`
}

type rawIssue struct {
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	HTMLURL     string           `json:"html_url"`
	User        rawUser          `json:"user"`
	Labels      []rawLabel       `json:"labels"`
	State       string           `json:"state"`
	StateReason string           `json:"state_reason"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	ClosedAt    *time.Time       `json:"closed_at"`
	PullRequest *json.RawMessage `json:"pull_request"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Assignees []rawUser `json:"assignees"`
	Type      *struct {
		Name string `json:"name"`
	} `json:"type"`
}

type rawComment struct {
	ID             int64      `json:"id"`
	User           rawUser    `json:"user"`
	Body           string     `json:"body"`
	HTMLURL        string     `json:"html_url"`
	IssueURL       string     `json:"issue_url"`
	PullRequestURL string     `json:"pull_request_url"`
	CreatedAt      *time.Time `json:"created_at"`
	UpdatedAt      *time.Time `json:"updated_at"`
	SubmittedAt    *time.Time `json:"submitted_at"`
	Path           string     `json:"path"`
	Line           *int       `json:"line"`
	OriginalLine   *int       `json:"original_line"`
	InReplyTo      int64      `json:"in_reply_to_id"`
	State          string     `json:"state"`
	CommitID       string     `json:"commit_id"`
}

type rawPull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	User      rawUser    `json:"user"`
	Labels    []rawLabel `json:"labels"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	// Mergeable is only on a single PR's read (and webhook bodies), never
	// on the list; nil while GitHub is still working it out.
	Mergeable *bool `json:"mergeable"`
}

type rawCheckRun struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	HTMLURL     string     `json:"html_url"`
	DetailsURL  string     `json:"details_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type rawStatus struct {
	ID        int64      `json:"id"`
	Context   string     `json:"context"`
	State     string     `json:"state"`
	TargetURL string     `json:"target_url"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
}

type rawIssueEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- reads ----------------------------------------------------------------

func repoPath(repo, format string, args ...any) string {
	return "/repos/" + repo + fmt.Sprintf(format, args...)
}

// Issue reads one issue (or the issue face of a PR).
func (c *Client) Issue(ctx context.Context, repo string, n int) (rawIssue, error) {
	var i rawIssue
	_, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/issues/%d", n), nil, &i)
	return i, err
}

// Pull reads one pull request.
func (c *Client) Pull(ctx context.Context, repo string, n int) (rawPull, error) {
	var p rawPull
	_, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/pulls/%d", n), nil, &p)
	return p, err
}

// reopenings lists an issue's reopened events in time order.
func (c *Client) reopenings(ctx context.Context, repo string, n int) ([]rawIssueEvent, error) {
	events, err := all[rawIssueEvent](ctx, c, repo, repoPath(repo, "/issues/%d/events", n))
	if err != nil {
		return nil, err
	}
	var out []rawIssueEvent
	for _, e := range events {
		if e.Event == "reopened" {
			out = append(out, e)
		}
	}
	return out, nil
}

// Reviews lists a PR's reviews.
func (c *Client) Reviews(ctx context.Context, repo string, n int) ([]rawComment, error) {
	return all[rawComment](ctx, c, repo, repoPath(repo, "/pulls/%d/reviews", n))
}

// checkState reads a commit's check runs and combined statuses. A
// fine-grained personal access token cannot be granted the Checks permission,
// so GitHub refuses check runs with an ordinary 403. Then the commit's GitHub
// Actions workflow runs (Actions: read) stand in for its check runs; when
// those are refused as well, the commit's runs could not be read at all, and
// only its statuses are known. from says which happened (RunsFrom*,
// RunsUnreadable). A rate limit is never treated as a refusal.
func (c *Client) checkState(ctx context.Context, repo, sha string) (runs []rawCheckRun, statuses []rawStatus, from string, err error) {
	from = RunsFromCheckRuns
	if runs, err = c.checkRuns(ctx, repo, sha); IsStatus(err, http.StatusForbidden) {
		from = RunsFromWorkflows
		runs, err = c.workflowRuns(ctx, repo, sha)
		if IsStatus(err, http.StatusForbidden) || IsStatus(err, http.StatusNotFound) {
			runs, err, from = nil, nil, RunsUnreadable
		}
	}
	if err != nil {
		return nil, nil, "", err
	}
	var status struct {
		Statuses []rawStatus `json:"statuses"`
	}
	if _, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/commits/%s/status", url.PathEscape(sha)), nil, &status); err != nil {
		return nil, nil, "", err
	}
	return runs, status.Statuses, from, nil
}

func (c *Client) checkRuns(ctx context.Context, repo, sha string) ([]rawCheckRun, error) {
	var runs []rawCheckRun
	for n := 1; n <= 20; n++ {
		var pageRuns struct {
			TotalCount int           `json:"total_count"`
			CheckRuns  []rawCheckRun `json:"check_runs"`
		}
		if _, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/commits/%s/check-runs?per_page=100&page=%d", url.PathEscape(sha), n), nil, &pageRuns); err != nil {
			return nil, err
		}
		runs = append(runs, pageRuns.CheckRuns...)
		if len(pageRuns.CheckRuns) < 100 || len(runs) >= pageRuns.TotalCount {
			break
		}
		if n == 20 {
			return nil, errors.New("github: check runs exceed the page bound")
		}
	}
	return runs, nil
}

// workflowRuns reads the GitHub Actions workflow runs for a head commit as
// check runs (name, status, conclusion and link), the fallback when check
// runs are refused.
func (c *Client) workflowRuns(ctx context.Context, repo, sha string) ([]rawCheckRun, error) {
	var runs []rawCheckRun
	for n := 1; n <= 20; n++ {
		var page struct {
			TotalCount int `json:"total_count"`
			Runs       []struct {
				ID         int64      `json:"id"`
				Name       string     `json:"name"`
				Status     string     `json:"status"`
				Conclusion string     `json:"conclusion"`
				HTMLURL    string     `json:"html_url"`
				StartedAt  *time.Time `json:"run_started_at"`
				UpdatedAt  *time.Time `json:"updated_at"`
			} `json:"workflow_runs"`
		}
		if _, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/actions/runs?head_sha=%s&per_page=100&page=%d", url.QueryEscape(sha), n), nil, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Runs {
			run := rawCheckRun{ID: r.ID, Name: r.Name, Status: r.Status, Conclusion: r.Conclusion, HTMLURL: r.HTMLURL, StartedAt: r.StartedAt}
			if r.Status == "completed" {
				run.CompletedAt = r.UpdatedAt
			}
			runs = append(runs, run)
		}
		if len(page.Runs) < 100 || len(runs) >= page.TotalCount {
			break
		}
		if n == 20 {
			return nil, errors.New("github: workflow runs exceed the page bound")
		}
	}
	return runs, nil
}

// pullsForCommit lists the PRs whose head (or history) includes sha.
func (c *Client) pullsForCommit(ctx context.Context, repo, sha string) ([]rawPull, error) {
	return all[rawPull](ctx, c, repo, repoPath(repo, "/commits/%s/pulls", url.PathEscape(sha)))
}

// IssueComments lists an issue's (or PR conversation's) comments.
func (c *Client) IssueComments(ctx context.Context, repo string, n int, since time.Time) ([]rawComment, error) {
	path := repoPath(repo, "/issues/%d/comments", n)
	if !since.IsZero() {
		path += "?since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	return all[rawComment](ctx, c, repo, path)
}

// Reactions reads the reaction counts on an issue comment.
func (c *Client) Reactions(ctx context.Context, repo string, commentID int64) (map[string]int, error) {
	var out struct {
		Reactions map[string]any `json:"reactions"`
	}
	if _, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/issues/comments/%d", commentID), nil, &out); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for k, v := range out.Reactions {
		if n, ok := v.(float64); ok && k != "total_count" && k != "url" {
			counts[k] = int(n)
		}
	}
	return counts, nil
}

// ---- writes ---------------------------------------------------------------

// EditLabels adds and removes issue labels as the writer. Removing a label the
// issue does not carry is not an error; both operations are idempotent.
func (c *Client) EditLabels(ctx context.Context, repo string, n int, add, remove []string) error {
	if len(add) > 0 {
		if _, err := c.call(ctx, true, repo, http.MethodPost, repoPath(repo, "/issues/%d/labels", n), map[string][]string{"labels": add}, nil); err != nil {
			return err
		}
	}
	for _, l := range remove {
		_, err := c.call(ctx, true, repo, http.MethodDelete, repoPath(repo, "/issues/%d/labels/%s", n, url.PathEscape(l)), nil, nil)
		if err != nil && !IsStatus(err, http.StatusNotFound) {
			return err
		}
	}
	return nil
}

// PostedComment identifies a comment created by Comment.
type PostedComment struct {
	ID  int64  `json:"id"`
	URL string `json:"html_url"`
}

// Comment posts an issue/PR conversation comment as the writer. Callers must
// include a discoverable marker: a lost response cannot be retried blindly.
func (c *Client) Comment(ctx context.Context, repo string, n int, body string) (PostedComment, error) {
	var out PostedComment
	_, err := c.call(ctx, true, repo, http.MethodPost, repoPath(repo, "/issues/%d/comments", n), map[string]string{"body": body}, &out)
	return out, err
}

// React acknowledges a comment with a reaction. GitHub returns the existing
// reaction for a repeat, so retries are idempotent and need no marker. Kinds:
// issue/pull_request conversation comments and inline review comments.
func (c *Client) React(ctx context.Context, repo, kind string, commentID int64, content string) error {
	var path string
	switch kind {
	case KindIssue, KindPullRequest:
		path = repoPath(repo, "/issues/comments/%d/reactions", commentID)
	case KindReviewComment:
		path = repoPath(repo, "/pulls/comments/%d/reactions", commentID)
	default:
		return nil // reviews have no reactions endpoint
	}
	_, err := c.call(ctx, true, repo, http.MethodPost, path, map[string]string{"content": content}, nil)
	return err
}

func parseNumber(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}
