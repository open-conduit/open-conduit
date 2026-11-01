package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// The agent-facing engineering tools (03-04): Open Supervisor's
// `agent …` commands as toolbelt operations registered through the public
// Registrar. An agent reaches them as `call {operation, input}` (MCP) or
// `os call <operation> --data <json>` (CLI). Every handler receives validated
// authority, reads the Work it serves from durable state, and acts only on
// that Work's issue and repository (or another scope's, for an authorized
// cross-scope wait). The PR operations also reach any PR in a repository the
// Work's scope registers, so Work without an issue (a Slack request, a job)
// can review one. Non-re-derivable writes are marked and idempotent.

// Event families the tools and the host add to the extension's ingress
// families.
const (
	// TypeDependencyMet is the alias a satisfied wait condition delivers to
	// the waiting Work (B04 dependency subscription).
	TypeDependencyMet = "github.dependency.met"
	// TypeHoldReleased starts a held issue's first agent turn.
	TypeHoldReleased = "github.hold.released"
)

// Payload schemas of the tools' families and outputs.
const (
	SchemaHoldReleased = "github.hold.released.v1"
	SchemaFilter       = "github.dependency.filter.v1"
	SchemaHold         = "github.hold.v1"
	// FileSchema is the generic toolbelt file result: the `os` client writes
	// base64 content to save_to in the agent's own directory.
	FileSchema = "open-conduit.toolbelt.file.v1"
)

// Operation names.
const (
	OpContext      = "github.context"
	OpIssueView    = "github.issue.view"
	OpIssueComment = "github.issue.comment"
	OpIssueList    = "github.issue.list"
	OpIssueCreate  = "github.issue.create"
	OpPRList       = "github.pr.list"
	OpPRView       = "github.pr.view"
	OpPRCreate     = "github.pr.create"
	OpPRComment    = "github.pr.comment"
	OpPRReply      = "github.pr.reply"
	OpPRReview     = "github.pr.review"
	OpChecks       = "github.checks"
	OpUpload       = "github.attachment.upload"
	OpFetch        = "github.attachment.get"
	OpWait         = "github.wait"
	OpHoldStatus   = "github.hold.status"
	OpHold         = "github.hold"
)

// Operations lists every tool operation (for binding operation lists).
func Operations() []string {
	return []string{OpContext, OpIssueView, OpIssueComment, OpIssueList, OpIssueCreate, OpPRList, OpPRView, OpPRCreate,
		OpPRComment, OpPRReply, OpPRReview, OpChecks, OpUpload, OpFetch, OpWait, OpHoldStatus, OpHold, OpGitCredential}
}

// Source is one registered GitHub extension entry the tools can act on.
type Source struct {
	Settings Settings
	// Connection is the ID of the connection the entry reads and writes
	// with (Client's).
	Connection string
	Client     *Client
	Projector  *Projector
}

// ToolsOptions configure Tools.
type ToolsOptions struct {
	Store storage.Store
	// Sources returns the registered sources (the host's live registry).
	Sources func() []Source
	Now     func() time.Time
	// Jobs resolves a `job:<name>` condition (05-02): the named job's
	// correlation key in the scope and the Event types announcing one of its
	// runs ending. Nil, or an unknown job, refuses the condition.
	Jobs func(ctx context.Context, scope domain.ID, name string) (key domain.CorrelationKey, types []string, ok bool)
	// WaitProgress sets the progress line of a Work's active Wait, when a
	// GitHub operation declared it (the host's wait.progress). Nil shows
	// no progress.
	WaitProgress func(ctx context.Context, work domain.ID, progress string) error
	// Writers supplies the client of the calling binding's own `github`
	// connection (Authority.Connection), which its comments, replies and
	// reviews go out as. Nil leaves every binding on the entry's connection.
	Writers *Writers
	// MayRead answers whether a scope may depend on another scope's events
	// (its authorization.read_scopes). A condition on a project the scope
	// may not read is refused when it is resolved, saying how to allow it,
	// rather than when the wait registers. Nil leaves the check to the
	// runtime.
	MayRead func(reader, target domain.ID) error
	// MayFile answers whether a scope may file issues in, and comment on,
	// another scope's tracker (its authorization.file_scopes). Nil allows
	// no other scope.
	MayFile func(filer, target domain.ID) error
}

// Tools implements the operations.
type Tools struct {
	o   ToolsOptions
	git gitGrants
}

