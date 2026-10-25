package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// Comment kinds carried in CommentPayload.Kind.
const (
	KindIssue         = "issue"
	KindPullRequest   = "pull_request"
	KindReviewComment = "review_comment"
	KindReview        = "review"
)

// Event families. Types name observed state (or author class / check outcome)
// rather than webhook actions, because reconcile sees state, not actions.
const (
	TypeIssueOpen     = "github.issue.open"
	TypeIssueClosed   = "github.issue.closed"
	TypePullOpen      = "github.pull_request.open"
	TypePullClosed    = "github.pull_request.closed"
	TypePullMerged    = "github.pull_request.merged"
	TypeCommentHuman  = "github.comment.human"
	TypeCommentBot    = "github.comment.bot"
	TypeCommentAgent  = "github.comment.agent"
	TypeChecksPending = "github.checks.pending"
	TypeChecksPassed  = "github.checks.passed"
	TypeChecksFailed  = "github.checks.failed"
	TypeChecksNone    = "github.checks.none"
	TypeDelivery      = "github.delivery"
)

// Payload schemas.
const (
	SchemaIssue    = "github.issue.v1"
	SchemaPull     = "github.pull_request.v1"
	SchemaComment  = "github.comment.v1"
	SchemaChecks   = "github.checks.v1"
	SchemaDelivery = "github.delivery.v1"
)

// DeliveryLoss is the declared loss limitation of raw webhook deliveries.
const DeliveryLoss = "Raw webhook deliveries (github.delivery, the trigger input for webhook jobs) are not replayed by reconcile: GitHub keeps no readable per-delivery history for them. The issue, pull request, comment, review and check state they carry is recovered by reconcile as canonical observations; a raw delivery missed while push is unavailable is lost."

// Families declares every emitted event family.
func Families() []extension.EventFamily {
	var out []extension.EventFamily
	for _, t := range []string{TypeIssueOpen, TypeIssueClosed, TypePullOpen, TypePullClosed, TypePullMerged,
		TypeCommentHuman, TypeCommentBot, TypeCommentAgent, TypeChecksPending, TypeChecksPassed, TypeChecksFailed, TypeChecksNone} {
		out = append(out, extension.EventFamily{Type: t, ReDerivable: true})
	}
	return append(out, extension.EventFamily{Type: TypeDelivery, Loss: DeliveryLoss})
}

// IssuePayload is the canonical observed issue state. It deliberately omits
// updated_at, comment counts and projected status labels, so neither comments
// nor status projection create issue observations. Lifecycle recurrences stay
// distinct through closed_at and the reopen generation.
type IssuePayload struct {
	Repo        string     `json:"repo"`
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	StateReason string     `json:"state_reason,omitempty"`
	Labels      []string   `json:"labels"`
	Assignees   []string   `json:"assignees"`
	Milestone   string     `json:"milestone,omitempty"`
	IssueType   string     `json:"issue_type,omitempty"`
	Author      string     `json:"author"`
	AuthorType  string     `json:"author_type,omitempty"`
	URL         string     `json:"url"`
	CreatedAt   time.Time  `json:"created_at"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	// Generation is the reopen generation ("" before any reopening);
	// Predecessor is the previous generation's external ID.
	Generation  string `json:"generation,omitempty"`
	Predecessor string `json:"predecessor,omitempty"`
}

// IssueRef names a tracker issue a pull request belongs to.
type IssueRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// PullPayload is the canonical observed pull request state.
type PullPayload struct {
	Repo      string     `json:"repo"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	Draft     bool       `json:"draft"`
	HeadRef   string     `json:"head_ref"`
	HeadSHA   string     `json:"head_sha"`
	BaseRef   string     `json:"base_ref"`
	Labels    []string   `json:"labels"`
	Author    string     `json:"author"`
	URL       string     `json:"url"`
	CreatedAt time.Time  `json:"created_at"`
	Issue     *IssueRef  `json:"issue,omitempty"`
}

