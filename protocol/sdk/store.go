package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// ErrReadOnly refuses a core-record write through Store: over the protocol
// an extension writes only its own KV namespaces (and reaches Events,
// timers and Work control through event.accept, timer.arm and work.cancel).
var ErrReadOnly = errors.New("sdk: core records are read-only over the protocol")

// Store is storage.Store over the host (07-03), so extension code written
// against the in-process contract (a projector, a toolbelt handler) runs in
// a subprocess unchanged:
//
//   - KV Get, List, Put and Delete reach the extension's own namespaces.
//     Reads carry version tokens; the writes of one Transact commit as a
//     single kv.commit batch whose preconditions are the versions (or
//     absence) of every key the callback read, so a concurrent change to a
//     key it read is storage.ErrConflict and nothing is applied. Conflict
//     detection covers keys read, not prefix ranges: a key added under a
//     listed prefix by another writer is not detected (weaker than the
//     host's serializable transactions; documented in the SDK guide).
//   - Work, ListWorks, ListExecutions, Event, Events, Scope, ListScopes,
//     ListTimers and their derived lookups are the scoped state reads: only
//     scopes configured for the extension are visible, and they are not
//     part of the KV preconditions.
//   - Every core write (AcceptEvent, PutWork, Claim, PutTimer, PutWait …)
//     returns ErrReadOnly.
type Store struct{ h *Host }

// NewStore returns the Store of a connection's Host.
func NewStore(h *Host) *Store { return &Store{h: h} }

type kvKey struct{ ns, key string }

type kvRead struct {
	found   bool
	version string
}

type storeTx struct {
	ctx    context.Context
	h      *Host
	reads  map[kvKey]kvRead
	writes map[kvKey][]byte // nil value: delete
	order  []kvKey
	emits  []protocol.Emission
}

// ErrNotBuffered refuses EmitInTx on a transaction Store.Transact did not
// open.
var ErrNotBuffered = errors.New("sdk: not a transaction of sdk.Store")

// EmitInTx returns an Event of one of the extension's own families from tx,
// a transaction of Store.Transact, for the host to accept in the same
// transaction as tx's writes when it commits (a kv.commit Emission): a
// root Event of scope when parent is empty, else one derived from parent.
//
// It answers as an in-process acceptance does, as far as the wire allows.
// An Event already accepted (the same source and delivery ID, read through
// state.event, or emitted earlier in tx) is answered as it stands with
// added false and is not emitted again. A new one is answered with added
// true, its ID the host's (EmittedEventID) and Sequence 0: its sequence is
// assigned only at commit, so a record must not be keyed by it. The host's
// answer at commit stays authoritative for a race. The host's refusal of
// the Event refuses the commit, except a causal bound: the rest of the
// batch commits and Transact returns an error wrapping
// inproc.ErrCausalLimit.
func EmitInTx(tx storage.Tx, scope, parent domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
	t, ok := tx.(*storeTx)
	if !ok {
		return domain.AcceptedEvent{}, false, ErrNotBuffered
	}
	if e.Source == "" || e.DeliveryID == "" {
		return domain.AcceptedEvent{}, false, storage.ErrInvalid
	}
	id := protocol.EmittedEventID(e.Source, e.DeliveryID)
	for _, em := range t.emits {
		if protocol.EmittedEventID(em.Event.Source, em.Event.DeliveryID) == id {
			e.ID = domain.ID(id)
			return domain.AcceptedEvent{Event: e}, false, nil
		}
	}
	var existing protocol.AcceptedEvent
	switch err := t.h.Call(t.ctx, "state.event", protocol.StateParams{ID: id}, &existing); {
	case err == nil:
		return existing.Domain(), false, nil
	case !errors.Is(err, storage.ErrNotFound):
		return domain.AcceptedEvent{}, false, err
	}
	t.emits = append(t.emits, protocol.Emission{ScopeID: string(scope), Parent: string(parent), Event: protocol.FromEvent(e)})
	e.ID = domain.ID(id)
	return domain.AcceptedEvent{Event: e}, true, nil
}

