package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

// Host is the extension's client for the scoped extension-to-host methods.
// The host binds every call to the identity it started the process with;
// nothing here can name another extension's namespaces or scopes.
type Host struct {
	conn *jsonrpc.Conn
	srv  *Server
}

// StateDir is the private state directory the host gave at initialize ("" on
// a Host made by NewHost).
func (h *Host) StateDir() string {
	if h.srv == nil {
		return ""
	}
	return h.srv.StateDir()
}

// ExecutorDir is the state directory the host named at initialize for one
// of the extension's executors ("" when it named none).
func (h *Host) ExecutorDir(name string) string {
	if h.srv == nil {
		return ""
	}
	return h.srv.ExecutorDir(name)
}

// ResourceDir is the state directory the host named at initialize for one
// of the extension's resource provider types ("" when it named none).
func (h *Host) ResourceDir(typ string) string {
	if h.srv == nil {
		return ""
	}
	return h.srv.ResourceDir(typ)
}

// ContainerDir is the state directory the host named at initialize for one
// of the extension's container provider types ("" when it named none).
func (h *Host) ContainerDir(typ string) string {
	if h.srv == nil {
		return ""
	}
	return h.srv.ContainerDir(typ)
}

// ContainerHost is the host's container label (extension.ContainerLabelHost)
// named at initialize ("" when it named none).
func (h *Host) ContainerHost() string {
	if h.srv == nil {
		return ""
	}
	return h.srv.ContainerHost()
}

// ConfigDir is the operator's config file's directory the host named at
// initialize ("" for none).
func (h *Host) ConfigDir() string {
	if h.srv == nil {
		return ""
	}
	return h.srv.ConfigDir()
}

// NewHost returns a Host on an existing connection (tests).
func NewHost(conn *jsonrpc.Conn) *Host { return &Host{conn: conn} }

// Call makes any extension-to-host call.
func (h *Host) Call(ctx context.Context, method string, params, out any) error {
	return h.conn.Call(ctx, method, params, out)
}

// Get returns a KV value and its version; storage.ErrNotFound when absent.
func (h *Host) Get(ctx context.Context, namespace, key string) ([]byte, string, error) {
	var out protocol.KVGetResult
	if err := h.conn.Call(ctx, "kv.get", protocol.KVGetParams{Namespace: namespace, Key: key}, &out); err != nil {
		return nil, "", err
	}
	if !out.Found {
		return nil, "", storage.ErrNotFound
	}
	return out.Entry.Value, out.Entry.Version, nil
}

// List returns the entries under prefix in lexical key order.
func (h *Host) List(ctx context.Context, namespace, prefix string) ([]protocol.KVEntry, error) {
	var out protocol.KVListResult
	err := h.conn.Call(ctx, "kv.list", protocol.KVListParams{Namespace: namespace, Prefix: prefix}, &out)
	return out.Entries, err
}

// Commit applies a precondition batch atomically; a failed check is
// storage.ErrConflict and changes nothing.
func (h *Host) Commit(ctx context.Context, b protocol.KVCommitParams) error {
	_, err := h.CommitBatch(ctx, b)
	return err
}

// CommitBatch is Commit answering each of the batch's Emissions' outcomes:
// the Events the host accepted in the same transaction as its writes.
func (h *Host) CommitBatch(ctx context.Context, b protocol.KVCommitParams) (protocol.KVCommitResult, error) {
	var out protocol.KVCommitResult
	err := h.conn.Call(ctx, "kv.commit", b, &out)
	return out, err
}

// ArmTimer arms (or re-arms, guarded by expected) a recurring timer of one
// of the extension's recurrence kinds, whose Event is of its own families.
func (h *Host) ArmTimer(ctx context.Context, scope domain.ID, t domain.Timer, expected domain.Revision, kind string, spec []byte) error {
	return h.conn.Call(ctx, "timer.arm", protocol.TimerArmParams{ScopeID: string(scope), Timer: protocol.FromTimer(t), Expected: uint64(expected), Kind: kind, Spec: spec}, nil)
}

// CancelTimer cancels one of the extension's timers (an absent one is a
// no-op), guarded by expected.
func (h *Host) CancelTimer(ctx context.Context, id domain.ID, expected domain.Revision) error {
	return h.conn.Call(ctx, "timer.cancel", protocol.TimerCancelParams{ID: string(id), Expected: uint64(expected)}, nil)
}

