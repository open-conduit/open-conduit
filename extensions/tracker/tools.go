package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

// The agent-facing tracker tools: Open Supervisor's `agent issue …` commands
// as toolbelt operations, the same for every tracker type. Every handler
// receives validated authority, reads the Work it serves from durable state
// and acts only on that Work's issue (or the entry's issues). Comments go
// through the projector's durable intents, so a retried call never posts
// twice.

// Operation suffixes; a tracker's operations are its type plus a suffix.
const (
	OpContext      = ".context"
	OpIssueView    = ".issue.view"
	OpIssueComment = ".issue.comment"
	OpIssueList    = ".issue.list"
	OpIssueCreate  = ".issue.create"
)

// Operations lists a tracker's tool operations.
func Operations(typ string) []string {
	return []string{typ + OpContext, typ + OpIssueView, typ + OpIssueComment, typ + OpIssueList, typ + OpIssueCreate}
}

// AgentOperations are the operations an agent binding gets for a tracker
// scope: the builtins it uses plus the tracker's tools.
func AgentOperations(typ string) []string {
	return append([]string{"read_context", "wait", "declare_outcome"}, Operations(typ)...)
}

// Source is one registered entry the tools can act on.
type Source struct {
	Settings  Settings
	Service   Service
	Projector *Projector
}

// ToolsOptions configure Tools.
type ToolsOptions struct {
	Type  string
	Store storage.Store
	// Sources returns the registered entries of this type (the host's live registry).
	Sources func() []Source
	Now     func() time.Time
}

// Tools implements one tracker type's operations.
type Tools struct{ o ToolsOptions }

