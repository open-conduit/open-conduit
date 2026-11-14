package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/storage"
)

// ReplySchema is the outcome data schema an execution uses to declare one
// reply to post in its conversation's thread. The projector posts it exactly
// once per execution, recovering an ambiguous post through its marker.
const ReplySchema = "slack.reply.v1"

// ReplyDeclaration is the ReplySchema payload.
type ReplyDeclaration struct {
	Text      string `json:"text"`
	Broadcast bool   `json:"broadcast,omitempty"`
}

// ErrIntentConflict means a stable intent ID was reused for different content.
var ErrIntentConflict = errors.New("slack intent reused with different content")

// Intent states. A reply is pending until a post is attempted; attempted
// (recorded before the call) until the post's answer or its marker proves it
// was published; posted once proven. failed records a definitive Slack
// refusal (the post did not happen), so it is never retried blindly.
const (
	IntentPending   = "pending"
	IntentAttempted = "attempted"
	IntentPosted    = "posted"
	IntentFailed    = "failed"
	// IntentRecovered is returned (never stored) when the marker search
	// found a post whose answer was lost.
	IntentRecovered = "recovered"
)

// ProjectorOptions configure a Projector.
type ProjectorOptions struct {
	Store    storage.Store
	Client   *Client
	Settings Settings
	Now      func() time.Time
	// SearchDelay is how long after an ambiguous post the marker search
	// waits, so the thread listing reflects a reply that was created
	// (default 30s).
	SearchDelay time.Duration
	// AfterPost runs after a reply was created and before local completion
	// is recorded. Tests use it to crash at exactly that boundary.
	AfterPost func(intent string)
	// Actions lists the actions offered on a waiting conversation's Work,
	// to prompt them in its thread; nil prompts nothing.
	Actions ActionsFunc
	// Act takes a prompt's button press for the person who pressed it.
	Act ActFunc
	// Open accepts the TypeThreadOpened Event that gives a posting Work the
	// thread of its new top-level message, in the transaction that records
	// the post as made: derived from parent when there is one, else a root
	// (inproc.Host.AcceptDerivedInTx and AcceptInTx). nil: top-level posts
	// are refused, since their thread could not become the Work's.
	Open func(tx storage.Tx, scope, parent domain.ID, e domain.Event) error
}

// Projector publishes conversation replies. A reply is not re-derivable: each
// has a stable intent ID, a durable intent record and a discoverable marker
// (Slack message metadata), so a post whose local completion was lost is
// found again by reading the thread instead of being repeated.
type Projector struct {
	o  ProjectorOptions
	ns string
}

// ProjectionResult reports one pass.
type ProjectionResult struct {
	Posted, Recovered, Failed int
	Codes                     []string
}

