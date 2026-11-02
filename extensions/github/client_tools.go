package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Client calls used by the agent-facing engineering tools (03-04). Writes
// that are not naturally idempotent are made idempotent by their callers:
// comments through Projector intents and markers, attachments through a
// deterministic path, PRs through GitHub's one-open-PR-per-head rule.

// AttachBranch holds files agents attach to comments: an orphan branch, so
// it shares no history with the base branch and runs none of its workflows.
const AttachBranch = "open-conduit-attachments"

// MaxAttachment keeps an upload well under what the contents API accepts.
const MaxAttachment = 50 << 20

// MaxFetch bounds an authenticated attachment download.
const MaxFetch = 50 << 20

// DefaultBranch is the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.call(ctx, false, repo, http.MethodGet, "/repos/"+repo, nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

// OpenIssues lists a repository's open issues (not PRs), oldest first.
func (c *Client) OpenIssues(ctx context.Context, repo string) ([]rawIssue, error) {
	items, err := all[rawIssue](ctx, c, repo, repoPath(repo, "/issues?state=open&sort=created&direction=asc"))
	if err != nil {
		return nil, err
	}
	var out []rawIssue
	for _, i := range items {
		if i.PullRequest == nil {
			out = append(out, i)
		}
	}
	return out, nil
}

// RecentIssues lists issues created since a time (for marker recovery).
func (c *Client) RecentIssues(ctx context.Context, repo string, since time.Time) ([]rawIssue, error) {
	return all[rawIssue](ctx, c, repo, repoPath(repo, "/issues?state=all&sort=created&direction=desc&since=%s", url.QueryEscape(since.UTC().Format(time.RFC3339))))
}

// CreateIssue opens an issue as the writer. Callers include a marker: a lost
// response cannot be retried blindly.
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (rawIssue, error) {
	req := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		req["labels"] = labels
	}
	var out rawIssue
	_, err := c.call(ctx, true, repo, http.MethodPost, repoPath(repo, "/issues"), req, &out)
	return out, err
}

// PullsForHead lists every PR (open or not) from owner:branch.
func (c *Client) PullsForHead(ctx context.Context, repo, branch string) ([]rawPull, error) {
	owner, _, _ := strings.Cut(repo, "/")
	return all[rawPull](ctx, c, repo, repoPath(repo, "/pulls?state=all&head=%s", url.QueryEscape(owner+":"+branch)))
}

// CreatePull opens a PR as the writer.
func (c *Client) CreatePull(ctx context.Context, repo, title, body, head, base string, draft bool) (rawPull, error) {
	var out rawPull
	_, err := c.call(ctx, true, repo, http.MethodPost, repoPath(repo, "/pulls"), map[string]any{"title": title, "body": body, "head": head, "base": base, "draft": draft}, &out)
	return out, err
}

// PullComments lists a PR's inline review comments.
func (c *Client) PullComments(ctx context.Context, repo string, n int) ([]rawComment, error) {
	return all[rawComment](ctx, c, repo, repoPath(repo, "/pulls/%d/comments", n))
}

// ReplyReview replies in an inline review thread as the writer.
func (c *Client) ReplyReview(ctx context.Context, repo string, n int, commentID int64, body string) (PostedComment, error) {
	var out PostedComment
	_, err := c.call(ctx, true, repo, http.MethodPost, repoPath(repo, "/pulls/%d/comments/%d/replies", n, commentID), map[string]string{"body": body}, &out)
	return out, err
}

// graphqlURL is the GraphQL endpoint of the client's GitHub:
// api.github.com/graphql, or <host>/api/graphql on Enterprise, whose REST
// API is <host>/api/v3.
func (c *Client) graphqlURL() string {
	api := strings.TrimRight(c.api, "/")
	if base, ok := strings.CutSuffix(api, "/api/v3"); ok {
		return base + "/api/graphql"
	}
	return api + "/graphql"
}

// graphql runs one GraphQL query or mutation as the reader (or writer) of
// repo and decodes its data into out. GitHub answers GraphQL errors with a
// 200; the first is returned as a StatusError (FORBIDDEN as 403, NOT_FOUND
// as 404, anything else as 422) so callers map it like a REST refusal.
func (c *Client) graphql(ctx context.Context, write bool, repo, query string, vars map[string]any, out any) error {
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := c.call(ctx, write, repo, http.MethodPost, c.graphqlURL(), map[string]any{"query": query, "variables": vars}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		e := res.Errors[0]
		status := http.StatusUnprocessableEntity
		switch e.Type {
		case "FORBIDDEN":
			status = http.StatusForbidden
		case "NOT_FOUND":
			status = http.StatusNotFound
		}
		msg := strings.Join(strings.Fields(e.Message), " ")
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return &StatusError{Status: status, Message: msg}
	}
	if out != nil && len(res.Data) > 0 {
		if err := json.Unmarshal(res.Data, out); err != nil {
			return fmt.Errorf("github: decoding GraphQL data: %w", err)
		}
	}
	return nil
}

