package slack

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Feedback buttons (issue #675). With feedback enabled, the host puts
// Slack's feedback buttons (a context_actions block) below the replies of
// the bindings the entry names: slack.reply's and a conversation's outcome
// reply, unless a slack.reply call turns them off. A press arrives over
// Socket Mode like a prompt's; Slack vouches for who pressed it. The
// extension accepts it as a TypeFeedback Event on the Work that posted the
// reply, which the slack route keeps in that Work's history without making
// it input (DeliverNone), so no run wakes for it and finished Work opens
// no successor. Each press is its own Event; the record is never
// overwritten, and a person's latest press on a reply is what counts. What
// feedback means, and what to do with it, is whoever reads the history's.

// feedback takes a press of a reply's feedback buttons: it is accepted as
// TypeFeedback on the Work whose reply it is, when that Work is this
// entry's and holds the reply's thread. Only a press the host could not
// record is answered, asking the person to press again.
func (p *Projector) feedback(ctx context.Context, in interaction, value, at string) error {
	var v feedbackValue
	if p.o.Accept == nil || json.Unmarshal([]byte(value), &v) != nil || v.Work == "" || v.Value != FeedbackPositive && v.Value != FeedbackNegative ||
		!tsPattern.MatchString(at) || !tsPattern.MatchString(in.Container.MessageTS) {
		return nil
	}
	thread := in.Container.ThreadTS
	if thread == "" {
		thread = in.Container.MessageTS
	}
	if !tsPattern.MatchString(thread) {
		return nil
	}
	data, err := json.Marshal(Feedback{Namespace: p.o.Settings.Namespace, Team: in.Team.ID, Channel: in.Container.ChannelID, Thread: thread,
		Message: in.Container.MessageTS, User: in.User.ID, Value: v.Value, Execution: string(v.Execution), Intent: v.Intent, PressedAt: at})
	if err != nil {
		return err
	}
	now := p.o.Now().UTC()
	pressed, ok := TSTime(at)
	if !ok {
		pressed = now
	}
	e := domain.Event{Type: TypeFeedback, Source: p.o.Settings.Namespace,
		DeliveryID: "feedback/" + digest(p.o.Settings.Namespace, in.Container.ChannelID, in.Container.MessageTS, in.User.ID, at)[:32],
		WorkID:     v.Work, OccurredAt: pressed.UTC(), ReceivedAt: now, Data: domain.Payload{Schema: SchemaFeedback, Data: data}}
	err = p.transact(ctx, func(tx storage.Tx) error {
		w, err := tx.Work(v.Work)
		if err != nil {
			return err
		}
		if held, _ := holds(w, p.o.Settings.Namespace, in.Container.ChannelID, thread); !held || string(w.ScopeID) != p.o.Settings.Scope {
			return storage.ErrNotFound
		}
		return p.o.Accept(tx, domain.ID(p.o.Settings.Scope), "", e)
	})
	if err == nil || errors.Is(err, storage.ErrNotFound) {
		// A reply of Work that is gone, or isn't this entry's: nothing
		// to record it on, and nothing the person can do about it.
		return nil
	}
	if !p.o.Settings.Projection.DryRun {
		if e := p.o.Client.PostEphemeral(ctx, Ephemeral{Channel: in.Container.ChannelID, User: in.User.ID, ThreadTS: thread,
			Text: "Open Conduit couldn't record that feedback just now. Please press it again."}); e != nil {
			return errors.Join(err, e)
		}
	}
	return err
}
