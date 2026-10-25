package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
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

// CheckState reads a commit's check runs (or, when those are refused, its
// Actions workflow runs) and statuses; unreadable reports that neither kind of
// run could be read.
func (c *Client) CheckState(ctx context.Context, repo, sha string) ([]rawCheckRun, []rawStatus, bool, error) {
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
	if c.write != c.read && (IsStatus(err, http.StatusForbidden) || IsStatus(err, http.StatusNotFound)) {
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

// Fetch downloads a file a comment links to (an attachment, an image, a file
// in a repository) signed in as the reader, which a private repository's
// attachments need. Links to other hosts are refused, never sent the token.
// Redirects are followed without the Authorization header once they leave
// the host (the signed storage link an attachment ends at).
func (c *Client) Fetch(ctx context.Context, repo, link string) ([]byte, string, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", ErrForeignLink
	}
	api, _ := url.Parse(c.api)
	switch {
	case u.Host == c.host:
		// github.com/<owner>/<repo>/blob/<ref>/<path> is a page; raw.githubusercontent.com serves the file.
		if parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 4); c.host == "github.com" && len(parts) == 4 && (parts[2] == "blob" || parts[2] == "raw") {
			u = &url.URL{Scheme: "https", Host: "raw.githubusercontent.com", Path: "/" + parts[0] + "/" + parts[1] + "/" + parts[3]}
		}
	case api != nil && u.Host == api.Host, c.host == "github.com" && strings.HasSuffix(u.Host, ".githubusercontent.com"):
	default:
		return nil, "", ErrForeignLink
	}
	tok, err := c.read.token(ctx, repo)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", ErrForeignLink
	}
	req.Header.Set("Authorization", "token "+tok)
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