// NewTools validates options.
func NewTools(o ToolsOptions) (*Tools, error) {
	if o.Store == nil || o.Sources == nil {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Tools{o: o}, nil
}

// Registration is the toolbelt registration: every operation, the families
// executions may emit, and the dependency filter.
func (t *Tools) Registration() extension.Registration {
	type handler = func(context.Context, *bound, json.RawMessage) (any, error)
	wrap := func(bind func(context.Context, extension.Authority) (*bound, error), f handler) func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
			b, err := bind(ctx, a)
			if err != nil {
				return domain.Payload{}, err
			}
			out, err := f(ctx, b, in.Data)
			if err != nil {
				return domain.Payload{}, refusal(err)
			}
			if p, ok := out.(domain.Payload); ok {
				return p, nil
			}
			data, err := json.Marshal(out)
			if err != nil {
				return domain.Payload{}, err
			}
			return domain.Payload{Schema: "github.tool.v1", Data: data}, nil
		}
	}
	// h binds the Work's issue; pr binds the Work with or without one;
	// repo binds a Work that has a repository, its issue's or its PR's.
	h := func(f handler) func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return wrap(t.bind, f)
	}
	repo := func(f handler) func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return wrap(t.bindRepo, f)
	}
	pr := func(f handler) func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return wrap(t.bindWork, f)
	}
	return extension.Registration{Version: extension.ContractVersion, Name: Type, EventTypes: []string{TypeHoldReleased},
		DependencyFilter: DependencyFilter,
		Operations: []extension.Operation{
			{Name: OpContext, Handler: pr(t.context)},
			// Issue operations bind any Work: one without an issue of its
			// own (a triage, a job, a Slack thread) names the issue, and
			// files in its scope's tracker or, with project, another's.
			{Name: OpIssueView, Handler: pr(t.issueView)},
			{Name: OpIssueComment, Handler: pr(t.issueComment)},
			{Name: OpIssueList, Handler: pr(t.issueList)},
			{Name: OpIssueCreate, Handler: pr(t.issueCreate)},
			{Name: OpPRList, Handler: h(t.prList)},
			{Name: OpPRView, Handler: pr(t.prView)},
			{Name: OpPRCreate, Handler: h(t.prCreate)},
			{Name: OpPRComment, Handler: pr(t.prComment)},
			{Name: OpPRReply, Handler: pr(t.prReply)},
			{Name: OpPRReview, Handler: pr(t.prReview)},
			{Name: OpChecks, Handler: pr(t.checks)},
			{Name: OpUpload, Handler: h(t.upload)},
			{Name: OpFetch, Handler: repo(t.fetch)},
			{Name: OpHoldStatus, Handler: h(t.holdStatus)},
			{Name: OpGitCredential, Handler: pr(t.gitCredential)},
			{Name: OpWait, Wait: t.waitPlan(false)},
			{Name: OpHold, Wait: t.waitPlan(true)},
		}}
}

// refuse is a safe refusal the agent reads verbatim.
func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// refusal maps service failures onto safe refusals: the agent learns what to
// do next, never a token, a reference or a raw service body.
func refusal(err error) error {
	var op *extension.OperationError
	var limit *RateLimitError
	switch {
	case errors.As(err, &op):
		return err
	case errors.Is(err, extension.ErrUnauthorized), errors.Is(err, extension.ErrInvalid):
		return err
	case errors.As(err, &limit):
		return refuse("github_rate_limited", "GitHub's rate limit is in force until %s; try again after that", limit.Until.UTC().Format(time.RFC3339))
	case errors.Is(err, ErrWriter):
		return refuse("github_writer", "this binding's own GitHub connection can't be written as (removed, invalid, or another GitHub than the repository's); report it as a problem")
	case errors.Is(err, ErrCredential):
		return refuse("github_credential", "the GitHub connection's credential could not be used; report it as a problem")
	case IsStatus(err, http.StatusNotFound), IsStatus(err, http.StatusGone):
		return refuse("not_found", "GitHub has no such item (or this connection cannot see it)")
	case IsStatus(err, http.StatusForbidden):
		return refuse("github_forbidden", "GitHub refused the request for this connection")
	case IsStatus(err, http.StatusUnprocessableEntity):
		return refuse("github_unprocessable", "GitHub rejected the request as invalid")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return refuse("github_unavailable", "GitHub could not be reached or answered with an error; try again")
}

// bound is the Work a call serves, resolved from durable state.
type bound struct {
	a      extension.Authority
	work   domain.Work
	key    domain.CorrelationKey
	issue  Source // owns the issue (its tracker)
	code   Source // owns the code (PRs); equal to issue without a shared tracker
	number int    // the issue; 0 when the Work owns none (bindWork only)
	// pull is the PR a PR's own Work is for (it owns the PR's key, key,
	// and no issue); 0 otherwise.
	pull int
}

// bind resolves the Work a call serves and the GitHub issue it owns.
func (t *Tools) bind(ctx context.Context, a extension.Authority) (*bound, error) {
	b, err := t.bindWork(ctx, a)
	if err != nil {
		return nil, err
	}
	if b.number == 0 {
		return nil, refuse("no_issue", "this Work is not a GitHub issue of a registered repository")
	}
	return b, nil
}

// bindRepo resolves a Work that has a repository: an issue's Work, or a
// PR's own Work, which reads issues and links in its PR's repository.
func (t *Tools) bindRepo(ctx context.Context, a extension.Authority) (*bound, error) {
	b, err := t.bindWork(ctx, a)
	if err != nil {
		return nil, err
	}
	if b.number == 0 && b.pull == 0 {
		return nil, refuse("no_issue", "this Work is not a GitHub issue of a registered repository")
	}
	return b, nil
}

// bindWork resolves the Work a call serves, with the GitHub issue it owns
// when it owns one.
func (t *Tools) bindWork(ctx context.Context, a extension.Authority) (*bound, error) {
	var w domain.Work
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(a.WorkID)
		return err
	}); err != nil {
		return nil, extension.ErrUnauthorized
	}
	if w.ScopeID != a.ScopeID {
		return nil, extension.ErrUnauthorized
	}
	sources := t.o.Sources()
	for _, k := range w.CorrelationKeys {
		n, _, ok := ParseIssueKey(k.ExternalID)
		if !ok {
			continue
		}
		b := &bound{a: a, work: w, key: k, number: n}
		found := false
		for _, s := range sources {
			if s.Settings.Scope == string(w.ScopeID) && s.Settings.Namespace == k.Source && s.Settings.TrackIssues {
				b.issue, b.code, found = s, s, true
				break
			}
		}
		if !found {
			continue
		}
		for _, s := range sources {
			if s.Settings.Scope == string(w.ScopeID) && s.Settings.SharedTracker && s.Settings.TrackerNS == k.Source {
				b.code = s
				break
			}
		}
		return b, nil
	}
	for _, k := range w.CorrelationKeys {
		n, ok := ParsePullRequestKey(k.ExternalID)
		if !ok {
			continue
		}
		for _, s := range sources {
			if s.Settings.Scope == string(w.ScopeID) && s.Settings.Namespace == k.Source {
				return &bound{a: a, work: w, key: k, code: s, issue: t.trackerOf(s), pull: n}, nil
			}
		}
	}
	return &bound{a: a, work: w}, nil
}

