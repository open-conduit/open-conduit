package conformance_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

func TestReferenceStorage(t *testing.T) {
	conformance.RunStorage(t, func() storage.Store { return reference.New() })
}

// brokenAtomic acknowledges callback errors but commits their writes anyway.
type brokenAtomic struct{ storage.Store }

func (b brokenAtomic) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	var callbackErr error
	err := b.Store.Transact(ctx, g, func(tx storage.Tx) error { callbackErr = fn(tx); return nil })
	if err != nil {
		return err
	}
	return callbackErr
}

type brokenDedup struct{ storage.Store }
type brokenDedupTx struct{ storage.Tx }

func (b brokenDedup) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenDedupTx{tx}) })
}
func (b brokenDedupTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	// Incorrectly includes the internal event ID in the external delivery identity.
	e.DeliveryID += ":" + string(e.ID)
	return b.Tx.AcceptEvent(e)
}

// forgetsRetired treats a tombstone as no record of the delivery, so a
// redelivery of a retired Event is accepted again.
type forgetsRetired struct{ storage.Store }
type forgetsRetiredTx struct{ storage.Tx }

func (b forgetsRetired) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(forgetsRetiredTx{tx}) })
}
func (b forgetsRetiredTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	a, added, err := b.Tx.AcceptEvent(e)
	if err == nil && !added && a.Retired {
		return domain.AcceptedEvent{Event: e, Sequence: a.Sequence + 1}, true, nil
	}
	return a, added, err
}

// liveMaxSequence answers LatestSequence from the live rows, so retiring
// the newest Event moves it backwards.
type liveMaxSequence struct{ storage.Store }
type liveMaxSequenceTx struct{ storage.Tx }

func (b liveMaxSequence) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(liveMaxSequenceTx{tx}) })
}
func (b liveMaxSequenceTx) LatestSequence() (uint64, error) {
	rows, err := b.Tx.Events(0, 0)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[len(rows)-1].Sequence, nil
}

// liveOnlyReferences checks one kind of reference against the live rows
// alone, as a backend does that updated AcceptEvent for tombstones and
// nothing else.
type liveOnlyReferences struct {
	storage.Store
	mode string
}
type liveOnlyReferencesTx struct {
	storage.Tx
	mode string
}

func (b liveOnlyReferences) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(liveOnlyReferencesTx{tx, b.mode}) })
}

func (b liveOnlyReferencesTx) retired(id domain.ID) bool {
	a, err := b.Tx.Event(id)
	return err == nil && a.Retired
}

func (b liveOnlyReferencesTx) PutTimer(v domain.Timer, expected domain.Revision) error {
	switch b.mode {
	case "cause":
		if b.retired(v.Event.CausationID) {
			return storage.ErrNotFound
		}
	case "id":
		if b.retired(v.Event.ID) {
			v.Event.ID += "-armed"
		}
	case "delivery":
		if a, err := b.Tx.EventByDelivery(v.Event.Source, v.Event.DeliveryID); err == nil && a.Retired {
			v.Event.DeliveryID += "-armed"
		}
	}
	return b.Tx.PutTimer(v, expected)
}

func (b liveOnlyReferencesTx) PutExecution(x domain.Execution, expected domain.Revision) error {
	if b.mode == "trigger" && b.retired(x.TriggerID) {
		return storage.ErrNotFound
	}
	return b.Tx.PutExecution(x, expected)
}

func (b liveOnlyReferencesTx) PutWait(v domain.Wait, expected domain.Revision) error {
	if b.mode == "wake" && b.retired(v.WakeEventID) {
		return storage.ErrNotFound
	}
	if b.mode == "boundary" {
		rows, err := b.Tx.Events(0, 0)
		if err != nil {
			return err
		}
		if len(rows) == 0 || v.AfterSequence > rows[len(rows)-1].Sequence {
			return storage.ErrInvalid
		}
	}
	return b.Tx.PutWait(v, expected)
}

// brokenStepBack renews the way backends did before a stepped-back clock
// was tolerated: it refuses a renewal whose new expiry is earlier than the
// stored one.
type brokenStepBack struct{ storage.Store }
type brokenStepBackTx struct {
	storage.Tx
	now time.Time
}