// EventScopeInTx is the scope an accepted Event was accepted into, read
// through state.event (only the extension's configured scopes are visible).
func EventScopeInTx(tx storage.Tx, a domain.AcceptedEvent) (domain.ID, error) {
	t, ok := tx.(*storeTx)
	if !ok {
		return "", ErrNotBuffered
	}
	var out protocol.AcceptedEvent
	if err := t.h.Call(t.ctx, "state.event", protocol.StateParams{ID: string(a.Event.ID)}, &out); err != nil {
		return "", err
	}
	return domain.ID(out.ScopeID), nil
}

// Transact runs fn once over a buffered transaction and commits its KV
// writes atomically with preconditions on what it read. It never retries.
func (s *Store) Transact(ctx context.Context, _ storage.Guard, fn func(storage.Tx) error) (err error) {
	tx := &storeTx{ctx: ctx, h: s.h, reads: map[kvKey]kvRead{}, writes: map[kvKey][]byte{}}
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(tx.writes) == 0 && len(tx.emits) == 0 {
		return nil
	}
	b := protocol.KVCommitParams{Emissions: tx.emits}
	for k, r := range tx.reads {
		b.Checks = append(b.Checks, protocol.KVCheck{Namespace: k.ns, Key: k.key, Version: r.version, Absent: !r.found})
	}
	sort.Slice(b.Checks, func(i, j int) bool {
		return b.Checks[i].Namespace+"\x00"+b.Checks[i].Key < b.Checks[j].Namespace+"\x00"+b.Checks[j].Key
	})
	for _, k := range tx.order {
		v, ok := tx.writes[k]
		if !ok {
			continue
		}
		if v == nil {
			b.Deletes = append(b.Deletes, protocol.KVDelete{Namespace: k.ns, Key: k.key})
		} else {
			b.Puts = append(b.Puts, protocol.KVPut{Namespace: k.ns, Key: k.key, Value: v})
		}
	}
	r, err := s.h.CommitBatch(ctx, b)
	if err != nil {
		return err
	}
	if refused := refusedEmissions(r); refused > 0 {
		return fmt.Errorf("%w: the batch committed, and the host refused %d of its Events", inproc.ErrCausalLimit, refused)
	}
	return nil
}

// refusedEmissions counts the Emissions the host refused over a causal
// bound while the rest of the batch committed.
func refusedEmissions(r protocol.KVCommitResult) int {
	n := 0
	for _, em := range r.Emissions {
		if em.Refused == protocol.CodeCausalLimit {
			n++
		}
	}
	return n
}

