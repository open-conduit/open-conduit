package bitbucket

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// ClientOptions tune a Client.
type ClientOptions struct {
	HTTP *http.Client
	Now  func() time.Time
}

// Client is a Bitbucket Cloud account.
type Client struct {
	conn ConnectionConfig
	api  *tracker.JSONClient
	cred *tracker.Credential
	now  func() time.Time
}

// NewClient binds a connection.
func NewClient(conn ConnectionConfig, secrets tracker.SecretFunc, o ClientOptions) *Client {
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Client{conn: conn, now: o.Now, cred: &tracker.Credential{Name: SecretToken, Secrets: secrets}}
	c.api = &tracker.JSONClient{HTTP: o.HTTP, Now: o.Now, UserAgent: "open-conduit", OnUnauthorized: c.cred.Invalidate,
		Auth: func(ctx context.Context, r *http.Request) error {
			token, err := c.cred.Get(ctx)
			if err != nil {
				return err
			}
			if conn.Auth == "bearer" {
				r.Header.Set("Authorization", "Bearer "+token)
			} else {
				r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(conn.Username+":"+token)))
			}
			return nil
		}}
	return c
}

func (c *Client) repoPath(s Settings, format string, args ...any) string {
	return c.conn.APIURL + fmt.Sprintf("/repositories/%s/%s/", url.PathEscape(s.Workspace), url.PathEscape(s.Slug)) + fmt.Sprintf(format, args...)
}

// sameAPI reports whether a next link stays on the configured API, so a
// pagination link can never send the credential elsewhere.
func (c *Client) sameAPI(link string) bool {
	return strings.HasPrefix(link, c.conn.APIURL+"/")
}

// Verify reads the signed-in user; an access token, which has no user, is
// proved by listing the repositories it can see.
func (c *Client) Verify(ctx context.Context) (string, error) {
	var u struct {
		DisplayName string `json:"display_name"`
		Username    string `json:"username"`
	}
	_, err := c.api.Do(ctx, http.MethodGet, c.conn.APIURL+"/user", nil, &u)
	if err == nil {
		name := u.DisplayName
		if name == "" {
			name = u.Username
		}
		return "signed in as " + name, nil
	}
	if c.conn.Auth != "bearer" {
		return "", err
	}
	if _, err2 := c.api.Do(ctx, http.MethodGet, c.conn.APIURL+"/repositories?role=member&pagelen=1", nil, nil); err2 != nil {
		return "", err
	}
	return "access token works (it has no user to name)", nil
}

type page[T any] struct {
	Values []T    `json:"values"`
	Next   string `json:"next"`
}

// all reads every page, following each page's next link on the same API.
// An unfinished listing is an error, never a short success.
func all[T any](ctx context.Context, c *Client, first string) ([]T, error) {
	var out []T
	next := first
	for n := 0; n < 100; n++ {
		var p page[T]
		if _, err := c.api.Do(ctx, http.MethodGet, next, nil, &p); err != nil {
			return nil, err
		}
		out = append(out, p.Values...)
		if p.Next == "" {
			return out, nil
		}
		if !c.sameAPI(p.Next) {
			return nil, errors.New("bitbucket: a page link left the API")
		}
		next = p.Next
	}
	return nil, errors.New("bitbucket: listing did not finish")
}

type rawUser struct {
	DisplayName string `json:"display_name"`
	Nickname    string `json:"nickname"`
	Type        string `json:"type"`
}

func (u rawUser) name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Nickname
}

type rawPR struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	State       string  `json:"state"`
	Author      rawUser `json:"author"`
	CreatedOn   string  `json:"created_on"`
	UpdatedOn   string  `json:"updated_on"`
	Source      struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"destination"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

