package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Requests as people read them (issue #762). A request is laid out from the
// provider's wording (extension.Wording) when it gives one: one sentence
// saying who asks for what, with each Slack person it names mentioned so
// Slack notifies them; the values it is about as fields; the whole input in
// a code block only when a value is nested; and its choices as buttons in
// the provider's words and tones, with a confirmation where it asks for
// one. Responders outside Slack are counted, never shown by their IDs.
// Once the request is settled the buttons give way to one line saying how
// it ended, which mentions whoever answered without notifying them (an
// edited message notifies nobody). A request without wording reads as its
// label and detail. What the request means stays the provider's.

// asked is one request to lay out: who asks, the action that answers it,
// and the request itself when the provider reports one.
type asked struct {
	who     string
	work    domain.ID
	offer   extension.ActionOffer
	request *extension.Request
	// direct lays it out for the one person it is sent to, in a DM: they
	// are not mentioned.
	direct bool
	// replies lays it out for an entry that can't receive a press
	// (Settings.Presses): its choices are named, without buttons, and a
	// reply in the thread that says one answers it (replies.go).
	replies bool
}

// laidOut is a request's message: its notification text, its blocks, and
// the blocks without its buttons, kept to say how it ended.
type laidOut struct {
	text   string
	blocks []map[string]any
	shown  []map[string]any
}

// escape makes plain text safe in mrkdwn: Slack reads &, < and > as markup.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// clipRunes cuts s to at most n bytes, on a rune boundary.
func clipRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// outsideSlack counts responders that are not of this surface: people who
// may answer elsewhere (an operator, a GitHub login).
func outsideSlack(responders []string) int {
	n := 0
	for _, r := range responders {
		if r != Type && !strings.HasPrefix(r, Type+":") {
			n++
		}
	}
	return n
}

// orAll joins choices as "A", "A or B", "A, B or C".
func orAll(words []string) string {
	if len(words) <= 1 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// mentionAll joins people as Slack mentions: "<@A>", "<@A> and <@B>",
// "<@A>, <@B> and <@C>".
func mentionAll(people []string) string {
	ms := make([]string, len(people))
	for i, u := range people {
		ms[i] = "<@" + u + ">"
	}
	if len(ms) <= 1 {
		return strings.Join(ms, "")
	}
	return strings.Join(ms[:len(ms)-1], ", ") + " and " + ms[len(ms)-1]
}

// layout is the message for a: false when it can't be prompted (no
// choices, too many, or a value too long for a button).
func (a asked) layout() (laidOut, bool) {
	o := a.offer
	if len(o.Choices) == 0 || len(o.Choices) > 20 || strings.TrimSpace(o.Label) == "" {
		return laidOut{}, false
	}
	var w *extension.Wording
	var responders []string
	if a.request != nil {
		w, responders = a.request.Wording, a.request.Responders
	}
	people := namedSlackPeople(responders)
	if a.direct {
		people = nil
	}
	who := strings.TrimSpace(a.who)
	worded := w != nil && (w.Wants != "" || w.Question != "")
	var lead, text string
	var shown []map[string]any
	switch {
	case w != nil && w.Wants != "":
		text = who + " wants to " + w.Wants
		lead = "*" + escape(who) + "* wants to " + escape(strings.TrimSuffix(w.Wants, ".")) + "."
		if len(people) > 0 {
			lead += " " + mentionAll(people) + ", do you approve?"
		} else {
			lead += " Do you approve?"
		}
	case w != nil && w.Question != "":
		text = who + " asks: " + w.Question
		lead = "*" + escape(who) + "* asks: " + escape(w.Question)
		if len(people) > 0 {
			lead += "\n" + mentionAll(people) + ", can you answer?"
		}
	default:
		text = who + " asks: " + o.Label
		lead = "*" + escape(who) + "* asks: " + escape(o.Label)
		if len(people) > 0 {
			lead += "\n" + mentionAll(people)
		}
	}
	shown = append(shown, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": clipRunes(lead, maxSection)}})
	if d := strings.TrimSpace(o.Detail); !worded && d != "" {
		// The provider's detail is plain text (a call's input, say), which
		// mrkdwn would read as formatting.
		shown = append(shown, map[string]any{"type": "section", "text": map[string]any{"type": "plain_text", "text": clipRunes(d, maxSection)}})
	}
	if w != nil {
		var fields []map[string]any
		for _, f := range w.Fields {
			if len(fields) == 10 {
				break
			}
			fields = append(fields, map[string]any{"type": "mrkdwn", "text": clipRunes("*"+escape(f.Name)+"*\n"+escape(f.Value), 2000)})
		}
		if len(fields) > 0 {
			shown = append(shown, map[string]any{"type": "section", "fields": fields})
		}
		if in := strings.TrimSpace(w.Input); in != "" {
			shown = append(shown, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "```" + clipRunes(escape(in), maxSection-6) + "```"}})
		}
	}
	blocks := append([]map[string]any{}, shown...)
	var notes []string
	if w != nil && strings.TrimSpace(w.Note) != "" {
		notes = append(notes, escape(clipRunes(strings.TrimSpace(w.Note), 2000)))
	}
	if n := outsideSlack(responders); n > 0 && !a.direct {
		if n == 1 {
			notes = append(notes, "1 person outside Slack can answer too.")
		} else {
			notes = append(notes, strconv.Itoa(n)+" people outside Slack can answer too.")
		}
	}
	if len(notes) > 0 {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": strings.Join(notes, "\n")}}})
	}
	if a.replies {
		labels := make([]string, len(o.Choices))
		for i, c := range o.Choices {
			labels[i] = "*" + escape(clipRunes(w.ChoiceWording(c).Label, 75)) + "*"
		}
		line := "Reply in this thread with " + orAll(labels) + " to answer."
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": clipRunes(line, 2000)}}})
		return laidOut{text: clipRunes(text, maxSection), blocks: blocks, shown: shown}, true
	}
	var buttons []map[string]any
	for i, c := range o.Choices {
		v, _ := json.Marshal(buttonValue{Work: a.work, Provider: o.Provider, Action: o.ID, Input: c})
		if len(v) > 2000 {
			return laidOut{}, false
		}
		cw := w.ChoiceWording(c)
		button := map[string]any{"type": "button", "action_id": buttonPrefix + strconv.Itoa(i),
			"text": map[string]any{"type": "plain_text", "text": clipRunes(cw.Label, 75)}, "value": string(v)}
		switch cw.Tone {
		case extension.ToneAffirm:
			button["style"] = "primary"
		case extension.ToneDanger:
			button["style"] = "danger"
		}
		if cw.Confirm != "" {
			confirm := map[string]any{"title": map[string]any{"type": "plain_text", "text": "Are you sure?"},
				"text":    map[string]any{"type": "plain_text", "text": clipRunes(cw.Confirm, 300)},
				"confirm": map[string]any{"type": "plain_text", "text": clipRunes(cw.Label, 30)},
				"deny":    map[string]any{"type": "plain_text", "text": "Cancel"}}
			if cw.Tone == extension.ToneDanger {
				confirm["style"] = "danger"
			}
			button["confirm"] = confirm
		}
		buttons = append(buttons, button)
	}
	blocks = append(blocks, map[string]any{"type": "actions", "elements": buttons})
	return laidOut{text: clipRunes(text, maxSection), blocks: blocks, shown: shown}, true
}

// answerer is who settled a request, as Slack shows them: a Slack person
// mentioned, anyone else as someone outside Slack.
func answerer(by string) string {
	if id, ok := strings.CutPrefix(by, Type+":"); ok && slackIDPattern.MatchString(id) {
		return "<@" + id + ">"
	}
	if by == "" {
		return ""
	}
	return "someone outside Slack"
}

// ended is the one line that says how a settled request ended, for the
// message that asked it: who answered, and when, in each reader's own
// time zone.
func ended(r extension.Request) string {
	by, at := "", ""
	if who := answerer(r.By); who != "" {
		by = " by " + who
	}
	if !r.SettledAt.IsZero() {
		t := r.SettledAt.UTC()
		at = fmt.Sprintf(" at <!date^%d^{time}|%s>", t.Unix(), t.Format("15:04 UTC"))
	}
	approval := r.Wording != nil && r.Wording.Wants != ""
	switch r.State {
	case extension.RequestAnswered:
		if approval {
			return "✅ Approved" + by + at
		}
		return "✅ Answered" + by + at
	case extension.RequestRefused:
		return "❌ Refused" + by + at
	case extension.RequestExpired:
		return "Nobody answered in time, so it expired."
	case extension.RequestWithdrawn:
		return "Withdrawn: the agent stopped waiting for an answer."
	}
	return "This request is closed."
}

// closedBlocks are a request's message once settled: what it asked,
// without its buttons, and the line saying how it ended.
func closedBlocks(shown []json.RawMessage, line string) []json.RawMessage {
	out := append([]json.RawMessage{}, shown...)
	return append(out, raw(map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": line}}}))
}

// requestFor is the open request the offer answers, among rs: the one of
// the offer's provider that names its action.
func requestFor(rs []extension.RequestOffer, o extension.ActionOffer) *extension.Request {
	for i := range rs {
		if rs[i].Provider == o.Provider && rs[i].Action == o.ID && rs[i].State == extension.RequestOpen {
			r := rs[i].Request
			return &r
		}
	}
	return nil
}

// plainLine is a line of mrkdwn as notification text: a date token reads
// as its fallback, a mention as it is.
func plainLine(line string) string {
	for {
		i := strings.Index(line, "<!date^")
		if i < 0 {
			return line
		}
		j := strings.IndexByte(line[i:], '>')
		bar := strings.LastIndexByte(line[i:i+max(j, 0)], '|')
		if j < 0 || bar < 0 {
			return line
		}
		line = line[:i] + line[i+bar+1:i+j] + line[i+j+1:]
	}
}

// closePrompts changes each posted prompt whose request is no longer open
// to say how it ended, without its buttons, at most limit of them. It
// returns the changes it made and safe codes for what failed.
func (p *Projector) closePrompts(ctx context.Context, limit int) (int, []string, error) {
	if p.o.Requests == nil || limit <= 0 {
		return 0, nil, nil
	}
	type open struct {
		key string
		rec intentRecord
	}
	var posted []open
	if err := p.transact(ctx, func(tx storage.Tx) error {
		posted = nil
		rows, err := tx.List(p.ns, "intent/")
		for _, row := range rows {
			var rec intentRecord
			if json.Unmarshal(row.Value, &rec) == nil && rec.Asks != nil && !rec.Closed && rec.State == IntentPosted && rec.TS != "" {
				posted = append(posted, open{row.Key, rec})
			}
		}
		return err
	}); err != nil {
		return 0, nil, err
	}
	n := 0
	var codes []string
	requests := map[domain.ID][]extension.RequestOffer{}
	for _, o := range posted {
		if n >= limit {
			break
		}
		rs, ok := requests[o.rec.Work]
		if !ok {
			var err error
			if rs, err = p.o.Requests(ctx, domain.ID(p.o.Settings.Scope), o.rec.Work); err != nil {
				p.hostRefused("requests", err)
				continue
			}
			requests[o.rec.Work] = rs
		}
		line := "This request is closed."
		settled := true
		for _, r := range rs {
			if r.Provider != o.rec.Asks.Provider || r.ID != o.rec.Asks.Request {
				continue
			}
			if r.State == extension.RequestOpen {
				settled = false
				break
			}
			line = ended(r.Request)
		}
		if !settled || p.o.Settings.Projection.DryRun {
			continue
		}
		n++
		text := plainLine(line)
		err := p.o.Client.UpdateMessage(ctx, Update{Channel: o.rec.Channel, TS: o.rec.TS, Text: text, Blocks: closedBlocks(o.rec.Shown, line)})
		var refused *APIError
		var rate *RateLimitError
		switch {
		case errors.As(err, &refused) && (refused.Code == "message_not_found" || refused.Code == "channel_not_found"):
			// Deleted in Slack: nothing is left to change.
		case errors.As(err, &rate):
			return n, append(codes, "prompt:rate_limited"), err
		case errors.Is(err, ErrCredential):
			return n, append(codes, "prompt:credential"), err
		case err != nil:
			codes = append(codes, "prompt:"+ErrorCode(err))
			continue
		}
		if err := p.transact(ctx, func(tx storage.Tx) error {
			var cur intentRecord
			if found, err := p.get(tx, o.key, &cur); err != nil || !found {
				return err
			}
			cur.Closed = true
			return p.put(tx, o.key, cur)
		}); err != nil {
			return n, codes, err
		}
	}
	return n, codes, nil
}
