package notes

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/protocol"
	"github.com/alsiola/open-conduit/protocol/sdk"
	"github.com/alsiola/open-conduit/storage"
)

// Build returns the served extension. onFault performs an armed fault for
// the subprocess (crash, partial response, hang); nil ignores faults.
func Build(o Options, onFault func(Fault)) *sdk.Extension {
	fault := func(method string) (Fault, bool) {
		f, ok := TakeFault(o.StateDir, method)
		if ok && onFault != nil && f.Mode != "reenter" {
			onFault(f)
		}
		return f, ok
	}
	m := Manifest(o)
	if o.Fixture {
		return fixture(m, fault)
	}
	var mu sync.Mutex
	sources := map[string]*Ingress{}
	ext := &sdk.Extension{Manifest: m, Upcasters: Upcasters()}
	ext.Migrate = func(ctx context.Context, h *sdk.Host, from, to int) error {
		if from >= 2 || to < 2 {
			return nil
		}
		entries, err := h.List(ctx, IndexNS, "")
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := h.Update(ctx, IndexNS, e.Key, func(old []byte, found bool) ([]byte, error) {
				if !found || strings.HasPrefix(string(old), "{") {
					return old, nil // already migrated: idempotent
				}
				return json.Marshal(map[string]string{"text": string(old)})
			}); err != nil {
				return err
			}
		}
		return nil
	}
	ext.Configure = func(ctx context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
		var out protocol.ConfigureResult
		next := map[string]*Ingress{}
		for _, e := range p.Entries {
			if e.Type != "notes" {
				continue
			}
			var cfg struct {
				Service string `json:"service"`
			}
			if err := json.Unmarshal(e.Config, &cfg); err != nil || cfg.Service == "" {
				return out, protocol.Errorf(protocol.CodeInvalid, "notes entry needs a service file")
			}
			name := Name + "/" + e.ScopeID
			next[name] = &Ingress{Service: cfg.Service, Release: o.Release, StableIdentity: o.StableIdentity}
			out.Sources = append(out.Sources, protocol.SourceDecl{Name: name, Source: protocol.Source{Namespace: name, ScopeID: e.ScopeID}, IntervalSeconds: 60, Families: []protocol.EventFamily{{Type: Changed, ReDerivable: true}, {Type: Deleted, Loss: m.Provides.Families[1].Loss}}})
		}
		mu.Lock()
		sources = next
		mu.Unlock()
		return out, nil
	}
	ext.Ingress = func(name string) (extension.Ingress, bool) {
		mu.Lock()
		defer mu.Unlock()
		in, ok := sources[name]
		if !ok {
			return nil, false
		}
		return &faulty{in: in, fault: fault}, true
	}
	ext.Routes = map[string]extension.RouteHandler{"notes": routeFunc(func(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
		e := in.Event.Event
		if e.Type != Changed || e.CorrelationKey == nil {
			return extension.RouteDecision{}, nil
		}
		return extension.RouteDecision{Match: true, CreateID: domain.ID("notes/" + e.CorrelationKey.ExternalID), Kind: Kind}, nil
	})}
	ext.Triggers = map[string]func([]domain.Event) int{Kind: func(in []domain.Event) int { return len(in) - 1 }}
	ext.Recurrences = map[string]sdk.RecurrenceFunc{Recurrence: func(_ []byte, fired domain.Timer, _ time.Time) (*domain.Event, *domain.Timer, error) {
		if f, ok := fault("recurrence.next"); ok && f.Mode == "reenter" {
			// A callback while serving an in-transaction method: the host
			// refuses it with reentrant_call.
			if _, _, err := reenter(); err != nil {
				return nil, nil, err
			}
		}
		occ := domain.Event{Type: Indexed, Source: Name + "/daily", DeliveryID: "daily/" + fired.FireAt.UTC().Format(time.RFC3339)}
		next := fired
		next.FireAt = fired.FireAt.Add(24 * time.Hour)
		return &occ, &next, nil
	}}
	ext.Operations = []extension.Operation{
		{Name: "notes.lookup", Handler: func(ctx context.Context, _ extension.Authority, in domain.Payload) (domain.Payload, error) {
			var req struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(in.Data, &req) != nil || req.ID == "" {
				return domain.Payload{}, &extension.OperationError{Code: "bad_request", Message: "lookup needs an id"}
			}
			v, _, err := sdk.HostFrom(ctx).Get(ctx, IndexNS, req.ID)
			if errors.Is(err, storage.ErrNotFound) {
				return domain.Payload{}, &extension.OperationError{Code: "unknown_note", Message: "no note " + req.ID}
			}
			if err != nil {
				return domain.Payload{}, err
			}
			out, _ := json.Marshal(map[string]string{"text": indexText(v)})
			return domain.Payload{Schema: "notes.lookup.v1", Data: out}, nil
		}},
		{Name: "notes.annotate", Emit: func(ctx context.Context, _ extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
			var req struct {
				Note string `json:"note"`
			}
			_ = json.Unmarshal(in.Data, &req)
			id := extension.CallRequestID(ctx)
			if id == "" {
				id = "annotation"
			}
			return extension.EmitPlan{Event: domain.Event{Type: Annotated, DeliveryID: "annotate/" + id}, Commit: func(em extension.Emitted) (extension.EmitCommit, error) {
				if _, ok := fault("emit.commit"); ok {
					return extension.EmitCommit{}, errors.New("unreachable after a fault")
				}
				out, _ := json.Marshal(map[string]any{"event": em.Event.Event.ID, "depth": em.Depth})
				return extension.EmitCommit{Records: []extension.Record{{Namespace: Name + ".annotations", Key: string(em.Event.Event.ID), Value: []byte(req.Note)}}, Output: domain.Payload{Schema: "notes.annotation.v1", Data: out}}, nil
			}}, nil
		}},
	}
	ext.Tasks = map[string]func(context.Context, *sdk.Host) error{
		"notes-index":  func(ctx context.Context, h *sdk.Host) error { return index(ctx, h, o, ext) },
		"notes-derive": func(ctx context.Context, h *sdk.Host) error { return derive(ctx, h) },
	}
	ext.Checks = map[string]func(context.Context, *sdk.Host, bool) (string, error){"notes": func(context.Context, *sdk.Host, bool) (string, error) { return "ok", nil }}
	ext.Render = func(e domain.Event) (string, bool) {
		if e.Type != Changed || e.CorrelationKey == nil {
			return "", false
		}
		return "note " + e.CorrelationKey.ExternalID + ": " + Text(e.Data), true
	}
	return ext
}

