package human

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Event families. TypeAnswer is the answer action's Event on the ask key
// (source core.action); TypeAnswered is its alias in the asking Work, the
// only own-key family the ask's wait wakes on.
const (
	TypeAnswer   = "human.answer"
	TypeAnswered = "human.answered"

	SchemaAnswer = "human.answer.v1"
	SchemaFilter = "human.filter.v1"
	SchemaTool   = "human.tool.v1"
)

// Namespace is the coordination KV namespace of ask records: "ask/<id>"
// holds an Ask, "work/<work id>" the ID of that Work's latest ask.
const Namespace = "human.asks"

// Ask states.
const (
	StateOpen     = "open"
	StateAnswered = "answered"
	StateExpired  = "expired"
	// StateWithdrawn: the asker stopped waiting without an answer (its Work
	// ended, or its wait was woken another way).
	StateWithdrawn = "withdrawn"
)

// Ask is one question's record. It stays in Open Conduit's store: nothing
// in it is projected to the Work's source.
type Ask struct {
	ID    string    `json:"id"`
	Scope domain.ID `json:"scope"`
	Work  domain.ID `json:"work"`
	// Execution is the asking attempt, whose wait carries the ask.
	Execution domain.ID `json:"execution,omitempty"`
	Provider  string    `json:"provider"`
	Question  string    `json:"question"`
	Options   []string  `json:"options,omitempty"`
	Context   string    `json:"context,omitempty"`
	Deadline  time.Time `json:"deadline,omitzero"`
	// Responders and Surfaces are the entry's at the time of asking.
	Responders []string  `json:"responders"`
	Surfaces   []string  `json:"surfaces"`
	AskedAt    time.Time `json:"asked_at"`
	// Sequence is the acceptance watermark when the ask was made: its
	// answers all come after it.
	Sequence uint64 `json:"sequence"`
	State    string `json:"state"`
	Answer   *Given `json:"answer,omitempty"`
	// Late are answers accepted after the first, which do not count.
	Late []Given `json:"late,omitempty"`
	// Checked is the sequence through which answers have been audited.
	Checked  uint64    `json:"checked,omitempty"`
	ClosedAt time.Time `json:"closed_at,omitzero"`
}

// Given is an accepted answer.
type Given struct {
	Event     domain.ID `json:"event"`
	Sequence  uint64    `json:"sequence"`
	Responder string    `json:"responder"`
	Answer    string    `json:"answer"`
	At        time.Time `json:"at"`
}

// Key is the ask's correlation key, which no Work owns.
func (a Ask) Key() domain.CorrelationKey {
	return domain.CorrelationKey{Source: Type, ExternalID: "ask/" + a.ID}
}

// AnswerPayload is a TypeAnswer Event's data.
type AnswerPayload struct {
	Ask       string `json:"ask"`
	Question  string `json:"question"`
	Answer    string `json:"answer"`
	Option    int    `json:"option,omitempty"`
	Responder string `json:"responder"`
}

// filter is a dependency's opaque filter: the ask, and the provider whose
// answer action alone counts.
type filter struct {
	Ask      string `json:"ask"`
	Provider string `json:"provider"`
}

// DependencyFilter counts only the answer action's Event for the ask: an
// Event of the same family and key that did not come through the
// provider's authorised action (a posted Event, another provider's action)
// is not an answer. It is pure: core calls it inside transactions.
func DependencyFilter(f domain.Payload, e domain.Event) bool {
	var want filter
	if f.Schema != SchemaFilter || json.Unmarshal(f.Data, &want) != nil || want.Ask == "" {
		return false
	}
	if e.Type != TypeAnswer || e.Source != extension.ActionSource || !strings.HasPrefix(e.DeliveryID, want.Provider+"/"+ActionAnswer+"/") {
		return false
	}
	var p AnswerPayload
	return e.Data.Schema == SchemaAnswer && json.Unmarshal(e.Data.Data, &p) == nil && p.Ask == want.Ask
}

func getAsk(tx storage.Tx, id string) (Ask, error) {
	var a Ask
	b, err := tx.Get(Namespace, "ask/"+id)
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(b, &a)
}