// NewProjector validates options.
func NewProjector(o ProjectorOptions) (*Projector, error) {
	if o.Store == nil || o.Client == nil || o.Settings.Namespace == "" {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SearchDelay <= 0 {
		o.SearchDelay = 30 * time.Second
	}
	return &Projector{o: o, ns: "slack.projection." + digest(o.Settings.Namespace)[:16]}, nil
}

// Settings returns the bound settings.
func (p *Projector) Settings() Settings { return p.o.Settings }

// Intent is one non-re-derivable reply, or with no Thread a new top-level
// message (slack.post). ID must be stable across retries and restarts (for
// example "outcome/<execution id>").
type Intent struct {
	ID              string
	Work, Execution domain.ID
	Channel, Thread string
	Text            string
	Broadcast       bool
	// Durable keeps the text in the intent record so Converge finishes a
	// delivery whose caller is gone (tool calls); outcome intents are
	// re-derived from durable outcomes instead.
	Durable bool
	// Blocks lay the message out (a prompt's buttons); they are derived
	// again on every pass, never stored.
	Blocks []map[string]any
	// Cause is the Event a top-level post derives from (the posting
	// attempt's trigger), so the thread's attachment joins its chain.
	Cause domain.ID
	// Binding is the binding the writing attempt ran on (the Work's
	// latest, for a prompt), read from its stored Execution, never from a
	// caller: the message posts under that binding's identity, and a
	// post's thread attachment names it, so a reply's follow-up is checked
	// against that binding's post config.
	Binding string
}

type intentRecord struct {
	ID          string    `json:"id"`
	Work        domain.ID `json:"work"`
	Execution   domain.ID `json:"execution,omitempty"`
	Channel     string    `json:"channel"`
	Thread      string    `json:"thread"`
	TextDigest  string    `json:"text_digest"`
	Broadcast   bool      `json:"broadcast,omitempty"`
	State       string    `json:"state"`
	Code        string    `json:"code,omitempty"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	TS          string    `json:"ts,omitempty"`
	Text        string    `json:"text,omitempty"`
	Cause       domain.ID `json:"cause,omitempty"`
	Binding     string    `json:"binding,omitempty"`
}

func (p *Projector) get(tx storage.Tx, key string, v any) (bool, error) {
	data, err := tx.Get(p.ns, key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

func (p *Projector) put(tx storage.Tx, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(p.ns, key, data)
}

func (p *Projector) transact(ctx context.Context, fn func(storage.Tx) error) error {
	return p.o.Store.Transact(ctx, storage.Guard{Now: p.o.Now()}, fn)
}

// CodeIdentityScope is the failed intent code when a binding posts under its
// own identity and the token lacks CustomizeScope: nothing was posted.
const CodeIdentityScope = "identity_scope_missing"

// ErrIdentityScope is returned for an outcome reply or prompt held pending
// because its binding's identity needs CustomizeScope, which the token lacks.
var ErrIdentityScope = errors.New("slack token lacks " + CustomizeScope)

// MarkerID is the marker form of an intent ID carried in reply metadata.
func MarkerID(intent string) string { return "i" + digest(intent)[:32] }

func intentKey(id string) string { return "intent/" + digest(id)[:40] }

// Marker returns the metadata an agent reply carries.
func Marker(work, execution domain.ID, intent string) *Metadata {
	data, _ := json.Marshal(Agent{Work: string(work), Execution: string(execution), Intent: MarkerID(intent)})
	return &Metadata{EventType: MarkerEventType, EventPayload: data}
}

// Deliver publishes an intent at most once as observed through its marker.
// It returns the durable record state: posted (or recovered), attempted or
// pending while delivery is in progress or waiting for its marker search, or
// failed after a definitive refusal.
func (p *Projector) Deliver(ctx context.Context, in Intent) (string, error) {
	// No Thread posts a new top-level message, which can't be broadcast.
	if in.ID == "" || strings.TrimSpace(in.Text) == "" || !slackIDPattern.MatchString(in.Channel) || in.Thread != "" && !tsPattern.MatchString(in.Thread) || in.Thread == "" && (in.Broadcast || in.Work == "" || p.o.Open == nil) {
		return "", storage.ErrInvalid
	}
	key, textDigest := intentKey(in.ID), digest(in.Text)
	var rec intentRecord
	err := p.transact(ctx, func(tx storage.Tx) error {
		found, err := p.get(tx, key, &rec)
		if err != nil {
			return err
		}
		if !found {
			rec = intentRecord{ID: in.ID, Work: in.Work, Execution: in.Execution, Channel: in.Channel, Thread: in.Thread, TextDigest: textDigest,
				Broadcast: in.Broadcast, State: IntentPending, CreatedAt: p.o.Now().UTC(), Cause: in.Cause, Binding: in.Binding}
			if in.Durable {
				rec.Text = in.Text
			}
			return p.put(tx, key, rec)
		}
		if rec.TextDigest != textDigest || rec.Channel != in.Channel || rec.Thread != in.Thread || rec.Broadcast != in.Broadcast {
			return ErrIntentConflict
		}
		return nil
	})
	if err != nil || rec.State == IntentPosted || rec.State == IntentFailed {
		return rec.State, err
	}
	marker := MarkerID(in.ID)
	if rec.State == IntentAttempted {
		if p.o.Now().Sub(rec.AttemptedAt) < p.o.SearchDelay {
			return rec.State, nil
		}
		messages, err := p.since(ctx, in.Channel, in.Thread, TS(rec.CreatedAt.Add(-time.Minute)))
		if err != nil {
			return rec.State, err
		}
		if ts, ok := p.find(ctx, messages, marker, in.Text); ok {
			return p.finish(ctx, key, IntentPosted, ts, "", true)
		}
	}
	if p.o.Settings.Projection.DryRun {
		return rec.State, nil
	}
	post := Post{Channel: in.Channel, ThreadTS: in.Thread, Text: in.Text, Broadcast: in.Broadcast, Blocks: in.Blocks, Metadata: Marker(in.Work, in.Execution, in.ID)}
	if id, ok := p.o.Settings.IdentityOf(rec.Binding); ok {
		// Slack posts under the app's name when the token lacks the
		// scope, so the scope is checked first: a binding with its own
		// identity is never shown as the app.
		self, err := p.o.Client.Self(ctx)
		if err != nil {
			return rec.State, err
		}
		if !self.Grants(CustomizeScope) {
			if in.Durable {
				// A tool call: the agent is told now and decides what to
				// do, so the intent ends here.
				return p.finish(ctx, key, IntentFailed, "", CodeIdentityScope, false)
			}
			// An outcome reply or a prompt nobody waits on: it stays
			// pending and is posted once the scope is granted.
			return rec.State, ErrIdentityScope
		}
		post.Username, post.IconEmoji, post.IconURL = id.Username, id.IconEmoji, id.IconURL
	}
	err = p.transact(ctx, func(tx storage.Tx) error {
		var cur intentRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.State, cur.Attempts, cur.AttemptedAt = IntentAttempted, cur.Attempts+1, p.o.Now().UTC()
		return p.put(tx, key, cur)
	})
	if err != nil {
		return rec.State, err
	}
	ts, err := p.o.Client.PostMessage(ctx, post)
	var limit *RateLimitError
	var refused *APIError
	switch {
	case errors.As(err, &limit), errors.Is(err, ErrCredential):
		// Refused before Slack published anything: not ambiguous.
		if _, e := p.finish(ctx, key, IntentPending, "", "", false); e != nil {
			return IntentAttempted, e
		}
		return IntentPending, err
	case errors.As(err, &refused):
		// Slack answered ok:false; nothing was posted.
		return p.finish(ctx, key, IntentFailed, "", refused.Code, false)
	case err != nil:
		// Ambiguous: the marker search decides after SearchDelay.
		return IntentAttempted, err
	}
	if p.o.AfterPost != nil {
		p.o.AfterPost(in.ID)
	}
	return p.finish(ctx, key, IntentPosted, ts, "", false)
}

// since lists the messages a post may have created since oldest: the
// thread's, or for a top-level post the channel's own messages.
func (p *Projector) since(ctx context.Context, channel, thread, oldest string) ([]Message, error) {
	if thread != "" {
		messages, _, err := p.o.Client.Thread(ctx, channel, thread, oldest, "")
		return messages, err
	}
	var out []Message
	cursor := ""
	for range 10 {
		page, next, err := p.o.Client.History(ctx, channel, oldest, "", cursor, 200)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// find locates a published reply: by its metadata marker, or, when Slack
// returned no metadata at all for the app's messages (a token or workspace
// that withholds it), by the app's bot user posting exactly this text.
func (p *Projector) find(ctx context.Context, messages []Message, marker, text string) (string, bool) {
	fallback := ""
	for _, m := range messages {
		if a := markerOf(m.Metadata); a != nil {
			if a.Intent == marker {
				return m.TS, true
			}
			continue
		}
		if fallback == "" && p.own(ctx, m) && m.Metadata == nil && m.Text == text {
			fallback = m.TS
		}
	}
	return fallback, fallback != ""
}

// own reports whether the app posted m: by its bot user, or by its bot ID,
// which a message posted under a binding's own name carries instead. Never
// by the name a message shows.
func (p *Projector) own(ctx context.Context, m Message) bool {
	if p.o.Settings.BotUser != "" && m.User == p.o.Settings.BotUser {
		return true
	}
	if m.BotID == "" {
		return false
	}
	self, err := p.o.Client.Self(ctx)
	return err == nil && self.BotID != "" && m.BotID == self.BotID
}

func (p *Projector) finish(ctx context.Context, key, state, ts, code string, recovered bool) (string, error) {
	err := p.transact(ctx, func(tx storage.Tx) error {
		var cur intentRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		cur.State, cur.TS, cur.Code = state, ts, code
		if state == IntentPosted && cur.Thread == "" {
			// A top-level post is made: its thread becomes the posting
			// Work's in the same transaction, however it was confirmed.
			if err := p.opened(tx, cur); err != nil {
				return err
			}
		}
		return p.put(tx, key, cur)
	})
	if err != nil {
		return IntentAttempted, err
	}
	if recovered {
		return IntentRecovered, nil
	}
	return state, nil
}

// opened accepts TypeThreadOpened for the Work that made post rec. Its
// delivery is the thread's, so it is accepted once however many passes
// confirm the post. A chain over its causal bounds is recorded by the host
// and does not undo the post.
func (p *Projector) opened(tx storage.Tx, rec intentRecord) error {
	if p.o.Open == nil || rec.Work == "" || rec.TS == "" {
		return storage.ErrInvalid
	}
	data, err := json.Marshal(ThreadOpened{Namespace: p.o.Settings.Namespace, Team: p.o.Settings.Team, Channel: rec.Channel, TS: rec.TS, Binding: rec.Binding})
	if err != nil {
		return err
	}
	thread := ThreadKey(rec.Channel, rec.TS)
	at := p.o.Now().UTC()
	e := domain.Event{Type: TypeThreadOpened, Source: p.o.Settings.Namespace, DeliveryID: "thread/" + digest(p.o.Settings.Namespace, thread)[:32],
		WorkID: rec.Work, OccurredAt: at, ReceivedAt: at, Data: domain.Payload{Schema: SchemaThreadOpened, Data: data}}
	err = p.o.Open(tx, domain.ID(p.o.Settings.Scope), rec.Cause, e)
	if errors.Is(err, inproc.ErrCausalLimit) {
		return nil
	}
	return err
}

// IntentState is an intent's durable record: its state, the posted
// message's ts and a refusal code.
type IntentState struct {
	State, TS, Code string
}

// Lookup reads an intent's durable record (State is "" when unknown).
func (p *Projector) Lookup(ctx context.Context, id string) (IntentState, error) {
	var rec intentRecord
	err := p.transact(ctx, func(tx storage.Tx) error { _, e := p.get(tx, intentKey(id), &rec); return e })
	return IntentState{State: rec.State, TS: rec.TS, Code: rec.Code}, err
}

func replyDeclaration(x domain.Execution) (ReplyDeclaration, bool) {
	var d ReplyDeclaration
	if x.Outcome == nil || x.Outcome.Data.Schema != ReplySchema || json.Unmarshal(x.Outcome.Data.Data, &d) != nil || strings.TrimSpace(d.Text) == "" {
		return d, false
	}
	return d, true
}

// Thread is the thread a Work owns in this projector's namespace.
func (p *Projector) Thread(w domain.Work) (channel, thread string, ok bool) {
	for _, k := range w.CorrelationKeys {
		if k.Source != p.o.Settings.Namespace {
			continue
		}
		if c, t, ok := ParseOwnedKey(k.ExternalID); ok {
			return c, t, true
		}
	}
	return "", "", false
}

// outcomeIntents re-derives replies declared by durable outcomes.
func (p *Projector) outcomeIntents(ctx context.Context) ([]Intent, error) {
	var out []Intent
	err := p.transact(ctx, func(tx storage.Tx) error {
		out = nil
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		for _, w := range works {
			if string(w.ScopeID) != p.o.Settings.Scope {
				continue
			}
			channel, thread, ok := p.Thread(w)
			if !ok {
				continue
			}
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
			for _, x := range xs {
				if d, ok := replyDeclaration(x); ok {
					out = append(out, Intent{ID: "outcome/" + string(x.ID), Work: w.ID, Execution: x.ID, Channel: channel, Thread: thread, Text: d.Text, Broadcast: d.Broadcast, Binding: x.ExecutorBinding})
				}
			}
		}
		return nil
	})
	return out, err
}

// Converge runs one bounded pass: outcome replies and unfinished durable
// (tool) intents. Rate limits and credential failures stop the pass; other
// per-item failures are reported as safe codes and retried next pass.
func (p *Projector) Converge(ctx context.Context, limit int) (ProjectionResult, error) {
	var res ProjectionResult
	if p.o.Settings.Projection.Disabled {
		return res, nil
	}
	if limit <= 0 {
		limit = 50
	}
	intents, err := p.outcomeIntents(ctx)
	if err != nil {
		return res, err
	}
	prompts, err := p.promptIntents(ctx)
	if err != nil {
		return res, err
	}
	intents = append(intents, prompts...)
	var durable []Intent
	if err := p.transact(ctx, func(tx storage.Tx) error {
		rows, err := tx.List(p.ns, "intent/")
		for _, row := range rows {
			var rec intentRecord
			if json.Unmarshal(row.Value, &rec) == nil && rec.Text != "" && rec.State != IntentPosted && rec.State != IntentFailed {
				durable = append(durable, Intent{ID: rec.ID, Work: rec.Work, Execution: rec.Execution, Channel: rec.Channel, Thread: rec.Thread, Text: rec.Text, Broadcast: rec.Broadcast, Durable: true, Cause: rec.Cause, Binding: rec.Binding})
			}
		}
		return err
	}); err != nil {
		return res, err
	}
	done := 0
	for _, in := range append(intents, durable...) {
		if done >= limit {
			break
		}
		st, err := p.Lookup(ctx, in.ID)
		if err != nil {
			return res, err
		}
		if st.State == IntentPosted || st.State == IntentFailed {
			continue
		}
		done++
		state, err := p.Deliver(ctx, in)
		var rate *RateLimitError
		switch {
		case errors.As(err, &rate):
			res.Codes = append(res.Codes, "reply:rate_limited")
			return res, err
		case errors.Is(err, ErrCredential):
			res.Codes = append(res.Codes, "reply:credential")
			return res, err
		case err != nil:
			res.Codes = append(res.Codes, "reply:"+ErrorCode(err))
		case state == IntentRecovered:
			res.Recovered++
		case state == IntentPosted:
			res.Posted++
		case state == IntentFailed:
			res.Failed++
			code := "reply:refused"
			if st, err := p.Lookup(ctx, in.ID); err == nil && st.Code == CodeIdentityScope {
				code = "reply:" + CodeIdentityScope
			}
			res.Codes = append(res.Codes, code)
		}
	}
	return res, nil
}

// ErrorCode is a safe diagnostic code for an extension error.
func ErrorCode(err error) string {
	var s *StatusError
	var a *APIError
	var r *RateLimitError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &r):
		return "rate_limited"
	case errors.Is(err, ErrCredential):
		return "credential"
	case errors.As(err, &a):
		return "slack_" + a.Code
	case errors.As(err, &s):
		return fmt.Sprintf("http_%d", s.Status)
	case errors.Is(err, ErrIntentConflict):
		return "intent_conflict"
	case errors.Is(err, ErrIdentityScope):
		return CodeIdentityScope
	case errors.Is(err, ErrThreadTooLong):
		return "thread_too_long"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	return "failed"
}