// CommentPayload is a conversation comment, inline review comment or review.
type CommentPayload struct {
	Repo       string    `json:"repo"`
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Number     int       `json:"number"`
	Author     string    `json:"author"`
	AuthorType string    `json:"author_type,omitempty"`
	Body       string    `json:"body"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Edited     bool      `json:"edited,omitempty"`
	State      string    `json:"state,omitempty"`
	Path       string    `json:"path,omitempty"`
	Line       int       `json:"line,omitempty"`
	InReplyTo  int64     `json:"in_reply_to,omitempty"`
	CommitID   string    `json:"commit_id,omitempty"`
	Agent      *Agent    `json:"agent,omitempty"`
}

// CheckRun is one check run or commit status in a checks observation.
type CheckRun struct {
	Kind  string `json:"kind"`
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// ChecksPayload is the evaluated check state of a pull request's head.
type ChecksPayload struct {
	Repo     string     `json:"repo"`
	Number   int        `json:"number"`
	SHA      string     `json:"sha"`
	Complete bool       `json:"complete"`
	Failed   []string   `json:"failed"`
	Preview  string     `json:"preview,omitempty"`
	Runs     []CheckRun `json:"runs"`
	// None marks a head that has had no checks at all for no_checks_minutes
	// (the repository has no CI): v1's "No checks ran".
	None bool `json:"none,omitempty"`
}

// DeliveryPayload is a raw webhook delivery (non-re-derivable).
type DeliveryPayload struct {
	Event    string   `json:"event"`
	Action   string   `json:"action,omitempty"`
	Delivery string   `json:"delivery"`
	Repo     string   `json:"repo"`
	Ref      string   `json:"ref,omitempty"`
	Before   string   `json:"before,omitempty"`
	After    string   `json:"after,omitempty"`
	Files    []string `json:"files,omitempty"`
}

// IssueKey is the correlation external ID of an issue generation.
func IssueKey(number int, generation string) string {
	if generation == "" {
		return "issue/" + strconv.Itoa(number)
	}
	return "issue/" + strconv.Itoa(number) + "/generation/" + generation
}

// PullKey is the correlation external ID of a PR no tracker issue claims.
func PullKey(number int) string { return "pr/" + strconv.Itoa(number) }

var issueKeyPattern = regexp.MustCompile(`^issue/([0-9]+)(?:/generation/(reopen-[0-9]+))?$`)

// ParseIssueKey extracts the issue number and generation from an external ID.
func ParseIssueKey(id string) (int, string, bool) {
	m := issueKeyPattern.FindStringSubmatch(id)
	if m == nil {
		return 0, "", false
	}
	n, ok := parseNumber(m[1])
	return n, m[2], ok
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func utc(t time.Time) time.Time { return t.UTC() }
func utcPtr(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// memo caches service reads for one push or one scan generation. It is a
// performance aid only; identity never depends on what it contains.
type memo struct {
	mu       sync.Mutex
	issues   map[string]rawIssue
	pulls    map[int]rawPull
	reopens  map[string][]rawIssueEvent
	notFound map[string]bool
}

func newMemo() *memo {
	return &memo{issues: map[string]rawIssue{}, pulls: map[int]rawPull{}, reopens: map[string][]rawIssueEvent{}, notFound: map[string]bool{}}
}

// normalizer turns service objects into canonical observations. Push and
// reconcile share it, so equal external state yields equal identity.
type normalizer struct {
	s        Settings
	c        *Client
	m        *memo
	received time.Time
}

var errGone = errors.New("github object gone")

func (n *normalizer) issue(ctx context.Context, repo string, number int) (rawIssue, error) {
	key := repo + "#" + strconv.Itoa(number)
	n.m.mu.Lock()
	i, ok := n.m.issues[key]
	gone := n.m.notFound["issue:"+key]
	n.m.mu.Unlock()
	if gone {
		return i, errGone
	}
	if ok {
		return i, nil
	}
	i, err := n.c.Issue(ctx, repo, number)
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusGone) {
		n.m.mu.Lock()
		n.m.notFound["issue:"+key] = true
		n.m.mu.Unlock()
		return i, errGone
	}
	if err != nil {
		return i, err
	}
	n.m.mu.Lock()
	n.m.issues[key] = i
	n.m.mu.Unlock()
	return i, nil
}

func (n *normalizer) pull(ctx context.Context, number int) (rawPull, error) {
	n.m.mu.Lock()
	p, ok := n.m.pulls[number]
	gone := n.m.notFound["pull:"+strconv.Itoa(number)]
	n.m.mu.Unlock()
	if gone {
		return p, errGone
	}
	if ok {
		return p, nil
	}
	p, err := n.c.Pull(ctx, n.s.Repo, number)
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusGone) {
		n.m.mu.Lock()
		n.m.notFound["pull:"+strconv.Itoa(number)] = true
		n.m.mu.Unlock()
		return p, errGone
	}
	if err != nil {
		return p, err
	}
	n.remember(p)
	return p, nil
}

func (n *normalizer) remember(p rawPull) {
	n.m.mu.Lock()
	n.m.pulls[p.Number] = p
	n.m.mu.Unlock()
}

func (n *normalizer) reopenings(ctx context.Context, repo string, number int) ([]rawIssueEvent, error) {
	key := repo + "#" + strconv.Itoa(number)
	n.m.mu.Lock()
	r, ok := n.m.reopens[key]
	n.m.mu.Unlock()
	if ok {
		return r, nil
	}
	r, err := n.c.reopenings(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(r, func(i, j int) bool {
		if !r[i].CreatedAt.Equal(r[j].CreatedAt) {
			return r[i].CreatedAt.Before(r[j].CreatedAt)
		}
		return r[i].ID < r[j].ID
	})
	n.m.mu.Lock()
	n.m.reopens[key] = r
	n.m.mu.Unlock()
	return r, nil
}

// generation resolves an issue's reopen generation at a time (zero: latest).
// The answer depends only on immutable history (reopened events), so push and
// reconcile agree. An open issue whose state_reason is not "reopened" has
// never been reopened and needs no history read.
func (n *normalizer) generation(ctx context.Context, repo string, issue rawIssue, at time.Time) (gen, predecessor string, reopenedAt time.Time, err error) {
	if issue.State == "open" && !strings.EqualFold(issue.StateReason, "reopened") {
		return "", "", time.Time{}, nil
	}
	events, err := n.reopenings(ctx, repo, issue.Number)
	if err != nil {
		return "", "", time.Time{}, err
	}
	prev := IssueKey(issue.Number, "")
	for _, e := range events {
		if !at.IsZero() && e.CreatedAt.After(at) {
			break
		}
		if gen != "" {
			prev = IssueKey(issue.Number, gen)
		}
		gen, reopenedAt = "reopen-"+strconv.FormatInt(e.ID, 10), e.CreatedAt
	}
	if gen == "" {
		prev = ""
	}
	return gen, prev, reopenedAt, nil
}

// observation builds a canonical observation. The version hashes the whole
// normalized envelope, so any change is a new version and identical state is
// the same version (and delivery) from either transport.
func (n *normalizer) observation(item, typ string, key domain.CorrelationKey, occurred time.Time, schema string, payload any) (extension.Observation, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return extension.Observation{}, err
	}
	occurred = occurred.UTC()
	version := digest(typ, key.Source, key.ExternalID, occurred.Format(time.RFC3339Nano), schema, string(data))[:32]
	delivery := item + "@" + version
	k := key
	return extension.Observation{ID: item, Version: version, Event: domain.Event{
		ID: domain.ID("github-" + digest(n.s.Namespace, delivery)[:40]), Type: typ, Source: n.s.Namespace, DeliveryID: delivery,
		OccurredAt: occurred, ReceivedAt: n.received, CorrelationKey: &k, Data: domain.Payload{Schema: schema, Data: data},
	}}, nil
}

func (n *normalizer) labels(ls []rawLabel) []string {
	skip := map[string]bool{}
	for _, s := range n.s.Labels.Statuses() {
		skip[strings.ToLower(s)] = true
	}
	out := []string{}
	for _, l := range ls {
		if !skip[strings.ToLower(l.Name)] {
			out = append(out, l.Name)
		}
	}
	sort.Strings(out)
	return out
}

// issueObservation normalizes a (non-PR) issue of the tracked repository.
func (n *normalizer) issueObservation(ctx context.Context, i rawIssue) (extension.Observation, error) {
	gen, pred, reopenedAt, err := n.generation(ctx, n.s.Repo, i, time.Time{})
	if err != nil {
		return extension.Observation{}, err
	}
	p := IssuePayload{Repo: n.s.Repo, Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, StateReason: i.StateReason,
		Labels: n.labels(i.Labels), Assignees: []string{}, Author: i.User.Login, AuthorType: i.User.Type, URL: i.HTMLURL,
		CreatedAt: utc(i.CreatedAt), ClosedAt: utcPtr(i.ClosedAt), Generation: gen, Predecessor: pred}
	if i.State != "closed" {
		p.ClosedAt = nil
	}
	for _, a := range i.Assignees {
		p.Assignees = append(p.Assignees, a.Login)
	}
	sort.Strings(p.Assignees)
	if i.Milestone != nil {
		p.Milestone = i.Milestone.Title
	}
	if i.Type != nil {
		p.IssueType = i.Type.Name
	}
	typ, occurred := TypeIssueOpen, i.CreatedAt
	switch {
	case i.State == "closed":
		typ = TypeIssueClosed
		if p.ClosedAt != nil {
			occurred = *p.ClosedAt
		}
	case gen != "":
		occurred = reopenedAt
	}
	// A repository's own issues are always keyed in its own namespace (equal
	// to the tracker namespace unless a shared tracker is configured).
	key := domain.CorrelationKey{Source: n.s.Namespace, ExternalID: IssueKey(i.Number, gen)}
	return n.observation("issue/"+strconv.Itoa(i.Number), typ, key, occurred, SchemaIssue, p)
}

var refsPattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|refs?)\s+([\w.-]+/[\w.-]+)?#(\d+)\b`)

