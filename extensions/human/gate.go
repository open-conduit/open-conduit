package human

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Approval options. A gate's ask offers exactly these, in this order.
const (
	OptionApprove = "approve"
	OptionRefuse  = "refuse"
)

// Refusal codes a gated call can receive.
const (
	CodeApprovalRefused     = "approval_refused"
	CodeApprovalTimedOut    = "approval_timed_out"
	CodeApprovalUnavailable = "approval_unavailable"
)

// Gate is the extension's toolbelt gate (extension.Gate). A call of an
// operation one of the scope's `human` gates names needs a person's
// approval: the first call records an approval request (an ask whose
// options are approve and refuse) and parks the caller on its answer, so
// the operation does not run. Once resumed, the caller makes the same call
// again (the same operation and input, in the same Work): an approval lets
// exactly that call through once, if it is made within the gate's timeout
// of the answer (later, it asks again); a refusal, or no answer by the gate's
// timeout, refuses it with an operation error, once. A later identical call
// asks again. Calls the scope's gates don't name pass, and so does every
// call in a scope without a `human` entry.
func (t *Tools) Gate(ctx context.Context, a extension.Authority, c extension.GateCall) (*extension.WaitPlan, error) {
	entry, ok := t.o.Entry(a.ScopeID)
	if !ok {
		return nil, nil
	}
	rule, ok := entry.Config.gate(c.Operation)
	if !ok {
		return nil, nil
	}
	now := t.o.Now()
	request := requestHash(c.Operation, c.Input)
	pointer := "gate/" + string(a.WorkID) + "/" + request
	var rec Ask
	var decided error
	allowed := false
	err := t.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		allowed, decided, rec = false, nil, Ask{}
		w, err := tx.Work(a.WorkID)
		if err != nil || w.ScopeID != a.ScopeID {
			return extension.ErrUnauthorized
		}
		if id, err := tx.Get(Namespace, pointer); err == nil {
			// The call asked before: settle that request and act on it.
			if err = settle(tx, string(id), now, &SyncResult{}); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			prior, err := getAsk(tx, string(id))
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			switch {
			case err == nil && prior.State == StateAnswered && prior.Answer != nil && now.Sub(prior.Answer.At) > rule.timeout:
				// Answered longer ago than the gate's timeout: a person
				// decided on the call then, not whenever it is made. It
				// is asked anew below.
			case err == nil && prior.State == StateAnswered && prior.Answer != nil:
				if prior.Answer.Answer == OptionApprove {
					allowed = true
				} else {
					decided = refuse(CodeApprovalRefused, "%s refused this call to %s; don't make it again unless someone asks you to", prior.Answer.Responder, c.Operation)
				}
				return tx.Delete(Namespace, pointer)
			case err == nil && prior.State == StateExpired:
				decided = refuse(CodeApprovalTimedOut, "nobody approved this call to %s in time, so it was refused; say in your status what it was for", c.Operation)
				return tx.Delete(Namespace, pointer)
			case err == nil && prior.State == StateOpen:
				// Still open, though its caller is running again: it no
				// longer waits for it, so it is withdrawn and asked anew.
				prior.State, prior.ClosedAt = StateWithdrawn, now
				if err = putAsk(tx, prior); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if len(w.CorrelationKeys) == 0 {
			decided = refuse(CodeApprovalUnavailable, "%s needs a person's approval, and this Work has no conversation to resume once it is given, so the call was refused", c.Operation)
			return nil
		}
		seq, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		rec = Ask{ID: gateAskID(a, request, now), Scope: a.ScopeID, Work: a.WorkID, Execution: a.ExecutionID, Provider: entry.Name,
			Gate: c.Operation, Request: request, Question: "Allow " + c.Operation + "?", Options: []string{OptionApprove, OptionRefuse},
			Context: gateContext(c.Input), Deadline: now.Add(rule.timeout), Responders: rule.Responders, Surfaces: entry.Config.Surfaces,
			AskedAt: now, Sequence: seq, State: StateOpen}
		if err = putAsk(tx, rec); err != nil {
			return err
		}
		if err = tx.Put(Namespace, "work/"+string(a.WorkID), []byte(rec.ID)); err != nil {
			return err
		}
		return tx.Put(Namespace, pointer, []byte(rec.ID))
	})
	switch {
	case err != nil:
		return nil, err
	case allowed:
		return nil, nil
	case decided != nil:
		return nil, decided
	}
	d := rec.Deadline
	out, _ := json.Marshal(map[string]any{"approval_required": true, "operation": c.Operation, "deadline": d.Format(time.RFC3339),
		"next": "this call needs a person's approval and did not run. End your turn now; any other call is refused until you are resumed. Once resumed with the answer, make the same call again with the same input: an approval lets exactly that call through once"})
	return &extension.WaitPlan{
		Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{TypeAnswered}}, Timeout: &d, Description: "a person's approval of " + c.Operation},
		Dependencies: []extension.Dependency{{ID: "answer", SourceKey: rec.Key(), Types: []string{TypeAnswer},
			AliasType: TypeAnswered, Filter: filterPayload(rec), Label: "a person's approval"}},
		Output: domain.Payload{Schema: SchemaTool, Data: out},
	}, nil
}

// requestHash is a call's identity: its operation and its input, with the
// input's JSON in a canonical form, so the same call made again matches
// whatever its whitespace or key order.
func requestHash(op string, in domain.Payload) string {
	data := in.Data
	var v any
	d := json.NewDecoder(bytes.NewReader(in.Data))
	d.UseNumber()
	if d.Decode(&v) == nil {
		if b, err := json.Marshal(v); err == nil {
			data = b
		}
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(op), []byte(in.Schema), data} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func gateAskID(a extension.Authority, request string, now time.Time) string {
	h := sha256.Sum256([]byte("gate\x00" + string(a.ExecutionID) + "\x00" + request + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h[:16])
}

// gateContext shows the responder the call's input, indented and clipped.
func gateContext(in domain.Payload) string {
	var b bytes.Buffer
	text := string(in.Data)
	if json.Indent(&b, in.Data, "", "  ") == nil {
		text = b.String()
	}
	if text == "" || text == "null" {
		return ""
	}
	text = "Input:\n" + text
	if len(text) > maxContext {
		cut := maxContext - len("\n…")
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "\n…"
	}
	return text
}

// renderApproval tells the caller how its approval request was answered.
func renderApproval(p AnswerPayload) string {
	if p.Answer == OptionApprove {
		return "### A person approved your call to " + p.Operation + "\nMake the same call again with the same input: it goes through once."
	}
	return "### A person refused your call to " + p.Operation + "\nDon't make it again unless someone asks you to."
}