func (b brokenStepBack) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenStepBackTx{tx, g.Now}) })
}
func (b brokenStepBackTx) Renew(ref domain.LeaseRef, ttl time.Duration) error {
	if x, err := b.Execution(ref.ExecutionID); err == nil && b.now.Add(ttl).Before(x.Lease.ExpiresAt) {
		return storage.ErrInvalid
	}
	return b.Tx.Renew(ref, ttl)
}

// clockStamped stamps each Work it writes with the transaction's clock,
// as storage did before #707, so a Work written after the clock steps back
// carries a time before changes a reader has already seen.
type clockStamped struct {
	storage.Store
	stamps map[domain.ID]time.Time
}
type clockStampedTx struct {
	storage.Tx
	b   *clockStamped
	now time.Time
}

func (b *clockStamped) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(&clockStampedTx{Tx: tx, b: b, now: g.Now}) })
}
func (t *clockStampedTx) PutWork(w domain.Work, expected domain.Revision) error {
	if err := t.Tx.PutWork(w, expected); err != nil {
		return err
	}
	t.b.stamps[w.ID] = t.now
	return nil
}
func (t *clockStampedTx) Claim(id domain.ID, owner string, ttl time.Duration) (domain.LeaseRef, error) {
	ref, err := t.Tx.Claim(id, owner, ttl)
	if x, e := t.Tx.Execution(id); err == nil && e == nil {
		t.b.stamps[x.WorkID] = t.now
	}
	return ref, err
}
func (t *clockStampedTx) restamp(w domain.Work) domain.Work {
	if at, ok := t.b.stamps[w.ID]; ok {
		w.UpdatedAt = at
	}
	return w
}
func (t *clockStampedTx) Work(id domain.ID) (domain.Work, error) {
	w, err := t.Tx.Work(id)
	return t.restamp(w), err
}
func (t *clockStampedTx) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	all, err := t.Tx.QueryWorks(storage.WorkQuery{})
	for i := range all {
		all[i] = t.restamp(all[i])
	}
	page, _ := storage.SelectWorks(all, q)
	return page, err
}

// serialView reads in a write transaction, so a View queues behind every
// Transact, as every read did before Views.
type serialView struct{ *reference.Memory }

func (b serialView) View(ctx context.Context, fn func(storage.Tx) error) error {
	return b.Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, func(tx storage.Tx) error { return fn(storage.ReadOnly(tx)) })
}

// writingView hands its callback a writable transaction.
type writingView struct{ *reference.Memory }

func (b writingView) View(ctx context.Context, fn func(storage.Tx) error) error {
	return b.Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, fn)
}

// brokenWorkEvents reads a Work's events by the Work they name alone,
// missing those routed to it.
type brokenWorkEvents struct{ storage.Store }
type brokenWorkEventsTx struct{ storage.Tx }

func (b brokenWorkEvents) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenWorkEventsTx{tx}) })
}
func (b brokenWorkEventsTx) WorkEvents(work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	all, err := b.Events(after, 0)
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if a.Event.WorkID == work && (limit <= 0 || len(out) < limit) {
			out = append(out, a)
		}
	}
	return out, err
}

// brokenUnroutedInput misses input that names one of the Work's attempts
// and not the Work itself.
type brokenUnroutedInput struct{ storage.Store }
type brokenUnroutedInputTx struct{ storage.Tx }

func (b brokenUnroutedInput) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenUnroutedInputTx{tx}) })
}
func (b brokenUnroutedInputTx) UnroutedInput(target storage.InputTarget, after, before uint64) ([]domain.AcceptedEvent, error) {
	all, err := b.Tx.UnroutedInput(target, after, before)
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if a.Event.WorkID == target.Work || a.Event.CorrelationKey != nil && slices.Contains(target.Keys, *a.Event.CorrelationKey) {
			out = append(out, a)
		}
	}
	return out, err
}

// brokenScopeEvents reads only recorded scopes: "" matches nothing, so
// Events not yet backfilled go missing.
type brokenScopeEvents struct{ storage.Store }
type brokenScopeEventsTx struct{ storage.Tx }