// pullIssue finds the tracker issue a PR belongs to: its branch, then a
// closing/reference keyword in its body. A shared tracker accepts only
// owner/repo#N references naming it, since a bare #N names this repository.
func (n *normalizer) pullIssue(p rawPull) (int, bool) {
	if n.s.Branch != nil {
		if m := n.s.Branch.FindStringSubmatch(p.Head.Ref); m != nil {
			if v, ok := parseNumber(m[1]); ok {
				return v, true
			}
		}
	}
	for _, m := range refsPattern.FindAllStringSubmatch(p.Body, -1) {
		if n.s.SharedTracker && !strings.EqualFold(m[1], n.s.TrackerRepo) || !n.s.SharedTracker && m[1] != "" && !strings.EqualFold(m[1], n.s.Repo) {
			continue
		}
		if v, ok := parseNumber(m[2]); ok {
			return v, true
		}
	}
	return 0, false
}

// pullKey correlates a PR (and everything on it) with its tracker issue's
// generation current when the PR was created, else with the PR itself. This
// is v1's PR-to-issue attachment, derived identically by push and reconcile.
func (n *normalizer) pullKey(ctx context.Context, p rawPull) (domain.CorrelationKey, *IssueRef, error) {
	if number, ok := n.pullIssue(p); ok {
		issue, err := n.issue(ctx, n.s.TrackerRepo, number)
		if err == nil && issue.PullRequest == nil {
			gen, _, _, err := n.generation(ctx, n.s.TrackerRepo, issue, p.CreatedAt)
			if err != nil {
				return domain.CorrelationKey{}, nil, err
			}
			return domain.CorrelationKey{Source: n.s.TrackerNS, ExternalID: IssueKey(number, gen)}, &IssueRef{Repo: n.s.TrackerRepo, Number: number}, nil
		}
		if err != nil && !errors.Is(err, errGone) {
			return domain.CorrelationKey{}, nil, err
		}
	}
	return domain.CorrelationKey{Source: n.s.Namespace, ExternalID: PullKey(p.Number)}, nil, nil
}