// reenter is replaced by the subprocess with a real host callback.
var reenter = func() ([]byte, string, error) { return nil, "", nil }

// SetReenter installs the callback used by the reenter fault.
func SetReenter(f func() ([]byte, string, error)) { reenter = f }

func indexText(v []byte) string {
	var m struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(v, &m) == nil && m.Text != "" {
		return m.Text
	}
	return string(v)
}

// index stores each changed note's text under notes.index and derives a
// notes.note.indexed Event whose record joins the same transaction.
func index(ctx context.Context, h *sdk.Host, o Options, ext *sdk.Extension) error {
	events, err := h.Events(ctx, 0, 1000)
	if err != nil {
		return err
	}
	for _, a := range events {
		e := a.Event
		if e.Type != Changed || e.CorrelationKey == nil {
			continue
		}
		data, err := sdk.Upcast(ext.Manifest, ext.Upcasters, e.Data)
		if err != nil {
			continue // an unreadable payload is skipped deterministically
		}
		value := []byte(Text(data))
		if kvVersion(o.Release) >= 2 {
			value, _ = json.Marshal(map[string]string{"text": Text(data)})
		}
		scope := strings.TrimPrefix(e.Source, Name+"/")
		if _, _, err := h.Accept(ctx, protocol.EventAcceptParams{ScopeID: scope, Parent: string(e.ID), Event: protocol.Event{Type: Indexed, Source: Name + "/index", DeliveryID: "indexed/" + string(e.ID)}, Records: []protocol.KVPut{{Namespace: IndexNS, Key: e.CorrelationKey.ExternalID, Value: value}}}); err != nil {
			return err
		}
	}
	return nil
}