func (b brokenScopeEvents) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenScopeEventsTx{tx}) })
}
func (b brokenScopeEventsTx) ScopeEvents(scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	all, err := b.Events(after, 0)
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if a.ScopeID != "" && slices.Contains(scopes, a.ScopeID) && (limit <= 0 || len(out) < limit) {
			out = append(out, a)
		}
	}
	return out, err
}

// brokenTypeEvents weakens one property of TypeEvents: it returns every type
// ("type"), rereads from the start ("cursor"), or lets "" match nothing, so
// Events not yet backfilled go missing ("unrecorded").
type brokenTypeEvents struct {
	storage.Store
	weaken string
}
type brokenTypeEventsTx struct {
	storage.Tx
	weaken string
}

func (b brokenTypeEvents) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenTypeEventsTx{tx, b.weaken}) })
}
func (b brokenTypeEventsTx) TypeEvents(eventType string, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if b.weaken == "cursor" {
		after = 0
	}
	all, err := b.Events(after, 0)
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if b.weaken != "type" && a.Event.Type != eventType {
			continue
		}
		if scopes != nil && (!slices.Contains(scopes, a.ScopeID) || b.weaken == "unrecorded" && a.ScopeID == "") {
			continue
		}
		if limit <= 0 || len(out) < limit {
			out = append(out, a)
		}
	}
	return out, err
}

// anyAttemptExecutor selects Work by the executor of any of its attempts,
// not the latest's, as a store indexing attempts without asking which is
// latest might.
type anyAttemptExecutor struct{ storage.Store }
type anyAttemptExecutorTx struct{ storage.Tx }

func (b anyAttemptExecutor) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(anyAttemptExecutorTx{tx}) })
}
func (b anyAttemptExecutorTx) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	executor := q.Executor
	q.Executor = ""
	all, err := b.Tx.QueryWorks(q)
	out := []domain.Work{}
	for _, w := range all {
		xs, _ := b.ListExecutions(w.ID)
		if executor == "" || slices.ContainsFunc(xs, func(x domain.Execution) bool { return x.Executor == executor }) {
			out = append(out, w)
		}
	}
	return out, err
}

// textTimes orders Work by its times as written, as a store indexing the
// records' RFC 3339 text might: a time written east of UTC sorts late.
type textTimes struct{ storage.Store }
type textTimesTx struct{ storage.Tx }

func (b textTimes) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(textTimesTx{tx}) })
}
func (b textTimesTx) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	all, err := b.Tx.QueryWorks(storage.WorkQuery{States: q.States, ScopeID: q.ScopeID, Kind: q.Kind, ParentID: q.ParentID, Sources: q.Sources, Executor: q.Executor, UpdatedSince: q.UpdatedSince})
	text := func(w domain.Work) string {
		k := q.Order.Key(w)
		return k.At.Format(time.RFC3339Nano) + "/" + string(k.ID)
	}
	slices.SortFunc(all, func(a, b domain.Work) int {
		if q.Order.Descending() {
			return strings.Compare(text(b), text(a))
		}
		return strings.Compare(text(a), text(b))
	})
	out := []domain.Work{}
	for _, w := range all {
		if q.After != nil && (q.Order.Descending() != (text(w) < q.After.At.Format(time.RFC3339Nano)+"/"+string(q.After.ID))) {
			continue
		}
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
		out = append(out, w)
	}
	return out, err
}

// unboundedWithin ignores Within, as a store that missed the field would:
// a reader whose authority covers some scopes would list every scope's
// Work.
type unboundedWithin struct{ storage.Store }
type unboundedWithinTx struct{ storage.Tx }

func (b unboundedWithin) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(unboundedWithinTx{tx}) })
}
func (b unboundedWithinTx) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	q.Within = nil
	return b.Tx.QueryWorks(q)
}

// forgetfulArtifacts publishes a finished attempt's stopped turn without
// keeping the artifacts it held, as a store that overwrote the list might.
type forgetfulArtifacts struct{ storage.Store }
type forgetfulArtifactsTx struct{ storage.Tx }

