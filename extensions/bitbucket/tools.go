package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/extensions/tracker"
	"github.com/alsiola/open-conduit/storage"
)

// The agent-facing code-host tools: Open Supervisor's `agent pr …` and
// `agent checks` commands for a Bitbucket repository, acting on the PRs of
// the Work's tracker issue. Comments go through a durable outbox with a
// discoverable mark, so a retried call never posts twice.

// Operation names.
const (
	OpContext   = "bitbucket.context"
	OpPRList    = "bitbucket.pr.list"
	OpPRView    = "bitbucket.pr.view"
	OpPRCreate  = "bitbucket.pr.create"
	OpPRComment = "bitbucket.pr.comment"
	OpPRReply   = "bitbucket.pr.reply"
	OpChecks    = "bitbucket.checks"
)

// Operations lists every tool operation.
func Operations() []string {
	return []string{OpContext, OpPRList, OpPRView, OpPRCreate, OpPRComment, OpPRReply, OpChecks}
}

// Source is one registered entry the tools can act on.
type Source struct {
	Settings Settings
	Client   *Client
	Outbox   *tracker.Outbox
}

// NewOutbox is an entry's durable comment outbox.
func NewOutbox(store storage.Store, s Settings, now func() time.Time) *tracker.Outbox {
	return &tracker.Outbox{Store: store, Namespace: "bitbucket.outbox." + digest(s.Namespace)[:16], Now: now, DryRun: s.DryRun}
}

// ToolsOptions configure Tools.
type ToolsOptions struct {
	Store   storage.Store
	Sources func() []Source
	Now     func() time.Time
}

// Tools implements the operations.
type Tools struct{ o ToolsOptions }

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

// Registration is the toolbelt registration.
func (t *Tools) Registration() extension.Registration {
	h := func(f func(context.Context, *bound, json.RawMessage) (any, error)) func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
			b, err := t.bind(ctx, a)
			if err != nil {
				return domain.Payload{}, err
			}
			out, err := f(ctx, b, in.Data)
			if err != nil {
				return domain.Payload{}, refusal(err)
			}
			data, err := json.Marshal(out)
			if err != nil {
				return domain.Payload{}, err
			}
			return domain.Payload{Schema: "bitbucket.tool.v1", Data: data}, nil
		}
	}
	return extension.Registration{Version: extension.ContractVersion, Name: Type, Operations: []extension.Operation{
		{Name: OpContext, Handler: h(t.context)},
		{Name: OpPRList, Handler: h(t.prList)},
		{Name: OpPRView, Handler: h(t.prView)},
		{Name: OpPRCreate, Handler: h(t.prCreate)},
		{Name: OpPRComment, Handler: h(t.prComment)},
		{Name: OpPRReply, Handler: h(t.prReply)},
		{Name: OpChecks, Handler: h(t.checks)},
	}}
}

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func refusal(err error) error {
	var op *extension.OperationError
	var limit *tracker.RateLimitError
	var status *tracker.StatusError
	switch {
	case errors.As(err, &op), errors.Is(err, extension.ErrUnauthorized), errors.Is(err, extension.ErrInvalid):
		return err
	case errors.As(err, &limit):
		return refuse("bitbucket_rate_limited", "Bitbucket's rate limit is in force until %s; try again after that", limit.Until.UTC().Format(time.RFC3339))
	case errors.Is(err, tracker.ErrCredential):
		return refuse("bitbucket_credential", "the Bitbucket connection's credential could not be used; report it as a problem")
	case errors.Is(err, tracker.ErrNotFound):
		return refuse("not_found", "Bitbucket has no such item (or this connection cannot see it)")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &status) && status.Status >= 400 && status.Status < 500:
		return refuse("bitbucket_refused", "Bitbucket refused the request (HTTP %d)", status.Status)
	}
	return refuse("bitbucket_unavailable", "Bitbucket could not be reached or answered with an error; try again")
}

// bound is the Work a call serves: its tracker issue (or its own PR) in a
// registered repository of the Work's scope.
type bound struct {
	a     extension.Authority
	work  domain.Work
	src   Source
	key   domain.CorrelationKey
	issue string // tracker issue key, "" for a PR-only Work
	pr    int
}

func (t *Tools) bind(ctx context.Context, a extension.Authority) (*bound, error) {
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
	for _, k := range w.CorrelationKeys {
		for _, s := range t.o.Sources() {
			if s.Settings.Scope != string(w.ScopeID) {
				continue
			}
			if issue, _, ok := tracker.ParseIssueKey(k.ExternalID); ok && s.Settings.TrackerNS == k.Source {
				return &bound{a: a, work: w, src: s, key: k, issue: issue}, nil
			}
			if pr, ok := ParsePullKey(k.ExternalID); ok && s.Settings.Namespace == k.Source {
				return &bound{a: a, work: w, src: s, key: k, pr: pr}, nil
			}
		}
	}
	return nil, refuse("no_repository", "this Work has no Bitbucket repository registered for it")
}

