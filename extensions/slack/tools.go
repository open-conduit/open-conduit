package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
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
// that Work's own thread, except slack.post, which opens one: it posts in a
// channel the entry's post config lets the calling binding post in, and the
// new thread becomes the Work's. Posts and replies are marked, intent-backed
// and re-driven, so each is written once; uploads are intent-backed and
// found again by their file's ID (upload.go).

// Operation names.
const (
	OpContext = "slack.context"
	OpThread  = "slack.thread"
	OpReply   = "slack.reply"
	OpPost    = "slack.post"
	OpUpload  = "slack.upload"
)

// Operations lists every tool operation (for binding operation lists).
func Operations() []string { return extension.OperationNames(Declared()) }

// Declared declares each operation: its name, one-line summary and input
// (inproc.Declarer); the host gives the registered operations both.
func Declared() []extension.Operation {
	return []extension.Operation{
		{Name: OpContext, Summary: "Show the Slack thread this Work serves and how to read it, reply in it and wait for the next message.",
			Input: extension.InputSchema[contextInput]()},
		{Name: OpThread, Summary: "Read the messages of this Work's Slack thread, oldest first.",
			Input: extension.InputSchema[threadInput]()},
		{Name: OpReply, Summary: "Post a reply in this Work's Slack thread; a retried call posts it once.",
			Input: extension.InputSchema[replyInput]()},
		{Name: OpPost, Summary: "Post a new message to a Slack channel; its thread becomes this Work's, so replies there reach it.",
			Input: extension.InputSchema[postInput](), Guidance: postGuidance},
		{Name: OpUpload, Summary: "Put a file from your directory (a screenshot, a log) into this Work's Slack thread; a retried call uploads it once.",
			Input: extension.InputSchema[uploadInput](), Guidance: uploadGuidance},
	}
}

// uploadGuidance tells an agent granted slack.upload how to use it.
const uploadGuidance = "Use `slack.upload` to put a file into this Work's Slack thread, such as a screenshot: `call slack.upload {\"file_path\": \"shot.png\", \"title\": \"…\"}` with a `request_id`, so a retried call uploads once. The client reads `file_path` for you. The Slack entry sets which media types and sizes are accepted; a refusal says what it allows. To say something about the file, reply in the thread as usual."

// postGuidance tells an agent granted slack.post when to use it.
const postGuidance = "Use `slack.post` to send what you have to say to a Slack channel when this Work did not start in Slack, for example a report at the end of a job: `call slack.post {\"channel\": \"C…\", \"text\": \"…\"}` with a `request_id`, so a retried call posts once. You may post only in the channels your binding is allowed; a refusal says which. The message's thread becomes this Work's: a person's reply there is your next input, and `slack.reply`, `slack.thread` and `slack.context` then act on that thread. Post once per thread you want; reply in it rather than posting again. A report that is a table or a chart can carry it as `blocks` after the text."

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

// Registration is the toolbelt registration. The tools emit no Events
// through the toolbelt: a post's TypeThreadOpened is accepted by the
// projector when the post is made, and a reply's TypeFeedback when a person
// presses its feedback buttons. The families are declared and sealed here so
// the builtin emit refuses them, as the package manifest's sealed families do.
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
	return extension.Registration{Version: extension.ContractVersion, Name: Type,
		EventTypes: []string{TypeThreadOpened, TypeFeedback}, SealedEventTypes: []string{TypeThreadOpened, TypeFeedback},
		Operations: []extension.Operation{
			{Name: OpContext, Handler: h(t.context)},
			{Name: OpThread, Handler: h(t.thread)},
			{Name: OpReply, Handler: h(t.reply)},
			{Name: OpUpload, Handler: h(t.upload)},
			{Name: OpPost, Handler: func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
				out, err := t.post(ctx, a, in.Data)
				if err != nil {
					return domain.Payload{}, refusal(err)
				}
				data, err := json.Marshal(out)
				if err != nil {
					return domain.Payload{}, err
				}
				return domain.Payload{Schema: "slack.tool.v1", Data: data}, nil
			}},
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
		return refuse("intent_conflict", "this request ID was already used for a different call; use a new request ID")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return refuse("slack_unavailable", "Slack could not be reached or answered with an error; try again")
}

