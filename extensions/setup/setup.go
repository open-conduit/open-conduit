// Package setup is the `setup` extension (issue #127): setup in the browser
// as a session the operator starts, talks to and answers, whose agent speaks
// only in typed Events.
//
// A session is Work of kind setup.session in a scope with a `setup` entry.
// The entry's "start" action produces setup.start on a fresh session key,
// and the `setup` route gives that key Work, with the operator's chosen
// model as its context. On the session's Work, "message" and "answer"
// produce setup.message and setup.answer on the same key, which the route
// takes as more input for it (injected into a running turn, or queued).
//
// The agent talks back through the toolbelt: setup.say emits setup.message
// and setup.ask records setup.question and parks the session until the
// operator's setup.answer. Neither carries a key, so nothing the agent
// writes routes back to its own Work.
package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Type is the extension type, the toolbelt registration name, the route
// handler name and the source of session keys.
const Type = "setup"

// WorkKind is a session's Work kind.
const WorkKind = "setup.session"

// Event families.
const (
	TypeStart    = "setup.start"
	TypeMessage  = "setup.message"
	TypeQuestion = "setup.question"
	TypeAnswer   = "setup.answer"
	TypeProposal = "setup.proposal"
	TypeDecision = "setup.decision"
	TypeChecks   = "setup.checks"
	TypeApplied  = "setup.applied"
	TypeProbe    = "setup.probe"

	SchemaStart    = "setup.start.v1"
	SchemaMessage  = "setup.message.v1"
	SchemaQuestion = "setup.question.v1"
	SchemaAnswer   = "setup.answer.v1"
	SchemaProposal = "setup.proposal.v1"
	SchemaDecision = "setup.decision.v1"
	SchemaChecks   = "setup.checks.v1"
	SchemaApplied  = "setup.applied.v1"
	SchemaProbe    = "setup.probe.v1"
	SchemaContext  = "setup.session.v1"
	SchemaTool     = "setup.tool.v1"
)

// Message roles.
const (
	RoleAgent    = "agent"
	RoleOperator = "operator"
)

// Namespace is the coordination KV namespace of session records:
// "question/<work id>" holds the session's latest Question.
const Namespace = "setup.sessions"

// Start is a setup.start Event's data and a session's Work context.
type Start struct {
	// Model is the operator's choice; empty is the binding's default.
	Model string `json:"model,omitempty"`
}

// Message is a setup.message Event's data.
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Question is a setup.question Event's data and the session's record of
// its latest question.
type Question struct {
	ID      string   `json:"question_id"`
	Text    string   `json:"text"`
	Context string   `json:"context,omitempty"`
	Choices []string `json:"choices,omitempty"`
	// Sequence is the acceptance watermark when it was asked: its answer
	// comes after it.
	Sequence uint64 `json:"sequence,omitempty"`
}

// Answer is a setup.answer Event's data.
type Answer struct {
	Question string `json:"question_id"`
	Answer   string `json:"answer"`
}

// SessionKey is the key of the session started as id in scope. No Work owns
// it until the route gives setup.start one.
func SessionKey(scope domain.ID, id string) domain.CorrelationKey {
	return domain.CorrelationKey{Source: Type, ExternalID: "session/" + string(scope) + "/" + id}
}

// sessionID is the session's ID from its key, when it is one.
func sessionID(scope domain.ID, k domain.CorrelationKey) (string, bool) {
	id, ok := strings.CutPrefix(k.ExternalID, "session/"+string(scope)+"/")
	return id, ok && k.Source == Type && id != "" && !strings.Contains(id, "/")
}

// WorkID is the ID of the session's Work.
func WorkID(scope domain.ID, id string) domain.ID {
	return domain.ID(Type + "/" + string(scope) + "/" + id)
}

// sessionKey is the session Work's own key.
func sessionKey(w domain.Work) (domain.CorrelationKey, bool) {
	for _, k := range w.CorrelationKeys {
		if _, ok := sessionID(w.ScopeID, k); ok {
			return k, true
		}
	}
	return domain.CorrelationKey{}, false
}

// Config is a scope's `setup` entry. It has no settings yet.
type Config struct{}

// ValidateExtension is the entry type's config validator.
func ValidateExtension(config json.RawMessage, secretNames []string) error {
	if len(secretNames) > 0 {
		return errors.New("setup takes no secrets")
	}
	var c Config
	return strict(config, &c)
}

// Executor is the one executor a setup route may bind.
const Executor = "claude-code"

// builtins are the core operations a session's agent may have besides the
// extension's own.
var builtins = []string{"read_context", "wait", "declare_outcome"}

// ValidateBinding refuses an agent a session must not run: one on another
// executor, one with Claude Code's built-in tools (a shell, file access) on,
// or one granted operations beyond the builtins and setup.*. Without a
// shell, everything the agent learns or says goes through the toolbelt, and
// nothing it is granted can change the config.
func ValidateBinding(b extension.RouteBinding) error {
	if b.Executor != Executor {
		return errors.New("a setup session runs on the claude-code executor")
	}
	var c struct {
		BuiltinTools *[]string `json:"builtin_tools"`
	}
	if json.Unmarshal(b.Config, &c) != nil || c.BuiltinTools == nil || len(*c.BuiltinTools) > 0 {
		return errors.New(`set "builtin_tools": [] so the agent has no shell or file access`)
	}
	for _, op := range b.Operations {
		if !slices.Contains(builtins, op) && !strings.HasPrefix(op, Type+".") {
			return fmt.Errorf("operation %q is not one a setup session may have (read_context, wait, declare_outcome and setup.*)", op)
		}
	}
	return nil
}

// ValidateRoute is the route config validator: the route takes none.
func ValidateRoute(config json.RawMessage, _ []string) error {
	var c struct{}
	return strict(config, &c)
}

func strict(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

func payload(schema string, v any) domain.Payload {
	data, _ := json.Marshal(v)
	return domain.Payload{Schema: schema, Data: data}
}