// pullSource is the registered repository a PR operation acts on: the one
// the call names, else the Work's own code repository (its issue's, or the
// PR's it is for), else the scope's
// only one. The PR need not belong to this Work, and its key is never
// attached to it: PRs are keyed to their tracker issue and may be another
// Work's (B04).
func (t *Tools) pullSource(b *bound, repo string) (Source, error) {
	repo = strings.TrimSpace(repo)
	if (b.number > 0 || b.pull > 0) && (repo == "" || strings.EqualFold(repo, b.code.Settings.Repo)) {
		return b.code, nil
	}
	var registered []Source
	seen := map[string]bool{}
	for _, s := range t.o.Sources() {
		if s.Settings.Scope != string(b.work.ScopeID) {
			continue
		}
		if repo != "" && strings.EqualFold(s.Settings.Repo, repo) {
			return s, nil
		}
		if r := strings.ToLower(s.Settings.Repo); !seen[r] {
			seen[r] = true
			registered = append(registered, s)
		}
	}
	switch {
	case repo != "":
		return Source{}, refuse("unregistered_repo", "%s isn't a repository this Work's scope registers", repo)
	case len(registered) == 1:
		return registered[0], nil
	case len(registered) == 0:
		return Source{}, refuse("no_repository", "this Work's scope registers no GitHub repository")
	}
	names := make([]string, len(registered))
	for i, s := range registered {
		names[i] = s.Settings.Repo
	}
	return Source{}, refuse("repo_required", "this Work's scope registers several repositories (%s): name one with repo", strings.Join(names, ", "))
}

// writer is the connection the calling execution's binding writes to GitHub
// as: the `github` connection the binding names (Authority.Connection), or ""
// for the entry's own when it names none, names the entry's, or names a
// connection of another type.
func (t *Tools) writer(b *bound, src Source) (string, error) {
	id, _, err := t.bindingClient(b, src)
	return id, err
}

// bindingClient is the connection the calling execution's binding acts as
// on src's GitHub and its client: as writer, or src's own ("" and
// src.Client) when the binding names no `github` connection of its own.
func (t *Tools) bindingClient(b *bound, src Source) (string, *Client, error) {
	id := b.a.Connection
	// Another project's tracker is written on its own connection: the
	// binding's is granted in this Work's scope, not that one.
	if id == "" || id == src.Connection || src.Settings.Scope != string(b.work.ScopeID) {
		return "", src.Client, nil
	}
	switch c, err := t.o.Writers.Client(b.work.ScopeID, id, src.Client); {
	case errors.Is(err, errNotWriter):
		return "", src.Client, nil
	case err != nil:
		return "", nil, err
	default:
		return id, c, nil
	}
}

// ownRepo reports whether src is the code repository of the Work's issue.
func (b *bound) ownRepo(src Source) bool {
	return b.number > 0 && src.Settings.Repo == b.code.Settings.Repo
}

// replyRepo is the repo field a reply hint carries: empty for the Work's own
// code repository, which the call finds without it.
func replyRepo(b *bound, src Source) string {
	if b.ownRepo(src) {
		return ""
	}
	return fmt.Sprintf("\"repo\": %q, ", src.Settings.Repo)
}

func decode(data json.RawMessage, v any) error {
	if err := strict(data, v); err != nil {
		return refuse("invalid_input", "the input is malformed or has unknown fields: %s", safeDecodeError(err))
	}
	return nil
}

func safeDecodeError(err error) string {
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}

// intentID is a call's stable write identity: its request ID when the agent
// supplied one, else the digest of the operation and input, so a retried
// call never writes twice.
func intentID(ctx context.Context, b *bound, op string, data json.RawMessage) string {
	if id := extension.CallRequestID(ctx); id != "" {
		return "call/" + string(b.a.ExecutionID) + "/" + id
	}
	return "call/" + string(b.a.ExecutionID) + "/" + op + "/" + digest(string(data))[:24]
}

func issueRef(s Settings, keyword string, number int) string {
	if s.SharedTracker {
		return keyword + " " + s.TrackerRepo + "#" + strconv.Itoa(number)
	}
	return keyword + " #" + strconv.Itoa(number)
}

// linkBody links a PR body to its issue with `Closes #N`, or `Refs #N` when
// closes is false (one PR of several, whose merge mustn't close the issue),
// unless the body already names the issue with a keyword of its own.
// Ingress links the PR by its `Refs` and by its branch either way.
func linkBody(s Settings, number int, body string, closes bool) string {
	if slices.Contains(bodyRefs(s, body), number) {
		return body
	}
	keyword := "Closes"
	if !closes {
		keyword = "Refs"
	}
	return issueRef(s, keyword, number) + "\n\n" + body
}

