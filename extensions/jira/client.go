package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// MarkProperty is the comment entity property that marks an agent's comment
// (Open Supervisor's key, so both runtimes recognize each other's notes).
const MarkProperty = "open-supervisor"

// MarkValue is the property's value.
type MarkValue struct {
	Agent  bool   `json:"agent"`
	From   string `json:"from,omitempty"`
	Work   string `json:"work,omitempty"`
	Intent string `json:"intent,omitempty"`
}

// ClientOptions tune a Client.
type ClientOptions struct {
	HTTP *http.Client
	Now  func() time.Time
	// DryRun makes every write a no-op (reads still go to Jira).
	DryRun bool
}

// Client is a Jira site bound to one entry's projects. It implements
// tracker.Service.
type Client struct {
	conn     ConnectionConfig
	s        Settings
	api      *tracker.JSONClient
	cred     *tracker.Credential
	now      func() time.Time
	dryRun   bool
	refRe    *regexp.Regexp
	pageSize int

	mu         sync.Mutex
	categories map[string]string // status id -> category key
}

var _ tracker.Service = (*Client)(nil)

// NewClient binds a connection to an entry.
func NewClient(conn ConnectionConfig, s Settings, secrets tracker.SecretFunc, o ClientOptions) (*Client, error) {
	if len(s.Projects) == 0 {
		return nil, errors.New("jira client: no projects")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	quoted := make([]string, len(s.Projects))
	for i, p := range s.Projects {
		quoted[i] = regexp.QuoteMeta(p)
	}
	c := &Client{conn: conn, s: s, now: o.Now, dryRun: o.DryRun, pageSize: 50,
		cred:  &tracker.Credential{Name: SecretToken, Secrets: secrets},
		refRe: regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)-\d+\b`)}
	c.api = &tracker.JSONClient{HTTP: o.HTTP, Now: o.Now, UserAgent: "open-conduit", OnUnauthorized: c.cred.Invalidate,
		Auth: func(ctx context.Context, r *http.Request) error {
			token, err := c.cred.Get(ctx)
			if err != nil {
				return err
			}
			if conn.Auth == "bearer" {
				r.Header.Set("Authorization", "Bearer "+token)
			} else {
				r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(conn.Email+":"+token)))
			}
			return nil
		}}
	return c, nil
}

func (c *Client) url(path string) string { return c.conn.BaseURL + "/" + strings.TrimPrefix(path, "/") }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	_, err := c.api.Do(ctx, method, c.url(path), in, out)
	return err
}

// Verify reads the signed-in account.
func (c *Client) Verify(ctx context.Context) (string, error) {
	var me struct {
		DisplayName string `json:"displayName"`
	}
	if err := c.do(ctx, http.MethodGet, "rest/api/2/myself", nil, &me); err != nil {
		return "", err
	}
	return "signed in as " + me.DisplayName, nil
}

// Owns reports whether an issue key is in one of the entry's projects.
func (c *Client) Owns(key string) bool {
	for _, p := range c.s.Projects {
		if strings.HasPrefix(strings.ToUpper(key), p+"-") {
			return true
		}
	}
	return false
}

// IssueURL is the issue's page.
func (c *Client) IssueURL(key string) string { return c.conn.BaseURL + "/browse/" + key }

// RefFor is the key itself: Jira links a PR that mentions it.
func (c *Client) RefFor(key string) string { return key }

// RefsIn returns the entry's issue keys a text mentions.
func (c *Client) RefsIn(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range c.refRe.FindAllString(text, -1) {
		k := strings.ToUpper(m)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// ProjectJQL selects the entry's projects.
func (c *Client) ProjectJQL() string {
	if len(c.s.Projects) == 1 {
		return fmt.Sprintf("project = %q", c.s.Projects[0])
	}
	q := make([]string, len(c.s.Projects))
	for i, p := range c.s.Projects {
		q[i] = fmt.Sprintf("%q", p)
	}
	return "project in (" + strings.Join(q, ", ") + ")"
}

// ---- issues ---------------------------------------------------------------

type rawUser struct {
	AccountID    string `json:"accountId"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	EmailAddress string `json:"emailAddress"`
	DisplayName  string `json:"displayName"`
	AccountType  string `json:"accountType"`
}

type rawStatus struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	StatusCategory struct {
		Key string `json:"key"`
	} `json:"statusCategory"`
}