// ReviewThread is the review thread an inline comment opened or replied in.
type ReviewThread struct {
	// ID is the thread's GraphQL node ID, which only resolving needs.
	ID       string
	Resolved bool
}

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes { id isResolved comments(first: 100) { nodes { databaseId } } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

// FindReviewThread finds the review thread on PR n that holds the inline
// comment commentID (the one that opened it or any reply), reading the PR's
// threads a page at a time. ok is false when no thread holds it: the ID is
// not an inline comment on that PR, or sits past a thread's first hundred
// comments.
func (c *Client) FindReviewThread(ctx context.Context, repo string, n int, commentID int64) (thread ReviewThread, ok bool, err error) {
	owner, name, _ := strings.Cut(repo, "/")
	vars := map[string]any{"owner": owner, "name": name, "number": n}
	for {
		var out struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, false, repo, reviewThreadsQuery, vars, &out); err != nil {
			return ReviewThread{}, false, err
		}
		if out.Repository == nil || out.Repository.PullRequest == nil {
			return ReviewThread{}, false, &StatusError{Status: http.StatusNotFound}
		}
		threads := out.Repository.PullRequest.ReviewThreads
		for _, t := range threads.Nodes {
			for _, x := range t.Comments.Nodes {
				if x.DatabaseID == commentID {
					return ReviewThread{ID: t.ID, Resolved: t.IsResolved}, true, nil
				}
			}
		}
		if !threads.PageInfo.HasNextPage || threads.PageInfo.EndCursor == "" {
			return ReviewThread{}, false, nil
		}
		vars["after"] = threads.PageInfo.EndCursor
	}
}

// ResolveReviewThread resolves a review thread as the writer. Resolving a
// resolved thread changes nothing, so a retry is safe.
func (c *Client) ResolveReviewThread(ctx context.Context, repo, threadID string) error {
	return c.graphql(ctx, true, repo, `mutation($thread: ID!) { resolveReviewThread(input: {threadId: $thread}) { thread { id isResolved } } }`,
		map[string]any{"thread": threadID}, nil)
}

// ReviewRequest is a pull request review to submit.
type ReviewRequest struct {
	Event    string         `json:"event"`
	Body     string         `json:"body"`
	Comments []ReviewInline `json:"comments,omitempty"`
}

// CreateReview submits a review, with its inline comments, as the writer.
// Callers must mark the body: a lost response cannot be retried blindly.
func (c *Client) CreateReview(ctx context.Context, repo string, n int, r ReviewRequest) (PostedComment, error) {
	var out PostedComment
	_, err := c.call(ctx, true, repo, http.MethodPost, repoPath(repo, "/pulls/%d/reviews", n), r, &out)
	return out, err
}

// CheckState reads a commit's check runs (or, when those are refused, its
// Actions workflow runs) and statuses; from is RunsFromCheckRuns,
// RunsFromWorkflows or RunsUnreadable (neither kind of run could be read).
func (c *Client) CheckState(ctx context.Context, repo, sha string) ([]rawCheckRun, []rawStatus, string, error) {
	return c.checkState(ctx, repo, sha)
}