func (b forgetfulArtifacts) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(forgetfulArtifactsTx{tx}) })
}
func (b forgetfulArtifactsTx) Execution(id domain.ID) (domain.Execution, error) {
	x, err := b.Tx.Execution(id)
	x.EarlierArtifacts = nil
	return x, err
}

// latestOnlyReferences answers ReferencedArtifacts from attempts' latest
// artifacts alone, as a reverse index fed only from the latest list would.
type latestOnlyReferences struct{ storage.Store }
type latestOnlyReferencesTx struct{ storage.Tx }

func (b latestOnlyReferences) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(latestOnlyReferencesTx{tx}) })
}
func (b latestOnlyReferencesTx) ReferencedArtifacts(ids []string) ([]string, error) {
	xs, err := b.Tx.ListExecutions("")
	if err != nil {
		return nil, err
	}
	for i := range xs {
		xs[i].EarlierArtifacts = nil
	}
	return storage.ReferencedIn(xs, ids)
}

// unsortedReferences reports each referenced ID once per time it was asked
// for, in the query's order, as a lookup that never grouped its rows might.
type unsortedReferences struct{ storage.Store }
type unsortedReferencesTx struct{ storage.Tx }

func (b unsortedReferences) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(unsortedReferencesTx{tx}) })
}
func (b unsortedReferencesTx) ReferencedArtifacts(ids []string) ([]string, error) {
	found, err := b.Tx.ReferencedArtifacts(ids)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, id := range ids {
		if slices.Contains(found, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// terminalWorkArtifacts refuses a finished attempt's stopped turn once its
// Work is finished, as one guarded like PutExecution would.
type terminalWorkArtifacts struct{ storage.Store }
type terminalWorkArtifactsTx struct{ storage.Tx }

func (b terminalWorkArtifacts) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(terminalWorkArtifactsTx{tx}) })
}
func (b terminalWorkArtifactsTx) PublishFinishedArtifacts(id domain.ID, refs []domain.ArtifactRef) error {
	x, err := b.Tx.Execution(id)
	if err != nil {
		return err
	}
	if w, err := b.Tx.Work(x.WorkID); err != nil || w.State.Terminal() {
		return storage.ErrInvalid
	}
	return b.Tx.PublishFinishedArtifacts(id, refs)
}

// brokenActor stores Events without who caused them, as a store mapping
// Events to columns might if it missed the field.
type brokenActor struct{ storage.Store }
type brokenActorTx struct{ storage.Tx }

func (b brokenActor) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenActorTx{tx}) })
}
func (b brokenActorTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	e.Actor = ""
	return b.Tx.AcceptEvent(e)
}

// autoincrementAudit numbers audit records from a counter that a rolled-back
// transaction still advances, as an identity column does, so the ledger
// has a gap where the rollback was.
type autoincrementAudit struct {
	storage.Store
	next      uint64
	committed []domain.AuditRecord
}
type autoincrementAuditTx struct {
	storage.Tx
	s       *autoincrementAudit
	now     time.Time
	pending *[]domain.AuditRecord
}

func (b *autoincrementAudit) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	var pending []domain.AuditRecord
	err := b.Store.Transact(ctx, g, func(tx storage.Tx) error {
		pending = nil
		return fn(autoincrementAuditTx{tx, b, g.Now, &pending})
	})
	if err == nil {
		b.committed = append(b.committed, pending...)
	}
	return err
}
func (b *autoincrementAudit) View(ctx context.Context, fn func(storage.Tx) error) error {
	return b.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error { return fn(storage.ReadOnly(tx)) })
}
func (t autoincrementAuditTx) AppendAudit(r domain.AuditRecord) (domain.AuditRecord, error) {
	all := append(slices.Clone(t.s.committed), *t.pending...)
	prev := ""
	if len(all) > 0 {
		prev = all[len(all)-1].Hash
	}
	sealed, ok := domain.SealAudit(r, t.s.next, prev, t.now)
	if !ok {
		return domain.AuditRecord{}, storage.ErrInvalid
	}
	t.s.next++
	*t.pending = append(*t.pending, sealed)
	return sealed, nil
}
func (t autoincrementAuditTx) AuditRecords(after uint64, limit int) ([]domain.AuditRecord, error) {
	out := []domain.AuditRecord{}
	for _, r := range append(slices.Clone(t.s.committed), *t.pending...) {
		if r.Sequence > after && (limit <= 0 || len(out) < limit) {
			out = append(out, r)
		}
	}
	return out, nil
}

