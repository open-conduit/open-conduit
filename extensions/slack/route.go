package slack

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// WorkKind is the kind of Work a thread creates: PRD workflow B's
// `conversation`, namespaced as B02 requires of every Work kind.
const WorkKind = "slack.conversation"

// Route is the deterministic `slack` route handler: the start policy for an
// unowned thread and the input selection for its conversation. It reads only
// the detached Event, Scope and Work snapshot and its own route config, whose
// channels narrow the threads it claims. The thread key is the Work's
// identity, so every later message of the thread reaches the same Work
// through its owned correlation, across any number of messages and restarts.
type Route struct{}

var _ extension.RouteHandler = Route{}

// scopeExtensions decodes the extension list from the effective scope
// metadata (core.config.scope.v1), ignoring everything else in it.
func scopeExtensions(scope domain.Scope) []struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
} {
	var meta struct {
		Extensions []struct {
			Type   string          `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil {
		return nil
	}
	return meta.Extensions
}

// settingsFor finds the slack entry whose namespace produced the event.
func settingsFor(scope domain.Scope, source string) (Settings, bool) {
	for _, x := range scopeExtensions(scope) {
		if x.Type != Type {
			continue
		}
		s, err := parseSettings(x.Config, nil, false)
		if err == nil && s.Namespace == source && s.Scope == string(scope.ID) {
			return s, true
		}
	}
	return Settings{}, false
}

// Starts is the start policy for a message in a thread no Work owns: a human
// message that is not deleted, was first received within start_window of
// when it was written (so a first reconcile does not turn history into Work),
// and is a top-level message (start all) or mentions the bot (start mention).
func (s Settings) Starts(e domain.Event, p MessagePayload) bool {
	if e.Type != TypeMessageHuman || p.Deleted {
		return false
	}
	written, ok := TSTime(p.TS)
	if !ok || e.ReceivedAt.Sub(written) > s.StartWindow {
		return false
	}
	switch s.Start {
	case StartMention:
		for _, m := range p.Mentions {
			if m == s.BotUser {
				return true
			}
		}
		return false
	default:
		return p.Root
	}
}

// Match selects input for an existing conversation and starts one for an
// admitted message.
//
//   - Human messages (including edits) are input.
//   - Agent replies marked by another Work are input; the Work's own replies,
//     unmarked app messages and other bots' messages never are, so a reply
//     cannot start an event loop.
//   - Deletions are recorded but are not input.
//
// A thread a Work posted (TypeThreadOpened, which the projector accepts when a post is made) is
// attached to that Work without being its input. Later messages in a thread
// owned by Work that is not a conversation (a job that posted its report)
// are input on the binding that Work runs on, not this route's, so the
// agent that wrote the post answers; once that Work has finished, a
// person's message opens its successor (followUp).
func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Type == TypeThreadOpened {
		return opened(in)
	}
	if !strings.HasPrefix(e.Type, "slack.message.") {
		return extension.Skip("not a Slack message"), nil
	}
	if e.CorrelationKey == nil || e.CorrelationKey.Source != e.Source {
		return extension.Skip("the message names no thread of its own source"), nil
	}
	s, ok := settingsFor(in.Scope, e.Source)
	if !ok {
		return extension.Skip("no slack entry in this scope reads " + e.Source), nil
	}
	channel, _, ok := ParseThreadKey(e.CorrelationKey.ExternalID)
	if !ok {
		return extension.Skip("the key is not a Slack thread"), nil
	}
	// A route's channels choose the binding a conversation runs on. A
	// thread a posting Work owns runs on that Work's binding instead, so
	// every slack route takes its messages.
	posted := in.Work != nil && in.Work.Kind != WorkKind
	if len(in.Route.Config.Data) > 0 && !posted {
		rc, err := ParseRoute(in.Route.Config.Data)
		if err != nil {
			return extension.Skip("the route config is unreadable"), nil
		}
		if !rc.Selects(channel) {
			return extension.Skip("channel " + channel + " is not one this route selects"), nil
		}
	}
	if e.Type == TypeMessageRemoved || e.Type == TypeMessageDeleted || e.Type == TypeMessageBot {
		return extension.Skip("a removed, deleted or bot message is not input"), nil
	}
	var p MessagePayload
	if e.Data.Schema != SchemaMessage {
		return extension.Skip("not a Slack message payload"), nil
	}
	if json.Unmarshal(e.Data.Data, &p) != nil {
		return extension.Skip("the message data is unreadable"), nil
	}
	if in.Work == nil {
		if !s.Starts(e, p) {
			return extension.Skip(s.startRefusal(e, p)), nil
		}
		return extension.RouteDecision{Match: true, Kind: WorkKind, Reason: s.startReason(), Display: Display(p)}, nil
	}
	switch e.Type {
	case TypeMessageHuman:
		if p.Deleted {
			return extension.Skip("the message was deleted"), nil
		}
		if posted && in.Work.State.Terminal() {
			return followUp(in.Work, e, channel, p), nil
		}
		d := extension.RouteDecision{Match: true, Reason: "a person wrote in the thread"}
		if posted {
			d.Reason, d.Deliver = "a person replied to a post of this Work", extension.DeliverCurrent
		}
		// The thread's first message says what the Work is when it was
		// edited, or when the Work has no display yet: Work from before
		// displays, which the router's backfill replays.
		if p.Root && (p.Edited != "" || in.Work.Display == nil) {
			d.Display = Display(p)
		}
		return d, nil
	case TypeMessageAgent:
		self := p.Agent == nil || p.Agent.Work == "" || p.Agent.Work == string(in.Work.ID)
		if self {
			return extension.Skip("the conversation's own reply"), nil
		}
		d := extension.RouteDecision{Match: true, Reason: "another Work's reply in the thread"}
		if posted {
			d.Deliver = extension.DeliverCurrent
		}
		return d, nil
	}
	return extension.Skip(e.Type + " is not passed to the agent"), nil
}

// followUp opens the next generation of a finished Work that posted the
// thread a person has now written in (RouteDecision.Successor): Work of the
// same kind, context and grants, run where the finished Work ran
// (deliver current), so the job that wrote the post answers questions
// about it. Its generation key names the thread, so the Slack tools act on
// it, and the thread's own key reaches it through the Work it succeeds;
// once it finishes, the next message opens the generation after it. The
// post is its handoff: it is the thread's first message, which slack.thread
// reads, marked with the Work that wrote it. An edit of an earlier reply
// opens nothing: only a new message is a question. A finished
// conversation (slack.conversation) is never succeeded: its thread ended
// when a person said it was resolved.
func followUp(w *domain.Work, e domain.Event, channel string, p MessagePayload) extension.RouteDecision {
	if p.Edited != "" {
		return extension.Skip("an edit in the thread of a finished Work's post")
	}
	thread := p.Thread
	if thread == "" {
		thread = p.TS
	}
	next, n := nextGeneration(w.ID)
	return extension.RouteDecision{Match: true, Successor: true, CreateID: next, Kind: w.Kind, Context: w.Context, Grants: append([]string(nil), w.Grants...),
		AttachKeys: []domain.CorrelationKey{{Source: e.Source, ExternalID: GenerationKey(channel, thread, n)}},
		Display:    Display(p), Deliver: extension.DeliverCurrent, Reason: "a person replied to a post of Work that has finished"}
}

// nextGeneration is the ID and number of the Work that succeeds finished
// Work id: <id>/g2 for the first, then /g3 and on.
func nextGeneration(id domain.ID) (domain.ID, int) {
	base, n := string(id), 1
	if i := strings.LastIndex(base, "/g"); i >= 0 {
		if g, err := strconv.Atoi(base[i+2:]); err == nil && g > 1 && base[i+2:] == strconv.Itoa(g) {
			base, n = base[:i], g
		}
	}
	return domain.ID(base + "/g" + strconv.Itoa(n+1)), n + 1
}

// opened attaches the thread of a message a Work posted to that Work. Only
// the projector accepts it, from the entry's own source, for a post
// slack.post checked against the post config; the family is sealed, so no
// executor can emit it, and the source and channel are checked again here.
// The attachment is not input: nobody has replied yet, and the post's own
// turn may still be running. The route's channel selection is not
// consulted, since no binding takes the Event.
func opened(in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if in.Work == nil {
		return extension.Skip("a posted thread is attached only to the Work that posted it"), nil
	}
	var p ThreadOpened
	if e.Data.Schema != SchemaThreadOpened || json.Unmarshal(e.Data.Data, &p) != nil {
		return extension.Skip("not a posted thread payload"), nil
	}
	if e.Source != p.Namespace {
		return extension.Skip("a posted thread comes only from its entry's own source"), nil
	}
	s, ok := settingsFor(in.Scope, p.Namespace)
	if !ok {
		return extension.Skip("no slack entry in this scope reads " + p.Namespace), nil
	}
	if !s.Posts(p.Channel) || !tsPattern.MatchString(p.TS) {
		return extension.Skip("channel " + p.Channel + " is not one the entry lets any binding post in"), nil
	}
	key := domain.CorrelationKey{Source: s.Namespace, ExternalID: ThreadKey(p.Channel, p.TS)}
	return extension.RouteDecision{Match: true, AttachKeys: []domain.CorrelationKey{key}, Deliver: extension.DeliverNone, Reason: "a thread this Work posted"}, nil
}

// startReason says what let a message start a conversation.
func (s Settings) startReason() string {
	if s.Start == StartMention {
		return "a mention in a watched channel"
	}
	return "a new top-level message"
}

// startRefusal says why Starts refuses the message ("" when it admits it).
func (s Settings) startRefusal(e domain.Event, p MessagePayload) string {
	if e.Type != TypeMessageHuman {
		return "only a person's message starts a conversation"
	}
	if p.Deleted {
		return "the message was deleted"
	}
	written, ok := TSTime(p.TS)
	if !ok {
		return "the message has no readable time"
	}
	if e.ReceivedAt.Sub(written) > s.StartWindow {
		return "the message is older than the start window"
	}
	if s.Start == StartMention {
		return "the message does not mention the bot"
	}
	return "a reply, not a top-level message"
}

// mentionRe matches Slack's user, channel and group mention tokens.
var mentionRe = regexp.MustCompile(`<[@#!][^>]*>`)

// Display says what a thread's Work is (issue #123): the first line of its
// first message without mention tokens, who wrote it and a link to the
// thread. nil when the message has no text to show.
func Display(p MessagePayload) *domain.WorkDisplay {
	title := extension.Clip(strings.Join(strings.Fields(mentionRe.ReplaceAllString(firstLine(p.Text), " ")), " "), extension.MaxDisplayTitle)
	if title == "" {
		return nil
	}
	d := &domain.WorkDisplay{Title: title, Requester: extension.Clip(p.User, extension.MaxDisplayRequester)}
	thread := p.Thread
	if thread == "" {
		thread = p.TS
	}
	link := "https://app.slack.com/client/" + url.PathEscape(p.Team) + "/" + url.PathEscape(p.Channel) + "/thread/" + url.PathEscape(p.Channel+"-"+thread)
	if p.Team != "" && p.Channel != "" && thread != "" && extension.DisplayURL(link) {
		d.Links = []domain.Link{{Label: "Slack thread", URL: link}}
	}
	return d
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}
