package setup

import (
	"context"
	"encoding/json"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// OpTrace traces a scope's named sample against the session's candidate.
const OpTrace = "setup.trace"

// trace runs the route trace for one of a scope's named samples ("a new
// issue is opened") against the session's candidate only, the base plus
// the approved changes, so the agent can show that the configuration
// would pick it up before anything is applied. It writes nothing but its
// setup.probe record. With no sample it lists the scope's samples.
func (t *Tools) trace(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var input struct {
		Scope  string `json:"scope"`
		Sample string `json:"sample,omitempty"`
		Why    string `json:"why"`
	}
	if err := decode(in.Data, &input, "{scope, sample?, why}"); err != nil {
		return domain.Payload{}, err
	}
	why, err := text(input.Why, maxContext, "why")
	if err != nil {
		return domain.Payload{}, err
	}
	if input.Scope == "" || len(input.Scope) > 64 || len(input.Sample) > 200 {
		return domain.Payload{}, refuse("invalid_input", "give the scope's ID, and a sample's ID from the list a trace without one returns")
	}
	if err = scanText(why); err != nil {
		return domain.Payload{}, err
	}
	base, err := t.o.Config.Base(ctx)
	if err != nil {
		return domain.Payload{}, refuse("unavailable", "the configuration can't be read now")
	}
	var text []byte
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		w, err := t.session(tx, a)
		if err != nil {
			return err
		}
		cs, err := t.changeSetFor(tx, w, base)
		if err != nil {
			return err
		}
		st, err := states(tx, w, cs)
		if err != nil {
			return err
		}
		text, _, _ = candidate(t.o.Config, cs, st)
		return nil
	}); err != nil {
		return domain.Payload{}, err
	}
	res, traceErr := t.o.Config.Trace(ctx, text, input.Scope, input.Sample)
	ev := ProbeEvent{Probe: OpTrace, Args: map[string]string{"scope": input.Scope}, Command: "open-conduit route trace " + input.Scope, Why: why}
	if input.Sample != "" {
		ev.Args["sample"] = input.Sample
		ev.Command += " --sample " + input.Sample
	}
	out := map[string]any{"scope": input.Scope}
	if traceErr != nil {
		ev.Error = probeError(traceErr)
		out["error"] = ev.Error
		if len(res.Samples) > 0 {
			out["samples"] = res.Samples
		}
	} else {
		b, _ := json.Marshal(res)
		ev.Result = b
		out["trace"] = json.RawMessage(b)
		out["against"] = "the candidate: the configuration plus the approved changes, not yet applied"
	}
	req := requestID(ctx, a)
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		return t.accept(tx, a, TypeProbe, SchemaProbe, req, ev)
	}); err != nil {
		return domain.Payload{}, err
	}
	return payload(SchemaTool, out), nil
}