// viaLostAudit stores audit records without the client their actor acted
// through, as a store mapping records to columns might if it missed one.
type viaLostAudit struct{ storage.Store }
type viaLostAuditTx struct{ storage.Tx }

func (b viaLostAudit) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(viaLostAuditTx{tx}) })
}
func (t viaLostAuditTx) AppendAudit(r domain.AuditRecord) (domain.AuditRecord, error) {
	r.Via = ""
	return t.Tx.AppendAudit(r)
}

// brokenVia stores Events without the client their actor acted through,
// as a store might that added Actor's column and missed Via's.
type brokenVia struct{ storage.Store }
type brokenViaTx struct{ storage.Tx }

func (b brokenVia) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(brokenViaTx{tx}) })
}
func (b brokenViaTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	e.Via = ""
	return b.Tx.AcceptEvent(e)
}

// identityColumn allocates sequences as an identity column does under
// concurrent writers: an Event takes its sequence when accepted and becomes
// visible when its transaction ends, and other transactions run meanwhile,
// so a later sequence can be visible before an earlier one. Each call runs
// in its own transaction of the inner store; only the calls the sequence
// case makes are supported.
type identityColumn struct {
	inner   storage.Store
	mu      sync.Mutex
	pending map[domain.ID]bool
}
type identityColumnTx struct {
	storage.Tx
	s        *identityColumn
	ctx      context.Context
	g        storage.Guard
	accepted []domain.ID
}

func (s *identityColumn) SerialWriters() bool { return false }
func (s *identityColumn) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	tx := &identityColumnTx{s: s, ctx: ctx, g: g}
	err := fn(tx)
	s.mu.Lock()
	for _, id := range tx.accepted {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	return err
}
func (t *identityColumnTx) AcceptEvent(e domain.Event) (a domain.AcceptedEvent, added bool, err error) {
	t.s.mu.Lock()
	t.s.pending[e.ID] = true
	t.s.mu.Unlock()
	t.accepted = append(t.accepted, e.ID)
	err = t.s.inner.Transact(t.ctx, t.g, func(tx storage.Tx) error {
		a, added, err = tx.AcceptEvent(e)
		return err
	})
	return a, added, err
}
func (t *identityColumnTx) Events(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	var all []domain.AcceptedEvent
	err := t.s.inner.Transact(t.ctx, t.g, func(tx storage.Tx) error {
		var err error
		all, err = tx.Events(after, 0)
		return err
	})
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if !t.s.pending[a.Event.ID] && (limit <= 0 || len(out) < limit) {
			out = append(out, a)
		}
	}
	return out, err
}
func (t *identityColumnTx) Event(id domain.ID) (a domain.AcceptedEvent, err error) {
	err = t.s.inner.Transact(t.ctx, t.g, func(tx storage.Tx) error {
		a, err = tx.Event(id)
		return err
	})
	return a, err
}
func (t *identityColumnTx) LatestSequence() (uint64, error) {
	events, err := t.Events(0, 0)
	if len(events) == 0 {
		return 0, err
	}
	return events[len(events)-1].Sequence, err
}

// likeKV lists KV keys the way a store matching prefix+"%" with SQL LIKE,
// unescaped, might: "_" in the prefix matches any one character and "%"
// any run of them.
type likeKV struct{ storage.Store }
type likeKVTx struct{ storage.Tx }

func (b likeKV) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(likeKVTx{tx}) })
}
func (b likeKVTx) List(ns, prefix string) ([]storage.Entry, error) {
	all, err := b.Tx.List(ns, "")
	re := regexp.MustCompile("^" + strings.NewReplacer("%", ".*", "_", ".").Replace(regexp.QuoteMeta(prefix)))
	out := []storage.Entry{}
	for _, e := range all {
		if re.MatchString(e.Key) {
			out = append(out, e)
		}
	}
	return out, err
}

