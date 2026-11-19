package slack

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Request messages (issue #758). A request's named responder may be nowhere
// it shows: Work a schedule started has no thread, and an approver need not
// be in the conversation that asked. With request_messages on, each Slack
// person a request names as a responder (slack:<member ID>; a bare slack
// names nobody) is sent a direct message by the app: who asks and what, the
// provider's detail, a link to the thread when the Work has one, and the
// buttons of the action that answers it. A press answers as slack:<user>,
// as a press in a thread does, and the provider decides whether they may.
// Once the request is no longer open, however it ended and wherever it was
// answered, the message is changed to say so and loses its buttons. Who may
// answer, and what an answer means, stay the provider's: the messages
// follow its requests (Host.Requests) and its actions (Host.Actions).
//
// Each message is one request's to one person, with a durable record: it
// is posted once (an ambiguous post is found again by its marker in the DM
// before it is tried again) and closed once.

// Request message states: pending until a post is tried; attempted until
// its answer or its marker proves it; posted while the request is open;
// closed once changed to how it ended; failed when Slack refused the post.
const (
	messagePending   = "pending"
	messageAttempted = "attempted"
	messagePosted    = "posted"
	messageClosed    = "closed"
	messageFailed    = "failed"
)

// requestMessage is the record of one request's direct message to one
// person.
type requestMessage struct {
	ID       string    `json:"id"`
	Work     domain.ID `json:"work"`
	Provider string    `json:"provider"`
	Request  string    `json:"request"`
	User     string    `json:"user"`
	// Title is the message's first line (who asks, and what), kept to
	// say how the request ended in the same words.
	Title       string    `json:"title"`
	Link        string    `json:"link,omitempty"`
	Channel     string    `json:"channel,omitempty"`
	TS          string    `json:"ts,omitempty"`
	State       string    `json:"state"`
	Code        string    `json:"code,omitempty"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at,omitzero"`
}

func requestMessageKey(id string) string { return "dm/" + digest(id)[:40] }

// requestMessageID names one request's message to one person.
func requestMessageID(work domain.ID, provider, request, user string) string {
	return "request/" + string(work) + "/" + provider + "/" + request + "/" + user
}

// namedSlackPeople are the Slack people responders name, each once: a
// slack:<member ID>, never a bare slack.
func namedSlackPeople(responders []string) []string {
	var out []string
	for _, r := range responders {
		id, ok := strings.CutPrefix(r, Type+":")
		if !ok || !slackIDPattern.MatchString(id) || id[0] != 'U' && id[0] != 'W' {
			continue
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// wanted is a request message the requests call for now, and what it
// shows.
type wanted struct {
	requestMessage
	text   string
	blocks []map[string]any
	// thread is the conversation the request was asked from, linked once
	// the message is first posted.
	channel, thread string
}

// requestMessages runs one pass: it posts the messages open requests call
// for, and closes those whose request is no longer open. It returns the
// writes it made and safe codes for what failed.
func (p *Projector) requestMessages(ctx context.Context, limit int) (int, []string, error) {
	if !p.o.Settings.RequestMessages || p.o.Requests == nil || p.o.Actions == nil || limit <= 0 {
		return 0, nil, nil
	}
	want, err := p.wantedMessages(ctx)
	if err != nil {
		return 0, nil, err
	}
	n := 0
	var codes []string
	stop := func(err error) bool {
		var rate *RateLimitError
		switch {
		case errors.As(err, &rate):
			codes = append(codes, "request_message:rate_limited")
			return true
		case errors.Is(err, ErrCredential):
			codes = append(codes, "request_message:credential")
			return true
		case err != nil:
			codes = append(codes, "request_message:"+ErrorCode(err))
		}
		return false
	}
	for _, w := range want {
		if n >= limit {
			return n, codes, nil
		}
		wrote, err := p.sendRequestMessage(ctx, w)
		if wrote {
			n++
		}
		if stop(err) {
			return n, codes, err
		}
	}
	open := map[string]bool{}
	for _, w := range want {
		open[w.ID] = true
	}
	var posted []requestMessage
	if err := p.transact(ctx, func(tx storage.Tx) error {
		posted = nil
		rows, err := tx.List(p.ns, "dm/")
		for _, row := range rows {
			var rec requestMessage
			// An attempted post is closed too: its request may have
			// settled before the marker search found it.
			if json.Unmarshal(row.Value, &rec) == nil && (rec.State == messagePosted || rec.State == messageAttempted) {
				posted = append(posted, rec)
			}
		}
		return err
	}); err != nil {
		return n, codes, err
	}
	for _, rec := range posted {
		if n >= limit {
			break
		}
		if open[rec.ID] {
			continue
		}
		if rec.State == messageAttempted {
			found, err := p.resolveAttempted(ctx, rec)
			if stop(err) {
				return n, codes, err
			}
			if err != nil || found.State != messagePosted {
				continue
			}
			rec = found
		}
		wrote, err := p.closeRequestMessage(ctx, rec)
		if wrote {
			n++
		}
		if stop(err) {
			return n, codes, err
		}
	}
	return n, codes, nil
}

// wantedMessages are the messages the scope's open requests call for: one
// to each Slack person an open request names, for a request its action
// answers alone (as for a child's prompt), with that action's buttons.
func (p *Projector) wantedMessages(ctx context.Context) ([]wanted, error) {
	type asking struct {
		asker
		channel, thread string
	}
	var as []asking
	err := p.transact(ctx, func(tx storage.Tx) error {
		as = nil
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		byID := make(map[domain.ID]domain.Work, len(works))
		for _, w := range works {
			byID[w.ID] = w
		}
		for _, w := range works {
			if string(w.ScopeID) != p.o.Settings.Scope || w.State != domain.WorkWaiting && w.State != domain.WorkReady {
				continue
			}
			binding, err := latestBinding(tx, w.ID)
			if err != nil {
				return err
			}
			a := asking{asker: asker{work: w, who: p.who(w, binding)}}
			if c, t, ok := p.Thread(w); ok {
				a.channel, a.thread = c, t
			} else if anc, ok := p.conversationOf(w, byID); ok {
				a.channel, a.thread, _ = p.Thread(anc)
			}
			as = append(as, a)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []wanted
	for _, a := range as {
		rs, err := p.o.Requests(ctx, domain.ID(p.o.Settings.Scope), a.work.ID)
		if err != nil {
			continue
		}
		named := map[string]int{}
		for _, r := range rs {
			if r.State == extension.RequestOpen && r.Action != "" {
				named[r.Provider+"/"+r.Action]++
			}
		}
		var offers []extension.ActionOffer
		for _, r := range rs {
			people := namedSlackPeople(r.Responders)
			if r.State != extension.RequestOpen || r.Action == "" || named[r.Provider+"/"+r.Action] != 1 || len(people) == 0 {
				continue
			}
			if offers == nil {
				if offers, err = p.o.Actions(ctx, domain.ID(p.o.Settings.Scope), a.work.ID); err != nil {
					break
				}
			}
			for _, o := range offers {
				if o.Provider != r.Provider || o.ID != r.Action {
					continue
				}
				shown := o
				shown.Label = a.who + " asks: " + o.Label
				shown.Detail = strings.TrimSpace(o.Detail + "\n\nWho can answer: " + strings.Join(r.Responders, ", "))
				blocks, ok := promptBlocks(a.work.ID, shown)
				if !ok {
					continue
				}
				for _, user := range people {
					id := requestMessageID(a.work.ID, r.Provider, r.ID, user)
					out = append(out, wanted{requestMessage: requestMessage{ID: id, Work: a.work.ID, Provider: r.Provider, Request: r.ID, User: user, Title: shown.Label},
						text: shown.Label, blocks: blocks, channel: a.channel, thread: a.thread})
				}
			}
		}
	}
	return out, nil
}

// sendRequestMessage posts w once, recovering an ambiguous post through
// its marker in the person's DM. wrote says it called Slack to post.
func (p *Projector) sendRequestMessage(ctx context.Context, w wanted) (bool, error) {
	key := requestMessageKey(w.ID)
	var rec requestMessage
	err := p.transact(ctx, func(tx storage.Tx) error {
		found, err := p.get(tx, key, &rec)
		if err != nil || found {
			return err
		}
		rec = w.requestMessage
		rec.State, rec.CreatedAt = messagePending, p.o.Now().UTC()
		return p.put(tx, key, rec)
	})
	if err != nil || rec.State != messagePending && rec.State != messageAttempted {
		return false, err
	}
	marker := MarkerID(w.ID)
	if rec.State == messageAttempted {
		if p.o.Now().Sub(rec.AttemptedAt) < p.o.SearchDelay {
			return false, nil
		}
		channel, ts, err := p.findRequestMessage(ctx, rec, marker)
		var refused *APIError
		if errors.As(err, &refused) {
			// The DM can't be read (a token without im:read or
			// im:history): it is sent again rather than found.
			err, ts = nil, ""
		}
		if err != nil {
			return false, err
		}
		if ts != "" {
			return false, p.recordRequestMessage(ctx, key, rec, messagePosted, channel, ts, "")
		}
	}
	if p.o.Settings.Projection.DryRun {
		return false, nil
	}
	if rec.Link == "" && w.thread != "" {
		// The link is best effort: a message without it still asks.
		if link, err := p.o.Client.Permalink(ctx, w.channel, w.thread); err == nil && strings.HasPrefix(link, "https://") {
			rec.Link = link
		}
	}
	blocks := w.blocks
	if rec.Link != "" {
		blocks = append(append([]map[string]any{}, w.blocks[:len(w.blocks)-1]...),
			map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": "<" + rec.Link + "|Open the conversation it was asked in>"}}},
			w.blocks[len(w.blocks)-1])
	}
	if err := p.transact(ctx, func(tx storage.Tx) error {
		var cur requestMessage
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.State, cur.Attempts, cur.AttemptedAt, cur.Link = messageAttempted, cur.Attempts+1, p.o.Now().UTC(), rec.Link
		rec = cur
		return p.put(tx, key, cur)
	}); err != nil {
		return false, err
	}
	channel, ts, err := p.o.Client.PostMessageIn(ctx, Post{Channel: w.User, Text: w.text, Blocks: raws(blocks), Metadata: Marker(w.Work, "", w.ID)})
	var limit *RateLimitError
	var refused *APIError
	switch {
	case errors.As(err, &limit), errors.Is(err, ErrCredential):
		// Refused before Slack published anything: not ambiguous.
		return true, errors.Join(err, p.recordRequestMessage(ctx, key, rec, messagePending, "", "", ""))
	case errors.As(err, &refused):
		return true, errors.Join(err, p.recordRequestMessage(ctx, key, rec, messageFailed, "", "", refused.Code))
	case err != nil:
		// Ambiguous: the marker search decides after SearchDelay.
		return true, err
	}
	return true, p.recordRequestMessage(ctx, key, rec, messagePosted, channel, ts, "")
}

// findRequestMessage looks for a post whose answer was lost: in the app's
// DM with the person, by its marker.
func (p *Projector) findRequestMessage(ctx context.Context, rec requestMessage, marker string) (channel, ts string, err error) {
	dms, _, _, err := p.o.Client.DirectMessages(ctx, 10)
	if err != nil {
		return "", "", err
	}
	for _, dm := range dms {
		if dm.User != rec.User {
			continue
		}
		messages, err := p.since(ctx, dm.ID, "", TS(rec.CreatedAt.Add(-time.Minute)))
		if err != nil {
			return "", "", err
		}
		if ts, ok := p.find(ctx, messages, marker, rec.Title); ok {
			return dm.ID, ts, nil
		}
	}
	return "", "", nil
}

// resolveAttempted settles an attempted post whose request no longer calls
// for it: once the search delay has passed, the post found in the DM by its
// marker is recorded as posted (and returned, to be closed), and one not
// there is closed as never posted, unless the request is still open for
// the person, when it is left to be sent once it is wanted again. Before
// the delay it is returned as it was.
func (p *Projector) resolveAttempted(ctx context.Context, rec requestMessage) (requestMessage, error) {
	if p.o.Now().Sub(rec.AttemptedAt) < p.o.SearchDelay {
		return rec, nil
	}
	key := requestMessageKey(rec.ID)
	channel, ts, err := p.findRequestMessage(ctx, rec, MarkerID(rec.ID))
	var refused *APIError
	if errors.As(err, &refused) {
		// Unreadable (no im:read or im:history, which doctor warns of):
		// whether it was posted can't be told, and asking again each
		// pass would never tell, so it is closed as it stands.
		return rec, p.recordRequestMessage(ctx, key, rec, messageClosed, "", "", "dm_unreadable")
	}
	if err != nil {
		return rec, err
	}
	if ts != "" {
		if err := p.recordRequestMessage(ctx, key, rec, messagePosted, channel, ts, ""); err != nil {
			return rec, err
		}
		rec.State, rec.Channel, rec.TS = messagePosted, channel, ts
		return rec, nil
	}
	rs, err := p.o.Requests(ctx, domain.ID(p.o.Settings.Scope), rec.Work)
	if err != nil {
		return rec, err
	}
	for _, r := range rs {
		if r.Provider == rec.Provider && r.ID == rec.Request && r.State == extension.RequestOpen && slices.Contains(namedSlackPeople(r.Responders), rec.User) {
			return rec, nil
		}
	}
	return rec, p.recordRequestMessage(ctx, key, rec, messageClosed, "", "", "")
}

// recordRequestMessage moves the record from rec's state to state.
func (p *Projector) recordRequestMessage(ctx context.Context, key string, rec requestMessage, state, channel, ts, code string) error {
	return p.transact(ctx, func(tx storage.Tx) error {
		var cur requestMessage
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.State, cur.Code = state, code
		if channel != "" {
			cur.Channel, cur.TS = channel, ts
		}
		return p.put(tx, key, cur)
	})
}

// closeRequestMessage changes a posted message whose request is no longer
// open to say how it ended, without its buttons. wrote says it called
// Slack.
func (p *Projector) closeRequestMessage(ctx context.Context, rec requestMessage) (bool, error) {
	ended := "This request is closed."
	rs, err := p.o.Requests(ctx, domain.ID(p.o.Settings.Scope), rec.Work)
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		if r.Provider != rec.Provider || r.ID != rec.Request {
			continue
		}
		if r.State == extension.RequestOpen {
			// Open, though no longer one this person is sent (they are no
			// longer a responder, or its action is not offered now): it
			// is closed once it settles.
			if slices.Contains(namedSlackPeople(r.Responders), rec.User) {
				return false, nil
			}
			ended = "You are no longer asked to answer this."
			break
		}
		ended = requestEnded(r)
	}
	if p.o.Settings.Projection.DryRun {
		return false, nil
	}
	text := rec.Title + "\n\n" + ended
	blocks := []map[string]any{{"type": "section", "text": map[string]any{"type": "plain_text", "text": truncate(text, 3000)}}}
	if rec.Link != "" {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": "<" + rec.Link + "|Open the conversation it was asked in>"}}})
	}
	err = p.o.Client.UpdateMessage(ctx, Update{Channel: rec.Channel, TS: rec.TS, Text: truncate(text, 3000), Blocks: blocks})
	var refused *APIError
	if errors.As(err, &refused) && (refused.Code == "message_not_found" || refused.Code == "channel_not_found") {
		// Deleted in Slack: nothing is left to change.
		err = nil
	}
	if err != nil {
		return true, err
	}
	return true, p.recordRequestMessage(ctx, requestMessageKey(rec.ID), rec, messageClosed, "", "", "")
}

// requestEnded says how a settled request ended.
func requestEnded(r extension.RequestOffer) string {
	by := ""
	if r.By != "" {
		by = " by " + r.By
	}
	switch r.State {
	case extension.RequestAnswered:
		return "Answered" + by + "."
	case extension.RequestRefused:
		return "Refused" + by + "."
	case extension.RequestExpired:
		return "Nobody answered in time, so it expired."
	case extension.RequestWithdrawn:
		return "Withdrawn: the agent stopped waiting for an answer."
	}
	return "This request is closed."
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