// CancelWork cancels Work of the extension's own kinds, idempotent per key
// and guarded by expected; it answers the cancellation's Event.
func (h *Host) CancelWork(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error) {
	var out protocol.WorkCancelResult
	err := h.conn.Call(ctx, "work.cancel", protocol.WorkCancelParams{WorkID: string(work), Expected: uint64(expected), Reason: reason, Key: key}, &out)
	return domain.ID(out.EventID), err
}

// Update reads a key, applies fn and commits with a version check, retrying
// on conflict. fn returns nil to delete.
func (h *Host) Update(ctx context.Context, namespace, key string, fn func(old []byte, found bool) ([]byte, error)) error {
	for attempt := 0; attempt < 16; attempt++ {
		old, version, err := h.Get(ctx, namespace, key)
		found := err == nil
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		value, err := fn(old, found)
		if err != nil {
			return err
		}
		check := protocol.KVCheck{Namespace: namespace, Key: key, Version: version, Absent: !found}
		b := protocol.KVCommitParams{Checks: []protocol.KVCheck{check}}
		if value == nil {
			b.Deletes = []protocol.KVDelete{{Namespace: namespace, Key: key}}
		} else {
			b.Puts = []protocol.KVPut{{Namespace: namespace, Key: key, Value: value}}
		}
		if err = h.Commit(ctx, b); !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	return storage.ErrConflict
}

// Accept derives one Event from parent (an accepted Event in a configured
// scope), committing the batch's KV writes atomically with it.
func (h *Host) Accept(ctx context.Context, p protocol.EventAcceptParams) (domain.AcceptedEvent, bool, error) {
	var out protocol.EventAcceptResult
	if err := h.conn.Call(ctx, "event.accept", p, &out); err != nil {
		return domain.AcceptedEvent{}, false, err
	}
	return out.Event.Domain(), out.Added, nil
}

// Deliver hands a delivery from the extension's own receiver to the host's
// ingress; the host answers only after durable acceptance, so the extension
// acknowledges upstream only after Deliver returns.
func (h *Host) Deliver(ctx context.Context, name string, d protocol.Delivery) ([]domain.AcceptedEvent, error) {
	var out protocol.IngressDeliverResult
	if err := h.conn.Call(ctx, "ingress.deliver", protocol.IngressDeliverParams{Name: name, Delivery: d}, &out); err != nil {
		return nil, err
	}
	accepted := make([]domain.AcceptedEvent, len(out.Accepted))
	for i, a := range out.Accepted {
		accepted[i] = a.Domain()
	}
	return accepted, nil
}

// Events reads accepted Events of configured scopes after a sequence.
func (h *Host) Events(ctx context.Context, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	var out []protocol.AcceptedEvent
	if err := h.conn.Call(ctx, "state.events", protocol.StateParams{After: after, Limit: limit}, &out); err != nil {
		return nil, err
	}
	accepted := make([]domain.AcceptedEvent, len(out))
	for i, a := range out {
		accepted[i] = a.Domain()
	}
	return accepted, nil
}

// Works lists Work of configured scopes (one scope when scope is set).
func (h *Host) Works(ctx context.Context, scope domain.ID) ([]domain.Work, error) {
	var out []protocol.Work
	if err := h.conn.Call(ctx, "state.works", protocol.StateParams{ScopeID: string(scope)}, &out); err != nil {
		return nil, err
	}
	works := make([]domain.Work, len(out))
	for i, w := range out {
		works[i] = w.Domain()
	}
	return works, nil
}

// WaitProgress sets the progress line of work's active Wait ("3 of 7 checks
// finished"), which one of the extension's own operations declared; ""
// clears it. Another's Wait is refused as unauthorized, and Work with no
// active Wait as not found.
func (h *Host) WaitProgress(ctx context.Context, work domain.ID, progress string) error {
	return h.conn.Call(ctx, "wait.progress", protocol.WaitProgressParams{WorkID: string(work), Progress: progress}, nil)
}

// Secrets resolves secret names bound to the extension's entry in scope.
func (h *Host) Secrets(ctx context.Context, scope domain.ID, names ...string) (map[string]string, error) {
	var out protocol.SecretResolveResult
	err := h.conn.Call(ctx, "secret.resolve", protocol.SecretResolveParams{ScopeID: string(scope), Names: names}, &out)
	return out.Values, err
}

// ConnectionSecrets resolves secret names of a connection one of the
// extension's entries in scope names (07-03), or one of the scope's
// bindings names when it is of one of the extension's connection types
// (an operation's Authority.Connection): the keys of the connection's
// secret_refs, never the references.
func (h *Host) ConnectionSecrets(ctx context.Context, scope domain.ID, connection string, names ...string) (map[string]string, error) {
	var out protocol.SecretResolveResult
	err := h.conn.Call(ctx, "secret.resolve", protocol.SecretResolveParams{ScopeID: string(scope), Connection: connection, Names: names}, &out)
	return out.Values, err
}

// Artifact writes an immutable artifact in acknowledged chunks.
func (h *Host) Artifact(ctx context.Context, mediaType string, data []byte) (domain.ArtifactRef, error) {
	var created protocol.ArtifactCreateResult
	if err := h.conn.Call(ctx, "artifact.create", protocol.ArtifactCreateParams{MediaType: mediaType}, &created); err != nil {
		return domain.ArtifactRef{}, err
	}
	for len(data) > 0 {
		n := min(len(data), protocol.MaxChunkBytes)
		if err := h.conn.Call(ctx, "artifact.write", protocol.ArtifactWriteParams{Writer: created.Writer, Chunk: data[:n]}, nil); err != nil {
			_ = h.conn.Call(context.Background(), "artifact.abort", protocol.ArtifactTarget{Writer: created.Writer}, nil)
			return domain.ArtifactRef{}, err
		}
		data = data[n:]
	}
	var ref protocol.ArtifactRef
	if err := h.conn.Call(ctx, "artifact.commit", protocol.ArtifactTarget{Writer: created.Writer}, &ref); err != nil {
		return domain.ArtifactRef{}, err
	}
	return domain.ArtifactRef{ID: ref.ID, Size: ref.Size, MediaType: ref.MediaType}, nil
}

// Capture records a named telemetry event in the host's telemetry events
// log, which every telemetry sink receives (permission telemetry.capture).
// scope is one of the extension's configured scopes, or "" for one about
// the machine; the zero time is the host's now.
func (h *Host) Capture(ctx context.Context, scope domain.ID, name string, at time.Time, props map[string]any) error {
	return h.conn.Call(ctx, "telemetry.capture", protocol.TelemetryEvent{ScopeID: string(scope), Name: name, At: at, Properties: props}, nil)
}

// SettleHold settles the hold the extension's start gate placed on work
// (permission work.start_gate): admit lets its next start through once;
// otherwise the Work is cancelled. reason goes in its history; key makes
// it idempotent.
func (h *Host) SettleHold(ctx context.Context, work domain.ID, admit bool, reason, key string) (domain.ID, error) {
	var out protocol.WorkSettleHoldResult
	err := h.conn.Call(ctx, "work.settle_hold", protocol.WorkSettleHoldParams{WorkID: string(work), Admit: admit, Reason: reason, Key: key}, &out)
	return domain.ID(out.EventID), err
}

// RenderOwned renders an input Event for an agent's prompt through the
// extension that owns its family (an executor's prompt; manifest
// executors); false leaves it to the caller's own rendering.
func (h *Host) RenderOwned(ctx context.Context, e domain.Event) (string, bool, error) {
	var out protocol.EventRenderResult
	err := h.conn.Call(ctx, "event.render_owned", protocol.EventRenderOwnedParams{Event: protocol.FromEvent(e)}, &out)
	return out.Text, out.OK, err
}

// WaitTarget resolves a named wait target of kind in one of the
// extension's scopes ("job" for a `job:<name>` condition) through the
// extensions that resolve that kind (a wait operation's; manifest wait
// operations): the correlation key and Event types that end it.
func (h *Host) WaitTarget(ctx context.Context, scope domain.ID, kind, name string) (domain.CorrelationKey, []string, bool, error) {
	var out protocol.WaitTargetResult
	err := h.conn.Call(ctx, "wait.target", protocol.WaitTargetParams{ScopeID: string(scope), Kind: kind, Name: name}, &out)
	return out.Key.Domain(), out.Types, out.OK, err
}

// Authorize answers whether scope from, one of the extension's, may read
// ("read", authorization.read_scopes) or file in ("file",
// authorization.file_scopes) scope to under the operator's configuration.
// A refusal is an error saying how the operator would allow it.
func (h *Host) Authorize(ctx context.Context, kind string, from, to domain.ID) error {
	var out protocol.ScopeAuthorizeResult
	if err := h.conn.Call(ctx, "scope.authorize", protocol.ScopeAuthorizeParams{Kind: kind, From: string(from), To: string(to)}, &out); err != nil {
		return err
	}
	if !out.Allowed {
		return errors.New(out.Reason)
	}
	return nil
}

// Act invokes an action for a person the extension identified on its own
// surface (permission action.submit), such as answering an approval
// request from a button they pressed. The host calls the provider with the
// caller {Kind: the extension's name, ID: r.Caller}; a refusal is the
// provider's *extension.OperationError.
func (h *Host) Act(ctx context.Context, r extension.ActRequest) (extension.ActResult, error) {
	var out protocol.ActionSubmitResult
	err := h.conn.Call(ctx, "action.submit", protocol.ActionSubmitParams{ScopeID: string(r.Scope), WorkID: string(r.Work), Provider: r.Provider, Action: r.Action, Input: r.Input, Caller: r.Caller, Key: r.Key}, &out)
	if err != nil {
		return extension.ActResult{}, err
	}
	return extension.ActResult{Event: out.Event.Domain(), Added: out.Added, Message: out.Message}, nil
}

// Actions lists what every provider offers now on scope, or on work in it
// (permission action.submit), so the package can show them on its own
// surface for Act to take.
func (h *Host) Actions(ctx context.Context, scope, work domain.ID) ([]extension.ActionOffer, error) {
	var out protocol.ActionListResult
	if err := h.conn.Call(ctx, "action.list", protocol.ActionListParams{ScopeID: string(scope), WorkID: string(work)}, &out); err != nil {
		return nil, err
	}
	offers := make([]extension.ActionOffer, 0, len(out.Actions))
	for _, a := range out.Actions {
		offers = append(offers, extension.ActionOffer{Provider: a.Provider, Action: extension.Action{ID: a.ID, Label: a.Label, Prompt: a.Prompt, Choices: a.Choices, Detail: a.Detail}})
	}
	return offers, nil
}

// ConfigBase reads the configuration now (permission config.read): its
// generation and hash, and a handle on its text with the text itself
// redacted. The host keeps the reference text.
func (h *Host) ConfigBase(ctx context.Context) (protocol.ConfigBaseResult, error) {
	var out protocol.ConfigBaseResult
	err := h.conn.Call(ctx, "config.base", struct{}{}, &out)
	return out, err
}

// ConfigEdit makes one edit (op add, replace or remove at a JSON pointer)
// to the text behind handle; the host holds the result as a new candidate
// (permission config.read). value may set a new reference.
func (h *Host) ConfigEdit(ctx context.Context, handle, op, pointer string, value json.RawMessage) (protocol.ConfigEditResult, error) {
	var out protocol.ConfigEditResult
	err := h.conn.Call(ctx, "config.edit", protocol.ConfigEditParams{Handle: handle, Op: op, Pointer: pointer, Value: value}, &out)
	return out, err
}

// ConfigValidate validates the candidate behind handle as POST
// /config/validate would (permission config.read).
func (h *Host) ConfigValidate(ctx context.Context, handle string) (protocol.ConfigValidateResult, error) {
	var out protocol.ConfigValidateResult
	err := h.conn.Call(ctx, "config.validate", protocol.ConfigHandleParams{Handle: handle}, &out)
	return out, err
}

// ConfigTrace traces one of a scope's named samples against the candidate
// behind handle, writing nothing; with no sample it lists the scope's
// samples (permission config.read).
func (h *Host) ConfigTrace(ctx context.Context, handle, scope, sample string) (protocol.ConfigTraceResult, error) {
	var out protocol.ConfigTraceResult
	err := h.conn.Call(ctx, "config.trace", protocol.ConfigTraceParams{Handle: handle, Scope: scope, Sample: sample}, &out)
	return out, err
}

// Probes lists the setup probes the host, its extensions and its executors
// offer, by full name (permission probes).
func (h *Host) Probes(ctx context.Context) ([]protocol.ProbeInfo, error) {
	var out protocol.ProbeListResult
	err := h.conn.Call(ctx, "probe.list", struct{}{}, &out)
	return out.Probes, err
}

// Probe runs one setup probe (permission probes). A probe that ran and
// failed answers its reason in Error.
func (h *Host) Probe(ctx context.Context, name string, args map[string]string) (protocol.ProbeRunResult, error) {
	var out protocol.ProbeRunResult
	err := h.conn.Call(ctx, "probe.run", protocol.ProbeRunParams{Name: name, Args: args}, &out)
	return out, err
}