func (n *normalizer) pullObservation(ctx context.Context, p rawPull) (extension.Observation, error) {
	key, ref, err := n.pullKey(ctx, p)
	if err != nil {
		return extension.Observation{}, err
	}
	merged := p.MergedAt != nil
	payload := PullPayload{Repo: n.s.Repo, Number: p.Number, Title: p.Title, Body: p.Body, State: p.State, Merged: merged,
		MergedAt: utcPtr(p.MergedAt), ClosedAt: utcPtr(p.ClosedAt), Draft: p.Draft, HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA,
		BaseRef: p.Base.Ref, Labels: n.labels(p.Labels), Author: p.User.Login, URL: p.HTMLURL, CreatedAt: utc(p.CreatedAt), Issue: ref}
	if p.State != "closed" {
		payload.ClosedAt, payload.MergedAt, payload.Merged = nil, nil, false
	}
	typ, occurred := TypePullOpen, p.CreatedAt
	switch {
	case payload.Merged:
		typ, occurred = TypePullMerged, *payload.MergedAt
	case p.State == "closed":
		typ = TypePullClosed
		if payload.ClosedAt != nil {
			occurred = *payload.ClosedAt
		}
	}
	return n.observation("pr/"+strconv.Itoa(p.Number), typ, key, occurred, SchemaPull, payload)
}