func putAsk(tx storage.Tx, a Ask) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return tx.Put(Namespace, "ask/"+a.ID, b)
}

// latestAsk is the Work's latest ask (ErrNotFound when it has none).
func latestAsk(tx storage.Tx, work domain.ID) (Ask, error) {
	id, err := tx.Get(Namespace, "work/"+string(work))
	if err != nil {
		return Ask{}, err
	}
	return getAsk(tx, string(id))
}

// askWait is the asking Work's wait for a, when it has registered: the
// asking attempt's wait, registered at or after the ask, that wakes only on
// its answer, preferring one still active. The wait's watermark is the
// attempt's acknowledged input, which can come before a.Sequence (an Event
// accepted while the turn ran), so it does not tell the wait apart.
func askWait(tx storage.Tx, a Ask) (domain.Wait, bool, error) {
	waits, err := tx.ListWaits(a.Work)
	if err != nil {
		return domain.Wait{}, false, err
	}
	var found domain.Wait
	ok := false
	for _, w := range waits {
		if !slices.Equal(w.Match.Types, []string{TypeAnswered}) || w.CreatedAt.Before(a.AskedAt) || a.Execution != "" && w.ExecutionID != a.Execution {
			continue
		}
		if w.State == domain.WaitActive {
			return w, true, nil
		}
		if !ok {
			found, ok = w, true
		}
	}
	return found, ok, nil
}

// expired reports whether the ask's deadline has passed.
func (a Ask) expired(now time.Time) bool {
	return !a.Deadline.IsZero() && !now.Before(a.Deadline)
}

// outstanding reports whether a person can answer the ask now: it is open
// and in time, and its Work is parked on its wait. It reads no Events.
func outstanding(tx storage.Tx, a Ask, now time.Time) (bool, error) {
	if a.State != StateOpen || a.expired(now) {
		return false, nil
	}
	w, err := tx.Work(a.Work)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if w.State != domain.WorkWaiting {
		return false, nil
	}
	wait, ok, err := askWait(tx, a)
	return ok && wait.State == domain.WaitActive, err
}

// answers are the answer Events accepted for the ask after through, in
// acceptance order, and the sequence they were read through.
func answers(tx storage.Tx, a Ask, through uint64) ([]Given, uint64, error) {
	after := max(a.Sequence, through)
	all, err := tx.Events(after, 0)
	if err != nil {
		return nil, after, err
	}
	key := a.Key()
	f := filterPayload(a)
	var out []Given
	for _, e := range all {
		after = e.Sequence
		if e.Event.CorrelationKey == nil || *e.Event.CorrelationKey != key || !DependencyFilter(f, e.Event) {
			continue
		}
		var p AnswerPayload
		_ = json.Unmarshal(e.Event.Data.Data, &p)
		out = append(out, Given{Event: e.Event.ID, Sequence: e.Sequence, Responder: p.Responder, Answer: p.Answer, At: e.Event.ReceivedAt})
	}
	return out, after, nil
}

func filterPayload(a Ask) domain.Payload {
	data, _ := json.Marshal(filter{Ask: a.ID, Provider: a.Provider})
	return domain.Payload{Schema: SchemaFilter, Data: data}
}

// choose matches one line of input against the ask's options: an option's
// text (case-insensitive) or its number. With no options any text is an
// answer. ok is false when it matches none.
func (a Ask) choose(input string) (answer string, option int, ok bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", 0, false
	}
	if len(a.Options) == 0 {
		return input, 0, true
	}
	if n, err := strconv.Atoi(input); err == nil && n >= 1 && n <= len(a.Options) {
		return a.Options[n-1], n, true
	}
	for i, o := range a.Options {
		if strings.EqualFold(o, input) {
			return o, i + 1, true
		}
	}
	return "", 0, false
}

// numbered lists the options as "1. yes, 2. no".
func numbered(options []string) string {
	parts := make([]string, len(options))
	for i, o := range options {
		parts[i] = strconv.Itoa(i+1) + ". " + o
	}
	return strings.Join(parts, ", ")
}
