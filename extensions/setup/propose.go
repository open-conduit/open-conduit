package setup

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/storage"
)

// maxValue bounds a proposed value.
const maxValue = 64 << 10

// accept records an Event of the extension's own family on the calling
// session, with no key, so it never routes back to it.
func (t *Tools) accept(tx storage.Tx, a extension.Authority, typ, schema, id string, data any) error {
	now := t.o.Now()
	_, _, err := t.o.Accept(tx, a.ScopeID, domain.Event{ID: domain.ID(Type + "/" + strings.TrimPrefix(typ, Type+".") + "/" + id), Type: typ, Source: Type,
		DeliveryID: strings.TrimPrefix(typ, Type+".") + "/" + id, OccurredAt: now, ReceivedAt: now, WorkID: a.WorkID, ExecutionID: a.ExecutionID, Data: payload(schema, data)})
	return err
}

type proposeInput struct {
	Op      string          `json:"op"`
	Pointer string          `json:"pointer"`
	Value   json.RawMessage `json:"value,omitempty"`
	Why     string          `json:"why"`
}

func parsePropose(data []byte) (proposeInput, error) {
	var in proposeInput
	if err := decode(data, &in, "{op, pointer, value?, why}"); err != nil {
		return in, err
	}
	var err error
	if in.Why, err = text(in.Why, maxContext, "why"); err != nil {
		return in, err
	}
	if !strings.HasPrefix(in.Pointer, "/") || len(in.Pointer) > 512 || strings.ContainsAny(in.Pointer, "\r\n") {
		return in, refuse("invalid_input", "pointer is a JSON pointer into the config file, such as /scopes/web/bindings/agent/config/model")
	}
	switch in.Op {
	case OpAdd, OpReplace:
		if len(in.Value) == 0 || len(in.Value) > maxValue || !json.Valid(in.Value) {
			return in, refuse("invalid_input", "%s needs a JSON value of at most %d bytes", in.Op, maxValue)
		}
		if err = checkReferences(in.Pointer, in.Value); err != nil {
			return in, err
		}
		in.Value = compact(in.Value)
	case OpRemove:
		if len(in.Value) > 0 {
			return in, refuse("invalid_input", "remove takes no value")
		}
	default:
		return in, refuse("invalid_input", "op is add, replace or remove")
	}
	return in, nil
}

// propose adds a proposed change to the session's change set. Its before
// is what the candidate (the base plus the approved changes) has at the
// pointer; the candidate plus this change is validated and the diagnostics
// attached. A change that doesn't validate on its own is still recorded,
// since a scope and its binding may come in two proposals.
func (t *Tools) propose(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	input, err := parsePropose(in.Data)
	if err != nil {
		return domain.Payload{}, err
	}
	base, err := t.o.Config.Base(ctx)
	if err != nil {
		return domain.Payload{}, refuse("unavailable", "the configuration can't be read now")
	}
	req := requestID(ctx, a)
	var c change
	var text []byte
	var count int
	var done bool
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		w, err := t.session(tx, a)
		if err != nil {
			return err
		}
		cs, err := t.changeSetFor(tx, w, base)
		if err != nil {
			return err
		}
		for _, x := range cs.Changes {
			if x.Request == req {
				c, done = x, true
				return nil
			}
		}
		if inFolder(cs, input.Pointer) {
			return refuse("in_folder", "that scope comes from an included folder, whose text is in another file; ask the operator to change it there")
		}
		st, err := states(tx, w, cs)
		if err != nil {
			return err
		}
		cand, _, _ := candidate(t.o.Config, cs, st)
		out, before, err := t.o.Config.Edit(cand, input.Op, input.Pointer, input.Value)
		if err != nil {
			return refuse("invalid_pointer", "%s", err.Error())
		}
		latest, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		c = change{ID: nextChangeID(cs), Request: req, Op: input.Op, Pointer: input.Pointer, Value: input.Value, Before: compact(before), Why: input.Why, Since: latest}
		text, count = out, len(cs.Changes)
		return nil
	}); err != nil {
		return domain.Payload{}, err
	}
	if !done {
		_, diags, err := t.o.Config.Validate(ctx, text)
		if err != nil {
			return domain.Payload{}, refuse("unavailable", "the configuration can't be validated now")
		}
		c.Diagnostics = diags
		if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
			w, err := t.session(tx, a)
			if err != nil {
				return err
			}
			cs, err := t.changeSetFor(tx, w, base)
			if err != nil {
				return err
			}
			if len(cs.Changes) != count {
				return refuse("change_set_moved", "another change landed meanwhile; propose again")
			}
			cs.Changes = append(cs.Changes, c)
			if err = saveChangeSet(tx, w.ID, cs); err != nil {
				return err
			}
			return t.accept(tx, a, TypeProposal, SchemaProposal, req, Proposal{Change: c.ID, Op: c.Op, Pointer: c.Pointer,
				Before: redact(c.Pointer, c.Before), After: redact(c.Pointer, c.Value), Why: c.Why, Diagnostics: c.Diagnostics})
		}); err != nil {
			return domain.Payload{}, err
		}
	}
	out := map[string]any{"change": c.ID, "state": StateProposed, "valid": len(c.Diagnostics) == 0,
		"next": "the operator approves or rejects it in the Setup page; you'll hear their decision"}
	if len(c.Diagnostics) > 0 {
		out["diagnostics"] = c.Diagnostics
	}
	return payload(SchemaTool, out), nil
}

