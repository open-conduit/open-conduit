package slack

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Prompts and button answers (issue #364). While a conversation's Work
// waits, every action offered on it with choices (an approval request's
// approve and refuse, a question's options) is posted once in its thread as
// a message with its label, the provider's detail (what is being decided)
// and one button per choice. A press arrives over Socket Mode as
// an interactive envelope; the extension hands it to the host's Act as the
// person who pressed it, slack:<user>, and the action's provider decides
// whether they may. The person is told the outcome in a message only they
// see. Nothing here knows what the actions mean.

// ActionsFunc lists the actions offered on Work (inproc.Host.Actions).
type ActionsFunc func(ctx context.Context, scope, work domain.ID) ([]extension.ActionOffer, error)

// ActFunc invokes an action for a person (inproc.Host.Act).
type ActFunc func(ctx context.Context, r extension.ActRequest) (extension.ActResult, error)

// buttonPrefix starts the action_id of every button a prompt carries.
const buttonPrefix = "oc_answer_"

// buttonValue is what a prompt's button carries back: the Work, the
// provider and action it offers, and the choice.
type buttonValue struct {
	Work     domain.ID `json:"w"`
	Provider string    `json:"p"`
	Action   string    `json:"a"`
	Input    string    `json:"i"`
}

// promptIntents derives a prompt for each action with choices offered on a
// conversation's Work that is waiting, or ready but not started (held for an
// approval, say), once for what it offers. What is offered, and when, is
// each provider's: Slack only shows it.
func (p *Projector) promptIntents(ctx context.Context) ([]Intent, error) {
	if p.o.Actions == nil {
		return nil, nil
	}
	type waiting struct {
		work            domain.ID
		channel, thread string
		// binding is the Work's latest attempt's: its prompts post under
		// that binding's identity.
		binding string
	}
	var ws []waiting
	err := p.transact(ctx, func(tx storage.Tx) error {
		ws = nil
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		for _, w := range works {
			if string(w.ScopeID) != p.o.Settings.Scope || w.State != domain.WorkWaiting && w.State != domain.WorkReady {
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
			binding := ""
			var latest uint32
			for _, x := range xs {
				if x.Attempt >= latest {
					latest, binding = x.Attempt, x.ExecutorBinding
				}
			}
			if w.State == domain.WorkReady {
				ws = append(ws, waiting{w.ID, channel, thread, binding})
				continue
			}
			waits, err := tx.ListWaits(w.ID)
			if err != nil {
				return err
			}
			for _, wt := range waits {
				if wt.State == domain.WaitActive {
					ws = append(ws, waiting{w.ID, channel, thread, binding})
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Intent
	seen := map[string]bool{}
	for _, w := range ws {
		offers, err := p.o.Actions(ctx, domain.ID(p.o.Settings.Scope), w.work)
		if err != nil {
			continue
		}
		for _, o := range offers {
			blocks, ok := promptBlocks(w.work, o)
			if !ok {
				continue
			}
			// Keyed by what is offered, not by the Wait: the same open
			// question is prompted once however often the Work waits, and
			// a new question (new text or options) is prompted afresh.
			id := "prompt/" + string(w.work) + "/" + o.Provider + "/" + o.ID + "/" + digest(append([]string{o.Label, o.Detail}, o.Choices...)...)[:16]
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, Intent{ID: id, Work: w.work, Channel: w.channel, Thread: w.thread, Text: o.Label, Blocks: blocks, Binding: w.binding})
		}
	}
	return out, nil
}

// promptBlocks is the message for one offered action: its label and a
// button per choice. An action without choices takes free text, which a
// button can't carry, so it is not prompted.
func promptBlocks(work domain.ID, o extension.ActionOffer) ([]map[string]any, bool) {
	if len(o.Choices) == 0 || len(o.Choices) > 20 || strings.TrimSpace(o.Label) == "" {
		return nil, false
	}
	var buttons []map[string]any
	for i, c := range o.Choices {
		v, _ := json.Marshal(buttonValue{Work: work, Provider: o.Provider, Action: o.ID, Input: c})
		if len(v) > 2000 {
			return nil, false
		}
		label := c
		if len(label) > 75 {
			label = strings.ToValidUTF8(label[:72], "") + "..."
		}
		buttons = append(buttons, map[string]any{"type": "button", "action_id": buttonPrefix + strconv.Itoa(i),
			"text": map[string]any{"type": "plain_text", "text": label}, "value": string(v)})
	}
	// The detail is what the person is deciding: the question, or the call
	// an approval is for. The provider writes it; Slack only shows it.
	text := o.Label
	if d := strings.TrimSpace(o.Detail); d != "" {
		text += "\n\n" + d
	}
	if len(text) > 3000 {
		text = strings.ToValidUTF8(text[:3000], "")
	}
	return []map[string]any{
		{"type": "section", "text": map[string]any{"type": "plain_text", "text": text}},
		{"type": "actions", "elements": buttons},
	}, true
}

// interaction is the part of a block_actions payload this extension reads.
type interaction struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
		ThreadTS  string `json:"thread_ts"`
	} `json:"container"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

// Interact takes one interactive payload: a press of a prompt's button is
// handed to Act as its presser, and they are told the outcome. Anything
// else is ignored. An error is one the host could not act on now; the
// press is not retried (Slack has been acknowledged), so the person is
// asked to press again.
func (p *Projector) Interact(ctx context.Context, payload []byte) error {
	var in interaction
	if json.Unmarshal(payload, &in) != nil || in.Type != "block_actions" || p.o.Act == nil {
		return nil
	}
	if !slackIDPattern.MatchString(in.User.ID) || p.o.Settings.Team != "" && in.Team.ID != p.o.Settings.Team || !p.o.Settings.Observes(in.Container.ChannelID) {
		return nil
	}
	for _, a := range in.Actions {
		if !strings.HasPrefix(a.ActionID, buttonPrefix) {
			continue
		}
		var v buttonValue
		if json.Unmarshal([]byte(a.Value), &v) != nil || v.Work == "" || v.Action == "" || !tsPattern.MatchString(a.ActionTS) {
			return nil
		}
		res, err := p.o.Act(ctx, extension.ActRequest{Scope: domain.ID(p.o.Settings.Scope), Work: v.Work, Provider: v.Provider, Action: v.Action, Input: v.Input,
			Caller: in.User.ID, Key: "block/" + in.User.ID + "/" + a.ActionTS})
		var oe *extension.OperationError
		msg := ""
		switch {
		case err == nil:
			msg = res.Message
			if msg == "" {
				msg = "Answered: " + v.Input
			}
		case errors.As(err, &oe):
			msg = oe.Message
		case errors.Is(err, storage.ErrNotFound):
			msg = "That request is no longer open."
		default:
			msg = "Open Conduit couldn't take that answer just now. Please press it again."
		}
		thread := in.Container.ThreadTS
		if thread == "" {
			thread = in.Container.MessageTS
		}
		if p.o.Settings.Projection.DryRun {
			return err
		}
		if e := p.o.Client.PostEphemeral(ctx, Ephemeral{Channel: in.Container.ChannelID, User: in.User.ID, ThreadTS: thread, Text: msg}); e != nil && err == nil {
			err = e
		}
		if oe != nil || errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}
