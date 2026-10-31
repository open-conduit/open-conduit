package sdk

import (
	"context"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

// Host is the extension's client for the scoped extension-to-host methods.
// The host binds every call to the identity it started the process with;
// nothing here can name another extension's namespaces or scopes.
type Host struct {
	conn *jsonrpc.Conn
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
	return h.conn.Call(ctx, "kv.commit", b, nil)
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