func (b *bound) branch() string {
	branch := "agent/issue-" + strconv.Itoa(b.number)
	if b.code.Settings.Branch != nil && !b.code.Settings.Branch.MatchString(branch) {
		return ""
	}
	return branch
}

func (t *Tools) base(ctx context.Context, b *bound) (string, error) {
	if b.code.Settings.BaseBranch != "" {
		return b.code.Settings.BaseBranch, nil
	}
	return b.code.Client.DefaultBranch(ctx, b.code.Settings.Repo)
}

// ---- context and issues ------------------------------------------------------

func (t *Tools) context(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if b.number == 0 {
		if b.pull == 0 {
			return nil, refuse("no_issue", "this Work is not a GitHub issue of a registered repository")
		}
		// A PR's own Work: the PR is what it is about.
		return map[string]any{"repo": b.code.Settings.Repo, "pr": b.pull,
			"pr_url": b.code.Client.web + "/" + b.code.Settings.Repo + "/pull/" + strconv.Itoa(b.pull),
			"work":   string(b.work.ID), "scope": string(b.work.ScopeID),
			"rules": "the repository's CLAUDE.md or AGENTS.md (and any rule skills they list)"}, nil
	}
	base, err := t.base(ctx, b)
	if err != nil {
		return nil, err
	}
	return map[string]any{"repo": b.code.Settings.Repo, "tracker": b.issue.Settings.Repo, "issue": b.number,
		"issue_url": b.issue.Client.web + "/" + b.issue.Settings.Repo + "/issues/" + strconv.Itoa(b.number),
		"branch":    b.branch(), "base": base, "work": string(b.work.ID), "scope": string(b.work.ScopeID),
		"rules": "the repository's CLAUDE.md or AGENTS.md (and any rule skills they list)"}, nil
}

type commentView struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Author string `json:"author"`
	Agent  string `json:"agent,omitempty"`
	Bot    bool   `json:"bot,omitempty"`
	At     string `json:"at"`
	URL    string `json:"url"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	State  string `json:"state,omitempty"`
	Reply  string `json:"reply,omitempty"`
	Body   string `json:"body"`
}

func view(kind string, c rawComment, reply string) commentView {
	v := commentView{ID: c.ID, Kind: kind, Author: c.User.Login, Bot: c.User.Type == "Bot", URL: c.HTMLURL, Path: c.Path, State: c.State, Reply: reply, Body: strings.TrimSpace(c.Body)}
	if a := ParseAgent(c.Body); a != nil {
		v.Agent = "agent"
		if a.Work != "" {
			v.Agent = "agent on " + a.Work
		} else if a.From != "" {
			v.Agent = "agent on " + a.From
		}
	}
	if t := firstTime(c.CreatedAt, c.SubmittedAt, c.UpdatedAt); !t.IsZero() {
		v.At = t.Format(time.RFC3339)
	}
	if c.Line != nil {
		v.Line = *c.Line
	} else if c.OriginalLine != nil {
		v.Line = *c.OriginalLine
	}
	return v
}

func (t *Tools) issueNumber(b *bound, n int) int {
	if n > 0 {
		return n
	}
	return b.number
}

func (t *Tools) issueView(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number  int    `json:"number"`
		Project string `json:"project"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	src, other, err := t.issueProject(ctx, b, in.Project, false)
	if err != nil {
		return nil, err
	}
	n, reply := t.issueNumber(b, in.Number), ""
	if other && in.Number <= 0 {
		return nil, refuse("number_required", "name the issue in project %s with number", in.Project)
	}
	if n <= 0 {
		return nil, refuse("number_required", "%s: name the issue with number", notIssue(b))
	}
	switch {
	case other:
		reply = fmt.Sprintf("\"project\": %q, \"number\": %d, ", in.Project, n)
	case b.number == 0:
		reply = fmt.Sprintf("\"number\": %d, ", n)
	}
	repo := src.Settings.Repo
	i, err := src.Client.Issue(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	comments, err := src.Client.IssueComments(ctx, repo, n, time.Time{})
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, l := range i.Labels {
		labels = append(labels, l.Name)
	}
	out := map[string]any{"number": n, "title": i.Title, "url": i.HTMLURL, "state": i.State, "author": i.User.Login, "labels": labels, "body": strings.TrimSpace(i.Body)}
	views := []commentView{}
	for _, c := range comments {
		views = append(views, view(KindIssue, c, fmt.Sprintf("call %s {%s\"body\": …}", OpIssueComment, reply)))
	}
	out["comments"] = views
	return out, nil
}

func (t *Tools) post(ctx context.Context, b *bound, src Source, intent Intent) (any, error) {
	writer, err := t.writer(b, src)
	if err != nil {
		return nil, err
	}
	intent.Writer = writer
	state, err := src.Projector.Deliver(ctx, intent)
	if err != nil && state != intentAttempted {
		return nil, err
	}
	out := map[string]any{"state": state}
	if id, url, ok, e := src.Projector.IntentComment(ctx, intent.ID); e == nil && ok {
		out["state"], out["comment_id"], out["url"] = intentPosted, id, url
		return out, nil
	}
	if src.Settings.Projection.DryRun {
		out["state"] = "dry_run"
		out["note"] = "dry run: nothing was posted"
		return out, nil
	}
	// An ambiguous post is confirmed through its marker by the projector, so
	// it is never repeated; the agent need not retry.
	out["state"] = "pending"
	out["note"] = "the comment is being confirmed and will appear once; do not post it again"
	return out, nil
}