// withdraw takes back a change the operator hasn't decided.
func (t *Tools) withdraw(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var input struct {
		Change string `json:"change"`
		Reason string `json:"reason,omitempty"`
	}
	if err := decode(in.Data, &input, "{change, reason?}"); err != nil {
		return domain.Payload{}, err
	}
	if len(input.Reason) > maxContext {
		return domain.Payload{}, refuse("invalid_input", "reason must be at most %d bytes", maxContext)
	}
	req := requestID(ctx, a)
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		w, err := t.session(tx, a)
		if err != nil {
			return err
		}
		cs, ok, err := loadChangeSet(tx, w.ID)
		if err != nil {
			return err
		}
		i := -1
		for j, c := range cs.Changes {
			if ok && c.ID == input.Change {
				i = j
			}
		}
		if i < 0 {
			return refuse("not_found", "no change %q in this session", input.Change)
		}
		st, err := states(tx, w, cs)
		if err != nil {
			return err
		}
		switch st[input.Change] {
		case StateWithdrawn:
			return nil
		case StateProposed:
		default:
			return refuse("decided", "the operator has already decided %s (%s); propose a new change instead", input.Change, st[input.Change])
		}
		cs.Changes[i].Withdrawn = true
		if err = saveChangeSet(tx, w.ID, cs); err != nil {
			return err
		}
		return t.accept(tx, a, TypeDecision, SchemaDecision, req, Decision{Changes: []string{input.Change}, Decision: StateWithdrawn, Reason: strings.TrimSpace(input.Reason)})
	})
	if err != nil {
		return domain.Payload{}, err
	}
	return payload(SchemaTool, map[string]any{"change": input.Change, "state": StateWithdrawn}), nil
}

// check validates the session's candidate: the base text plus the approved
// changes, in proposal order.
func check(ctx context.Context, store storage.Store, host inproc.ConfigHost, now func() time.Time, w domain.Work, source string) (Checks, error) {
	base, err := host.Base(ctx)
	if err != nil {
		return Checks{}, refuse("unavailable", "the configuration can't be read now")
	}
	var text []byte
	var ids []string
	var diags []inproc.ConfigDiagnostic
	if err = store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		cs, _, err := loadChangeSet(tx, w.ID)
		if err != nil {
			return err
		}
		latest, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		if cs.BaseGeneration == "" {
			cs = changeSet{BaseGeneration: base.Generation, BaseHash: base.Hash, BaseText: string(base.Text), Folders: base.Folders}
		}
		cs = refresh(cs, base, host, latest)
		st, err := states(tx, w, cs)
		if err != nil {
			return err
		}
		text, ids, diags = candidate(host, cs, st)
		return nil
	}); err != nil {
		return Checks{}, err
	}
	hash, more, err := host.Validate(ctx, text)
	if err != nil {
		return Checks{}, refuse("unavailable", "the configuration can't be validated now")
	}
	out := Checks{Source: source, Diagnostics: append(diags, more...), Approved: len(ids)}
	out.Valid = len(out.Diagnostics) == 0
	if out.Valid {
		out.CandidateHash = hash
	}
	return out, nil
}

// validate validates the candidate and records the result on the session.
func (t *Tools) validate(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var input struct{}
	if len(in.Data) > 0 {
		if err := decode(in.Data, &input, "{}"); err != nil {
			return domain.Payload{}, err
		}
	}
	var w domain.Work
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var err error
		w, err = t.session(tx, a)
		return err
	}); err != nil {
		return domain.Payload{}, err
	}
	c, err := check(ctx, t.o.Store, t.o.Config, t.o.Now, w, RoleAgent)
	if err != nil {
		return domain.Payload{}, err
	}
	req := requestID(ctx, a)
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		return t.accept(tx, a, TypeChecks, SchemaChecks, req, c)
	}); err != nil {
		return domain.Payload{}, err
	}
	out := map[string]any{"valid": c.Valid, "approved": c.Approved}
	if len(c.Diagnostics) > 0 {
		out["diagnostics"] = c.Diagnostics
	}
	if c.Valid && c.Approved > 0 {
		out["next"] = "ask the operator to review and apply the approved changes in the Setup page; setup.applied tells you when they have"
	}
	return payload(SchemaTool, out), nil
}
