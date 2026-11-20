package slack

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Answers by reply (issue #743). An entry that can't receive a button press
// (receiver none: Settings.Presses) posts its prompts without buttons. Each
// names its choices and asks for a reply in the thread. A person's reply
// there that says one of them, and nothing else, answers the prompt's
// action through Host.Act as that person, slack:<user>, exactly as a press
// would. The action's provider still decides whether they may. They are
// told the outcome in a message only they see. The reply is still the
// thread's message, delivered to the Work as any other.
//
// Replies are read from the scope's accepted Events, not from Slack, behind
// one cursor in the projector's namespace, so a pass costs no Slack call
// until a reply matches. Each answer is keyed reply/<user>/<ts>, so a pass
// retried after a crash answers once.

// promptReplies is the action a prompt without buttons is answered through,
// and its choices.
type promptReplies struct {
	Provider string         `json:"provider"`
	Action   string         `json:"action"`
	Choices  []promptChoice `json:"choices"`
}

// promptChoice is one of a prompt's choices: the input it answers with and
// the label the prompt shows.
type promptChoice struct {
	Input string `json:"input"`
	Label string `json:"label"`
}

// repliesCursor keys the sequence of the last scope Event the reply pass
// read.
const repliesCursor = "replies/cursor"

type sequenceCursor struct {
	After uint64 `json:"after"`
}

// replyPages bounds how many pages of Events one pass reads.
const replyPages = 4

// replyWords is text as a reply is matched against a choice: the app's
// mention and surrounding punctuation and space dropped, case folded.
func replyWords(text, botUser string) string {
	if botUser != "" {
		text = strings.ReplaceAll(text, "<@"+botUser+">", "")
	}
	return strings.ToLower(strings.Trim(text, " \t\r\n.!*_`\"'"))
}

// choose is the choice text names, by its label or its input; false when
// it names none, or more than one.
func (r *promptReplies) choose(text, botUser string) (promptChoice, bool) {
	said := replyWords(text, botUser)
	if said == "" {
		return promptChoice{}, false
	}
	var found []promptChoice
	for _, c := range r.Choices {
		if replyWords(c.Label, "") == said || replyWords(c.Input, "") == said {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		return promptChoice{}, false
	}
	return found[0], true
}

// tsAfter reports whether Slack timestamp a is later than b.
func tsAfter(a, b string) bool {
	as, af, _ := strings.Cut(a, ".")
	bs, bf, _ := strings.Cut(b, ".")
	ai, _ := strconv.ParseInt(as, 10, 64)
	bi, _ := strconv.ParseInt(bs, 10, 64)
	if ai != bi {
		return ai > bi
	}
	for len(af) < len(bf) {
		af += "0"
	}
	for len(bf) < len(af) {
		bf += "0"
	}
	return af > bf
}

// answerReplies takes the replies that answer prompts without buttons, at
// most limit of them. It reads only while such a prompt is open; the cursor
// stays before a reply the host could not act on, so the next pass tries it
// again.
func (p *Projector) answerReplies(ctx context.Context, limit int) (int, []string, error) {
	if p.o.Settings.Presses() || p.o.Act == nil || limit <= 0 {
		return 0, nil, nil
	}
	type open struct {
		key string
		rec intentRecord
	}
	var prompts []open
	var cur sequenceCursor
	if err := p.transact(ctx, func(tx storage.Tx) error {
		prompts, cur = nil, sequenceCursor{}
		if _, err := p.get(tx, repliesCursor, &cur); err != nil {
			return err
		}
		rows, err := tx.List(p.ns, "intent/")
		for _, row := range rows {
			var rec intentRecord
			if json.Unmarshal(row.Value, &rec) == nil && rec.Replies != nil && !rec.Answered && !rec.Closed && rec.State != IntentFailed && rec.State != IntentPending {
				prompts = append(prompts, open{row.Key, rec})
			}
		}
		return err
	}); err != nil || len(prompts) == 0 {
		return 0, nil, err
	}
	n := 0
	var codes []string
	save := func() error {
		return p.transact(ctx, func(tx storage.Tx) error { return p.put(tx, repliesCursor, cur) })
	}
	for page := 0; page < replyPages && n < limit; page++ {
		var events []domain.AcceptedEvent
		if err := p.transact(ctx, func(tx storage.Tx) error {
			var err error
			events, err = tx.ScopeEvents([]domain.ID{domain.ID(p.o.Settings.Scope)}, cur.After, 500)
			return err
		}); err != nil {
			return n, codes, err
		}
		for _, a := range events {
			if n >= limit {
				return n, codes, save()
			}
			e := a.Event
			var pl MessagePayload
			if e.Type != TypeMessageHuman || e.Source != p.o.Settings.Namespace || e.Data.Schema != SchemaMessage ||
				json.Unmarshal(e.Data.Data, &pl) != nil || pl.Edited != "" || pl.Deleted || pl.Thread == "" || !slackIDPattern.MatchString(pl.User) || !tsPattern.MatchString(pl.TS) {
				cur.After = a.Sequence
				continue
			}
			for i := range prompts {
				o := &prompts[i]
				if o.rec.Answered || o.rec.Channel != pl.Channel || o.rec.Thread != pl.Thread || o.rec.TS != "" && !tsAfter(pl.TS, o.rec.TS) {
					continue
				}
				c, ok := o.rec.Replies.choose(pl.Text, p.o.Settings.BotUser)
				if !ok {
					continue
				}
				n++
				v := buttonValue{Work: o.rec.Work, Provider: o.rec.Replies.Provider, Action: o.rec.Replies.Action, Input: c.Input}
				taken, err := p.answer(ctx, v, pl.User, pl.Channel, pl.Thread, "reply/"+pl.User+"/"+pl.TS, "")
				if taken {
					o.rec.Answered = true
					if e := p.markAnswered(ctx, o.key); e != nil {
						return n, codes, e
					}
				}
				if err != nil {
					var rate *RateLimitError
					if errors.As(err, &rate) || errors.Is(err, ErrCredential) || !taken {
						// Not taken, or Slack is refusing for now: the
						// reply is read again next pass.
						return n, append(codes, "prompt_reply:"+ErrorCode(err)), save()
					}
					codes = append(codes, "prompt_reply:"+ErrorCode(err))
				}
				break
			}
			cur.After = a.Sequence
		}
		if len(events) < 500 {
			break
		}
	}
	return n, codes, save()
}

// markAnswered records that a reply answered the prompt at key.
func (p *Projector) markAnswered(ctx context.Context, key string) error {
	return p.transact(ctx, func(tx storage.Tx) error {
		var rec intentRecord
		if found, err := p.get(tx, key, &rec); err != nil || !found {
			return err
		}
		rec.Answered = true
		return p.put(tx, key, rec)
	})
}