func commentType(body string, user rawUser) (string, *Agent) {
	if a := ParseAgent(body); a != nil {
		return TypeCommentAgent, a
	}
	if user.Type == "Bot" {
		return TypeCommentBot, nil
	}
	return TypeCommentHuman, nil
}

func firstTime(ts ...*time.Time) time.Time {
	for _, t := range ts {
		if t != nil && !t.IsZero() {
			return t.UTC()
		}
	}
	return time.Time{}
}

func (n *normalizer) commentObservation(kind string, number int, c rawComment, key domain.CorrelationKey) (extension.Observation, error) {
	typ, agent := commentType(c.Body, c.User)
	created := firstTime(c.CreatedAt, c.SubmittedAt, c.UpdatedAt)
	updated := firstTime(c.UpdatedAt, c.SubmittedAt, c.CreatedAt)
	p := CommentPayload{Repo: n.s.Repo, ID: c.ID, Kind: kind, Number: number, Author: c.User.Login, AuthorType: c.User.Type,
		Body: c.Body, URL: c.HTMLURL, CreatedAt: created, UpdatedAt: updated, Edited: updated.After(created), Agent: agent}
	// Items nest under their issue or PR: the runtime publishes a generation's
	// changes in lexical item order, so a newly observed issue (which may
	// create Work) precedes the comments that become its input.
	parent := "issue/" + strconv.Itoa(number)
	if kind != KindIssue {
		parent = "pr/" + strconv.Itoa(number)
	}
	item := parent + "/comment/" + strconv.FormatInt(c.ID, 10)
	switch kind {
	case KindReview:
		item = parent + "/review/" + strconv.FormatInt(c.ID, 10)
		p.State, p.CommitID = c.State, c.CommitID
		created = firstTime(c.SubmittedAt, c.CreatedAt)
		p.CreatedAt, p.UpdatedAt, p.Edited = created, created, false
		updated = created
	case KindReviewComment:
		item = parent + "/review_comment/" + strconv.FormatInt(c.ID, 10)
		p.Path, p.InReplyTo, p.CommitID = c.Path, c.InReplyTo, c.CommitID
		if c.Line != nil {
			p.Line = *c.Line
		} else if c.OriginalLine != nil {
			p.Line = *c.OriginalLine
		}
	}
	if updated.IsZero() {
		return extension.Observation{}, errors.New("github comment without timestamps")
	}
	return n.observation(item, typ, key, updated, SchemaComment, p)
}

