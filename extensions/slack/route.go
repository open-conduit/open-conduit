package slack

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
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
func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
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
	if len(in.Route.Config.Data) > 0 {
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
		d := extension.RouteDecision{Match: true, Reason: "a person wrote in the thread"}
		if p.Root && p.Edited != "" {
			d.Display = Display(p) // the thread's first message was edited
		}
		return d, nil
	case TypeMessageAgent:
		self := p.Agent == nil || p.Agent.Work == "" || p.Agent.Work == string(in.Work.ID)
		if self {
			return extension.Skip("the conversation's own reply"), nil
		}
		return extension.RouteDecision{Match: true, Reason: "another Work's reply in the thread"}, nil
	}
	return extension.Skip(e.Type + " is not passed to the agent"), nil
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
