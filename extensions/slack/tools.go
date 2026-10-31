package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// The agent-facing conversation tools, registered through the public
// Registrar. An agent reaches them as `call {operation, input}` (MCP) or
// `os call <operation> --data <json>` (CLI). Every handler receives validated
// authority, reads the Work it serves from durable state, and acts only on
// that Work's own thread. Replies are marked, intent-backed and re-driven, so
// each is written once.

// Operation names.
const (
	OpContext = "slack.context"
	OpThread  = "slack.thread"
	OpReply   = "slack.reply"
)

// Operations lists every tool operation (for binding operation lists).
func Operations() []string { return []string{OpContext, OpThread, OpReply} }

// Source is one registered Slack extension entry the tools can act on.
type Source struct {
	Settings  Settings
	Client    *Client
	Projector *Projector
}

// ToolsOptions configure Tools.
type ToolsOptions struct {
	Store storage.Store
	// Sources returns the registered sources (the host's live registry).
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

// Registration is the toolbelt registration. The tools emit no Events.
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
			return domain.Payload{Schema: "slack.tool.v1", Data: data}, nil
		}
	}
	return extension.Registration{Version: extension.ContractVersion, Name: Type, Operations: []extension.Operation{
		{Name: OpContext, Handler: h(t.context)},
		{Name: OpThread, Handler: h(t.thread)},
		{Name: OpReply, Handler: h(t.reply)},
	}}
}

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// refusal maps service failures onto safe refusals: the agent learns what to
// do next, never a token, a reference or a raw service body.
func refusal(err error) error {
	var op *extension.OperationError
	var limit *RateLimitError
	var api *APIError
	switch {
	case errors.As(err, &op):
		return err
	case errors.Is(err, extension.ErrUnauthorized), errors.Is(err, extension.ErrInvalid):
		return err
	case errors.As(err, &limit):
		return refuse("slack_rate_limited", "Slack's rate limit is in force until %s; try again after that", limit.Until.UTC().Format(time.RFC3339))
	case errors.Is(err, ErrCredential):
		return refuse("slack_credential", "the Slack connection's credential could not be used; report it as a problem")
	case errors.As(err, &api):
		return refuse("slack_refused", "Slack refused the request (%s)", api.Code)
	case errors.Is(err, ErrThreadTooLong):
		return refuse("thread_too_long", "the thread is too long to read in full")
	case errors.Is(err, ErrIntentConflict):
		return refuse("intent_conflict", "this request ID was already used for a different reply; use a new request ID")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return refuse("slack_unavailable", "Slack could not be reached or answered with an error; try again")
}

// bound is the conversation a call serves, resolved from durable state.
type bound struct {
	a               extension.Authority
	work            domain.Work
	src             Source
	channel, thread string
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
		channel, thread, ok := ParseThreadKey(k.ExternalID)
		if !ok {
			continue
		}
		for _, s := range t.o.Sources() {
			if s.Settings.Scope == string(w.ScopeID) && s.Settings.Namespace == k.Source {
				return &bound{a: a, work: w, src: s, channel: channel, thread: thread}, nil
			}
		}
	}
	return nil, refuse("no_thread", "this Work is not a conversation of a registered Slack entry")
}

func decode(data json.RawMessage, v any) error {
	if err := strict(data, v); err != nil {
		msg := err.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return refuse("invalid_input", "the input is malformed or has unknown fields: %s", msg)
	}
	return nil
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

func (t *Tools) context(_ context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	return map[string]any{"team": b.src.Settings.Team, "channel": b.channel, "thread_ts": b.thread, "work": string(b.work.ID), "scope": string(b.work.ScopeID),
		"reply": fmt.Sprintf("call %s {\"text\": …}", OpReply), "read": fmt.Sprintf("call %s {}", OpThread),
		// The builtin wait's input for "until a person writes in this
		// thread": a type-only wait matches the Work's own thread key.
		"wait": map[string]any{"match": map[string]any{"types": []string{TypeMessageHuman}}, "timeout_seconds": ThreadWaitSeconds}}, nil
}

// MessageView is one thread message as the tools present it.
type MessageView struct {
	TS      string `json:"ts"`
	Author  string `json:"author"`
	Class   string `json:"class"`
	Agent   string `json:"agent,omitempty"`
	At      string `json:"at"`
	Edited  bool   `json:"edited,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Text    string `json:"text,omitempty"`
}

func (t *Tools) thread(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Limit int `json:"limit"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if in.Limit <= 0 || in.Limit > 1000 {
		in.Limit = 200
	}
	messages, _, err := b.src.Client.Thread(ctx, b.channel, b.thread, "", "")
	if err != nil {
		return nil, err
	}
	views := []MessageView{}
	for _, m := range messages {
		if m.ThreadTS == "" {
			m.ThreadTS = b.thread
		}
		o, err := normalize(b.src.Settings, b.channel, m, t.o.Now())
		if err != nil {
			continue
		}
		var p MessagePayload
		if json.Unmarshal(o.Event.Data.Data, &p) != nil {
			continue
		}
		v := MessageView{TS: p.TS, Author: author(p), Class: strings.TrimPrefix(o.Event.Type, "slack.message."), Edited: p.Edited != "", Deleted: p.Deleted, Text: p.Text}
		if at, ok := TSTime(p.TS); ok {
			v.At = at.Format(time.RFC3339)
		}
		if p.Agent != nil {
			v.Agent = "agent"
			if p.Agent.Work != "" {
				v.Agent = "agent on " + p.Agent.Work
			}
		}
		views = append(views, v)
	}
	if len(views) > in.Limit {
		views = views[len(views)-in.Limit:]
	}
	return map[string]any{"channel": b.channel, "thread_ts": b.thread, "messages": views,
		"reply": fmt.Sprintf("call %s {\"text\": …}", OpReply)}, nil
}

func author(p MessagePayload) string {
	switch {
	case p.User != "":
		return "<@" + p.User + ">"
	case p.BotID != "":
		return "bot " + p.BotID
	}
	return "unknown"
}

func (t *Tools) reply(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct {
		Text      string `json:"text"`
		Broadcast bool   `json:"broadcast"`
	}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, refuse("empty_text", "the text is empty")
	}
	if len(in.Text) > maxText {
		return nil, refuse("text_too_long", "the text is longer than Slack's 40,000 characters")
	}
	intent := Intent{ID: intentID(ctx, b, OpReply, data), Work: b.work.ID, Execution: b.a.ExecutionID, Channel: b.channel, Thread: b.thread,
		Text: in.Text, Broadcast: in.Broadcast, Durable: true}
	state, err := b.src.Projector.Deliver(ctx, intent)
	if err != nil && state != IntentAttempted && state != IntentPending {
		return nil, err
	}
	st, e := b.src.Projector.Lookup(ctx, intent.ID)
	if e == nil && st.State == IntentPosted {
		return map[string]any{"state": IntentPosted, "ts": st.TS}, nil
	}
	if e == nil && st.State == IntentFailed {
		return nil, refuse("slack_refused", "Slack refused the reply (%s)", st.Code)
	}
	if b.src.Settings.Projection.DryRun {
		return map[string]any{"state": "dry_run", "note": "dry run: nothing was posted"}, nil
	}
	// An ambiguous or deferred post is finished by the projector, confirmed
	// through its marker, so it is never repeated; the agent need not retry.
	return map[string]any{"state": "pending", "note": "the reply is being confirmed and will appear once; do not post it again"}, nil
}