func (t *Tools) issueComment(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Body    string `json:"body"`
		Number  int    `json:"number"`
		Project string `json:"project"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, refuse("empty_body", "the body is empty")
	}
	src, other, err := t.issueProject(ctx, b, in.Project, true)
	if err != nil {
		return nil, err
	}
	if other && in.Number <= 0 {
		return nil, refuse("number_required", "name the issue in project %s with number", in.Project)
	}
	if t.issueNumber(b, in.Number) <= 0 {
		return nil, refuse("number_required", "%s: name the issue with number", notIssue(b))
	}
	return t.post(ctx, b, src, Intent{ID: intentID(ctx, b, OpIssueComment, data), Work: b.work.ID, Number: t.issueNumber(b, in.Number), Body: in.Body, Durable: true})
}

func (t *Tools) issueList(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	src, _, err := t.issueProject(ctx, b, in.Project, false)
	if err != nil {
		return nil, err
	}
	issues, err := src.Client.OpenIssues(ctx, src.Settings.Repo)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, i := range issues {
		var labels []string
		for _, l := range i.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, map[string]any{"number": i.Number, "title": i.Title, "labels": labels, "url": i.HTMLURL})
	}
	return map[string]any{"issues": out}, nil
}

// notIssue says what a Work that owns no issue is, for a refusal.
func notIssue(b *bound) string {
	if b.pull > 0 {
		return fmt.Sprintf("this Work is PR #%d, not an issue", b.pull)
	}
	return "this Work is not a GitHub issue"
}

// ownTracker is the tracker and code repository of the Work's own scope:
// its issue's (or its PR's) when it has one, else the scope's first
// registered tracker, so a Work without an issue (a triage, a scheduled
// job, a Slack thread) still files and reads in its own project.
func (t *Tools) ownTracker(b *bound) (issue, code Source, err error) {
	if b.number > 0 || b.pull > 0 {
		return b.issue, b.code, nil
	}
	issue, code, found := t.trackerIn(b.work.ScopeID)
	if !found {
		return Source{}, Source{}, refuse("no_repository", "project %s has no GitHub issue tracker registered; name another project with project", b.work.ScopeID)
	}
	return issue, code, nil
}

// issueProject is the tracker an issue operation acts on: the Work's own
// scope's (ownTracker) when project is empty or names its own scope, else
// that project's registered tracker (other is then true). Filing (create, comment) needs
// the project in this scope's authorization.file_scopes; looking (list,
// view) is allowed by file_scopes or read_scopes. A refusal names the grant
// to add, so the agent can tell the owner.
func (t *Tools) issueProject(ctx context.Context, b *bound, project string, filing bool) (src Source, other bool, err error) {
	project = strings.TrimSpace(project)
	if project == "" || project == string(b.work.ScopeID) {
		src, _, err = t.ownTracker(b)
		return src, false, err
	}
	scope, err := t.scopeNamed(ctx, project)
	if err != nil {
		return Source{}, false, err
	}
	if scope == "" {
		return Source{}, false, refuse("no_project", "no project here is called %s", project)
	}
	if scope == b.work.ScopeID {
		src, _, err = t.ownTracker(b)
		return src, false, err
	}
	mayFile := t.o.MayFile != nil && t.o.MayFile(b.work.ScopeID, scope) == nil
	mayRead := t.o.MayRead != nil && t.o.MayRead(b.work.ScopeID, scope) == nil
	switch {
	case filing && !mayFile:
		return Source{}, false, refuse("not_allowed", "project %s may not file issues in %s; to allow it, add %q to %s's authorization.file_scopes", b.work.ScopeID, scope, string(scope), b.work.ScopeID)
	case !filing && !mayFile && !mayRead:
		return Source{}, false, refuse("not_allowed", "project %s may not read %s's issues; to allow it, add %q to %s's authorization.file_scopes (or read_scopes)", b.work.ScopeID, scope, string(scope), b.work.ScopeID)
	}
	issue, _, found := t.trackerIn(scope)
	if !found {
		return Source{}, false, refuse("no_project", "project %s has no GitHub repository registered", project)
	}
	return issue, true, nil
}

// holdFor reads conditions and a time into a hold, checking each names
// something that exists and has not already happened (v1 holdFor/checkCond).
func (t *Tools) holdFor(ctx context.Context, b *bound, conds []string, until string) (Hold, []resolvedCond, error) {
	var h Hold
	if until != "" {
		at, err := ParseTime(until)
		if err != nil {
			return h, nil, refuse("invalid_time", "%q isn't a time: use an ISO instant like 2026-10-25T09:00Z", until)
		}
		if !at.After(t.o.Now()) {
			return h, nil, refuse("time_past", "%s is already past", at.Format(time.RFC3339))
		}
		h.Until = &at
	}
	var resolved []resolvedCond
	for _, arg := range conds {
		c, err := ParseCond(arg)
		if err != nil {
			return h, nil, refuse("invalid_condition", "%s", err.Error())
		}
		r, err := t.resolveCond(ctx, b, c)
		if err != nil {
			return h, nil, err
		}
		if r.met != "" {
			return h, nil, refuse("already_met", "%s: no need to wait, carry on now", r.met)
		}
		h.Conds = append(h.Conds, r.cond)
		resolved = append(resolved, r)
	}
	if len(h.Conds) == 0 && h.Until == nil {
		return h, nil, refuse("nothing_to_wait_for", "say what to wait for: a condition, until <time>, or both")
	}
	return h, resolved, nil
}

func (t *Tools) issueCreate(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Title   string   `json:"title"`
		Body    string   `json:"body"`
		Wait    []string `json:"wait"`
		Until   string   `json:"until"`
		Project string   `json:"project"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
		return nil, refuse("missing_field", "title and body are required")
	}
	body, labels := in.Body, []string(nil)
	held := len(in.Wait) > 0 || in.Until != ""
	src, other, err := t.issueProject(ctx, b, in.Project, true)
	if err != nil {
		return nil, err
	}
	if other && held {
		return nil, refuse("hold_unsupported", "an issue in project %s can't be held: open it without wait or until, and that project's routes pick it up", in.Project)
	}
	var h Hold
	if held {
		var err error
		if h, _, err = t.holdFor(ctx, b, in.Wait, in.Until); err != nil {
			return nil, err
		}
		body += "\n\n" + h.Marker()
		labels = []string{src.Settings.Labels.Scheduled}
	}
	issue, state, err := src.Projector.CreateIssue(ctx, IssueIntent{ID: intentID(ctx, b, OpIssueCreate, data), Work: b.work.ID, Title: in.Title, Body: body, Labels: labels})
	if err != nil && state != intentAttempted {
		return nil, err
	}
	out := map[string]any{"state": state}
	if issue.Number > 0 {
		out["state"], out["number"], out["url"] = intentPosted, issue.Number, issue.URL
	} else if src.Settings.Projection.DryRun {
		out["state"], out["note"] = "dry_run", "dry run: nothing was opened"
	} else {
		out["state"], out["note"] = "pending", "the issue is being confirmed and will be opened once; do not open it again"
	}
	if held {
		out["held"] = "it starts once " + h.Describe()
	}
	return out, nil
}

