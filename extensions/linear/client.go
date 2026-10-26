package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/extensions/tracker"
)

// ClientOptions tune a Client.
type ClientOptions struct {
	HTTP *http.Client
	Now  func() time.Time
	// DryRun makes every write a no-op (reads still go to Linear).
	DryRun bool
}

// Client is a Linear workspace bound to one entry's teams. It implements
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

	mu     sync.Mutex
	labels map[string]string // lowercased name -> id
	states map[string]rawState
}

var _ tracker.Service = (*Client)(nil)

// NewClient binds a connection to an entry.
func NewClient(conn ConnectionConfig, s Settings, secrets tracker.SecretFunc, o ClientOptions) (*Client, error) {
	if len(s.Teams) == 0 {
		return nil, errors.New("linear client: no teams")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	quoted := make([]string, len(s.Teams))
	for i, t := range s.Teams {
		quoted[i] = regexp.QuoteMeta(t)
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
			// OAuth tokens go as bearer tokens; personal API keys bare.
			if strings.HasPrefix(token, "lin_oauth_") {
				r.Header.Set("Authorization", "Bearer "+token)
			} else {
				r.Header.Set("Authorization", token)
			}
			return nil
		}}
	return c, nil
}

// query runs a GraphQL query or mutation and decodes its data into out.
// Linear answers a bad query or a rate limit with a 400 and the errors in
// the body; neither the body nor the messages ever reach an error.
func (c *Client) query(ctx context.Context, q string, vars map[string]any, out any) error {
	resp, err := c.api.Raw(ctx, http.MethodPost, c.conn.APIURL, map[string]any{"query": q, "variables": vars})
	if err != nil {
		return err
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal(resp.Body, &res) != nil && (resp.Status < 200 || resp.Status > 299) {
		return &tracker.StatusError{Status: resp.Status}
	}
	for _, e := range res.Errors {
		switch strings.ToUpper(e.Extensions.Code) {
		case "RATELIMITED":
			until := c.now().Add(time.Minute)
			if ms, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Requests-Reset"), 10, 64); err == nil && ms > 0 {
				if t := time.UnixMilli(ms); t.After(c.now()) && t.Before(c.now().Add(24*time.Hour)) {
					until = t
				}
			}
			c.api.Pause(until)
			return &tracker.RateLimitError{Until: until}
		case "AUTHENTICATION_ERROR":
			c.cred.Invalidate()
			return tracker.ErrCredential
		}
		if strings.Contains(strings.ToLower(e.Message), "not found") || strings.Contains(strings.ToLower(e.Message), "entity not found") {
			return tracker.ErrNotFound
		}
	}
	if len(res.Errors) > 0 {
		return errors.New("linear: the query was refused")
	}
	if resp.Status < 200 || resp.Status > 299 {
		return &tracker.StatusError{Status: resp.Status}
	}
	if out == nil || len(res.Data) == 0 || string(res.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return errors.New("linear: unexpected answer")
	}
	return nil
}

// Verify reads the signed-in account.
func (c *Client) Verify(ctx context.Context) (string, error) {
	var r struct {
		Viewer struct {
			Name string `json:"name"`
		} `json:"viewer"`
	}
	if err := c.query(ctx, `query { viewer { name email } }`, nil, &r); err != nil {
		return "", err
	}
	return "signed in as " + r.Viewer.Name, nil
}

// owns reports whether an issue identifier is in one of the entry's teams.
func (c *Client) Owns(key string) bool {
	for _, team := range c.s.Teams {
		if strings.HasPrefix(strings.ToUpper(key), team+"-") {
			return true
		}
	}
	return false
}

// IssueURL is the issue's page; Linear redirects /issue/<key> to the workspace's own.
func (c *Client) IssueURL(key string) string { return "https://linear.app/issue/" + key }

// RefFor uses one of Linear's magic words, so the PR links and closes the issue.
func (c *Client) RefFor(key string) string { return "Closes " + key }

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

// ---- issues ---------------------------------------------------------------

type rawUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type rawState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type rawIssue struct {
	ID            string    `json:"id"`
	Identifier    string    `json:"identifier"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	PriorityLabel string    `json:"priorityLabel"`
	Estimate      *float64  `json:"estimate"`
	State         *rawState `json:"state"`
	Team          *struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`
	Project *struct {
		Name string `json:"name"`
	} `json:"project"`
	Cycle *struct {
		Number int    `json:"number"`
		Name   string `json:"name"`
	} `json:"cycle"`
	Assignee *rawUser `json:"assignee"`
	Creator  *rawUser `json:"creator"`
	Labels   struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Parent *struct {
		Identifier string `json:"identifier"`
	} `json:"parent"`
}