func decode(data json.RawMessage, v any) error {
	if err := tracker.Strict(data, v); err != nil {
		msg := err.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return refuse("invalid_input", "the input is malformed or has unknown fields: %s", msg)
	}
	return nil
}

// branch is the Work's branch: claude-code's `agent/{key}` template.
func (b *bound) branch() string { return "agent/" + b.key.ExternalID }

func (b *bound) ref() string {
	if b.issue == "" {
		return ""
	}
	return strings.ReplaceAll(b.src.Settings.IssueRef, "{key}", b.issue)
}

func (t *Tools) base(ctx context.Context, b *bound) (string, error) {
	if b.src.Settings.BaseBranch != "" {
		return b.src.Settings.BaseBranch, nil
	}
	return b.src.Client.DefaultBranch(ctx, b.src.Settings)
}

func (t *Tools) context(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	base, err := t.base(ctx, b)
	if err != nil {
		return nil, err
	}
	return map[string]any{"repo": b.src.Settings.Repo, "issue": b.issue, "ref": b.ref(), "branch": b.branch(), "base": base,
		"url": "https://bitbucket.org/" + b.src.Settings.Repo, "work": string(b.work.ID)}, nil
}

type pullView struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	SHA    string `json:"sha,omitempty"`
	URL    string `json:"url,omitempty"`
}

func viewPull(p rawPR) pullView {
	state, _ := pullState(p)
	return pullView{Number: p.ID, Title: p.Title, State: state, Head: p.Source.Branch.Name, Base: p.Destination.Branch.Name, SHA: p.Source.Commit.Hash, URL: p.Links.HTML.Href}
}

func (t *Tools) pulls(ctx context.Context, b *bound) ([]rawPR, error) {
	if b.issue == "" {
		p, err := b.src.Client.Pull(ctx, b.src.Settings, b.pr)
		if err != nil {
			return nil, err
		}
		return []rawPR{p}, nil
	}
	return b.src.Client.PullsFor(ctx, b.src.Settings, b.branch(), b.issue)
}

func (t *Tools) prList(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	prs, err := t.pulls(ctx, b)
	if err != nil {
		return nil, err
	}
	out := []pullView{}
	for _, p := range prs {
		out = append(out, viewPull(p))
	}
	return map[string]any{"pulls": out}, nil
}

// owned checks that a PR number belongs to the Work's issue (or is its PR).
func (t *Tools) owned(ctx context.Context, b *bound, number int) (rawPR, error) {
	if number <= 0 {
		return rawPR{}, refuse("invalid_input", "number is a PR number")
	}
	p, err := b.src.Client.Pull(ctx, b.src.Settings, number)
	if err != nil {
		return rawPR{}, err
	}
	if b.issue != "" && b.src.Settings.IssueOf(p.Source.Branch.Name, p.Title, p.Description) != b.issue {
		return rawPR{}, refuse("not_this_issue", "PR #%d does not belong to %s", number, b.issue)
	}
	if b.issue == "" && number != b.pr {
		return rawPR{}, refuse("not_this_issue", "PR #%d is not this Work's", number)
	}
	return p, nil
}

