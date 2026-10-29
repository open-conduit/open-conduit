package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/storage"
)

// Agent-facing operations.
const (
	OpSay      = "setup.say"
	OpAsk      = "setup.ask"
	OpPropose  = "setup.propose"
	OpWithdraw = "setup.withdraw"
	OpValidate = "setup.validate"
	OpProbe    = "setup.probe"
	OpRead     = "setup.read"
)

// Operations lists every tool operation (for binding operation lists).
func Operations() []string {
	return []string{OpSay, OpAsk, OpProbe, OpRead, OpPropose, OpWithdraw, OpValidate, OpTrace}
}

// AskTimeout is how long a question waits for the operator. With no answer
// by then the agent is resumed without one, sums up and finishes.
const AskTimeout = 24 * time.Hour

// Input limits.
const (
	maxText    = 8 << 10
	maxContext = 4000
	maxChoices = 20
	maxChoice  = 200
)

// AcceptFunc accepts a root Event of the extension's own families inside
// the caller's transaction (inproc.Host.AcceptInTx).
type AcceptFunc func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error)

// Options configure Tools.
type Options struct {
	Store  storage.Store
	Accept AcceptFunc
	// Enabled reports whether the scope has a `setup` entry in the current
	// configuration.
	Enabled func(scope domain.ID) bool
	Now     func() time.Time
	// Config reads, validates and edits config text for change sets.
	Config inproc.ConfigHost
	// OwnProbes are the extension's own probes, and Probes those of other
	// extensions and executors, by full name.
	OwnProbes Probes
	Probes    func() map[string]extension.SetupProbe
}

// Tools implements the agent's operations.
type Tools struct{ o Options }

// NewTools validates options.
func NewTools(o Options) (*Tools, error) {
	if o.Store == nil || o.Accept == nil || o.Enabled == nil || o.Config == nil {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Tools{o: o}, nil
}

// Registration is the toolbelt registration.
func (t *Tools) Registration() extension.Registration {
	return extension.Registration{Version: extension.ContractVersion, Name: Type, EventTypes: []string{TypeMessage},
		Operations: []extension.Operation{{Name: OpSay, Emit: t.say}, {Name: OpAsk, Wait: t.ask},
			{Name: OpProbe, Handler: t.probe}, {Name: OpRead, Handler: t.read},
			{Name: OpPropose, Handler: t.propose}, {Name: OpWithdraw, Handler: t.withdraw}, {Name: OpValidate, Handler: t.validate},
			{Name: OpTrace, Handler: t.trace}}}
}

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// session checks that the call comes from a session's Work in a scope with
// a `setup` entry.
func (t *Tools) session(tx storage.Tx, a extension.Authority) (domain.Work, error) {
	w, err := tx.Work(a.WorkID)
	if err != nil || w.ScopeID != a.ScopeID {
		return w, extension.ErrUnauthorized
	}
	if w.Kind != WorkKind || !t.o.Enabled(a.ScopeID) {
		return w, refuse("not_a_session", "the setup operations work only in a setup session")
	}
	return w, nil
}

func decode(data []byte, out any, shape string) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return refuse("invalid_input", "give %s", shape)
	}
	return nil
}

func text(s string, limit int, name string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", refuse("invalid_input", "%s is required", name)
	}
	if !utf8.ValidString(s) || len(s) > limit {
		return "", refuse("invalid_input", "%s must be text of at most %d bytes", name, limit)
	}
	return s, nil
}

// requestID is a stable identity for the call: derived from the execution
// and the caller's request ID, so a retried call does nothing twice, else
// fresh.
func requestID(ctx context.Context, a extension.Authority) string {
	if req := extension.CallRequestID(ctx); req != "" {
		h := sha256.Sum256([]byte(string(a.ExecutionID) + "\x00" + req))
		return hex.EncodeToString(h[:16])
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// say emits the agent's message on its own Work. It carries no key, so it
// is shown on the session and never routes back to it.
func (t *Tools) say(ctx context.Context, a extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
	var input struct {
		Text string `json:"text"`
	}
	if err := decode(in.Data, &input, "{text}"); err != nil {
		return extension.EmitPlan{}, err
	}
	msg, err := text(input.Text, maxText, "text")
	if err != nil {
		return extension.EmitPlan{}, err
	}
	if err = scanText(msg); err != nil {
		return extension.EmitPlan{}, err
	}
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		_, e := t.session(tx, a)
		return e
	}); err != nil {
		return extension.EmitPlan{}, err
	}
	return extension.EmitPlan{
		Event: domain.Event{Type: TypeMessage, DeliveryID: "say/" + requestID(ctx, a), Data: payload(SchemaMessage, Message{Role: RoleAgent, Text: msg})},
		Commit: func(extension.Emitted) (extension.EmitCommit, error) {
			return extension.EmitCommit{Output: payload(SchemaTool, map[string]any{"said": true})}, nil
		},
	}, nil
}

