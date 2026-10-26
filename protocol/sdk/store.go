package sdk

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/protocol"
	"github.com/alsiola/open-conduit/storage"
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
	if len(tx.writes) == 0 {
		return nil
	}
	var b protocol.KVCommitParams
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
	return s.h.Commit(ctx, b)
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
	var all []domain.AcceptedEvent
	for {
		page := 1000
		if limit > 0 && limit-len(all) < page {
			page = limit - len(all)
		}
		events, err := t.h.Events(t.ctx, after, page)
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
func (t *storeTx) ListWaits(domain.ID) ([]domain.Wait, error)                { return nil, ErrReadOnly }
func (t *storeTx) Wait(domain.ID) (domain.Wait, error)                       { return domain.Wait{}, ErrReadOnly }
func (t *storeTx) PutWait(domain.Wait, domain.Revision) error                { return ErrReadOnly }
func (t *storeTx) PutScope(domain.Scope, domain.Revision) error              { return ErrReadOnly }

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