// conversationComment normalizes an issue/PR conversation comment given its
// parent issue object (the issue face of a PR for PR conversations).
func (n *normalizer) conversationComment(ctx context.Context, parent rawIssue, c rawComment) (extension.Observation, error) {
	if parent.PullRequest != nil {
		p, err := n.pull(ctx, parent.Number)
		if err != nil {
			return extension.Observation{}, err
		}
		key, _, err := n.pullKey(ctx, p)
		if err != nil {
			return extension.Observation{}, err
		}
		return n.commentObservation(KindPullRequest, parent.Number, c, key)
	}
	if n.s.SharedTracker {
		// A code repository's own issues are outside a shared tracker's claims.
		return n.commentObservation(KindIssue, parent.Number, c, domain.CorrelationKey{Source: n.s.Namespace, ExternalID: IssueKey(parent.Number, "")})
	}
	gen, _, _, err := n.generation(ctx, n.s.Repo, parent, firstTime(c.CreatedAt, c.UpdatedAt))
	if err != nil {
		return extension.Observation{}, err
	}
	return n.commentObservation(KindIssue, parent.Number, c, domain.CorrelationKey{Source: n.s.Namespace, ExternalID: IssueKey(parent.Number, gen)})
}

func (n *normalizer) pullComment(ctx context.Context, kind string, number int, c rawComment) (extension.Observation, error) {
	p, err := n.pull(ctx, number)
	if err != nil {
		return extension.Observation{}, err
	}
	key, _, err := n.pullKey(ctx, p)
	if err != nil {
		return extension.Observation{}, err
	}
	return n.commentObservation(kind, number, c, key)
}

var badConclusions = map[string]bool{"failure": true, "timed_out": true, "cancelled": true, "action_required": true, "startup_failure": true}

// checksObservation evaluates a PR head's runs and statuses (v1 semantics).
// It reports false when there is nothing to observe yet.
func (n *normalizer) checksObservation(ctx context.Context, p rawPull, runs []rawCheckRun, statuses []rawStatus) (extension.Observation, bool, error) {
	if len(runs)+len(statuses) == 0 {
		return extension.Observation{}, false, nil
	}
	key, _, err := n.pullKey(ctx, p)
	if err != nil {
		return extension.Observation{}, false, err
	}
	out := ChecksPayload{Repo: n.s.Repo, Number: p.Number, SHA: p.Head.SHA, Complete: true, Failed: []string{}, Runs: []CheckRun{}}
	failed := map[string]bool{}
	var occurred time.Time
	later := func(ts ...*time.Time) {
		for _, t := range ts {
			if t != nil && t.After(occurred) {
				occurred = t.UTC()
			}
		}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].Name != runs[j].Name {
			return runs[i].Name < runs[j].Name
		}
		return runs[i].ID < runs[j].ID
	})
	sort.SliceStable(statuses, func(i, j int) bool {
		if statuses[i].Context != statuses[j].Context {
			return statuses[i].Context < statuses[j].Context
		}
		return statuses[i].ID < statuses[j].ID
	})
	for _, r := range runs {
		state := "success"
		switch {
		case r.Status != "completed":
			state, out.Complete = "pending", false
		case badConclusions[r.Conclusion]:
			state = "failure"
			if !failed[r.Name] {
				failed[r.Name] = true
				out.Failed = append(out.Failed, r.Name)
			}
		case r.Conclusion == "success" && r.DetailsURL != "" && n.s.Preview != nil && n.s.Preview.MatchString(r.Name):
			out.Preview = r.DetailsURL
		}
		link := r.HTMLURL
		if link == "" {
			link = r.DetailsURL
		}
		later(r.StartedAt, r.CompletedAt)
		out.Runs = append(out.Runs, CheckRun{Kind: "run", ID: r.ID, Name: r.Name, State: state, URL: link})
	}
	for _, s := range statuses {
		state := "success"
		switch s.State {
		case "pending":
			state, out.Complete = "pending", false
		case "failure", "error":
			state = "failure"
			if !failed[s.Context] {
				failed[s.Context] = true
				out.Failed = append(out.Failed, s.Context)
			}
		case "success":
			if s.TargetURL != "" && n.s.Preview != nil && n.s.Preview.MatchString(s.Context) {
				out.Preview = s.TargetURL
			}
		}
		later(s.CreatedAt, s.UpdatedAt)
		out.Runs = append(out.Runs, CheckRun{Kind: "status", ID: s.ID, Name: s.Context, State: state, URL: s.TargetURL})
	}
	if occurred.IsZero() {
		occurred = p.CreatedAt
	}
	typ := TypeChecksPending
	switch {
	case out.Complete && len(out.Failed) > 0:
		typ = TypeChecksFailed
	case out.Complete:
		typ = TypeChecksPassed
	}
	o, err := n.observation("pr/"+strconv.Itoa(p.Number)+"/checks", typ, key, occurred, SchemaChecks, out)
	return o, err == nil, err
}

