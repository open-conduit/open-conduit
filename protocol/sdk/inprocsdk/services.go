package inprocsdk

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// The inproc.Host services that act inside the extension's transaction or
// on the host's control surfaces, carried by the protocol (#428).
//
// AcceptInTx and AcceptDerivedInTx return the Event from the transaction
// (sdk.EmitInTx): the host accepts it as a kv.commit Emission, in its own
// transaction with the transaction's KV writes. The answer keeps the
// in-process contract as far as the wire allows: an Event already accepted
// is answered as it stands with added false (so a caller's dedupe holds),
// and a new one with added true but Sequence 0, since its sequence is
// assigned only at commit (see inproc.Host.AcceptInTx). A causal bound
// can't refuse the call itself: the host records it, the rest of the batch
// commits, and the transaction's Transact returns an error wrapping
// inproc.ErrCausalLimit.

// services sets the inproc.Host services carried by h.
func (b *Bridge) services(host *inproc.Host, h *sdk.Host) {
	m := b.ext.Manifest
	own := func(t string) bool {
		return slices.ContainsFunc(m.Provides.Families, func(f protocol.Family) bool { return f.Type == t })
	}
	host.AcceptInTx = func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
		if !own(e.Type) {
			return domain.AcceptedEvent{}, false, inproc.ErrNotOwn
		}
		return sdk.EmitInTx(tx, scope, "", e)
	}
	host.AcceptDerivedInTx = func(tx storage.Tx, scope, parent domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
		if !own(e.Type) {
			return domain.AcceptedEvent{}, false, inproc.ErrNotOwn
		}
		// In process an Event may name its parent as its cause, as the host
		// would; on the wire the host assigns causation, so it goes, and a
		// cause other than the parent is refused as the runtime refuses it.
		if parent == "" || e.CausationID != "" && e.CausationID != parent {
			return domain.AcceptedEvent{}, false, storage.ErrInvalid
		}
		e.CausationID = ""
		return sdk.EmitInTx(tx, scope, parent, e)
	}
	host.EventScope = sdk.EventScopeInTx
	if len(m.Provides.Recurrences) > 0 {
		host.Timers = timers{h}
	}
	if len(m.Provides.WorkKinds) > 0 {
		host.CancelWork = h.CancelWork
	}
	if slices.Contains(m.Permissions, protocol.PermissionConfigRead) {
		host.Config = configHost{h}
	}
	if slices.Contains(m.Permissions, protocol.PermissionProbes) {
		host.Probes = func() map[string]extension.SetupProbe { return probes(h) }
	}
}

// timers is inproc.Timers over timer.arm and timer.cancel.
type timers struct{ h *sdk.Host }

func (t timers) ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error {
	return t.h.ArmTimer(ctx, scope, timer, expected, kind, spec)
}

func (t timers) Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error {
	return t.h.CancelTimer(ctx, id, expected)
}

// configHost is inproc.ConfigHost over config.* (permission config.read).
type configHost struct{ h *sdk.Host }

func (c configHost) Base(ctx context.Context) (inproc.ConfigBase, error) {
	r, err := c.h.ConfigBase(ctx)
	if err != nil {
		return inproc.ConfigBase{}, err
	}
	return inproc.ConfigBase{Generation: r.Generation, Hash: r.Hash, Handle: r.Handle, Text: []byte(r.Text), Folders: r.Folders}, nil
}

func (c configHost) Edit(ctx context.Context, handle, op, pointer string, value json.RawMessage) (inproc.ConfigEdited, error) {
	r, err := c.h.ConfigEdit(ctx, handle, op, pointer, value)
	if err != nil {
		return inproc.ConfigEdited{}, err
	}
	return inproc.ConfigEdited{Handle: r.Handle, Text: []byte(r.Text), Before: r.Before}, nil
}

func (c configHost) Validate(ctx context.Context, handle string) (string, []inproc.ConfigDiagnostic, error) {
	r, err := c.h.ConfigValidate(ctx, handle)
	if err != nil {
		return "", nil, err
	}
	var out []inproc.ConfigDiagnostic
	for _, d := range r.Diagnostics {
		out = append(out, inproc.ConfigDiagnostic{Path: d.Path, Code: d.Code, Message: d.Message})
	}
	return r.Hash, out, nil
}

func (c configHost) Trace(ctx context.Context, handle, scope, sample string) (inproc.CandidateTrace, error) {
	r, err := c.h.ConfigTrace(ctx, handle, scope, sample)
	if err != nil {
		return inproc.CandidateTrace{}, err
	}
	if r.Error != "" {
		return inproc.CandidateTrace{}, errors.New(r.Error)
	}
	out := inproc.CandidateTrace{Sample: r.Sample, Effect: inproc.CandidateEffect{Kind: r.Effect.Kind, WorkID: r.Effect.WorkID, Binding: r.Effect.Binding, Detail: r.Effect.Detail}}
	for _, s := range r.Samples {
		out.Samples = append(out.Samples, inproc.CandidateSample{ID: s.ID, Title: s.Title})
	}
	for _, rt := range r.Routes {
		out.Routes = append(out.Routes, inproc.CandidateRoute{Route: rt.Route, Handler: rt.Handler, Binding: rt.Binding, Verdict: rt.Verdict, Reason: rt.Reason})
	}
	return out, nil
}

// probes are the host's setup probes (probe.list), each run through
// probe.run. A listing that fails is empty.
func probes(h *sdk.Host) map[string]extension.SetupProbe {
	infos, err := h.Probes(context.Background())
	if err != nil {
		return nil
	}
	out := make(map[string]extension.SetupProbe, len(infos))
	for _, info := range infos {
		name := info.Name
		p := extension.SetupProbe{Name: name, Summary: info.Summary}
		for _, a := range info.Args {
			p.Args = append(p.Args, extension.ProbeArg{Name: a.Name, Summary: a.Summary, Required: a.Required})
		}
		p.Run = func(ctx context.Context, args map[string]string) (extension.ProbeResult, error) {
			r, err := h.Probe(ctx, name, args)
			if err != nil {
				return extension.ProbeResult{}, err
			}
			if r.Error != "" {
				return extension.ProbeResult{Command: r.Command}, errors.New(r.Error)
			}
			var output any
			if len(r.Output) > 0 {
				if err := json.Unmarshal(r.Output, &output); err != nil {
					return extension.ProbeResult{}, err
				}
			}
			return extension.ProbeResult{Command: r.Command, Output: output}, nil
		}
		out[name] = p
	}
	return out
}