// ---- pull requests -------------------------------------------------------------

func (t *Tools) issuePulls(ctx context.Context, b *bound) ([]rawPull, error) {
	found := map[int]rawPull{}
	if branch := b.branch(); branch != "" {
		pulls, err := b.code.Client.PullsForHead(ctx, b.code.Settings.Repo, branch)
		if err != nil {
			return nil, err
		}
		for _, p := range pulls {
			found[p.Number] = p
		}
	}
	n := &normalizer{s: b.code.Settings, c: b.code.Client, m: newMemo()}
	open, err := all[rawPull](ctx, b.code.Client, b.code.Settings.Repo, repoPath(b.code.Settings.Repo, "/pulls?state=open"))
	if err != nil {
		return nil, err
	}
	for _, p := range open {
		if v, ok := n.pullIssue(p); ok && v == b.number {
			found[p.Number] = p
		}
	}
	out := make([]rawPull, 0, len(found))
	for _, p := range found {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func pullState(p rawPull) string {
	switch {
	case p.MergedAt != nil:
		return "merged"
	case p.State == "closed":
		return "closed"
	}
	return "open"
}

func (t *Tools) prList(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	pulls, err := t.issuePulls(ctx, b)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, p := range pulls {
		out = append(out, map[string]any{"number": p.Number, "state": pullState(p), "branch": p.Head.Ref, "title": p.Title, "url": p.HTMLURL})
	}
	return map[string]any{"prs": out}, nil
}

func (t *Tools) prView(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number int    `json:"number"`
		Repo   string `json:"repo"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if in.Number <= 0 {
		return nil, refuse("missing_field", "number (the PR) is required")
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	repo, c, hint := src.Settings.Repo, src.Client, replyRepo(b, src)
	p, err := c.Pull(ctx, repo, in.Number)
	if err != nil {
		return nil, err
	}
	conv, err := c.IssueComments(ctx, repo, in.Number, time.Time{})
	if err != nil {
		return nil, err
	}
	inline, err := c.PullComments(ctx, repo, in.Number)
	if err != nil {
		return nil, err
	}
	reviews, err := c.Reviews(ctx, repo, in.Number)
	if err != nil {
		return nil, err
	}
	views := []commentView{}
	for _, x := range conv {
		views = append(views, view(KindPullRequest, x, fmt.Sprintf("call %s {%s\"number\": %d, \"body\": …}", OpPRComment, hint, in.Number)))
	}
	for _, x := range inline {
		views = append(views, view(KindReviewComment, x, fmt.Sprintf("call %s {%s\"number\": %d, \"comment_id\": %d, \"body\": …}", OpPRReply, hint, in.Number, x.ID)))
	}
	for _, x := range reviews {
		if x.State == "PENDING" {
			continue
		}
		views = append(views, view(KindReview, x, fmt.Sprintf("call %s {%s\"number\": %d, \"body\": …}", OpPRComment, hint, in.Number)))
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].At < views[j].At })
	return map[string]any{"repo": repo, "number": p.Number, "title": p.Title, "url": p.HTMLURL, "state": pullState(p), "branch": p.Head.Ref, "base": p.Base.Ref,
		"head": p.Head.SHA, "body": strings.TrimSpace(p.Body), "comments": views}, nil
}

func (t *Tools) prCreate(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Base  string `json:"base"`
		Head  string `json:"head"`
		Draft bool   `json:"draft"`
		// ClosesIssue false links the PR without closing the issue on
		// merge: every PR but the last of a multi-PR issue.
		ClosesIssue *bool `json:"closes_issue"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
		return nil, refuse("missing_field", "title and body are required")
	}
	head, body := in.Head, in.Body
	if head == "" {
		if head = b.branch(); head == "" {
			return nil, refuse("no_branch", "the issue has no branch under this repository's branch_pattern; pass head")
		}
		body = linkBody(b.code.Settings, b.number, body, in.ClosesIssue == nil || *in.ClosesIssue)
	}
	base := in.Base
	if base == "" {
		var err error
		if base, err = t.base(ctx, b); err != nil {
			return nil, err
		}
	}
	if b.code.Settings.Projection.DryRun {
		return map[string]any{"state": "dry_run", "note": fmt.Sprintf("dry run: would open %s → %s", head, base)}, nil
	}
	repo := b.code.Settings.Repo
	p, err := b.code.Client.CreatePull(ctx, repo, in.Title, body, head, base, in.Draft)
	if err != nil {
		// GitHub allows one open PR per head, so a retried (or lost) create
		// finds the PR the first attempt opened instead of opening another.
		pulls, e := b.code.Client.PullsForHead(ctx, repo, head)
		if e == nil {
			for _, x := range pulls {
				if x.State == "open" {
					return map[string]any{"state": "open", "number": x.Number, "url": x.HTMLURL, "note": "a PR from this branch is already open"}, nil
				}
			}
		}
		return nil, err
	}
	return map[string]any{"state": "open", "number": p.Number, "url": p.HTMLURL}, nil
}

func (t *Tools) prComment(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number int    `json:"number"`
		Body   string `json:"body"`
		Repo   string `json:"repo"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if in.Number <= 0 || strings.TrimSpace(in.Body) == "" {
		return nil, refuse("missing_field", "number (the PR) and body are required")
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	return t.post(ctx, b, src, Intent{ID: intentID(ctx, b, OpPRComment, data), Work: b.work.ID, Number: in.Number, Body: in.Body, Durable: true})
}

func (t *Tools) prReply(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number    int             `json:"number"`
		CommentID json.RawMessage `json:"comment_id"`
		Body      string          `json:"body"`
		Repo      string          `json:"repo"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(strings.Trim(string(in.CommentID), `"`), 10, 64)
	if in.Number <= 0 || err != nil || id <= 0 || strings.TrimSpace(in.Body) == "" {
		return nil, refuse("missing_field", "number (the PR), comment_id (the inline comment) and body are required")
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	return t.post(ctx, b, src, Intent{ID: intentID(ctx, b, OpPRReply, data), Work: b.work.ID, Number: in.Number, ReplyTo: id, Body: in.Body, Durable: true})
}

// reviewEvents maps the operation's review events onto GitHub's.
var reviewEvents = map[string]string{"approve": "APPROVE", "request_changes": "REQUEST_CHANGES", "comment": "COMMENT"}

// prReview submits one review: an approval, a change request or a comment,
// with any inline comments. It is a marked durable intent like a comment, so
// a retry or a lost response never submits a second review. GitHub posts it
// as the connection's writer (the bot App, or the token's account), never as
// the person who asked for it.
func (t *Tools) prReview(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number   int    `json:"number"`
		Event    string `json:"event"`
		Body     string `json:"body"`
		Repo     string `json:"repo"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Side string `json:"side"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	event, ok := reviewEvents[strings.ToLower(strings.TrimSpace(in.Event))]
	if in.Number <= 0 || !ok {
		return nil, refuse("missing_field", "number (the PR) and event (approve, request_changes or comment) are required")
	}
	if event != "APPROVE" && strings.TrimSpace(in.Body) == "" {
		return nil, refuse("missing_field", "body is required to request changes or comment")
	}
	review := &Review{Event: event}
	for i, c := range in.Comments {
		side := strings.ToUpper(strings.TrimSpace(c.Side))
		if strings.TrimSpace(c.Path) == "" || c.Line <= 0 || strings.TrimSpace(c.Body) == "" || (side != "" && side != "LEFT" && side != "RIGHT") {
			return nil, refuse("invalid_comment", "comments[%d] needs path, line (a line of the diff) and body; side is LEFT (the old file) or RIGHT (the new, the default)", i)
		}
		review.Comments = append(review.Comments, ReviewInline{Path: c.Path, Line: c.Line, Side: side, Body: c.Body})
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	out, err := t.post(ctx, b, src, Intent{ID: intentID(ctx, b, OpPRReview, data), Work: b.work.ID, Number: in.Number, Body: in.Body, Review: review, Durable: true})
	var status *StatusError
	switch {
	case err == nil:
	case errors.As(err, &status) && status.Status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(status.Message), "own pull request"):
		return nil, refuse("own_pull_request", "GitHub doesn't let the account that opened PR #%d approve it or request changes on it, and this binding writes as that account; submit a comment review instead, or ask a person to review it", in.Number)
	case IsStatus(err, http.StatusUnprocessableEntity):
		return nil, refuse("github_unprocessable", "GitHub rejected the review: each inline comment must be on a line of the PR's diff (side RIGHT for the new file, LEFT for the old), and the PR must be open; nothing was posted")
	case IsStatus(err, http.StatusForbidden):
		return nil, refuse("github_forbidden", "this connection can't submit reviews on %s: it needs Pull requests write access", src.Settings.Repo)
	default:
		return nil, err
	}
	if m, ok := out.(map[string]any); ok {
		if id, ok := m["comment_id"]; ok {
			delete(m, "comment_id")
			m["review_id"] = id
		}
		if m["state"] == "pending" {
			m["note"] = "the review is being confirmed and will appear once; do not submit it again"
		}
	}
	return out, nil
}

func (t *Tools) checks(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number int    `json:"number"`
		Repo   string `json:"repo"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	if in.Number <= 0 && !b.ownRepo(src) {
		// Only the Work's own issue has PRs to default to.
		return nil, refuse("missing_field", "number (the PR) is required outside this Work's own issue")
	}
	repo, c := src.Settings.Repo, src.Client
	var p rawPull
	if in.Number > 0 {
		if p, err = c.Pull(ctx, repo, in.Number); err != nil {
			return nil, err
		}
	} else {
		pulls, err := t.issuePulls(ctx, b)
		if err != nil {
			return nil, err
		}
		for _, x := range pulls {
			if x.State == "open" {
				p = x
			}
		}
		if p.Number == 0 {
			return nil, refuse("no_open_pr", "the issue has no open PR")
		}
	}
	runs, statuses, from, err := c.CheckState(ctx, repo, p.Head.SHA)
	if err != nil {
		return nil, err
	}
	n := &normalizer{s: src.Settings, c: c, m: newMemo(), received: t.o.Now()}
	n.remember(p)
	// The checks the base branch requires (08-01), read once here and
	// shared with the evaluation through the normalizer's memo.
	req, err := n.requiredChecks(ctx, p.Base.Ref)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"pr": p.Number, "sha": p.Head.SHA, "state": "no checks", "runs": []CheckRun{}}
	if len(req.Names) > 0 || len(req.Refused) > 0 {
		out["required"] = map[string]any{"names": nonNil(req.Names), "from": nonNil(req.From), "refused": req.Refused}
	}
	var notes []string
	if from == RunsUnreadable {
		notes = append(notes, "this connection cannot read check runs or workflow runs (a fine-grained token has no Checks permission; grant Actions: read or use a GitHub App), so only commit statuses are shown")
		if len(statuses) == 0 {
			out["state"] = "unknown"
		}
	}
	if note := RequiredNote(req, from); note != "" {
		notes = append(notes, note)
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, "; ")
	}
	verify := from == RunsFromCheckRuns
	if len(runs)+len(statuses) == 0 && verify && len(req.Names) > 0 {
		// Nothing reported at all, though the base branch requires checks.
		out["state"], out["missing"] = "waiting on required checks", req.Names
		if _, due := n.checksDue(ctx, p); due {
			out["state"] = "required checks missing"
		}
	}
	if o, ok, err := n.checksObservation(ctx, p, runs, statuses, from); err != nil {
		return nil, err
	} else if ok {
		var cp ChecksPayload
		_ = json.Unmarshal(o.Event.Data.Data, &cp)
		switch {
		case o.Event.Type == TypeChecksPassed:
			out["state"] = "passed"
		case o.Event.Type == TypeChecksFailed:
			out["state"], out["failed"] = "failed", cp.Failed
		case o.Event.Type == TypeChecksMissing:
			out["state"] = "required checks missing"
		case cp.Complete && len(cp.Missing) > 0:
			out["state"] = "waiting on required checks"
		default:
			out["state"] = "pending"
		}
		if len(cp.Missing) > 0 {
			out["missing"] = cp.Missing
		}
		out["runs"] = cp.Runs
		if cp.Preview != "" {
			out["preview"] = cp.Preview
		}
	}
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- attachments ---------------------------------------------------------------

func (t *Tools) upload(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Name          string `json:"name"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	content, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil || len(content) == 0 {
		return nil, refuse("missing_field", "content_base64 (the file, base64) is required; pass file_path to the os client to fill it")
	}
	if len(content) > MaxAttachment {
		return nil, refuse("too_large", "attachments go up to %d MB", MaxAttachment>>20)
	}
	name := SafeName(in.Name)
	if b.issue.Settings.Projection.DryRun {
		return map[string]any{"state": "dry_run", "note": "dry run: would upload " + name}, nil
	}
	// The path is deterministic for the request, so a retry keeps one file.
	file := strconv.Itoa(b.number) + "/" + digest(intentID(ctx, b, OpUpload, data))[:12] + "-" + name
	link, err := b.issue.Client.Attach(ctx, b.issue.Settings.Repo, file, fmt.Sprintf("Attach %s for #%d", name, b.number), content)
	if err != nil {
		return nil, err
	}
	md := "[" + name + "](" + link + ")"
	if strings.HasPrefix(http.DetectContentType(content), "image/") {
		md = "![" + name + "](" + link + ")"
	}
	return map[string]any{"url": link, "markdown": md}, nil
}

func (t *Tools) fetch(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		URL    string `json:"url"`
		SaveTo string `json:"save_to"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	var content []byte
	var ctype string
	var err error
	for _, s := range []Source{b.issue, b.code} {
		content, ctype, err = s.Client.Fetch(ctx, s.Settings.Repo, in.URL)
		if !errors.Is(err, ErrForeignLink) {
			break
		}
	}
	if errors.Is(err, ErrForeignLink) {
		return nil, refuse("foreign_link", "%s isn't a link to this GitHub host: fetch it with curl", in.URL)
	}
	if err != nil {
		return nil, err
	}
	name := SafeName(in.URL[strings.LastIndex(in.URL, "/")+1:])
	if i := strings.IndexAny(name, "?#"); i > 0 {
		name = name[:i]
	}
	file, _ := json.Marshal(map[string]any{"name": name, "content_type": ctype, "size": len(content), "base64": base64.StdEncoding.EncodeToString(content), "save_to": in.SaveTo})
	return domain.Payload{Schema: FileSchema, Data: file}, nil
}