// collatedKV lists KV keys in a case-insensitive collation's order, as a
// store comparing keys under a locale might.
type collatedKV struct{ storage.Store }
type collatedKVTx struct{ storage.Tx }

func (b collatedKV) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return b.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(collatedKVTx{tx}) })
}
func (b collatedKVTx) List(ns, prefix string) ([]storage.Entry, error) {
	out, err := b.Tx.List(ns, prefix)
	slices.SortStableFunc(out, func(x, y storage.Entry) int { return strings.Compare(strings.ToLower(x.Key), strings.ToLower(y.Key)) })
	return out, err
}

// abortOnce aborts the first run of every transaction that writes, as a
// backend under contention would, with abort: ErrConflict for one that
// reports contention as a clash, or ErrTransient after committing for one
// that reports an abort it cannot be sure of. The rest commit.
type abortOnce struct {
	storage.Store
	abort  error
	commit bool
	mu     sync.Mutex
	seen   map[string]bool
}

func (b *abortOnce) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	var name string
	err := b.Store.Transact(ctx, g, func(tx storage.Tx) error {
		r := &abortOnceTx{Tx: tx}
		if err := fn(r); err != nil {
			return err
		}
		name = r.event
		b.mu.Lock()
		first := name != "" && !b.seen[name]
		if first {
			b.seen[name] = true
		}
		b.mu.Unlock()
		if first && !b.commit {
			return b.abort
		}
		if first {
			name = "commit-then-abort"
		}
		return nil
	})
	if err == nil && name == "commit-then-abort" {
		return b.abort
	}
	return err
}

type abortOnceTx struct {
	storage.Tx
	event string
}

func (t *abortOnceTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	t.event = string(e.ID)
	return t.Tx.AcceptEvent(e)
}