type rawComment struct {
	ID      int `json:"id"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	User      rawUser `json:"user"`
	CreatedOn string  `json:"created_on"`
	UpdatedOn string  `json:"updated_on"`
	Deleted   bool    `json:"deleted"`
	Inline    *struct {
		Path string `json:"path"`
		From *int   `json:"from"`
		To   *int   `json:"to"`
	} `json:"inline"`
	Parent *struct {
		ID int `json:"id"`
	} `json:"parent"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

type rawStatus struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	State     string `json:"state"`
	URL       string `json:"url"`
	UpdatedOn string `json:"updated_on"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

const allStates = "&state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED"

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// ListPulls reads one page of the repository's PRs in creation order: every
// PR updated since `since`, any state. cursor is a next link from an earlier
// page ("" starts).
func (c *Client) ListPulls(ctx context.Context, s Settings, since time.Time, cursor string) ([]rawPR, string, error) {
	link := cursor
	if link == "" {
		q := "updated_on >= " + since.UTC().Format("2006-01-02T15:04:05Z") + " OR state = \"OPEN\""
		link = c.repoPath(s, "pullrequests?pagelen=50&sort=created_on&q=%s%s", url.QueryEscape(q), allStates)
	} else if !c.sameAPI(link) {
		return nil, "", errors.New("bitbucket: invalid cursor")
	}
	var p page[rawPR]
	if _, err := c.api.Do(ctx, http.MethodGet, link, nil, &p); err != nil {
		return nil, "", err
	}
	if p.Next != "" && !c.sameAPI(p.Next) {
		return nil, "", errors.New("bitbucket: a page link left the API")
	}
	return p.Values, p.Next, nil
}

// Pull reads one PR.
func (c *Client) Pull(ctx context.Context, s Settings, id int) (rawPR, error) {
	var p rawPR
	_, err := c.api.Do(ctx, http.MethodGet, c.repoPath(s, "pullrequests/%d", id), nil, &p)
	return p, err
}

// PullsFor finds PRs from the branch, or whose description mentions ref, in
// any state.
func (c *Client) PullsFor(ctx context.Context, s Settings, branch, ref string) ([]rawPR, error) {
	found := map[int]bool{}
	var out []rawPR
	add := func(q string) error {
		prs, err := all[rawPR](ctx, c, c.repoPath(s, "pullrequests?pagelen=50&q=%s%s", url.QueryEscape(q), allStates))
		for _, p := range prs {
			if !found[p.ID] {
				found[p.ID] = true
				out = append(out, p)
			}
		}
		return err
	}
	if branch != "" {
		if err := add("source.branch.name=" + quote(branch)); err != nil {
			return nil, err
		}
	}
	if ref != "" {
		if err := add("description ~ " + quote(ref)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// OpenPullsAt lists open PRs whose head is sha.
func (c *Client) OpenPullsAt(ctx context.Context, s Settings, sha string) ([]rawPR, error) {
	prs, err := all[rawPR](ctx, c, c.repoPath(s, "pullrequests?pagelen=50&state=OPEN"))
	if err != nil {
		return nil, err
	}
	var out []rawPR
	for _, p := range prs {
		if p.Source.Commit.Hash != "" && (strings.HasPrefix(sha, p.Source.Commit.Hash) || strings.HasPrefix(p.Source.Commit.Hash, sha)) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Comments lists a PR's comments (conversation and inline), deleted ones left out.
func (c *Client) Comments(ctx context.Context, s Settings, id int) ([]rawComment, error) {
	raw, err := all[rawComment](ctx, c, c.repoPath(s, "pullrequests/%d/comments?pagelen=100", id))
	if err != nil {
		return nil, err
	}
	out := raw[:0]
	for _, x := range raw {
		if !x.Deleted {
			out = append(out, x)
		}
	}
	return out, nil
}

// Comment reads one PR comment.
func (c *Client) Comment(ctx context.Context, s Settings, pr, id int) (rawComment, error) {
	var x rawComment
	_, err := c.api.Do(ctx, http.MethodGet, c.repoPath(s, "pullrequests/%d/comments/%d", pr, id), nil, &x)
	return x, err
}

// Statuses lists a commit's statuses.
func (c *Client) Statuses(ctx context.Context, s Settings, sha string) ([]rawStatus, error) {
	return all[rawStatus](ctx, c, c.repoPath(s, "commit/%s/statuses?pagelen=100", url.PathEscape(sha)))
}

// DefaultBranch is the repository's main branch.
func (c *Client) DefaultBranch(ctx context.Context, s Settings) (string, error) {
	var repo struct {
		MainBranch struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
	}
	if _, err := c.api.Do(ctx, http.MethodGet, strings.TrimSuffix(c.repoPath(s, ""), "/"), nil, &repo); err != nil {
		return "", err
	}
	if repo.MainBranch.Name == "" {
		return "main", nil
	}
	return repo.MainBranch.Name, nil
}

// CreatePull opens a PR.
func (c *Client) CreatePull(ctx context.Context, s Settings, title, body, head, base string, draft bool) (rawPR, error) {
	req := map[string]any{"title": title, "description": body,
		"source":      map[string]any{"branch": map[string]string{"name": head}},
		"destination": map[string]any{"branch": map[string]string{"name": base}}, "draft": draft}
	var p rawPR
	_, err := c.api.Do(ctx, http.MethodPost, c.repoPath(s, "pullrequests"), req, &p)
	return p, err
}

// PostComment posts on a PR's conversation, or replies to a comment.
func (c *Client) PostComment(ctx context.Context, s Settings, pr int, body string, parent int) (rawComment, error) {
	req := map[string]any{"content": map[string]string{"raw": body}}
	if parent > 0 {
		req["parent"] = map[string]int{"id": parent}
	}
	var x rawComment
	_, err := c.api.Do(ctx, http.MethodPost, c.repoPath(s, "pullrequests/%d/comments", pr), req, &x)
	return x, err
}

var hookEvents = []string{
	"pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected",
	"pullrequest:comment_created", "pullrequest:comment_updated", "repo:commit_status_created", "repo:commit_status_updated",
}

// EnsureWebhook points the repository's hook at url, updating the one
// already registered while it still exists.
func (c *Client) EnsureWebhook(ctx context.Context, s Settings, hookURL, secret, existing string, dryRun bool) (string, error) {
	body := map[string]any{"description": "open-conduit", "url": hookURL, "active": true, "secret": secret, "events": hookEvents}
	if dryRun {
		if existing == "" {
			existing = "dry-run"
		}
		return existing, nil
	}
	var h struct {
		UUID string `json:"uuid"`
	}
	if existing != "" {
		_, err := c.api.Do(ctx, http.MethodPut, c.repoPath(s, "hooks/%s", url.PathEscape(existing)), body, &h)
		if err == nil {
			if h.UUID == "" {
				h.UUID = existing
			}
			return h.UUID, nil
		}
		if !errors.Is(err, tracker.ErrNotFound) {
			return "", err
		}
	}
	if _, err := c.api.Do(ctx, http.MethodPost, c.repoPath(s, "hooks"), body, &h); err != nil {
		return "", err
	}
	if h.UUID == "" {
		return "", errors.New("bitbucket: the webhook was made without an id")
	}
	return h.UUID, nil
}

// ---- marks --------------------------------------------------------------------

var (
	legacyMark  = regexp.MustCompile(`(?m)\n*^\[//\]: # \(open-supervisor:agent(?: from=([^)\s]+))?\)\s*$`)
	conduitMark = regexp.MustCompile(`(?m)\n*^\[//\]: # \(open-conduit:agent((?: [a-z]+=[A-Za-z0-9_.:/@-]+)*)\)\s*$`)
	markAttr    = regexp.MustCompile(`([a-z]+)=([A-Za-z0-9_.:/@-]+)`)
	markValue   = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,200}$`)
)

// Mark appends the invisible lines that say an agent posted the comment:
// Open Supervisor's (which it still reads) and Open Conduit's intent.
func Mark(body string, m tracker.Mark) string {
	legacy := "[//]: # (open-supervisor:agent)"
	if markValue.MatchString(m.From) {
		legacy = "[//]: # (open-supervisor:agent from=" + m.From + ")"
	}
	line := "[//]: # (open-conduit:agent"
	if markValue.MatchString(m.Work) {
		line += " work=" + m.Work
	}
	if markValue.MatchString(m.Intent) {
		line += " intent=" + m.Intent
	}
	return strings.TrimRight(body, "\n") + "\n\n" + legacy + "\n" + line + ")"
}

// Unmark takes the agent lines off a body and says who posted it.
func Unmark(body string) (string, *tracker.Agent) {
	var a *tracker.Agent
	if m := legacyMark.FindStringSubmatch(body); m != nil {
		a = &tracker.Agent{From: m[1]}
		body = legacyMark.ReplaceAllString(body, "")
	}
	if m := conduitMark.FindStringSubmatch(body); m != nil {
		if a == nil {
			a = &tracker.Agent{}
		}
		for _, kv := range markAttr.FindAllStringSubmatch(m[1], -1) {
			switch kv[1] {
			case "work":
				a.Work = kv[2]
			case "intent":
				a.Intent = kv[2]
			}
		}
		body = conduitMark.ReplaceAllString(body, "")
	}
	return strings.TrimRight(body, "\n"), a
}

// PausedUntil reports a rate limit in force (doctor).
func (c *Client) PausedUntil() (time.Time, bool) { return c.api.PausedUntil() }