func (t *Tools) prView(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number int `json:"number"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	p, err := b.src.Client.Pull(ctx, b.src.Settings, in.Number)
	if err != nil {
		return nil, err
	}
	comments, err := b.src.Client.Comments(ctx, b.src.Settings, in.Number)
	if err != nil {
		return nil, err
	}
	type commentView struct {
		ID     int    `json:"id"`
		Author string `json:"author,omitempty"`
		Agent  string `json:"agent,omitempty"`
		Path   string `json:"path,omitempty"`
		Line   int    `json:"line,omitempty"`
		Reply  string `json:"reply,omitempty"`
		Body   string `json:"body"`
	}
	views := []commentView{}
	for _, c := range comments {
		body, agent := Unmark(c.Content.Raw)
		v := commentView{ID: c.ID, Author: c.User.name(), Body: strings.TrimSpace(body)}
		if agent != nil {
			v.Agent = "agent"
			if agent.Work != "" {
				v.Agent = "agent on " + agent.Work
			} else if agent.From != "" {
				v.Agent = "agent on " + agent.From
			}
		}
		if c.Inline != nil {
			v.Path = c.Inline.Path
			if c.Inline.To != nil {
				v.Line = *c.Inline.To
			}
			v.Reply = fmt.Sprintf("call %s {\"number\": %d, \"comment_id\": %d, \"body\": …}", OpPRReply, in.Number, c.ID)
		}
		views = append(views, v)
	}
	out := map[string]any{"pull": viewPull(p), "body": strings.TrimSpace(p.Description), "comments": views}
	return out, nil
}

func (t *Tools) prCreate(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, refuse("empty_title", "the title is empty")
	}
	if b.issue == "" {
		return nil, refuse("no_issue", "this Work has no tracker issue to open a PR for")
	}
	// Idempotent by state: an open PR from the branch is the answer.
	existing, err := b.src.Client.PullsFor(ctx, b.src.Settings, b.branch(), "")
	if err != nil {
		return nil, err
	}
	for _, p := range existing {
		if p.State == "OPEN" {
			return map[string]any{"state": "exists", "pull": viewPull(p)}, nil
		}
	}
	base := in.Base
	if base == "" {
		if base, err = t.base(ctx, b); err != nil {
			return nil, err
		}
	}
	body := in.Body
	if ref := b.ref(); ref != "" && !strings.Contains(strings.ToUpper(body), strings.ToUpper(b.issue)) {
		body = strings.TrimRight(body, "\n") + "\n\n" + ref
	}
	if b.src.Settings.DryRun {
		return map[string]any{"state": "dry_run", "note": "dry run: no PR was opened"}, nil
	}
	p, err := b.src.Client.CreatePull(ctx, b.src.Settings, in.Title, body, b.branch(), base, in.Draft)
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": "created", "pull": viewPull(p)}, nil
}

func intentID(ctx context.Context, b *bound, op string, data json.RawMessage) string {
	if id := extension.CallRequestID(ctx); id != "" {
		return "call/" + string(b.a.ExecutionID) + "/" + id
	}
	return "call/" + string(b.a.ExecutionID) + "/" + op + "/" + digest(string(data))[:24]
}

func (t *Tools) post(ctx context.Context, b *bound, op string, data json.RawMessage, number, parent int, body string) (any, error) {
	if strings.TrimSpace(body) == "" {
		return nil, refuse("empty_body", "the body is empty")
	}
	if _, err := t.owned(ctx, b, number); err != nil {
		return nil, err
	}
	target := "pr/" + strconv.Itoa(number)
	if parent > 0 {
		target += "/reply/" + strconv.Itoa(parent)
	}
	from := b.issue
	id := intentID(ctx, b, op, data)
	s := b.src.Settings
	state, err := b.src.Outbox.Deliver(ctx, tracker.OutboxItem{ID: id, Work: b.work.ID, Target: target, Body: body, Durable: false},
		func(ctx context.Context, mark string) (tracker.Posted, error) {
			c, err := b.src.Client.PostComment(ctx, s, number, Mark(body, tracker.Mark{Work: string(b.work.ID), Intent: mark, From: from}), parent)
			if err != nil {
				return tracker.Posted{}, err
			}
			return tracker.Posted{ID: strconv.Itoa(c.ID), URL: c.Links.HTML.Href}, nil
		},
		func(ctx context.Context, mark string) (tracker.Posted, bool, error) {
			comments, err := b.src.Client.Comments(ctx, s, number)
			if err != nil {
				return tracker.Posted{}, false, err
			}
			for _, c := range comments {
				if _, a := Unmark(c.Content.Raw); a != nil && a.Intent == mark {
					return tracker.Posted{ID: strconv.Itoa(c.ID), URL: c.Links.HTML.Href}, true, nil
				}
			}
			return tracker.Posted{}, false, nil
		})
	if err != nil && state != tracker.StateAttempted {
		return nil, err
	}
	out := map[string]any{"state": state}
	if p, ok, e := b.src.Outbox.Posted(ctx, id); e == nil && ok {
		out["state"], out["comment_id"], out["url"] = tracker.StatePosted, p.ID, p.URL
		return out, nil
	}
	if s.DryRun {
		out["state"], out["note"] = "dry_run", "dry run: nothing was posted"
		return out, nil
	}
	out["state"], out["note"] = "pending", "the comment's answer was lost; call again with the same input later to confirm it (it is never posted twice)"
	return out, nil
}

func (t *Tools) prComment(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number int    `json:"number"`
		Body   string `json:"body"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	return t.post(ctx, b, OpPRComment, data, in.Number, 0, in.Body)
}

func (t *Tools) prReply(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number    int    `json:"number"`
		CommentID int    `json:"comment_id"`
		Body      string `json:"body"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if in.CommentID <= 0 {
		return nil, refuse("invalid_input", "comment_id is the comment to reply to")
	}
	return t.post(ctx, b, OpPRReply, data, in.Number, in.CommentID, in.Body)
}

func (t *Tools) checks(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Number int `json:"number"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	var p rawPR
	if in.Number > 0 {
		var err error
		if p, err = t.owned(ctx, b, in.Number); err != nil {
			return nil, err
		}
	} else {
		prs, err := t.pulls(ctx, b)
		if err != nil {
			return nil, err
		}
		for _, x := range prs {
			if x.State == "OPEN" {
				p = x
			}
		}
		if p.ID == 0 {
			return nil, refuse("no_pull", "there is no open PR for this issue")
		}
	}
	statuses, err := b.src.Client.Statuses(ctx, b.src.Settings, p.Source.Commit.Hash)
	if err != nil {
		return nil, err
	}
	n := &normalizer{s: b.src.Settings}
	o, ok, err := n.checks(p, statuses, b.key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"number": p.ID, "sha": p.Source.Commit.Hash, "state": "none", "note": "no checks have reported on this head yet"}, nil
	}
	var payload ChecksPayload
	_ = json.Unmarshal(o.Event.Data.Data, &payload)
	return payload, nil
}