type rawNamed struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type rawFields struct {
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Labels      []string   `json:"labels"`
	Status      rawStatus  `json:"status"`
	Created     string     `json:"created"`
	Updated     string     `json:"updated"`
	Reporter    *rawUser   `json:"reporter"`
	Assignee    *rawUser   `json:"assignee"`
	Project     *rawNamed  `json:"project"`
	IssueType   *rawNamed  `json:"issuetype"`
	Priority    *rawNamed  `json:"priority"`
	Components  []rawNamed `json:"components"`
	FixVersions []rawNamed `json:"fixVersions"`
	Parent      *rawNamed  `json:"parent"`
	// all are every field as it came, for the extra ones routes match on.
	all map[string]json.RawMessage
}

func (f *rawFields) UnmarshalJSON(data []byte) error {
	type plain rawFields
	if err := json.Unmarshal(data, (*plain)(f)); err != nil {
		return err
	}
	return json.Unmarshal(data, &f.all)
}

type rawIssue struct {
	Key       string    `json:"key"`
	Fields    rawFields `json:"fields"`
	Changelog *struct {
		Histories []rawHistory `json:"histories"`
	} `json:"changelog"`
}

type rawHistory struct {
	Created string    `json:"created"`
	Changes []rawItem `json:"items"`
}

type rawItem struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

const issueFields = "summary,labels,status,created,updated,description,reporter,assignee,project,issuetype,priority,components,fixVersions,parent"

func (c *Client) fields() []string { return append(strings.Split(issueFields, ","), c.s.Fields...) }

func parseTime(s string) time.Time {
	for _, l := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// flatten reads a field's value as strings: a string or number as it is, an
// option or user by its value or name, a list as each of its items.
func flatten(raw json.RawMessage) []string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		case bool:
			out = append(out, strconv.FormatBool(x))
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			for _, k := range []string{"value", "name", "key", "displayName"} {
				if s, ok := x[k].(string); ok && s != "" {
					out = append(out, s)
					return
				}
			}
		}
	}
	walk(v)
	return out
}

func (c *Client) issue(r rawIssue) tracker.Issue {
	f := r.Fields
	i := tracker.Issue{Key: strings.ToUpper(r.Key), Title: f.Summary, Body: ToMarkdown(f.Description), URL: c.IssueURL(r.Key),
		Labels: f.Labels, Open: f.Status.StatusCategory.Key != "done", Status: f.Status.Name,
		CreatedAt: parseTime(f.Created), UpdatedAt: parseTime(f.Updated), Attrs: map[string][]string{}}
	set := func(name string, vs ...string) {
		for _, v := range vs {
			if v != "" {
				i.Attrs[name] = append(i.Attrs[name], v)
			}
		}
	}
	if f.Reporter != nil {
		i.Author = f.Reporter.DisplayName
	}
	if f.Project != nil {
		set("project", f.Project.Key, f.Project.Name)
	}
	if f.IssueType != nil {
		set("issuetype", f.IssueType.Name)
	}
	if f.Priority != nil {
		set("priority", f.Priority.Name)
	}
	if f.Assignee != nil {
		set("assignee", f.Assignee.DisplayName, f.Assignee.EmailAddress)
	}
	if f.Parent != nil {
		set("parent", f.Parent.Key)
	}
	for _, x := range f.Components {
		set("components", x.Name)
	}
	for _, v := range f.FixVersions {
		set("fixversions", v.Name)
	}
	for _, id := range c.s.Fields {
		set(strings.ToLower(id), flatten(f.all[id])...)
	}
	return i
}