// CommitTime is a commit's committer date.
func (c *Client) CommitTime(ctx context.Context, repo, sha string) (time.Time, error) {
	var out struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	_, err := c.call(ctx, false, repo, http.MethodGet, repoPath(repo, "/commits/%s", url.PathEscape(sha)), nil, &out)
	return out.Commit.Committer.Date.UTC(), err
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SafeName is an uploaded file's name with anything unsafe replaced.
func SafeName(name string) string {
	name = strings.Trim(unsafeName.ReplaceAllString(path.Base(name), "-"), "-")
	if name == "" || name == "." {
		return "file"
	}
	return name
}

// Attach commits data to the attachments branch at file (a path the caller
// makes deterministic for the request) and links to it. A ?raw=true link on
// the web host serves the file itself, so an image shows inline even in a
// private repository. A repeated upload to the same path keeps the first.
func (c *Client) Attach(ctx context.Context, repo, file, message string, data []byte) (string, error) {
	if len(data) > MaxAttachment {
		return "", fmt.Errorf("github: attachment exceeds %d MB", MaxAttachment>>20)
	}
	err := c.attach(ctx, true, repo, file, message, data)
	if c.hasBot() && (IsStatus(err, http.StatusForbidden) || IsStatus(err, http.StatusNotFound)) {
		err = c.attach(ctx, false, repo, file, message, data)
	}
	if err != nil {
		return "", err
	}
	segs := strings.Split(file, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/blob/%s/%s?raw=true", c.web, repo, AttachBranch, strings.Join(segs, "/")), nil
}

func (c *Client) attach(ctx context.Context, write bool, repo, file, message string, data []byte) error {
	if err := c.ensureAttachBranch(ctx, write, repo); err != nil {
		return err
	}
	req := map[string]string{"message": message, "content": base64.StdEncoding.EncodeToString(data), "branch": AttachBranch}
	segs := strings.Split(file, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	_, err := c.call(ctx, write, repo, http.MethodPut, repoPath(repo, "/contents/%s", strings.Join(segs, "/")), req, nil)
	if IsStatus(err, http.StatusUnprocessableEntity) {
		return nil // the path exists: an earlier attempt of this request uploaded it
	}
	return err
}

// ensureAttachBranch creates the attachments branch the first time: a
// commit with a README and no parents.
func (c *Client) ensureAttachBranch(ctx context.Context, write bool, repo string) error {
	_, err := c.call(ctx, write, repo, http.MethodGet, repoPath(repo, "/branches/%s", AttachBranch), nil, nil)
	if !IsStatus(err, http.StatusNotFound) {
		return err
	}
	readme := "Files Open Conduit's agents attached to issue and PR comments, one folder per issue. Nothing here is built or merged.\n"
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if _, err := c.call(ctx, write, repo, http.MethodPost, repoPath(repo, "/git/trees"), map[string]any{
		"tree": []map[string]string{{"path": "README.md", "mode": "100644", "type": "blob", "content": readme}},
	}, &tree); err != nil {
		return err
	}
	if _, err := c.call(ctx, write, repo, http.MethodPost, repoPath(repo, "/git/commits"), map[string]any{
		"message": "Start the attachments branch", "tree": tree.SHA, "parents": []string{},
	}, &commit); err != nil {
		return err
	}
	_, err = c.call(ctx, write, repo, http.MethodPost, repoPath(repo, "/git/refs"), map[string]string{"ref": "refs/heads/" + AttachBranch, "sha": commit.SHA}, nil)
	if IsStatus(err, http.StatusUnprocessableEntity) {
		return nil // another agent made it first
	}
	return err
}

// ErrForeignLink means a link is not on this GitHub host, so the sign-in
// token must not be sent to it.
var ErrForeignLink = errors.New("github: link is not on this GitHub host")

// ErrNoRepoLink means a link is on this GitHub host but under no
// repository (an organisation's settings, a user's profile, an API call
// outside /repos/), so no repository's grant covers it.
var ErrNoRepoLink = errors.New("github: link is under no repository")

// FetchLink is a link Fetch may get: the URL it requests, the repository
// the link is under ("" for a user attachment, which belongs to whatever
// issue or PR it was posted on) and whether it is sent signed in.
type FetchLink struct {
	URL    *url.URL
	Repo   string
	Signed bool
}

// unsignedHosts serve links that carry their own signature or none at
// all: images posted in comments (user-images, and private-user-images
// with its jwt parameter) and camo, GitHub's proxy for images hosted
// elsewhere. They are fetched without the sign-in token.
var unsignedHosts = map[string]bool{
	"user-images.githubusercontent.com":         true,
	"private-user-images.githubusercontent.com": true,
	"camo.githubusercontent.com":                true,
}

// ParseFetchLink reads a link a comment holds into what Fetch gets and
// which repository it is under:
//
//   - <web>/user-attachments/… is a user attachment, signed in;
//   - <web>/<owner>/<repo>/… and raw.githubusercontent.com/<owner>/<repo>/…
//     are under <owner>/<repo>, signed in (a github.com blob or raw page is
//     fetched from raw.githubusercontent.com);
//   - <api>/repos/<owner>/<repo>/… is under <owner>/<repo>, signed in;
//   - user-images, private-user-images and camo on githubusercontent.com
//     are fetched without the token.
//
// Any other host is ErrForeignLink; anything else on this host is
// ErrNoRepoLink.
func (c *Client) ParseFetchLink(link string) (FetchLink, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return FetchLink{}, ErrForeignLink
	}
	// The path is checked as the server will read it: a dot segment or an
	// encoded slash could resolve to a repository other than the one named.
	esc := strings.ToLower(u.EscapedPath())
	if strings.Contains(esc, "%2f") || strings.Contains(esc, "%5c") || strings.Contains(u.Path, "\\") {
		return FetchLink{}, ErrNoRepoLink
	}
	var parts []string
	for _, s := range strings.Split(u.Path, "/") {
		switch s {
		case "":
		case ".", "..":
			return FetchLink{}, ErrNoRepoLink
		default:
			parts = append(parts, s)
		}
	}
	host := strings.ToLower(u.Host)
	repoOf := func(parts []string) (string, bool) {
		if len(parts) < 2 {
			return "", false
		}
		return parts[0] + "/" + parts[1], true
	}
	api, _ := url.Parse(c.api)
	if api != nil && host == strings.ToLower(api.Host) {
		prefix := strings.Split(strings.Trim(api.Path, "/"), "/")
		if prefix[0] == "" {
			prefix = nil
		}
		if len(parts) > len(prefix) && slices.Equal(parts[:len(prefix)], prefix) && parts[len(prefix)] == "repos" {
			if repo, ok := repoOf(parts[len(prefix)+1:]); ok {
				return FetchLink{URL: u, Repo: repo, Signed: true}, nil
			}
			return FetchLink{}, ErrNoRepoLink
		}
		if host != strings.ToLower(c.host) {
			return FetchLink{}, ErrNoRepoLink
		}
	}
	switch {
	case host == strings.ToLower(c.host):
		if len(parts) > 0 && parts[0] == "user-attachments" {
			return FetchLink{URL: u, Signed: true}, nil
		}
		repo, ok := repoOf(parts)
		if !ok || len(parts) < 3 {
			return FetchLink{}, ErrNoRepoLink
		}
		// github.com/<owner>/<repo>/blob/<ref>/<path> is a page; raw.githubusercontent.com serves the file.
		if c.host == "github.com" && len(parts) >= 4 && (parts[2] == "blob" || parts[2] == "raw") {
			u = &url.URL{Scheme: "https", Host: "raw.githubusercontent.com", Path: "/" + repo + "/" + strings.Join(parts[3:], "/")}
		}
		return FetchLink{URL: u, Repo: repo, Signed: true}, nil
	case c.host == "github.com" && host == "raw.githubusercontent.com":
		repo, ok := repoOf(parts)
		if !ok || len(parts) < 3 {
			return FetchLink{}, ErrNoRepoLink
		}
		return FetchLink{URL: u, Repo: repo, Signed: true}, nil
	case c.host == "github.com" && unsignedHosts[host]:
		return FetchLink{URL: u}, nil
	}
	return FetchLink{}, ErrForeignLink
}

// fetchToken is the token a signed fetch under repo is sent with, and
// what to call once the fetch is done. A sign-in App mints one read-only
// for that one repository (readPermissions), revoked afterwards, so a link
// can never reach another repository the installation covers. A token
// connection has nothing to mint: its own token goes, and the repository
// check before the fetch is what bounds it.
func (c *Client) fetchToken(ctx context.Context, repo string) (string, func(), error) {
	read := c.who.Load().read
	app, ok := read.(*appToken)
	if !ok {
		tok, err := read.token(ctx, repo)
		return tok, func() {}, err
	}
	tok, _, err := app.scoped(ctx, repo, readPermissions)
	if err != nil {
		return "", nil, err
	}
	return tok, func() {
		// A token left unrevoked expires within the hour on its own.
		_ = c.RevokeToken(context.WithoutCancel(ctx), tok)
	}, nil
}

// Fetch downloads a file a comment links to (an attachment, an image, a
// file in a repository) parsed by ParseFetchLink. A signed link is sent
// signed in for repo, which the caller has checked the Work may read: the
// link's own repository, or the Work's for a user attachment. Redirects are
// followed without the Authorization header once they leave the host (the
// signed storage link an attachment ends at).
func (c *Client) Fetch(ctx context.Context, repo string, link FetchLink) ([]byte, string, error) {
	if link.URL == nil {
		return nil, "", ErrForeignLink
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL.String(), nil)
	if err != nil {
		return nil, "", ErrForeignLink
	}
	if link.Signed {
		tok, done, err := c.fetchToken(ctx, repo)
		if err != nil {
			return nil, "", err
		}
		defer done()
		req.Header.Set("Authorization", "token "+tok)
	}
	req.Header.Set("User-Agent", "open-conduit")
	client := &http.Client{Transport: c.tr.base, Timeout: 2 * time.Minute, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("github: fetch: too many redirects")
		}
		// The sign-in token goes only to the host it was given for: never to
		// the signed storage link (or any other host:port) a redirect names.
		if next.URL.Host != via[0].URL.Host {
			next.Header.Del("Authorization")
		}
		return nil
	}}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", errors.New("github: fetch: transport failure")
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return nil, "", &StatusError{Status: res.StatusCode, Message: "fetch refused"}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxFetch+1))
	if err != nil {
		return nil, "", errors.New("github: fetch: reading the body failed")
	}
	if len(data) > MaxFetch {
		return nil, "", fmt.Errorf("github: fetch exceeds %d MB", MaxFetch>>20)
	}
	return data, res.Header.Get("Content-Type"), nil
}