const issueFields = `id identifier title description url createdAt updatedAt priorityLabel estimate
	state { id name type } team { key name } project { name } cycle { number name }
	assignee { id name displayName email } creator { id name displayName email }
	labels(first: 100) { nodes { id name } } parent { identifier }`

// ClosedTypes are the state types an issue is closed in.
var ClosedTypes = []string{"completed", "canceled", "duplicate"}

func closedType(typ string) bool {
	for _, c := range ClosedTypes {
		if strings.EqualFold(typ, c) {
			return true
		}
	}
	return false
}

func first(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func (c *Client) claimable(s *rawState) bool {
	if s == nil {
		return false
	}
	for _, st := range c.s.States {
		if st == strings.ToLower(s.Type) || st == strings.ToLower(s.Name) {
			return true
		}
	}
	return false
}

// issue is the raw issue with the properties routes can match: team,
// project, statetype, priority, estimate, cycle, assignee, creator and parent.
func (c *Client) issue(r rawIssue) tracker.Issue {
	i := tracker.Issue{Key: strings.ToUpper(r.Identifier), Title: r.Title, Body: r.Description, URL: r.URL,
		Open: r.State == nil || !closedType(r.State.Type), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
		Attrs: map[string][]string{}}
	for _, l := range r.Labels.Nodes {
		i.Labels = append(i.Labels, l.Name)
	}
	set := func(name string, vs ...string) {
		for _, v := range vs {
			if v != "" {
				i.Attrs[name] = append(i.Attrs[name], v)
			}
		}
	}
	if r.Creator != nil {
		i.Author = first(r.Creator.DisplayName, r.Creator.Name)
		set("creator", r.Creator.Name, r.Creator.DisplayName)
	}
	if r.Team != nil {
		set("team", r.Team.Key, r.Team.Name)
	}
	if r.Project != nil {
		set("project", r.Project.Name)
	}
	if r.State != nil {
		i.Status = r.State.Name
		set("statetype", r.State.Type)
	}
	set("priority", r.PriorityLabel)
	if r.Estimate != nil {
		set("estimate", strconv.FormatFloat(*r.Estimate, 'f', -1, 64))
	}
	if r.Cycle != nil {
		set("cycle", strconv.Itoa(r.Cycle.Number), r.Cycle.Name)
	}
	if r.Assignee != nil {
		set("assignee", r.Assignee.Name, r.Assignee.DisplayName)
	}
	if r.Parent != nil {
		set("parent", r.Parent.Identifier)
	}
	i.Waiting = i.Open && !c.claimable(r.State)
	return i
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// ListIssues lists the teams' issues in creation order: open ones and those
// updated since closedSince. Issues outside the states to claim from are
// Waiting: listed, so held issues aren't taken for closed, never claimed.
func (c *Client) ListIssues(ctx context.Context, closedSince time.Time, cursor string) (tracker.IssuePage, error) {
	q := `query($teams: [String!], $since: DateTimeOrDuration, $after: String, $first: Int) {
		issues(first: $first, after: $after, orderBy: createdAt,
			filter: { team: { key: { in: $teams } }, or: [ { state: { type: { nin: ["completed", "canceled", "duplicate"] } } }, { updatedAt: { gte: $since } } ] }) {
			nodes { ` + issueFields + ` }
			pageInfo { hasNextPage endCursor }
		}
	}`
	vars := map[string]any{"teams": c.s.Teams, "since": closedSince.UTC().Format(time.RFC3339), "first": c.pageSize}
	if cursor != "" {
		vars["after"] = cursor
	}
	var r struct {
		Issues struct {
			Nodes    []rawIssue `json:"nodes"`
			PageInfo pageInfo   `json:"pageInfo"`
		} `json:"issues"`
	}
	if err := c.query(ctx, q, vars, &r); err != nil {
		return tracker.IssuePage{}, err
	}
	page := tracker.IssuePage{}
	for _, raw := range r.Issues.Nodes {
		page.Issues = append(page.Issues, c.issue(raw))
	}
	sort.SliceStable(page.Issues, func(i, j int) bool { return page.Issues[i].CreatedAt.Before(page.Issues[j].CreatedAt) })
	if r.Issues.PageInfo.HasNextPage {
		if r.Issues.PageInfo.EndCursor == "" {
			return tracker.IssuePage{}, errors.New("linear: a page without its cursor")
		}
		page.Next = r.Issues.PageInfo.EndCursor
	}
	return page, nil
}

func (c *Client) raw(ctx context.Context, key string) (rawIssue, error) {
	var r struct {
		Issue *rawIssue `json:"issue"`
	}
	if err := c.query(ctx, `query($id: String!) { issue(id: $id) { `+issueFields+` } }`, map[string]any{"id": key}, &r); err != nil {
		return rawIssue{}, err
	}
	if r.Issue == nil || r.Issue.ID == "" {
		return rawIssue{}, tracker.ErrNotFound
	}
	return *r.Issue, nil
}

// Issue reads one issue.
func (c *Client) Issue(ctx context.Context, key string) (tracker.Issue, error) {
	r, err := c.raw(ctx, key)
	if err != nil {
		return tracker.Issue{}, err
	}
	return c.issue(r), nil
}

// Reopens lists every time the issue left a closed state.
func (c *Client) Reopens(ctx context.Context, key string) ([]time.Time, error) {
	var r struct {
		Issue *struct {
			History struct {
				Nodes []struct {
					CreatedAt time.Time `json:"createdAt"`
					FromState *rawState `json:"fromState"`
					ToState   *rawState `json:"toState"`
				} `json:"nodes"`
			} `json:"history"`
		} `json:"issue"`
	}
	q := `query($id: String!) { issue(id: $id) { history(first: 100) { nodes { createdAt fromState { type } toState { type } } } } }`
	if err := c.query(ctx, q, map[string]any{"id": key}, &r); err != nil {
		return nil, err
	}
	if r.Issue == nil {
		return nil, tracker.ErrNotFound
	}
	var out []time.Time
	for _, h := range r.Issue.History.Nodes {
		if h.FromState != nil && h.ToState != nil && closedType(h.FromState.Type) && !closedType(h.ToState.Type) {
			out = append(out, h.CreatedAt.UTC())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

// ---- comments ---------------------------------------------------------------

type rawComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	User      *rawUser  `json:"user"`
	BotActor  *struct {
		Name string `json:"name"`
	} `json:"botActor"`
	Issue *struct {
		Identifier string `json:"identifier"`
	} `json:"issue"`
}

const commentFields = `id body url createdAt updatedAt user { id name displayName email } botActor { name } issue { identifier }`

func (c *Client) comment(r rawComment) tracker.Comment {
	out := tracker.Comment{ID: r.ID, Body: r.Body, URL: r.URL, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
	if r.User != nil {
		out.Author = first(r.User.DisplayName, r.User.Name)
		if a := c.conn.AgentAccount; a != "" && (a == r.User.ID || strings.EqualFold(a, r.User.Name) || strings.EqualFold(a, r.User.DisplayName) || strings.EqualFold(a, r.User.Email)) {
			out.Agent = &tracker.Agent{}
		}
	}
	if r.BotActor != nil && r.User == nil {
		out.Author, out.Bot = r.BotActor.Name, true
	}
	if a := ParseMark(r.Body); a != nil {
		out.Agent = a
	}
	return out
}

// Comments lists every comment on the issue, oldest first.
func (c *Client) Comments(ctx context.Context, key string) ([]tracker.Comment, error) {
	q := `query($id: String!, $after: String) { issue(id: $id) {
		comments(first: 100, after: $after, orderBy: createdAt) { nodes { ` + commentFields + ` } pageInfo { hasNextPage endCursor } } } }`
	var out []tracker.Comment
	var after any
	for n := 0; n < 50; n++ {
		var r struct {
			Issue *struct {
				Comments struct {
					Nodes    []rawComment `json:"nodes"`
					PageInfo pageInfo     `json:"pageInfo"`
				} `json:"comments"`
			} `json:"issue"`
		}
		if err := c.query(ctx, q, map[string]any{"id": key, "after": after}, &r); err != nil {
			return nil, err
		}
		if r.Issue == nil {
			return nil, tracker.ErrNotFound
		}
		for _, raw := range r.Issue.Comments.Nodes {
			out = append(out, c.comment(raw))
		}
		if !r.Issue.Comments.PageInfo.HasNextPage {
			sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
			return out, nil
		}
		if r.Issue.Comments.PageInfo.EndCursor == "" {
			break
		}
		after = r.Issue.Comments.PageInfo.EndCursor
	}
	// An unfinished listing is an error, never a short success.
	return nil, errors.New("linear: comment listing did not finish")
}

// ReadComment reads one comment and its issue's key.
func (c *Client) ReadComment(ctx context.Context, ref tracker.CommentRef) (string, tracker.Comment, error) {
	var r struct {
		Comment *rawComment `json:"comment"`
	}
	if err := c.query(ctx, `query($id: String!) { comment(id: $id) { `+commentFields+` } }`, map[string]any{"id": ref.ID}, &r); err != nil {
		return "", tracker.Comment{}, err
	}
	if r.Comment == nil || r.Comment.Issue == nil {
		return "", tracker.Comment{}, tracker.ErrNotFound
	}
	return strings.ToUpper(r.Comment.Issue.Identifier), c.comment(*r.Comment), nil
}

// PostComment posts on the issue with the footer that marks it as an agent's.
func (c *Client) PostComment(ctx context.Context, key, body string, m tracker.Mark) (tracker.Posted, error) {
	if c.dryRun {
		return tracker.Posted{}, nil
	}
	issue, err := c.raw(ctx, key)
	if err != nil {
		return tracker.Posted{}, err
	}
	var r struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	q := `mutation($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { id url } } }`
	if err := c.query(ctx, q, map[string]any{"input": map[string]any{"issueId": issue.ID, "body": WithMark(body, m)}}, &r); err != nil {
		return tracker.Posted{}, err
	}
	if !r.CommentCreate.Success || r.CommentCreate.Comment.ID == "" {
		return tracker.Posted{}, errors.New("linear: the comment was not confirmed")
	}
	return tracker.Posted{ID: r.CommentCreate.Comment.ID, URL: r.CommentCreate.Comment.URL}, nil
}

// CreateIssue opens an issue in the entry's first team.
func (c *Client) CreateIssue(ctx context.Context, n tracker.NewIssue) (tracker.Issue, error) {
	if c.dryRun {
		return tracker.Issue{}, nil
	}
	var teams struct {
		Teams struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.query(ctx, `query($key: String!) { teams(filter: { key: { eq: $key } }) { nodes { id } } }`, map[string]any{"key": c.s.Teams[0]}, &teams); err != nil {
		return tracker.Issue{}, err
	}
	if len(teams.Teams.Nodes) == 0 {
		return tracker.Issue{}, tracker.ErrNotFound
	}
	input := map[string]any{"teamId": teams.Teams.Nodes[0].ID, "title": n.Title, "description": n.Body}
	if len(n.Labels) > 0 {
		ids, err := c.labelIDs(ctx, false)
		if err != nil {
			return tracker.Issue{}, err
		}
		for _, l := range n.Labels {
			if _, ok := ids[strings.ToLower(l)]; !ok {
				if ids, err = c.labelIDs(ctx, true); err != nil {
					return tracker.Issue{}, err
				}
				break
			}
		}
		want := []string{}
		for _, l := range n.Labels {
			if id, ok := ids[strings.ToLower(l)]; ok {
				want = append(want, id)
			}
		}
		input["labelIds"] = want
	}
	var r struct {
		IssueCreate struct {
			Issue rawIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	q := `mutation($input: IssueCreateInput!) { issueCreate(input: $input) { issue { ` + issueFields + ` } } }`
	if err := c.query(ctx, q, map[string]any{"input": input}, &r); err != nil {
		return tracker.Issue{}, err
	}
	return c.issue(r.IssueCreate.Issue), nil
}

// ---- labels and workflow ------------------------------------------------------

func (c *Client) ownsTeam(key string) bool {
	for _, team := range c.s.Teams {
		if strings.EqualFold(team, key) {
			return true
		}
	}
	return false
}

// labelIDs reads the workspace's labels and the teams', by lowercased name.
func (c *Client) labelIDs(ctx context.Context, fresh bool) (map[string]string, error) {
	c.mu.Lock()
	cached := c.labels
	c.mu.Unlock()
	if cached != nil && !fresh {
		return cached, nil
	}
	q := `query($after: String) { issueLabels(first: 250, after: $after) { nodes { id name team { key } } pageInfo { hasNextPage endCursor } } }`
	out := map[string]string{}
	var after any
	for n := 0; ; n++ {
		if n >= 20 {
			return nil, errors.New("linear: label listing did not finish")
		}
		var r struct {
			IssueLabels struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Team *struct {
						Key string `json:"key"`
					} `json:"team"`
				} `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"issueLabels"`
		}
		if err := c.query(ctx, q, map[string]any{"after": after}, &r); err != nil {
			return nil, err
		}
		for _, l := range r.IssueLabels.Nodes {
			name := strings.ToLower(l.Name)
			// A workspace label serves every team; a team's own only that team.
			if l.Team == nil {
				out[name] = l.ID
			} else if _, taken := out[name]; !taken && c.ownsTeam(l.Team.Key) {
				out[name] = l.ID
			}
		}
		if !r.IssueLabels.PageInfo.HasNextPage {
			break
		}
		after = r.IssueLabels.PageInfo.EndCursor
	}
	c.mu.Lock()
	c.labels = out
	c.mu.Unlock()
	return out, nil
}

// EditLabels adds and removes labels by name, creating a missing workspace
// label the projector sets (Linear labels must exist before use).
func (c *Client) EditLabels(ctx context.Context, key string, add, remove []string) error {
	if c.dryRun {
		return nil
	}
	issue, err := c.raw(ctx, key)
	if err != nil {
		return err
	}
	ids, err := c.labelIDs(ctx, false)
	if err != nil {
		return err
	}
	var missing []string
	for _, name := range add {
		if _, ok := ids[strings.ToLower(name)]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		if ids, err = c.labelIDs(ctx, true); err != nil {
			return err
		}
		created := false
		for _, name := range missing {
			if _, ok := ids[strings.ToLower(name)]; ok {
				continue
			}
			q := `mutation($input: IssueLabelCreateInput!) { issueLabelCreate(input: $input) { success } }`
			if err := c.query(ctx, q, map[string]any{"input": map[string]any{"name": name}}, nil); err != nil {
				return err
			}
			created = true
		}
		if created {
			if ids, err = c.labelIDs(ctx, true); err != nil {
				return err
			}
		}
	}
	set := map[string]bool{}
	var order []string
	for _, l := range issue.Labels.Nodes {
		set[l.ID] = true
		order = append(order, l.ID)
	}
	changed := false
	for _, name := range remove {
		for _, l := range issue.Labels.Nodes {
			if strings.EqualFold(l.Name, name) && set[l.ID] {
				delete(set, l.ID)
				changed = true
			}
		}
	}
	for _, name := range add {
		id, ok := ids[strings.ToLower(name)]
		if !ok {
			return fmt.Errorf("linear: label could not be created")
		}
		if !set[id] {
			set[id] = true
			order = append(order, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	labelIDs := []string{}
	for _, id := range order {
		if set[id] {
			labelIDs = append(labelIDs, id)
		}
	}
	q := `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
	return c.query(ctx, q, map[string]any{"id": issue.ID, "input": map[string]any{"labelIds": labelIDs}}, nil)
}

// workflowStates reads the teams' workflow states, by lowercased "TEAM/name".
func (c *Client) workflowStates(ctx context.Context) (map[string]rawState, error) {
	c.mu.Lock()
	cached := c.states
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	var r struct {
		WorkflowStates struct {
			Nodes []struct {
				rawState
				Team struct {
					Key string `json:"key"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"workflowStates"`
	}
	q := `query($teams: [String!]) { workflowStates(first: 250, filter: { team: { key: { in: $teams } } }) { nodes { id name type team { key } } } }`
	if err := c.query(ctx, q, map[string]any{"teams": c.s.Teams}, &r); err != nil {
		return nil, err
	}
	out := map[string]rawState{}
	for _, s := range r.WorkflowStates.Nodes {
		out[strings.ToLower(s.Team.Key+"/"+s.Name)] = s.rawState
	}
	c.mu.Lock()
	c.states = out
	c.mu.Unlock()
	return out, nil
}

// ErrNoState means the team has no workflow state with the configured name.
var ErrNoState = errors.New("linear: the team has no such workflow state")

// Transition moves the issue to the named workflow state of its team.
func (c *Client) Transition(ctx context.Context, key, status string) error {
	if status == "" || c.dryRun {
		return nil
	}
	team, _, _ := strings.Cut(strings.ToUpper(key), "-")
	states, err := c.workflowStates(ctx)
	if err != nil {
		return err
	}
	target, ok := states[strings.ToLower(team+"/"+status)]
	if !ok {
		return ErrNoState
	}
	issue, err := c.raw(ctx, key)
	if err != nil {
		return err
	}
	if issue.State != nil && issue.State.ID == target.ID {
		return nil
	}
	q := `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
	return c.query(ctx, q, map[string]any{"id": issue.ID, "input": map[string]any{"stateId": target.ID}}, nil)
}

// ---- webhooks -----------------------------------------------------------------

var resourceTypes = []string{"Issue", "Comment"}

// EnsureWebhook points the hook at url: one team's hook, or every public
// team's for several (deliveries for other teams are dropped). Creating
// webhooks needs a workspace admin.
func (c *Client) EnsureWebhook(ctx context.Context, url, secret, existing string) (string, error) {
	if c.dryRun {
		if existing == "" {
			existing = "dry-run"
		}
		return existing, nil
	}
	if existing != "" {
		var r struct {
			WebhookUpdate struct {
				Success bool `json:"success"`
			} `json:"webhookUpdate"`
		}
		q := `mutation($id: String!, $input: WebhookUpdateInput!) { webhookUpdate(id: $id, input: $input) { success } }`
		err := c.query(ctx, q, map[string]any{"id": existing, "input": map[string]any{"url": url, "secret": secret, "resourceTypes": resourceTypes, "enabled": true}}, &r)
		if err == nil && r.WebhookUpdate.Success {
			return existing, nil
		}
		// Only a hook that is gone is made again; any other failure would leave the old one behind.
		if err != nil && !errors.Is(err, tracker.ErrNotFound) {
			return "", err
		}
	}
	input := map[string]any{"url": url, "secret": secret, "resourceTypes": resourceTypes, "label": "open-conduit " + strings.Join(c.s.Teams, ","), "enabled": true}
	if len(c.s.Teams) == 1 {
		input["teamId"] = c.s.Teams[0]
	} else {
		input["allPublicTeams"] = true
	}
	var r struct {
		WebhookCreate struct {
			Success bool `json:"success"`
			Webhook struct {
				ID string `json:"id"`
			} `json:"webhook"`
		} `json:"webhookCreate"`
	}
	q := `mutation($input: WebhookCreateInput!) { webhookCreate(input: $input) { success webhook { id } } }`
	if err := c.query(ctx, q, map[string]any{"input": input}, &r); err != nil {
		return "", err
	}
	if r.WebhookCreate.Webhook.ID == "" {
		return "", errors.New("linear: the webhook was made without an id")
	}
	return r.WebhookCreate.Webhook.ID, nil
}

// MaxAge is how old a delivery may be before it is taken for a replay.
const MaxAge = 10 * time.Minute

type delivery struct {
	Action           string          `json:"action"`
	Type             string          `json:"type"`
	Data             json.RawMessage `json:"data"`
	WebhookTimestamp int64           `json:"webhookTimestamp"`
}

var deliveryID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// ParseHook checks Linear's signature (hex HMAC-SHA256 of the body, header
// Linear-Signature) and the delivery's age, and says what it is about. Push
// reads the issue or comment back so it is normalized as reconcile would.
func (c *Client) ParseHook(h http.Header, body []byte, secret string, now time.Time) (tracker.Hook, error) {
	if !tracker.VerifyHMAC(secret, body, h.Get("Linear-Signature")) {
		return tracker.Hook{}, tracker.ErrSignature
	}
	var p delivery
	if err := json.Unmarshal(body, &p); err != nil {
		return tracker.Hook{}, errors.New("linear: a delivery that isn't JSON")
	}
	if p.WebhookTimestamp > 0 && now.Sub(time.UnixMilli(p.WebhookTimestamp)) > MaxAge {
		return tracker.Hook{}, tracker.ErrSignature
	}
	hook := tracker.Hook{Event: p.Type + ":" + p.Action}
	if id := h.Get("Linear-Delivery"); deliveryID.MatchString(id) {
		hook.ID = id
	} else {
		hook.ID = tracker.Digest(string(body))[:32]
	}
	switch p.Type {
	case "Issue":
		var d struct {
			Identifier string `json:"identifier"`
		}
		if json.Unmarshal(p.Data, &d) == nil && d.Identifier != "" && c.Owns(d.Identifier) {
			hook.Issues = []string{strings.ToUpper(d.Identifier)}
		}
	case "Comment":
		var d struct {
			ID    string `json:"id"`
			Issue *struct {
				Identifier string `json:"identifier"`
			} `json:"issue"`
		}
		if p.Action != "remove" && json.Unmarshal(p.Data, &d) == nil && d.ID != "" {
			ref := tracker.CommentRef{ID: d.ID}
			if d.Issue != nil {
				ref.Issue = strings.ToUpper(d.Issue.Identifier)
			}
			hook.Comments = []tracker.CommentRef{ref}
		}
	}
	return hook, nil
}

// PausedUntil reports a rate limit in force (doctor).
func (c *Client) PausedUntil() (time.Time, bool) { return c.api.PausedUntil() }