func (t *storeTx) read(k kvKey) ([]byte, bool, error) {
	if v, ok := t.writes[k]; ok {
		return v, v != nil, nil
	}
	v, version, err := t.h.Get(t.ctx, k.ns, k.key)
	if errors.Is(err, storage.ErrNotFound) {
		if _, seen := t.reads[k]; !seen {
			t.reads[k] = kvRead{}
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if _, seen := t.reads[k]; !seen {
		t.reads[k] = kvRead{found: true, version: version}
	}
	return v, true, nil
}

func (t *storeTx) Get(namespace, key string) ([]byte, error) {
	v, found, err := t.read(kvKey{namespace, key})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, storage.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (t *storeTx) write(k kvKey, v []byte) {
	if _, ok := t.writes[k]; !ok {
		t.order = append(t.order, k)
	}
	t.writes[k] = v
}

func (t *storeTx) Put(namespace, key string, value []byte) error {
	if namespace == "" || key == "" || value == nil {
		return storage.ErrInvalid
	}
	t.write(kvKey{namespace, key}, append([]byte(nil), value...))
	return nil
}

func (t *storeTx) Delete(namespace, key string) error {
	if namespace == "" || key == "" {
		return storage.ErrInvalid
	}
	t.write(kvKey{namespace, key}, nil)
	return nil
}

func (t *storeTx) List(namespace, prefix string) ([]storage.Entry, error) {
	entries, err := t.h.List(t.ctx, namespace, prefix)
	if err != nil {
		return nil, err
	}
	merged := map[string][]byte{}
	for _, e := range entries {
		k := kvKey{namespace, e.Key}
		if _, seen := t.reads[k]; !seen {
			t.reads[k] = kvRead{found: true, version: e.Version}
		}
		merged[e.Key] = e.Value
	}
	for k, v := range t.writes {
		if k.ns != namespace || !strings.HasPrefix(k.key, prefix) {
			continue
		}
		if v == nil {
			delete(merged, k.key)
		} else {
			merged[k.key] = v
		}
	}
	out := make([]storage.Entry, 0, len(merged))
	for k, v := range merged {
		out = append(out, storage.Entry{Key: k, Value: append([]byte(nil), v...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Scoped state reads.

func (t *storeTx) Work(id domain.ID) (domain.Work, error) {
	var out protocol.Work
	if err := t.h.Call(t.ctx, "state.work", protocol.StateParams{ID: string(id)}, &out); err != nil {
		return domain.Work{}, err
	}
	return out.Domain(), nil
}

func (t *storeTx) ListWorks() ([]domain.Work, error) { return t.h.Works(t.ctx, "") }

// LiveWorks filters ListWorks: the host serves no narrower read.
func (t *storeTx) LiveWorks() ([]domain.Work, error) {
	works, err := t.ListWorks()
	if err != nil {
		return nil, err
	}
	out := []domain.Work{}
	for _, w := range works {
		if !w.State.Terminal() {
			out = append(out, w)
		}
	}
	return out, nil
}

// LiveExecutions filters ListExecutions: the host serves no narrower read.
func (t *storeTx) LiveExecutions() ([]domain.Execution, error) {
	xs, err := t.ListExecutions("")
	if err != nil {
		return nil, err
	}
	out := []domain.Execution{}
	for _, x := range xs {
		if !x.State.Terminal() {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (t *storeTx) WorkByCorrelation(k domain.CorrelationKey) (domain.Work, error) {
	works, err := t.ListWorks()
	if err != nil {
		return domain.Work{}, err
	}
	for _, w := range works {
		for _, own := range w.CorrelationKeys {
			if own == k {
				return w, nil
			}
		}
	}
	return domain.Work{}, storage.ErrNotFound
}

func (t *storeTx) ListExecutions(work domain.ID) ([]domain.Execution, error) {
	if work == "" {
		works, err := t.ListWorks()
		if err != nil {
			return nil, err
		}
		var all []domain.Execution
		for _, w := range works {
			xs, err := t.ListExecutions(w.ID)
			if err != nil {
				return nil, err
			}
			all = append(all, xs...)
		}
		return all, nil
	}
	var out []protocol.Execution
	if err := t.h.Call(t.ctx, "state.executions", protocol.StateParams{ID: string(work)}, &out); err != nil {
		return nil, err
	}
	xs := make([]domain.Execution, len(out))
	for i, x := range out {
		xs[i] = x.Domain()
	}
	return xs, nil
}

func (t *storeTx) Execution(id domain.ID) (domain.Execution, error) {
	xs, err := t.ListExecutions("")
	if err != nil {
		return domain.Execution{}, err
	}
	for _, x := range xs {
		if x.ID == id {
			return x, nil
		}
	}
	return domain.Execution{}, storage.ErrNotFound
}

func (t *storeTx) Event(id domain.ID) (domain.AcceptedEvent, error) {
	var out protocol.AcceptedEvent
	if err := t.h.Call(t.ctx, "state.event", protocol.StateParams{ID: string(id)}, &out); err != nil {
		return domain.AcceptedEvent{}, err
	}
	return out.Domain(), nil
}

func (t *storeTx) Events(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	return pages(after, limit, func(after uint64, page int) ([]domain.AcceptedEvent, error) { return t.h.Events(t.ctx, after, page) })
}

// pages reads read's pages of at most 1000 after a sequence until limit
// (zero = all) or a short page, which the host sends only at the end.
func pages(after uint64, limit int, read func(after uint64, page int) ([]domain.AcceptedEvent, error)) ([]domain.AcceptedEvent, error) {
	var all []domain.AcceptedEvent
	for {
		page := 1000
		if limit > 0 && limit-len(all) < page {
			page = limit - len(all)
		}
		events, err := read(after, page)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
		if len(events) < page || limit > 0 && len(all) >= limit {
			return all, nil
		}
		after = events[len(events)-1].Sequence
	}
}

// UnroutedEvents is answered from Events: the protocol has no dedicated
// call for it, and no extension reads the routing backlog on a request path.
func (t *storeTx) UnroutedEvents(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	all, err := t.Events(after, 0)
	if err != nil {
		return nil, err
	}
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if !a.Routed && (limit <= 0 || len(out) < limit) {
			out = append(out, a)
		}
	}
	return out, nil
}

// WorkEvents reads one Work's events through state.work_events, which the
// host serves from its index: the cost follows the Work's own events, not
// the retained log.
func (t *storeTx) WorkEvents(work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if work == "" {
		return []domain.AcceptedEvent{}, nil
	}
	out, err := pages(after, limit, func(after uint64, page int) ([]domain.AcceptedEvent, error) {
		return t.h.WorkEvents(t.ctx, work, after, page)
	})
	if out == nil && err == nil {
		out = []domain.AcceptedEvent{}
	}
	return out, err
}

// ScopeEvents reads through state.events, which the host already limits to
// the extension's configured scopes and serves from its scope index, and
// keeps those of scopes. Every event the host returns carries its scope, so
// none is unrecorded here and "" in scopes matches nothing.
func (t *storeTx) ScopeEvents(scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	out := []domain.AcceptedEvent{}
	if len(scopes) == 0 {
		return out, nil
	}
	all, err := t.Events(after, 0)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if slices.Contains(scopes, a.ScopeID) && (limit <= 0 || len(out) < limit) {
			out = append(out, a)
		}
	}
	return out, nil
}

// LatestSequence is the log's latest sequence, through state.sequence: a
// cursor for Events, which may lie past the last Event of the extension's
// own scopes.
func (t *storeTx) LatestSequence() (uint64, error) {
	var out protocol.StateSequenceResult
	if err := t.h.Call(t.ctx, "state.sequence", protocol.StateParams{}, &out); err != nil {
		return 0, err
	}
	return out.Sequence, nil
}

func (t *storeTx) EventByDelivery(source, deliveryID string) (domain.AcceptedEvent, error) {
	all, err := t.Events(0, 0)
	if err != nil {
		return domain.AcceptedEvent{}, err
	}
	for _, a := range all {
		if a.Event.Source == source && a.Event.DeliveryID == deliveryID {
			return a, nil
		}
	}
	return domain.AcceptedEvent{}, storage.ErrNotFound
}

func (t *storeTx) ListScopes() ([]domain.Scope, error) {
	var out []protocol.Scope
	if err := t.h.Call(t.ctx, "state.scopes", protocol.StateParams{}, &out); err != nil {
		return nil, err
	}
	scopes := make([]domain.Scope, len(out))
	for i, s := range out {
		scopes[i] = s.Domain()
	}
	return scopes, nil
}

func (t *storeTx) Scope(id domain.ID) (domain.Scope, error) {
	var out protocol.Scope
	if err := t.h.Call(t.ctx, "state.scopes", protocol.StateParams{ID: string(id)}, &out); err != nil {
		return domain.Scope{}, err
	}
	return out.Domain(), nil
}

func (t *storeTx) ListTimers() ([]domain.Timer, error) {
	var out []protocol.Timer
	if err := t.h.Call(t.ctx, "state.timers", protocol.StateParams{}, &out); err != nil {
		return nil, err
	}
	timers := make([]domain.Timer, len(out))
	for i, x := range out {
		timers[i] = x.Domain()
	}
	return timers, nil
}

func (t *storeTx) Timer(id domain.ID) (domain.Timer, error) {
	timers, err := t.ListTimers()
	if err != nil {
		return domain.Timer{}, err
	}
	for _, x := range timers {
		if x.ID == id {
			return x, nil
		}
	}
	return domain.Timer{}, storage.ErrNotFound
}

// Core writes and unexported reads are not offered.

func (t *storeTx) AcceptEvent(domain.Event) (domain.AcceptedEvent, bool, error) {
	return domain.AcceptedEvent{}, false, ErrReadOnly
}
func (t *storeTx) MarkRouted(domain.ID, domain.ID, string) error        { return ErrReadOnly }
func (t *storeTx) SetEventScope(domain.ID, domain.ID) error             { return ErrReadOnly }
func (t *storeTx) PutWork(domain.Work, domain.Revision) error           { return ErrReadOnly }
func (t *storeTx) PutExecution(domain.Execution, domain.Revision) error { return ErrReadOnly }
func (t *storeTx) Claim(domain.ID, string, time.Duration) (domain.LeaseRef, error) {
	return domain.LeaseRef{}, ErrReadOnly
}
func (t *storeTx) Renew(domain.LeaseRef, time.Duration) error                { return ErrReadOnly }
func (t *storeTx) Release(domain.LeaseRef) error                             { return ErrReadOnly }
func (t *storeTx) DeleteTimer(domain.ID, domain.Revision) error              { return ErrReadOnly }
func (t *storeTx) PutTimer(domain.Timer, domain.Revision) error              { return ErrReadOnly }
func (t *storeTx) FireTimer(domain.ID, domain.Revision, *domain.Timer) error { return ErrReadOnly }

// ListWaits is a Work's Waits, through state.waits (a Work outside the
// extension's scopes is not found).
func (t *storeTx) ListWaits(work domain.ID) ([]domain.Wait, error) {
	var out []protocol.Wait
	if err := t.h.Call(t.ctx, "state.waits", protocol.StateParams{ID: string(work)}, &out); err != nil {
		return nil, err
	}
	waits := make([]domain.Wait, 0, len(out))
	for _, w := range out {
		waits = append(waits, w.Domain())
	}
	return waits, nil
}
func (t *storeTx) ActiveWaits() ([]domain.Wait, error)          { return nil, ErrReadOnly }
func (t *storeTx) Wait(domain.ID) (domain.Wait, error)          { return domain.Wait{}, ErrReadOnly }
func (t *storeTx) PutWait(domain.Wait, domain.Revision) error   { return ErrReadOnly }
func (t *storeTx) PutScope(domain.Scope, domain.Revision) error { return ErrReadOnly }

var _ storage.Store = (*Store)(nil)

// Artifacts is storage.Artifacts over the host's artifact.* methods: Create
// writes in acknowledged chunks of at most protocol.MaxChunkBytes and Commit
// publishes the immutable artifact. Open and Delete are host-only.
type Artifacts struct{ h *Host }

// NewArtifacts returns the Artifacts of a connection's Host.
func NewArtifacts(h *Host) *Artifacts { return &Artifacts{h: h} }

func (a *Artifacts) Create(ctx context.Context, mediaType string) (storage.ArtifactWriter, error) {
	var created protocol.ArtifactCreateResult
	if err := a.h.Call(ctx, "artifact.create", protocol.ArtifactCreateParams{MediaType: mediaType}, &created); err != nil {
		return nil, err
	}
	return &artifactWriter{h: a.h, ctx: ctx, id: created.Writer}, nil
}
func (a *Artifacts) Open(context.Context, storage.ArtifactRef) (io.ReadCloser, error) {
	return nil, ErrReadOnly
}
func (a *Artifacts) Delete(context.Context, storage.ArtifactRef) error { return ErrReadOnly }

type artifactWriter struct {
	h    *Host
	ctx  context.Context
	id   string
	done bool
}

func (w *artifactWriter) Write(b []byte) (int, error) {
	if w.done {
		return 0, storage.ErrInvalid
	}
	n := 0
	for len(b) > 0 {
		c := min(len(b), protocol.MaxChunkBytes)
		if err := w.h.Call(w.ctx, "artifact.write", protocol.ArtifactWriteParams{Writer: w.id, Chunk: b[:c]}, nil); err != nil {
			return n, err
		}
		n += c
		b = b[c:]
	}
	return n, nil
}
func (w *artifactWriter) Close() error {
	if w.done {
		return nil
	}
	w.done = true
	return w.h.Call(context.Background(), "artifact.abort", protocol.ArtifactTarget{Writer: w.id}, nil)
}
func (w *artifactWriter) Commit(ctx context.Context) (storage.ArtifactRef, error) {
	if w.done {
		return storage.ArtifactRef{}, storage.ErrInvalid
	}
	w.done = true
	var ref protocol.ArtifactRef
	if err := w.h.Call(ctx, "artifact.commit", protocol.ArtifactTarget{Writer: w.id}, &ref); err != nil {
		return storage.ArtifactRef{}, err
	}
	return storage.ArtifactRef{ID: ref.ID, Size: ref.Size, MediaType: ref.MediaType}, nil
}

var _ storage.Artifacts = (*Artifacts)(nil)
