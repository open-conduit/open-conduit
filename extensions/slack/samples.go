package slack

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Sample Events for a route trace ("Suppose…"), one set per slack entry of
// the scope, written as the entry's normalizer would produce them for a
// message in its first watched channel, sent just now so the start window
// admits it.

var _ extension.RouteSampler = Route{}

func scopeSettings(scope domain.Scope) []Settings {
	var out []Settings
	for _, x := range scopeExtensions(scope) {
		if x.Type != Type {
			continue
		}
		if s, err := parseSettings(x.Config, nil, false); err == nil && s.Scope == string(scope.ID) {
			out = append(out, s)
		}
	}
	return out
}

func sampleMessage(s Settings, typ, thread string, p MessagePayload) domain.Event {
	p.Team, p.Thread = s.Team, thread
	p.Channel = s.Channels[0]
	data, _ := json.Marshal(p)
	return domain.Event{Type: typ, Source: s.Namespace,
		CorrelationKey: &domain.CorrelationKey{Source: s.Namespace, ExternalID: ThreadKey(p.Channel, thread)},
		Data:           domain.Payload{Schema: SchemaMessage, Data: data}}
}

// sampleID keeps samples of several entries in one scope apart.
func sampleID(all []Settings, s Settings, id string) string {
	if len(all) > 1 {
		return s.Namespace + "/" + id
	}
	return id
}

// Samples offers a mention that starts a conversation, a top-level message
// without a mention, and a reply in an existing thread.
func (Route) Samples(scope domain.Scope, _ domain.Route) []extension.RouteSample {
	var out []extension.RouteSample
	all := scopeSettings(scope)
	now := time.Now()
	for _, s := range all {
		if len(s.Channels) == 0 {
			continue
		}
		ts := TS(now)
		reply := TS(now.Add(time.Second))
		mention := []string{s.BotUser}
		out = append(out,
			extension.RouteSample{ID: sampleID(all, s, "mention"), Title: "Someone mentions the bot in a channel",
				Event: sampleMessage(s, TypeMessageHuman, ts, MessagePayload{TS: ts, Root: true, User: "U0EXAMPLE", Text: "<@" + s.BotUser + "> can you take a look?", Mentions: mention})},
			extension.RouteSample{ID: sampleID(all, s, "top-level-message"), Title: "Someone posts in a channel without a mention",
				Event: sampleMessage(s, TypeMessageHuman, ts, MessagePayload{TS: ts, Root: true, User: "U0EXAMPLE", Text: "Is anyone around?"})},
			extension.RouteSample{ID: sampleID(all, s, "thread-reply"), Title: "Someone replies in a thread",
				Event: sampleMessage(s, TypeMessageHuman, ts, MessagePayload{TS: reply, User: "U0EXAMPLE", Text: "One more detail: it fails on retry."})},
		)
	}
	return out
}