// derive extends one causal chain by a derived Event per run, from the
// first changed note; the chain's depth and rate bounds are the host's.
func derive(ctx context.Context, h *sdk.Host) error {
	last, _, err := h.Get(ctx, Name+".chain", "last")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	parent, scope := string(last), ""
	if parent != "" {
		raw, _, err := h.Get(ctx, Name+".chain", "scope")
		if err != nil {
			return err
		}
		scope = string(raw)
	}
	if parent == "" {
		events, err := h.Events(ctx, 0, 1000)
		if err != nil {
			return err
		}
		for _, a := range events {
			if a.Event.Type == Changed {
				parent, scope = string(a.Event.ID), strings.TrimPrefix(a.Event.Source, Name+"/")
				break
			}
		}
		if parent == "" {
			return nil
		}
	}
	n := 0
	if raw, _, err := h.Get(ctx, Name+".chain", "n"); err == nil {
		n, _ = strconv.Atoi(string(raw))
	}
	accepted, _, err := h.Accept(ctx, protocol.EventAcceptParams{ScopeID: scope, Parent: parent, Event: protocol.Event{Type: Indexed, Source: Name + "/chain", DeliveryID: "chain/" + strconv.Itoa(n)}})
	if err != nil {
		return err
	}
	return h.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{
		{Namespace: Name + ".chain", Key: "last", Value: []byte(accepted.Event.ID)},
		{Namespace: Name + ".chain", Key: "scope", Value: []byte(scope)},
		{Namespace: Name + ".chain", Key: "n", Value: []byte(strconv.Itoa(n + 1))},
	}})
}

func fixture(m protocol.Manifest, fault func(string) (Fault, bool)) *sdk.Extension {
	return &sdk.Extension{Manifest: m, Operations: []extension.Operation{
		{Name: "fixture.echo", Handler: func(_ context.Context, _ extension.Authority, in domain.Payload) (domain.Payload, error) {
			fault("operation.call")
			return in, nil
		}},
		{Name: "fixture.wait", Wait: func(context.Context, extension.Authority, domain.Payload) (extension.WaitPlan, error) {
			return extension.WaitPlan{}, extension.ErrInvalid
		}},
		{Name: "fixture.report", Emit: func(ctx context.Context, _ extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
			return extension.EmitPlan{Event: domain.Event{Type: "fixture.changed.v1", DeliveryID: "report/" + extension.CallRequestID(ctx)}, Commit: func(em extension.Emitted) (extension.EmitCommit, error) {
				if _, ok := fault("emit.commit"); ok {
					return extension.EmitCommit{}, errors.New("unreachable after a fault")
				}
				out, _ := json.Marshal(map[string]any{"event": em.Event.Event.ID, "added": em.Added})
				return extension.EmitCommit{Records: []extension.Record{{Namespace: "fixture.reports", Key: string(em.Event.Event.ID), Value: in.Data}}, Output: domain.Payload{Schema: "fixture.report.v1", Data: out}}, nil
			}}, nil
		}},
	}, Filter: func(filter domain.Payload, e domain.Event) bool { return string(filter.Data) == string(e.Data.Data) }}
}

type routeFunc func(context.Context, extension.RouteInput) (extension.RouteDecision, error)

func (f routeFunc) Match(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	return f(ctx, in)
}

// faulty applies armed faults to ingress calls.
type faulty struct {
	in    *Ingress
	fault func(string) (Fault, bool)
}

func (f *faulty) Spec() extension.IngressSpec { return f.in.Spec() }
func (f *faulty) Push(ctx context.Context, s extension.Source, d extension.Delivery) (extension.PushResult, error) {
	f.fault("ingress.push")
	return f.in.Push(ctx, s, d)
}
func (f *faulty) Reconcile(ctx context.Context, s extension.Scan) (extension.Snapshot, error) {
	f.fault("ingress.reconcile")
	return f.in.Reconcile(ctx, s)
}