type askInput struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
	Context  string   `json:"context,omitempty"`
}

func parseAsk(data []byte) (askInput, error) {
	var in askInput
	if err := decode(data, &in, "{question, choices?, context?}"); err != nil {
		return in, err
	}
	var err error
	if in.Question, err = text(in.Question, maxText, "question"); err != nil {
		return in, err
	}
	in.Context = strings.TrimSpace(in.Context)
	if !utf8.ValidString(in.Context) || len(in.Context) > maxContext {
		return in, refuse("invalid_input", "context must be text of at most %d bytes", maxContext)
	}
	if len(in.Choices) == 1 || len(in.Choices) > maxChoices {
		return in, refuse("invalid_input", "give between 2 and %d choices, or none for a free answer", maxChoices)
	}
	seen := map[string]bool{}
	for i, c := range in.Choices {
		c = strings.TrimSpace(c)
		if c == "" || !utf8.ValidString(c) || len(c) > maxChoice || strings.ContainsAny(c, "\r\n") {
			return in, refuse("invalid_input", "each choice must be one line of at most %d bytes", maxChoice)
		}
		if seen[strings.ToLower(c)] {
			return in, refuse("invalid_input", "choice %q is given twice", c)
		}
		seen[strings.ToLower(c)] = true
		in.Choices[i] = c
	}
	return in, nil
}

// ask records the question on the session as setup.question and parks the
// session until the operator answers it, or AskTimeout passes. Only
// setup.answer wakes it: a message sent meanwhile is queued for the turn
// the answer starts.
func (t *Tools) ask(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
	input, err := parseAsk(in.Data)
	if err != nil {
		return extension.WaitPlan{}, err
	}
	if err = scanText(append([]string{input.Question, input.Context}, input.Choices...)...); err != nil {
		return extension.WaitPlan{}, err
	}
	now := t.o.Now()
	id := requestID(ctx, a)
	err = t.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		w, err := t.session(tx, a)
		if err != nil {
			return err
		}
		if _, ok := sessionKey(w); !ok {
			return refuse("not_a_session", "this session has no key to wait on")
		}
		if old, err := latestQuestion(tx, w.ID); err == nil && old.ID == id {
			return nil
		}
		seq, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		q := Question{ID: id, Text: input.Question, Context: input.Context, Choices: input.Choices, Sequence: seq}
		if _, _, err = t.o.Accept(tx, a.ScopeID, domain.Event{ID: domain.ID(Type + "/question/" + id), Type: TypeQuestion, Source: Type, DeliveryID: "question/" + id,
			OccurredAt: now, ReceivedAt: now, WorkID: a.WorkID, ExecutionID: a.ExecutionID, Data: payload(SchemaQuestion, q)}); err != nil {
			return err
		}
		b, err := json.Marshal(q)
		if err != nil {
			return err
		}
		return tx.Put(Namespace, "question/"+string(w.ID), b)
	})
	if err != nil {
		return extension.WaitPlan{}, err
	}
	deadline := now.Add(AskTimeout)
	return extension.WaitPlan{
		Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{TypeAnswer}}, Timeout: &deadline, Description: "the operator's answer"},
		Output: payload(SchemaTool, map[string]any{"asked": true, "deadline": deadline.Format(time.RFC3339),
			"next": "end your turn now; any other call is refused until you are resumed with the answer, or after a day without one"}),
	}, nil
}

// latestQuestion is the session's latest question (storage.ErrNotFound
// when it has asked none).
func latestQuestion(tx storage.Tx, work domain.ID) (Question, error) {
	var q Question
	b, err := tx.Get(Namespace, "question/"+string(work))
	if err != nil {
		return q, err
	}
	return q, json.Unmarshal(b, &q)
}