// NewTools validates options.
func NewTools(o ToolsOptions) (*Tools, error) {
	if o.Store == nil || o.Sources == nil || o.Type == "" {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Tools{o: o}, nil
}

// Registration is the toolbelt registration of the tracker's operations.
func (t *Tools) Registration() extension.Registration {
	h := func(f func(context.Context, *bound, json.RawMessage) (any, error)) func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
			b, err := t.bind(ctx, a)
			if err != nil {
				return domain.Payload{}, err
			}
			out, err := f(ctx, b, in.Data)
			if err != nil {
				return domain.Payload{}, t.refusal(err)
			}
			data, err := json.Marshal(out)
			if err != nil {
				return domain.Payload{}, err
			}
			return domain.Payload{Schema: t.o.Type + ".tool.v1", Data: data}, nil
		}
	}
	typ := t.o.Type
	return extension.Registration{Version: extension.ContractVersion, Name: typ, Operations: []extension.Operation{
		{Name: typ + OpContext, Handler: h(t.context)},
		{Name: typ + OpIssueView, Handler: h(t.issueView)},
		{Name: typ + OpIssueComment, Handler: h(t.issueComment)},
		{Name: typ + OpIssueList, Handler: h(t.issueList)},
		{Name: typ + OpIssueCreate, Handler: h(t.issueCreate)},
	}}
}

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// refusal maps service failures onto safe refusals: the agent learns what to
// do next, never a token, a reference or a raw service body.
func (t *Tools) refusal(err error) error {
	var op *extension.OperationError
	var limit *RateLimitError
	name := t.o.Type
	switch {
	case errors.As(err, &op), errors.Is(err, extension.ErrUnauthorized), errors.Is(err, extension.ErrInvalid):
		return err
	case errors.As(err, &limit):
		return refuse(name+"_rate_limited", "the %s rate limit is in force until %s; try again after that", name, limit.Until.UTC().Format(time.RFC3339))
	case errors.Is(err, ErrCredential):
		return refuse(name+"_credential", "the %s connection's credential could not be used; report it as a problem", name)
	case errors.Is(err, ErrNotFound):
		return refuse("not_found", "%s has no such item (or this connection cannot see it)", name)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	var s *StatusError
	if errors.As(err, &s) && s.Status >= 400 && s.Status < 500 {
		return refuse(name+"_refused", "%s refused the request (HTTP %d)", name, s.Status)
	}
	return refuse(name+"_unavailable", "%s could not be reached or answered with an error; try again", name)
}

type bound struct {
	a     extension.Authority
	work  domain.Work
	src   Source
	issue string
	// external is the Work's correlation external ID (generation included),
	// which names its branch: claude-code's `agent/{key}` template.
	external string
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
	sources := t.o.Sources()
	for _, k := range w.CorrelationKeys {
		key, _, ok := ParseIssueKey(k.ExternalID)
		if !ok {
			continue
		}
		for _, s := range sources {
			if s.Settings.Scope == string(w.ScopeID) && s.Settings.Namespace == k.Source {
				return &bound{a: a, work: w, src: s, issue: key, external: k.ExternalID}, nil
			}
		}
	}
	return nil, refuse("no_issue", "this Work is not a %s issue of a registered entry", t.o.Type)
}

func decode(data json.RawMessage, v any) error {
	if err := Strict(data, v); err != nil {
		msg := err.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return refuse("invalid_input", "the input is malformed or has unknown fields: %s", msg)
	}
	return nil
}

// intentID is a call's stable write identity: its request ID when the agent
// supplied one, else the digest of the operation and input.
func intentID(ctx context.Context, b *bound, op string, data json.RawMessage) string {
	if id := extension.CallRequestID(ctx); id != "" {
		return "call/" + string(b.a.ExecutionID) + "/" + id
	}
	return "call/" + string(b.a.ExecutionID) + "/" + op + "/" + digest(string(data))[:24]
}

// otherIssue resolves an optional issue key argument within the entry.
func (b *bound) otherIssue(key string) (string, error) {
	if key == "" {
		return b.issue, nil
	}
	key = strings.ToUpper(strings.TrimSpace(key))
	if !ValidKey(key) || !b.src.Service.Owns(key) {
		return "", refuse("not_found", "%s is not an issue of this entry", key)
	}
	return key, nil
}

func (t *Tools) context(_ context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	return map[string]any{"tracker": t.o.Type, "issue": b.issue, "issue_url": b.src.Service.IssueURL(b.issue),
		"ref": b.src.Service.RefFor(b.issue), "branch": "agent/" + b.external, "work": string(b.work.ID), "scope": string(b.work.ScopeID),
		"rules": "the repository's CLAUDE.md or AGENTS.md (and any rule skills they list)"}, nil
}

type commentView struct {
	ID     string `json:"id"`
	Author string `json:"author,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Bot    bool   `json:"bot,omitempty"`
	At     string `json:"at,omitempty"`
	URL    string `json:"url,omitempty"`
	Body   string `json:"body"`
}

func (t *Tools) issueView(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Key string `json:"key"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	key, err := b.otherIssue(in.Key)
	if err != nil {
		return nil, err
	}
	i, err := b.src.Service.Issue(ctx, key)
	if err != nil {
		return nil, err
	}
	comments, err := b.src.Service.Comments(ctx, key)
	if err != nil {
		return nil, err
	}
	state := "open"
	if !i.Open {
		state = "closed"
	}
	views := []commentView{}
	for _, c := range comments {
		v := commentView{ID: c.ID, Author: c.Author, Bot: c.Bot, URL: c.URL, Body: strings.TrimSpace(c.Body)}
		if !c.CreatedAt.IsZero() {
			v.At = c.CreatedAt.UTC().Format(time.RFC3339)
		}
		if c.Agent != nil {
			v.Agent = "agent"
			if c.Agent.Work != "" {
				v.Agent = "agent on " + c.Agent.Work
			} else if c.Agent.From != "" {
				v.Agent = "agent on " + c.Agent.From
			}
		}
		views = append(views, v)
	}
	return map[string]any{"key": key, "title": i.Title, "url": i.URL, "state": state, "status": i.Status, "author": i.Author,
		"labels": i.Labels, "attrs": i.Attrs, "body": strings.TrimSpace(i.Body), "comments": views,
		"reply": fmt.Sprintf("call %s {\"body\": …}", t.o.Type+OpIssueComment)}, nil
}

func (t *Tools) issueComment(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Body string `json:"body"`
		Key  string `json:"key"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, refuse("empty_body", "the body is empty")
	}
	key, err := b.otherIssue(in.Key)
	if err != nil {
		return nil, err
	}
	intent := Intent{ID: intentID(ctx, b, t.o.Type+OpIssueComment, data), Work: b.work.ID, Issue: key, Body: in.Body, From: b.issue, Durable: true}
	state, err := b.src.Projector.Deliver(ctx, intent)
	if err != nil && state != StateAttempted {
		return nil, err
	}
	out := map[string]any{"state": state}
	if id, url, ok, e := b.src.Projector.IntentComment(ctx, intent.ID); e == nil && ok {
		out["state"], out["comment_id"], out["url"] = StatePosted, id, url
		return out, nil
	}
	if b.src.Settings.Projection.DryRun {
		out["state"], out["note"] = "dry_run", "dry run: nothing was posted"
		return out, nil
	}
	out["state"], out["note"] = "pending", "the comment is being confirmed and will appear once; do not post it again"
	return out, nil
}

func (t *Tools) issueList(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	type item struct {
		Key     string   `json:"key"`
		Title   string   `json:"title"`
		Status  string   `json:"status,omitempty"`
		Labels  []string `json:"labels"`
		Waiting bool     `json:"waiting,omitempty"`
		URL     string   `json:"url,omitempty"`
	}
	out := []item{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		page, err := b.src.Service.ListIssues(ctx, t.o.Now(), cursor)
		if err != nil {
			return nil, err
		}
		for _, i := range page.Issues {
			if i.Open && len(out) < 200 {
				out = append(out, item{Key: i.Key, Title: i.Title, Status: i.Status, Labels: i.Labels, Waiting: i.Waiting, URL: i.URL})
			}
		}
		if page.Next == "" || len(out) >= 200 {
			break
		}
		cursor = page.Next
	}
	return map[string]any{"issues": out}, nil
}

type createRecord struct {
	State string `json:"state"`
	Key   string `json:"key,omitempty"`
	URL   string `json:"url,omitempty"`
}

// issueCreate opens a follow-up issue once per call identity. An ambiguous
// creation (the answer was lost) is never repeated: the agent is told to
// look for it instead.
func (t *Tools) issueCreate(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, refuse("empty_title", "the title is empty")
	}
	if b.src.Settings.Projection.DryRun {
		return map[string]any{"state": "dry_run", "note": "dry run: nothing was created"}, nil
	}
	ns, key := t.o.Type+".tools."+digest(b.src.Settings.Namespace)[:16], "create/"+digest(intentID(ctx, b, t.o.Type+OpIssueCreate, data))[:40]
	var rec createRecord
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		raw, err := tx.Get(ns, key)
		if err == nil {
			return json.Unmarshal(raw, &rec)
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		rec = createRecord{State: StateAttempted}
		raw, _ = json.Marshal(rec)
		rec.State = ""
		return tx.Put(ns, key, raw)
	})
	if err != nil {
		return nil, err
	}
	switch rec.State {
	case StatePosted:
		return map[string]any{"state": StatePosted, "key": rec.Key, "url": rec.URL}, nil
	case StateAttempted:
		return nil, refuse("creation_uncertain", "an earlier identical call may already have created this issue; list the issues to find it rather than creating it again")
	}
	issue, err := b.src.Service.CreateIssue(ctx, NewIssue{Title: in.Title, Body: in.Body, Labels: in.Labels})
	if err != nil {
		return nil, err
	}
	done := createRecord{State: StatePosted, Key: issue.Key, URL: issue.URL}
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		raw, _ := json.Marshal(done)
		return tx.Put(ns, key, raw)
	}); err != nil {
		return nil, err
	}
	return map[string]any{"state": StatePosted, "key": issue.Key, "url": issue.URL}, nil
}