// checksForPull reads and evaluates an open PR's current head.
func (n *normalizer) checksForPull(ctx context.Context, p rawPull) (extension.Observation, bool, error) {
	if p.State != "open" || p.Head.SHA == "" {
		return extension.Observation{}, false, nil
	}
	runs, statuses, unreadable, err := n.c.checkState(ctx, n.s.Repo, p.Head.SHA)
	if err != nil {
		return extension.Observation{}, false, err
	}
	if len(runs)+len(statuses) == 0 {
		if unreadable {
			// Neither check runs nor workflow runs are readable with this
			// credential, so "no checks" cannot be told apart from checks
			// it may not see: observe nothing rather than a false none.
			return extension.Observation{}, false, nil
		}
		return n.noChecksObservation(ctx, p)
	}
	return n.checksObservation(ctx, p, runs, statuses)
}

// noChecksObservation reports a head with no checks at all once its commit
// is older than NoChecks (v1 noChecksMinutes): a repository without CI is
// not waited on for ever. Its occurrence is the commit time plus the delay,
// so push and reconcile agree once both see it; before then nothing is
// observed. It is a latency signal only, so a failed commit read observes
// nothing rather than failing the scan.
func (n *normalizer) noChecksObservation(ctx context.Context, p rawPull) (extension.Observation, bool, error) {
	if n.s.NoChecks <= 0 || n.received.IsZero() {
		return extension.Observation{}, false, nil
	}
	at, err := n.c.CommitTime(ctx, n.s.Repo, p.Head.SHA)
	if err != nil || at.IsZero() || n.received.Sub(at) < n.s.NoChecks {
		return extension.Observation{}, false, nil
	}
	key, _, err := n.pullKey(ctx, p)
	if err != nil {
		return extension.Observation{}, false, err
	}
	out := ChecksPayload{Repo: n.s.Repo, Number: p.Number, SHA: p.Head.SHA, Complete: true, Failed: []string{}, Runs: []CheckRun{}, None: true}
	o, err := n.observation("pr/"+strconv.Itoa(p.Number)+"/checks", TypeChecksNone, key, at.Add(n.s.NoChecks), SchemaChecks, out)
	return o, err == nil, err
}

// reviewObservations normalizes a PR's submitted reviews (pending reviews are
// private drafts and never observed).
func (n *normalizer) reviewObservations(ctx context.Context, p rawPull) ([]extension.Observation, error) {
	reviews, err := n.c.Reviews(ctx, n.s.Repo, p.Number)
	if err != nil {
		return nil, err
	}
	key, _, err := n.pullKey(ctx, p)
	if err != nil {
		return nil, err
	}
	var out []extension.Observation
	for _, r := range reviews {
		// A review without a body carries nothing to read (its inline
		// comments are observed on their own). GitHub creates one around
		// every inline comment and reply, including the agent's own, so
		// observing it would wake waits with nothing (v1 Comments keeps
		// only reviews with a body).
		if r.State == "PENDING" || r.ID == 0 || strings.TrimSpace(r.Body) == "" {
			continue
		}
		o, err := n.commentObservation(KindReview, p.Number, r, key)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

var trailingNumber = regexp.MustCompile(`/(?:issues|pulls)/([0-9]+)$`)

func numberFromURL(u string) (int, bool) {
	m := trailingNumber.FindStringSubmatch(u)
	if m == nil {
		return 0, false
	}
	return parseNumber(m[1])
}