// failed is the refusal for a reply or post that was not made.
func failed(what, binding string, st IntentState) error {
	if st.Code == CodeIdentityScope {
		return refuse("identity_scope_missing", "nothing was posted: binding %s posts under its own name and icon, which needs the Slack app's %s scope, and its token lacks it; report it as a problem so the owner adds the scope and reinstalls the app", binding, CustomizeScope)
	}
	if st.Detail != "" {
		return refuse("slack_refused", "Slack refused the %s (%s): %s", what, st.Code, st.Detail)
	}
	return refuse("slack_refused", "Slack refused the %s (%s)", what, st.Code)
}

// execution is the calling attempt's stored Execution, read within its own
// Work's executions.
func (t *Tools) execution(ctx context.Context, a extension.Authority) (domain.Execution, error) {
	var x domain.Execution
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(a.WorkID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(xs, func(x domain.Execution) bool { return x.ID == a.ExecutionID })
		if i < 0 {
			return storage.ErrNotFound
		}
		x = xs[i]
		return nil
	}); err != nil {
		return x, extension.ErrUnauthorized
	}
	return x, nil
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
		channel, thread, ok := ParseOwnedKey(k.ExternalID)
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

// contextInput is slack.context's input: it takes none.
type contextInput struct{}

func (t *Tools) context(_ context.Context, b *bound, data json.RawMessage) (any, error) {
	var in contextInput
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

// threadInput is slack.thread's input.
type threadInput struct {
	Limit int `json:"limit,omitempty" doc:"how many of the latest messages to return, 1 to 1000; default (and any value outside that range) 200"`
}

func (t *Tools) thread(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in threadInput
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

// replyInput is slack.reply's input.
type replyInput struct {
	Text      string            `json:"text" doc:"the reply, not blank and at most 40000 bytes; written in markdown, unless the Slack entry is set to Slack's own mrkdwn"`
	Blocks    []json.RawMessage `json:"blocks,omitempty" doc:"Block Kit blocks shown after the text: markdown, section, header, divider, context, image, rich_text, table, data_table, data_visualization or card, with no buttons, selects or other interactive elements"`
	Broadcast bool              `json:"broadcast,omitempty" doc:"also post the reply to the channel, not only the thread; default false"`
	Feedback  *bool             `json:"feedback,omitempty" doc:"false leaves the feedback buttons off this reply when the Slack entry puts them below your replies; default true"`
}

// checkMessage admits a message's text and blocks for an entry.
func checkMessage(s Settings, text string, blocks []json.RawMessage) error {
	if strings.TrimSpace(text) == "" {
		return refuse("empty_text", "the text is empty")
	}
	if len(text) > maxText {
		return refuse("text_too_long", "the text is longer than Slack's 40,000 characters")
	}
	if err := checkBlocks(blocks); err != nil {
		return err
	}
	return checkLayout(s.Projection.Format, text, blocks)
}

func (t *Tools) reply(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in replyInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	if err := checkMessage(b.src.Settings, in.Text, in.Blocks); err != nil {
		return nil, err
	}
	// The binding, and so the name the reply posts under, is the calling
	// attempt's own, read from its stored Execution, never the input.
	x, err := t.execution(ctx, b.a)
	if err != nil {
		return nil, err
	}
	intent := Intent{ID: intentID(ctx, b, OpReply, data), Work: b.work.ID, Execution: b.a.ExecutionID, Channel: b.channel, Thread: b.thread,
		Text: in.Text, Content: in.Blocks, Broadcast: in.Broadcast, Durable: true, Binding: x.ExecutorBinding,
		Feedback: b.src.Settings.AsksFeedback(x.ExecutorBinding) && (in.Feedback == nil || *in.Feedback)}
	state, err := b.src.Projector.Deliver(ctx, intent)
	if err != nil && state != IntentAttempted && state != IntentPending {
		return nil, err
	}
	st, e := b.src.Projector.Lookup(ctx, intent.ID)
	if e == nil && st.State == IntentPosted {
		return map[string]any{"state": IntentPosted, "ts": st.TS}, nil
	}
	if e == nil && st.State == IntentFailed {
		return nil, failed("reply", x.ExecutorBinding, st)
	}
	if b.src.Settings.Projection.DryRun {
		return map[string]any{"state": "dry_run", "note": "dry run: nothing was posted"}, nil
	}
	// An ambiguous or deferred post is finished by the projector, confirmed
	// through its marker, so it is never repeated; the agent need not retry.
	return map[string]any{"state": "pending", "note": "the reply is being confirmed and will appear once; do not post it again"}, nil
}

// postInput is slack.post's input.
type postInput struct {
	Channel string            `json:"channel" doc:"the Slack channel ID (C… or G…) to post in; it must be one your binding may post in"`
	Text    string            `json:"text" doc:"the message, not blank and at most 40000 bytes; written in markdown, unless the Slack entry is set to Slack's own mrkdwn"`
	Blocks  []json.RawMessage `json:"blocks,omitempty" doc:"Block Kit blocks shown after the text: markdown, section, header, divider, context, image, rich_text, table, data_table, data_visualization or card, with no buttons, selects or other interactive elements"`
}

// post posts a new top-level message for the calling Work in a channel its
// binding may post in. The binding comes from the stored Execution, never
// the caller. The post is intent-backed: with a request ID its identity is
// the Work's, so a retried turn posts once. When it is made, however it is
// confirmed (this call or the projector's next pass), the projector gives
// the Work its thread (TypeThreadOpened, which the slack route attaches).
func (t *Tools) post(ctx context.Context, a extension.Authority, data json.RawMessage) (any, error) {
	var in postInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	// The text and the blocks themselves are checked before the channel,
	// and the layout once the entry, whose format it follows, is known.
	if err := checkMessage(Settings{}, in.Text, nil); err != nil {
		return nil, err
	}
	if err := checkBlocks(in.Blocks); err != nil {
		return nil, err
	}
	var w domain.Work
	var x domain.Execution
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var err error
		if w, err = tx.Work(a.WorkID); err != nil {
			return err
		}
		// The caller's own Work's executions: one read, where a lookup by
		// execution alone lists every Work's.
		xs, err := tx.ListExecutions(a.WorkID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(xs, func(x domain.Execution) bool { return x.ID == a.ExecutionID })
		if i < 0 {
			return storage.ErrNotFound
		}
		x = xs[i]
		return nil
	}); err != nil || w.ScopeID != a.ScopeID {
		return nil, extension.ErrUnauthorized
	}
	var src *Source
	entries := 0
	for _, s := range t.o.Sources() {
		if s.Settings.Scope != string(w.ScopeID) {
			continue
		}
		entries++
		if s.Settings.MayPost(x.ExecutorBinding, in.Channel) {
			src = &s
			break
		}
	}
	if src == nil {
		if entries == 0 {
			return nil, refuse("no_slack_entry", "this scope has no Slack entry to post through")
		}
		return nil, refuse("channel_not_allowed", "binding %s may not post in channel %s; the Slack entry's post config lists the channels each binding may post in", x.ExecutorBinding, in.Channel)
	}
	if err := checkLayout(src.Settings.Projection.Format, in.Text, in.Blocks); err != nil {
		return nil, err
	}
	id := "post/" + string(a.ExecutionID) + "/" + digest(string(data))[:24]
	if r := extension.CallRequestID(ctx); r != "" {
		id = "post/" + string(w.ID) + "/" + r
	}
	cause := a.TriggerID
	if cause == "" {
		cause = x.TriggerID
	}
	intent := Intent{ID: id, Work: w.ID, Execution: a.ExecutionID, Channel: in.Channel, Text: in.Text, Content: in.Blocks, Durable: true, Cause: cause, Binding: x.ExecutorBinding}
	state, err := src.Projector.Deliver(ctx, intent)
	if err != nil && state != IntentAttempted && state != IntentPending {
		return nil, err
	}
	st, e := src.Projector.Lookup(ctx, intent.ID)
	switch {
	case e != nil:
		return nil, e
	case st.State == IntentPosted:
		return map[string]any{"state": IntentPosted, "channel": in.Channel, "ts": st.TS, "reply": fmt.Sprintf("call %s {\"text\": …}", OpReply)}, nil
	case st.State == IntentFailed:
		return nil, failed("post", x.ExecutorBinding, st)
	case src.Settings.Projection.DryRun:
		return map[string]any{"state": "dry_run", "note": "dry run: nothing was posted"}, nil
	}
	// An ambiguous or deferred post is finished by the projector, confirmed
	// through its marker, so it is never repeated; its thread becomes this
	// Work's when it is made. The agent need not call again.
	return map[string]any{"state": "pending", "note": "the post is being confirmed and will appear once; its thread becomes this Work's when it does; do not post it again"}, nil
}

// uploadInput is slack.upload's input.
type uploadInput struct {
	Name          string `json:"name,omitempty" doc:"the file's name in Slack; file_path's base name when the os client fills content_base64"`
	Title         string `json:"title,omitempty" doc:"the title Slack shows for the file, at most 255 characters; default its name"`
	ContentBase64 string `json:"content_base64" doc:"the file's content, base64; pass file_path instead and the os client reads the file into it"`
}

// maxTitle bounds an upload's name and title.
const maxTitle = 255

func (t *Tools) upload(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in uploadInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	content, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil || len(content) == 0 {
		return nil, refuse("missing_field", "content_base64 (the file, base64) is required; pass file_path to the os client to fill it")
	}
	s := b.src.Settings
	if !s.Uploads {
		return nil, refuse("uploads_off", "nothing was uploaded: the Slack entry does not take uploads; its uploads setting turns them on")
	}
	// The host's limits, checked before anything reaches Slack. The type is
	// what the content sniffs as, so a name can't pass one file off as another.
	if len(content) > s.UploadMax {
		return nil, refuse("too_large", "nothing was uploaded: the file is %d bytes and the Slack entry accepts at most %d", len(content), s.UploadMax)
	}
	mediaType := http.DetectContentType(content)
	if !s.UploadsType(mediaType) {
		return nil, refuse("media_type_not_allowed", "nothing was uploaded: the file is %s and the Slack entry accepts %s", mediaType, strings.Join(s.UploadTypes, ", "))
	}
	name := uploadName(in.Name)
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = name
	}
	if len([]rune(title)) > maxTitle || strings.ContainsAny(title, "\r\n\t") {
		return nil, refuse("invalid_title", "the title must be one line of at most %d characters", maxTitle)
	}
	intent := UploadIntent{ID: intentID(ctx, b, OpUpload, data), Work: b.work.ID, Execution: b.a.ExecutionID, Channel: b.channel, Thread: b.thread,
		Name: name, Title: title, Content: content}
	state, err := b.src.Projector.Upload(ctx, intent)
	if err != nil && state != IntentAttempted {
		return nil, err
	}
	st, e := b.src.Projector.LookupUpload(ctx, intent.ID)
	switch {
	case e != nil:
		return nil, e
	case st.State == IntentPosted:
		return map[string]any{"state": IntentPosted, "file": st.File, "name": name}, nil
	case st.State == IntentFailed:
		if st.Code == "missing_scope" {
			return nil, refuse("upload_scope_missing", "nothing was uploaded: the Slack app lacks the %s scope; report it as a problem so the owner adds the scope and reinstalls the app", FilesScope)
		}
		return nil, refuse("slack_refused", "Slack refused the upload (%s)", st.Code)
	case s.Projection.DryRun:
		return map[string]any{"state": "dry_run", "note": "dry run: would upload " + name}, nil
	}
	// The content was sent and the share's answer was lost or deferred: the
	// projector finishes it with the same file, so it appears once.
	return map[string]any{"state": "pending", "note": "the file is being confirmed and will appear once; do not upload it again"}, nil
}

// uploadName is a file name safe to give Slack: the base name, on one line,
// at most maxTitle characters; "file" when nothing is left.
func uploadName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if r := []rune(name); len(r) > maxTitle {
		name = string(r[:maxTitle])
	}
	if name == "" || name == "." || name == "/" || name == ".." {
		return "file"
	}
	return name
}