func TestWeakenedBackendsAreRejected(t *testing.T) {
	cases := []struct {
		name, diagnostic string
		factory          func() storage.Store
	}{
		{"event_identity_not_correlation", "duplicate delivery", func() storage.Store { return brokenDedup{reference.New()} }},
		{"retired_event_keeps_identity_and_sequence", "redelivery accepted again", func() storage.Store { return forgetsRetired{reference.New()} }},
		{"retired_event_keeps_identity_and_sequence", "sequence went backwards", func() storage.Store { return liveMaxSequence{reference.New()} }},
		{"retired_event_keeps_identity_and_sequence", "timer citing a retired cause", func() storage.Store { return liveOnlyReferences{reference.New(), "cause"} }},
		{"retired_event_keeps_identity_and_sequence", "timer reused a retired event ID", func() storage.Store { return liveOnlyReferences{reference.New(), "id"} }},
		{"retired_event_keeps_identity_and_sequence", "timer reused a retired delivery", func() storage.Store { return liveOnlyReferences{reference.New(), "delivery"} }},
		{"retired_event_keeps_identity_and_sequence", "attempt whose trigger was retired", func() storage.Store { return liveOnlyReferences{reference.New(), "trigger"} }},
		{"retired_event_keeps_identity_and_sequence", "wait at the boundary of a retired event", func() storage.Store { return liveOnlyReferences{reference.New(), "boundary"} }},
		{"retired_event_keeps_identity_and_sequence", "wait woken by a retired event", func() storage.Store { return liveOnlyReferences{reference.New(), "wake"} }},
		{"event_actor_round_trip", "event actor lost", func() storage.Store { return brokenActor{reference.New()} }},
		{"event_actor_round_trip", "event via lost", func() storage.Store { return brokenVia{reference.New()} }},
		{"atomic_route_work_kv_timer", "event leaked", func() storage.Store { return brokenAtomic{reference.New()} }},
		{"timer_fire_rearm_rollback", "rollback was not atomic", func() storage.Store { return brokenAtomic{reference.New()} }},
		{"lease_renew_survives_clock_step_back", "renewal after clock step back", func() storage.Store { return brokenStepBack{reference.New()} }},
		{"work_changes_stamped_in_order_across_clock_step_back", "not listed as changed", func() storage.Store { return &clockStamped{Store: reference.New(), stamps: map[domain.ID]time.Time{}} }},
		{"work_events_follow_naming_and_routing", "work events", func() storage.Store { return brokenWorkEvents{reference.New()} }},
		{"event_scope_recorded_once_and_indexed", "scope events", func() storage.Store { return brokenScopeEvents{reference.New()} }},
		{"type_events_follow_type_scope_cursor_and_limit", "type events of a type", func() storage.Store { return brokenTypeEvents{reference.New(), "type"} }},
		{"type_events_follow_type_scope_cursor_and_limit", "type events after a cursor", func() storage.Store { return brokenTypeEvents{reference.New(), "cursor"} }},
		{"type_events_follow_type_scope_cursor_and_limit", "type events in a scope and the unrecorded", func() storage.Store { return brokenTypeEvents{reference.New(), "unrecorded"} }},
		{"unrouted_input_follows_work_attempts_and_keys", "unrouted input of a Work", func() storage.Store { return brokenUnroutedInput{reference.New()} }},
		{"work_queries_select_order_page_and_count", "latest attempt is claude's", func() storage.Store { return anyAttemptExecutor{reference.New()} }},
		{"work_queries_select_order_page_and_count", "every Work by creation", func() storage.Store { return textTimes{reference.New()} }},
		{"work_queries_select_order_page_and_count", "within scopes", func() storage.Store { return unboundedWithin{reference.New()} }},
		{"view_reads_committed_state_alongside_writes", "view waited for a write", func() storage.Store { return serialView{reference.New()} }},
		{"view_reads_committed_state_alongside_writes", "view write", func() storage.Store { return writingView{reference.New()} }},
		{"finished_attempt_takes_its_stopped_turns_artifacts", "earlier", func() storage.Store { return forgetfulArtifacts{reference.New()} }},
		{"finished_attempt_takes_its_stopped_turns_artifacts", "after the Work's cancel", func() storage.Store { return terminalWorkArtifacts{reference.New()} }},
		{"referenced_artifacts_follow_attempts", "one latest and one earlier", func() storage.Store { return latestOnlyReferences{reference.New()} }},
		{"referenced_artifacts_follow_attempts", `got ["b-latest" "a-earlier" "b-latest"]`, func() storage.Store { return unsortedReferences{reference.New()} }},
		{"sequences_visible_in_order_to_a_waiting_reader", "was skipped", func() storage.Store {
			return &identityColumn{inner: reference.New(), pending: map[domain.ID]bool{}}
		}},
		{"kv_prefixes_and_order_are_bytes", "prefix matched a wildcard", func() storage.Store { return likeKV{reference.New()} }},
		{"kv_prefixes_and_order_are_bytes", "not in byte order", func() storage.Store { return collatedKV{reference.New()} }},
		{"audit_ledger_numbers_chains_and_pages", "does not chain", func() storage.Store { return &autoincrementAudit{Store: reference.New()} }},
		{"audit_ledger_numbers_chains_and_pages", "changed in storage", func() storage.Store { return viaLostAudit{reference.New()} }},
		{"transient_aborts_commit_nothing_and_retry", "reported as a conflict", func() storage.Store {
			return &abortOnce{Store: reference.New(), abort: storage.ErrConflict, seen: map[string]bool{}}
		}},
		{"transient_aborts_commit_nothing_and_retry", "aborted transaction had committed", func() storage.Store {
			return &abortOnce{Store: reference.New(), abort: storage.ErrTransient, commit: true, seen: map[string]bool{}}
		}},
	}
	for _, negative := range cases {
		t.Run(negative.name, func(t *testing.T) {
			for _, c := range conformance.StorageCases() {
				if c.Name == negative.name {
					err := c.Check(negative.factory())
					if err == nil || !strings.Contains(err.Error(), negative.diagnostic) {
						t.Fatalf("wanted conformance rejection containing %q, got %v", negative.diagnostic, err)
					}
					t.Logf("weakened backend rejected: %v", err)
					return
				}
			}
			t.Fatal("negative control did not select a real conformance case")
		})
	}
}