func (c *Client) query(closedSince time.Time) string {
	minutes := int(math.Ceil(c.now().Sub(closedSince).Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	q := c.ProjectJQL() + fmt.Sprintf(" AND (statusCategory != Done OR updated >= -%dm)", minutes)
	if c.s.JQL != "" {
		q += " AND (" + c.s.JQL + ")"
	}
	return q + " ORDER BY created ASC, key ASC"
}

// ListIssues lists the projects' issues in creation order: open ones and
// those updated since closedSince. It uses the enhanced search (Cloud,
// token pages) and falls back to the classic search (offsets) when the site
// has no enhanced search.
func (c *Client) ListIssues(ctx context.Context, closedSince time.Time, cursor string) (tracker.IssuePage, error) {
	body := map[string]any{"jql": c.query(closedSince), "fields": c.fields(), "maxResults": c.pageSize}
	var raw []rawIssue
	next := ""
	switch {
	case cursor == "" || strings.HasPrefix(cursor, "t:"):
		if token := strings.TrimPrefix(cursor, "t:"); token != "" {
			body["nextPageToken"] = token
		}
		var page struct {
			Issues        []rawIssue `json:"issues"`
			NextPageToken string     `json:"nextPageToken"`
			IsLast        bool       `json:"isLast"`
		}
		err := c.do(ctx, http.MethodPost, "rest/api/2/search/jql", body, &page)
		if errors.Is(err, tracker.ErrNotFound) && cursor == "" {
			return c.ListIssues(ctx, closedSince, "s:0")
		}
		if err != nil {
			return tracker.IssuePage{}, err
		}
		raw = page.Issues
		if !page.IsLast && page.NextPageToken != "" {
			next = "t:" + page.NextPageToken
		}
	case strings.HasPrefix(cursor, "s:"):
		start, err := strconv.Atoi(strings.TrimPrefix(cursor, "s:"))
		if err != nil || start < 0 {
			return tracker.IssuePage{}, errors.New("jira: invalid cursor")
		}
		body["startAt"] = start
		var page struct {
			Issues []rawIssue `json:"issues"`
			Total  int        `json:"total"`
		}
		if err := c.do(ctx, http.MethodPost, "rest/api/2/search", body, &page); err != nil {
			return tracker.IssuePage{}, err
		}
		raw = page.Issues
		if end := start + len(page.Issues); len(page.Issues) > 0 && end < page.Total {
			next = "s:" + strconv.Itoa(end)
		}
	default:
		return tracker.IssuePage{}, errors.New("jira: invalid cursor")
	}
	out := tracker.IssuePage{Next: next}
	for _, r := range raw {
		out.Issues = append(out.Issues, c.issue(r))
	}
	return out, nil
}

// Issue reads one issue.
func (c *Client) Issue(ctx context.Context, key string) (tracker.Issue, error) {
	var r rawIssue
	if err := c.do(ctx, http.MethodGet, "rest/api/2/issue/"+url.PathEscape(key)+"?fields="+url.QueryEscape(strings.Join(c.fields(), ",")), nil, &r); err != nil {
		return tracker.Issue{}, err
	}
	if r.Key == "" {
		return tracker.Issue{}, tracker.ErrNotFound
	}
	return c.issue(r), nil
}

// statusCategories maps each status id to its category key, read once.
func (c *Client) statusCategories(ctx context.Context) (map[string]string, error) {
	c.mu.Lock()
	cached := c.categories
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	var all []rawStatus
	if err := c.do(ctx, http.MethodGet, "rest/api/2/status", nil, &all); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, s := range all {
		m[s.ID] = s.StatusCategory.Key
	}
	c.mu.Lock()
	c.categories = m
	c.mu.Unlock()
	return m, nil
}

// Reopens lists every status change out of a done-category status (or,
// without the categories, every time the resolution was cleared).
func (c *Client) Reopens(ctx context.Context, key string) ([]time.Time, error) {
	var r rawIssue
	if err := c.do(ctx, http.MethodGet, "rest/api/2/issue/"+url.PathEscape(key)+"?fields=status&expand=changelog", nil, &r); err != nil {
		return nil, err
	}
	if r.Changelog == nil {
		return nil, nil
	}
	cats, catErr := c.statusCategories(ctx)
	var out []time.Time
	for _, h := range r.Changelog.Histories {
		at := parseTime(h.Created)
		for _, it := range h.Changes {
			reopen := false
			switch {
			case catErr == nil && it.Field == "status":
				reopen = cats[it.From] == "done" && cats[it.To] != "done"
			case catErr != nil && it.Field == "resolution":
				reopen = it.From != "" && it.To == ""
			}
			if reopen && !at.IsZero() {
				out = append(out, at)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

// ---- comments ---------------------------------------------------------------

type rawComment struct {
	ID         string   `json:"id"`
	Author     *rawUser `json:"author"`
	Body       string   `json:"body"`
	Created    string   `json:"created"`
	Updated    string   `json:"updated"`
	Properties []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	} `json:"properties"`
}

func (c *Client) comment(key string, r rawComment) tracker.Comment {
	out := tracker.Comment{ID: r.ID, Body: ToMarkdown(r.Body), URL: c.IssueURL(key) + "?focusedCommentId=" + r.ID,
		CreatedAt: parseTime(r.Created), UpdatedAt: parseTime(r.Updated)}
	if r.Author != nil {
		out.Author = r.Author.DisplayName
		out.Bot = r.Author.AccountType == "app"
		if a := c.conn.AgentAccount; a != "" && (a == r.Author.AccountID || a == r.Author.Name || a == r.Author.Key || strings.EqualFold(a, r.Author.EmailAddress)) {
			out.Agent = &tracker.Agent{}
		}
	}
	for _, p := range r.Properties {
		if p.Key != MarkProperty {
			continue
		}
		var v MarkValue
		if json.Unmarshal(p.Value, &v) == nil && v.Agent {
			out.Agent = &tracker.Agent{From: v.From, Work: v.Work, Intent: v.Intent}
		}
	}
	return out
}

// Comments lists every comment on the issue, oldest first.
func (c *Client) Comments(ctx context.Context, key string) ([]tracker.Comment, error) {
	var out []tracker.Comment
	for start := 0; start < 10000; {
		var page struct {
			Comments []rawComment `json:"comments"`
			Total    int          `json:"total"`
		}
		path := fmt.Sprintf("rest/api/2/issue/%s/comment?expand=properties&orderBy=created&maxResults=100&startAt=%d", url.PathEscape(key), start)
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Comments {
			out = append(out, c.comment(key, r))
		}
		start += len(page.Comments)
		if len(page.Comments) == 0 || start >= page.Total {
			sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
			return out, nil
		}
	}
	return nil, errors.New("jira: comment listing did not finish")
}

// ReadComment reads one comment (Jira deliveries leave out its properties).
func (c *Client) ReadComment(ctx context.Context, ref tracker.CommentRef) (string, tracker.Comment, error) {
	if ref.Issue == "" {
		return "", tracker.Comment{}, tracker.ErrNotFound
	}
	var r rawComment
	path := "rest/api/2/issue/" + url.PathEscape(ref.Issue) + "/comment/" + url.PathEscape(ref.ID) + "?expand=properties"
	if err := c.do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return "", tracker.Comment{}, err
	}
	return strings.ToUpper(ref.Issue), c.comment(ref.Issue, r), nil
}

// PostComment posts on the issue with the property that marks it as an agent's.
func (c *Client) PostComment(ctx context.Context, key, body string, m tracker.Mark) (tracker.Posted, error) {
	if c.dryRun {
		return tracker.Posted{}, nil
	}
	req := map[string]any{
		"body":       ToWiki(body),
		"properties": []map[string]any{{"key": MarkProperty, "value": MarkValue{Agent: true, From: m.From, Work: m.Work, Intent: m.Intent}}},
	}
	var r rawComment
	if err := c.do(ctx, http.MethodPost, "rest/api/2/issue/"+url.PathEscape(key)+"/comment", req, &r); err != nil {
		return tracker.Posted{}, err
	}
	if r.ID == "" {
		return tracker.Posted{}, errors.New("jira: the comment was not confirmed")
	}
	return tracker.Posted{ID: r.ID, URL: c.IssueURL(key) + "?focusedCommentId=" + r.ID}, nil
}

// CreateIssue opens a Task in the entry's first project.
func (c *Client) CreateIssue(ctx context.Context, n tracker.NewIssue) (tracker.Issue, error) {
	if c.dryRun {
		return tracker.Issue{}, nil
	}
	for _, l := range n.Labels {
		if strings.ContainsAny(l, " \t") {
			return tracker.Issue{}, &tracker.StatusError{Status: http.StatusBadRequest}
		}
	}
	fields := map[string]any{"project": map[string]string{"key": c.s.Projects[0]}, "summary": n.Title,
		"description": ToWiki(n.Body), "issuetype": map[string]string{"name": "Task"}}
	if len(n.Labels) > 0 {
		fields["labels"] = n.Labels
	}
	var r struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "rest/api/2/issue", map[string]any{"fields": fields}, &r); err != nil {
		return tracker.Issue{}, err
	}
	return tracker.Issue{Key: r.Key, Title: n.Title, Body: n.Body, URL: c.IssueURL(r.Key), Labels: n.Labels, Open: true}, nil
}

// ---- labels and workflow ------------------------------------------------------

// ErrLabelSpace means a label contains a space, which Jira labels cannot.
var ErrLabelSpace = errors.New("jira: labels cannot contain spaces")

// EditLabels adds and removes labels in one update (Jira labels exist once used).
func (c *Client) EditLabels(ctx context.Context, key string, add, remove []string) error {
	var ops []map[string]string
	for _, l := range add {
		if strings.ContainsAny(l, " \t") {
			return ErrLabelSpace
		}
		ops = append(ops, map[string]string{"add": l})
	}
	for _, l := range remove {
		if strings.ContainsAny(l, " \t") {
			return ErrLabelSpace
		}
		ops = append(ops, map[string]string{"remove": l})
	}
	if len(ops) == 0 || c.dryRun {
		return nil
	}
	return c.do(ctx, http.MethodPut, "rest/api/2/issue/"+url.PathEscape(key), map[string]any{"update": map[string]any{"labels": ops}}, nil)
}

// ErrNoTransition means the issue's workflow offers no transition to the status.
var ErrNoTransition = errors.New("jira: no transition to that status from here")

// Transition moves the issue to the named status (by the transition's target
// status first, else by the transition's own name).
func (c *Client) Transition(ctx context.Context, key, status string) error {
	if status == "" || c.dryRun {
		return nil
	}
	var res struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	path := "rest/api/2/issue/" + url.PathEscape(key) + "/transitions"
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return err
	}
	id := ""
	for _, tr := range res.Transitions {
		if strings.EqualFold(tr.To.Name, status) {
			id = tr.ID
			break
		}
	}
	if id == "" {
		for _, tr := range res.Transitions {
			if strings.EqualFold(tr.Name, status) {
				id = tr.ID
				break
			}
		}
	}
	if id == "" {
		return ErrNoTransition
	}
	return c.do(ctx, http.MethodPost, path, map[string]any{"transition": map[string]string{"id": id}}, nil)
}

// ---- webhooks -----------------------------------------------------------------

// ErrHookPermission means only a Jira administrator can register the webhook:
// add it by hand (System → WebHooks) or rely on reconcile.
var ErrHookPermission = errors.New("jira: only an administrator can register webhooks")

// EnsureWebhook registers the projects' hook through the admin API.
func (c *Client) EnsureWebhook(ctx context.Context, hookURL, secret, existing string) (string, error) {
	body := map[string]any{
		"name":        "open-conduit " + strings.Join(c.s.Projects, ","),
		"url":         hookURL,
		"events":      []string{"jira:issue_created", "jira:issue_updated", "comment_created", "comment_updated"},
		"filters":     map[string]string{"issue-related-events-section": c.ProjectJQL()},
		"secret":      secret,
		"excludeBody": false,
	}
	if c.dryRun {
		if existing == "" {
			existing = "dry-run"
		}
		return existing, nil
	}
	var res struct {
		Self string `json:"self"`
	}
	manual := func(err error) error {
		if errors.Is(err, tracker.ErrCredential) || tracker.IsStatus(err, http.StatusForbidden) {
			return ErrHookPermission
		}
		return err
	}
	if existing != "" {
		err := c.do(ctx, http.MethodPut, "rest/webhooks/1.0/webhook/"+url.PathEscape(existing), body, &res)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, tracker.ErrNotFound) {
			return "", manual(err)
		}
	}
	if err := c.do(ctx, http.MethodPost, "rest/webhooks/1.0/webhook", body, &res); err != nil {
		return "", manual(err)
	}
	id := res.Self[strings.LastIndex(res.Self, "/")+1:]
	if id == "" {
		return "", errors.New("jira: the webhook was registered without an id")
	}
	return id, nil
}

var deliveryID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// ParseHook checks the delivery's signature (X-Hub-Signature, HMAC-SHA256)
// and says what it is about. Unlike Open Supervisor, an unsigned delivery is
// refused: reconcile recovers whatever it would have reported.
func (c *Client) ParseHook(h http.Header, body []byte, secret string, _ time.Time) (tracker.Hook, error) {
	if !tracker.VerifyHMAC(secret, body, h.Get("X-Hub-Signature")) {
		return tracker.Hook{}, tracker.ErrSignature
	}
	var p struct {
		WebhookEvent string `json:"webhookEvent"`
		Issue        *struct {
			Key string `json:"key"`
		} `json:"issue"`
		Comment *struct {
			ID string `json:"id"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return tracker.Hook{}, errors.New("jira: a delivery that isn't JSON")
	}
	hook := tracker.Hook{Event: p.WebhookEvent}
	if id := h.Get("X-Atlassian-Webhook-Identifier"); deliveryID.MatchString(id) {
		hook.ID = id
	} else {
		hook.ID = tracker.Digest(string(body))[:32]
	}
	if p.Issue == nil || !c.Owns(p.Issue.Key) {
		return hook, nil
	}
	key := strings.ToUpper(p.Issue.Key)
	switch p.WebhookEvent {
	case "jira:issue_created", "jira:issue_updated":
		hook.Issues = []string{key}
	case "comment_created", "comment_updated":
		if p.Comment != nil && p.Comment.ID != "" {
			hook.Comments = []tracker.CommentRef{{Issue: key, ID: p.Comment.ID}}
		}
	}
	return hook, nil
}

// PausedUntil reports a rate limit in force (doctor).
func (c *Client) PausedUntil() (time.Time, bool) { return c.api.PausedUntil() }
