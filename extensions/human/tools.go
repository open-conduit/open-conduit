package human

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// OpAsk is the agent-facing operation.
const OpAsk = "human.ask"

// Operations lists every tool operation (for binding operation lists).
func Operations() []string { return []string{OpAsk} }

// Input limits.
const (
	maxQuestion = 2000
	maxContext  = 4000
	maxOptions  = 20
	maxOption   = 200
)

// Options configure Tools.
type Options struct {
	Store storage.Store
	// Entry is the scope's `human` entry in the current configuration (ok
	// false when the scope is disabled or has none).
	Entry func(scope domain.ID) (Entry, bool)
	Now   func() time.Time
}

// Tools implements human.ask.
type Tools struct{ o Options }

// NewTools validates options.
func NewTools(o Options) (*Tools, error) {
	if o.Store == nil || o.Entry == nil {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Tools{o: o}, nil
}

// Registration is the toolbelt registration. The extension emits no Events
// through the toolbelt: answers come only through its action.
func (t *Tools) Registration() extension.Registration {
	return extension.Registration{Version: extension.ContractVersion, Name: Type,
		Operations:       []extension.Operation{{Name: OpAsk, Wait: t.ask, Guidance: strings.TrimSpace(askGuidance)}},
		DependencyFilter: DependencyFilter}
}

// askGuidance tells an agent when and how to ask; the toolbelt serves it
// to executions granted human.ask.
//
//go:embed guidance/ask.md
var askGuidance string

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// askInput is human.ask's input.
type askInput struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	Context  string   `json:"context,omitempty"`
	// Deadline is an ISO time (with Z or an offset): with no answer by
	// then, the asker is resumed without one.
	Deadline string `json:"deadline,omitempty"`
}

func parseInput(data []byte, now time.Time) (askInput, time.Time, error) {
	var in askInput
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, time.Time{}, refuse("invalid_input", "give {question, options?, context?, deadline?}")
	}
	in.Question, in.Context = strings.TrimSpace(in.Question), strings.TrimSpace(in.Context)
	switch {
	case in.Question == "":
		return in, time.Time{}, refuse("invalid_input", "question is required")
	case !utf8.ValidString(in.Question) || len(in.Question) > maxQuestion:
		return in, time.Time{}, refuse("invalid_input", "question must be text of at most %d bytes", maxQuestion)
	case !utf8.ValidString(in.Context) || len(in.Context) > maxContext:
		return in, time.Time{}, refuse("invalid_input", "context must be text of at most %d bytes", maxContext)
	case len(in.Options) == 1 || len(in.Options) > maxOptions:
		return in, time.Time{}, refuse("invalid_input", "give between 2 and %d options, or none for a free answer", maxOptions)
	}
	seen := map[string]bool{}
	for i, o := range in.Options {
		o = strings.TrimSpace(o)
		if o == "" || !utf8.ValidString(o) || len(o) > maxOption || strings.ContainsAny(o, "\r\n") {
			return in, time.Time{}, refuse("invalid_input", "each option must be one line of at most %d bytes", maxOption)
		}
		if seen[strings.ToLower(o)] {
			return in, time.Time{}, refuse("invalid_input", "option %q is given twice", o)
		}
		seen[strings.ToLower(o)] = true
		in.Options[i] = o
	}
	var deadline time.Time
	if in.Deadline != "" {
		var err error
		if deadline, err = parseTime(in.Deadline); err != nil {
			return in, time.Time{}, refuse("invalid_deadline", "deadline must be an ISO time with Z or an offset, such as 2026-10-25T09:00Z")
		}
		if !deadline.After(now) {
			return in, time.Time{}, refuse("invalid_deadline", "the deadline %s has already passed", in.Deadline)
		}
	}
	return in, deadline, nil
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q", s)
}

// askID is the ask's identity: derived from the execution and the caller's
// request ID, so a retried call makes no second ask, else fresh.
func askID(ctx context.Context, a extension.Authority) string {
	if req := extension.CallRequestID(ctx); req != "" {
		h := sha256.Sum256([]byte(string(a.ExecutionID) + "\x00" + req))
		return hex.EncodeToString(h[:16])
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ask records the question and parks the Work on its answer. The wait
// wakes only on the answer's alias (or at the deadline), so the Work's
// other input queues meanwhile instead of resuming it with the question
// unanswered. Neither the ask key nor where the ask goes is returned.
func (t *Tools) ask(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
	now := t.o.Now()
	input, deadline, err := parseInput(in.Data, now)
	if err != nil {
		return extension.WaitPlan{}, err
	}
	var rec Ask
	err = t.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		w, err := tx.Work(a.WorkID)
		if err != nil || w.ScopeID != a.ScopeID {
			return extension.ErrUnauthorized
		}
		if len(w.CorrelationKeys) == 0 {
			return refuse("not_supported", "this Work has no conversation to resume, so it cannot wait for an answer")
		}
		entry, ok := t.o.Entry(a.ScopeID)
		if !ok {
			return refuse("not_configured", "nobody is set up to answer questions in this project; carry on without asking, or say what you need in your status")
		}
		id := askID(ctx, a)
		if old, err := getAsk(tx, id); err == nil {
			rec = old
			return nil
		}
		seq, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		rec = Ask{ID: id, Scope: a.ScopeID, Work: a.WorkID, Execution: a.ExecutionID, Provider: entry.Name, Question: input.Question, Options: input.Options,
			Context: input.Context, Deadline: deadline, Responders: entry.Config.Responders, Surfaces: entry.Config.Surfaces,
			AskedAt: now, Sequence: seq, State: StateOpen}
		if err = putAsk(tx, rec); err != nil {
			return err
		}
		return tx.Put(Namespace, "work/"+string(a.WorkID), []byte(id))
	})
	if err != nil {
		return extension.WaitPlan{}, err
	}
	// The question may be private: the description never repeats it.
	req := extension.WaitRequest{Match: domain.EventMatch{Types: []string{TypeAnswered}}, Description: "a person's answer"}
	if !rec.Deadline.IsZero() {
		d := rec.Deadline
		req.Timeout = &d
	}
	// The plan releases the execution's authority as soon as it is
	// declared, so the agent can make no further call this turn.
	out := map[string]any{"asked": true, "next": "end your turn now; any other call is refused until you are resumed with the answer"}
	if !rec.Deadline.IsZero() {
		out["deadline"] = rec.Deadline.Format(time.RFC3339)
		out["next"] = "end your turn now; any other call is refused until you are resumed with the answer, or at the deadline without one"
	}
	data, _ := json.Marshal(out)
	return extension.WaitPlan{
		Wait: req,
		Dependencies: []extension.Dependency{{ID: "answer", SourceKey: rec.Key(), Types: []string{TypeAnswer},
			AliasType: TypeAnswered, Filter: filterPayload(rec), Label: "a person's answer"}},
		Output: domain.Payload{Schema: SchemaTool, Data: data},
	}, nil
}
