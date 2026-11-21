// Package storage specifies atomic coordination storage and separate artifacts.
package storage

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("revision or identity conflict")
	ErrInvalid   = errors.New("invalid record or transition")
	ErrLeaseLost = errors.New("lease expired or superseded")
	ErrClaimed   = errors.New("execution or work already claimed")
	// ErrReadOnly refuses a write in a View.
	ErrReadOnly = errors.New("write in a read-only view")
	// ErrTransient aborts a transaction for a reason that says nothing
	// about the records it touched: a serialization conflict with another
	// transaction, a lock it could not take in time. Nothing it did was
	// committed, so running the whole transaction again, callback and all,
	// rereads the state and may succeed (Retry does). It is never an
	// ErrConflict: that is a revision or identity clash, which rerunning
	// the same logic over the same records cannot cure. A backend that
	// cannot tell whether a commit took effect (the connection dropped
	// during COMMIT) must not report ErrTransient.
	ErrTransient = errors.New("transient abort, nothing committed: retry the transaction")
)

// Guard.Now is supplied by the trusted runtime clock, never by an executor.
// Lease fences every mutation in an executor-originated transaction, including KV.
// A nil Lease denotes trusted control-plane authority, not unauthenticated access.
type Guard struct {
	Now   time.Time
	Lease *domain.LeaseRef
}
type Store interface {
	// Transact is serializable, all-or-nothing, and acknowledges only after commit.
	// It invokes fn once, never retries user callbacks, and rolls back on error,
	// panic or context cancellation before commit. Tx must not escape the callback.
	// A backend whose transactions can abort under contention returns
	// ErrTransient (on any call or from Transact itself) and commits
	// nothing; Retry is the one policy that reruns such a transaction.
	Transact(context.Context, Guard, func(Tx) error) error
}

// Viewer is a Store that can also read outside its transactions. View
// invokes fn once over a consistent snapshot of committed state, and may
// fail with ErrTransient as Transact may. It is
// read-only: every write fails with ErrReadOnly and nothing fn does is kept.
// Views run alongside each other and alongside Transact, so a read never
// waits for another read, or for a write to commit, and never sees a write
// that has not. Tx must not escape the callback.
type Viewer interface {
	View(context.Context, func(Tx) error) error
}

// View reads s through fn: through its View when s is a Viewer, else in a
// Transact at now whose writes are refused, which reads committed state as
// a View does but waits its turn with the writes.
func View(ctx context.Context, s Store, now time.Time, fn func(Tx) error) error {
	if v, ok := s.(Viewer); ok {
		return v.View(ctx, fn)
	}
	return s.Transact(ctx, Guard{Now: now}, func(tx Tx) error { return fn(ReadOnly(tx)) })
}

// ReadOnly is tx with every write refused with ErrReadOnly, for a backend's
// View to hand its callback.
func ReadOnly(tx Tx) Tx { return readOnly{tx} }

type readOnly struct{ Tx }

func (readOnly) AcceptEvent(domain.Event) (domain.AcceptedEvent, bool, error) {
	return domain.AcceptedEvent{}, false, ErrReadOnly
}
func (readOnly) MarkRouted(domain.ID, domain.ID, string) error        { return ErrReadOnly }
func (readOnly) SetEventScope(domain.ID, domain.ID) error             { return ErrReadOnly }
func (readOnly) RetireEvent(domain.ID) error                          { return ErrReadOnly }
func (readOnly) PutWork(domain.Work, domain.Revision) error           { return ErrReadOnly }
func (readOnly) PutExecution(domain.Execution, domain.Revision) error { return ErrReadOnly }
func (readOnly) SetExecutionExecutor(domain.ID, string) error         { return ErrReadOnly }
func (readOnly) PublishFinishedArtifacts(domain.ID, []domain.ArtifactRef) error {
	return ErrReadOnly
}
func (readOnly) Claim(domain.ID, string, time.Duration) (domain.LeaseRef, error) {
	return domain.LeaseRef{}, ErrReadOnly
}
func (readOnly) Renew(domain.LeaseRef, time.Duration) error                { return ErrReadOnly }
func (readOnly) Release(domain.LeaseRef) error                             { return ErrReadOnly }
func (readOnly) DeleteTimer(domain.ID, domain.Revision) error              { return ErrReadOnly }
func (readOnly) PutTimer(domain.Timer, domain.Revision) error              { return ErrReadOnly }
func (readOnly) FireTimer(domain.ID, domain.Revision, *domain.Timer) error { return ErrReadOnly }
func (readOnly) PutWait(domain.Wait, domain.Revision) error                { return ErrReadOnly }
func (readOnly) PutScope(domain.Scope, domain.Revision) error              { return ErrReadOnly }
func (readOnly) Put(string, string, []byte) error                          { return ErrReadOnly }
func (readOnly) AppendAudit(domain.AuditRecord) (domain.AuditRecord, error) {
	return domain.AuditRecord{}, ErrReadOnly
}
func (readOnly) Delete(string, string) error { return ErrReadOnly }

// Sequences: every accepted Event has a sequence greater than any accepted
// before it, with gaps allowed. Sequences become visible in sequence order:
// a reader that sees sequence n, through any of the event reads or
// LatestSequence, already sees every Event with a sequence below n that
// will ever commit. So an exclusive "after" cursor, or a Wait's
// AfterSequence boundary, never skips an Event that commits later. A
// backend that allocates sequences before commit (an identity column
// under concurrent writers) must hold readers back to the highest sequence
// below which every allocation has committed or rolled back.
type Tx interface {
	AcceptEvent(domain.Event) (domain.AcceptedEvent, bool, error)
	Event(domain.ID) (domain.AcceptedEvent, error)
	// Events returns acceptance order, exclusively after sequence; zero limit = all.
	Events(after uint64, limit int) ([]domain.AcceptedEvent, error)
	// UnroutedEvents is Events restricted to events not yet marked routed.
	// Its cost follows the unrouted backlog, not retained history.
	UnroutedEvents(after uint64, limit int) ([]domain.AcceptedEvent, error)
	// UnroutedInput is UnroutedEvents restricted to the Events that could
	// be input to one Work (InputTarget), accepted after sequence after and
	// before sequence before (zero for no bound), in acceptance order. It is
	// the read routing makes for each Event: its cost follows that Work's
	// own unrouted input, not the unrouted backlog.
	UnroutedInput(target InputTarget, after, before uint64) ([]domain.AcceptedEvent, error)
	// UnroutedOfType is UnroutedEvents restricted to Events of the given
	// types, in acceptance order: the timer-driven Events the scheduler
	// acts on itself. Its cost follows those Events, not the backlog.
	UnroutedOfType(types []string) ([]domain.AcceptedEvent, error)
	// WorkEvents is Events restricted to one Work's events: those naming it
	// (Event.WorkID) and those routed to it (RoutedWorkID). Like
	// UnroutedEvents, its cost follows the Work's own events, not retained
	// history.
	WorkEvents(work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error)
	// ScopeEvents is Events restricted to those whose recorded ScopeID is
	// one of scopes. An empty ID in scopes stands for "not recorded yet", so
	// a reader can ask for its scopes plus "" and resolve only the
	// unrecorded few. Like WorkEvents, its cost follows the scopes' own
	// events, not retained history.
	ScopeEvents(scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error)
	// TypeEvents is Events restricted to one type and, when scopes is not
	// nil, to those recorded in scopes ("" standing for not recorded yet).
	// Its cost follows the Events of that type, not retained history.
	TypeEvents(eventType string, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error)
	// LatestSequence is the last accepted sequence visible to this
	// transaction, zero before any event. Every Event at or below it is
	// visible too (see Sequences above).
	LatestSequence() (uint64, error)
	// EventByDelivery finds an event by its delivery identity.
	EventByDelivery(source, deliveryID string) (domain.AcceptedEvent, error)
	// MarkRouted commits alongside route effects; replay with identical args is safe.
	MarkRouted(eventID, workID domain.ID, disposition string) error
	// SetEventScope records the Scope an Event was accepted into. It is
	// write-once: the same scope again is nil, another is ErrConflict, an
	// empty one ErrInvalid, and an unknown Event or Scope ErrNotFound.
	SetEventScope(eventID, scopeID domain.ID) error
	// RetireEvent moves a routed Event out of the live log, leaving a
	// tombstone: its envelope and routing metadata without its payload,
	// marked Retired. Event and EventByDelivery answer with the tombstone, so
	// a redelivery is still a duplicate, the ID stays taken and a new Event
	// can still cite it as its cause. The list reads (Events, WorkEvents,
	// ScopeEvents, TypeEvents and the unrouted reads) omit it, as a gap in
	// the sequence, and LatestSequence and later sequences never go back
	// past it. Its routing and scope stay fixed: the same MarkRouted again
	// is nil, and any change is ErrConflict. Retiring it again is nil, an
	// unrouted Event is ErrInvalid and an unknown one ErrNotFound.
	RetireEvent(eventID domain.ID) error
	ListWorks() ([]domain.Work, error)
	// LiveWorks is ListWorks restricted to Work that has not finished
	// (State not Terminal), in ID order. Like UnroutedEvents, its cost
	// follows the live set, not the history retained.
	LiveWorks() ([]domain.Work, error)
	// QueryWorks lists the Work q selects (WorkQuery), in q.Order, after
	// q.After, at most q.Limit. CountWorks is how many Work q selects, and
	// WorkFacets what they can be told apart by. Their cost follows the
	// Work they select, read through indexes, not the history retained;
	// SelectWorks gives their meaning.
	QueryWorks(q WorkQuery) ([]domain.Work, error)
	CountWorks(q WorkQuery) (int, error)
	WorkFacets(q WorkQuery) (WorkFacets, error)
	Work(domain.ID) (domain.Work, error)
	WorkByCorrelation(domain.CorrelationKey) (domain.Work, error)
	// Put methods require expected=0 for create, otherwise the exact current revision.
	// Backend assigns revision/timestamps. Work correlations are exclusively owned.
	// A Work's UpdatedAt (and a new Work's CreatedAt, equal to it) is the
	// transaction's Now, or the latest UpdatedAt already committed when
	// that is later (the clock stepped back): a Work written after another
	// is never stamped before it, so a reader listing Work changed since
	// the latest change it saw (LiveOrChanged) misses none. Claim stamps
	// the Work it moves to running the same way.
	PutWork(domain.Work, domain.Revision) error
	// Empty work ID lists all attempts for recovery.
	ListExecutions(domain.ID) ([]domain.Execution, error)
	// LiveExecutions lists every attempt that has not finished (State not
	// Terminal: pending, running or parked), in ID order, at a cost that
	// follows the live set.
	LiveExecutions() ([]domain.Execution, error)
	Execution(domain.ID) (domain.Execution, error)
	PutExecution(domain.Execution, domain.Revision) error
	// SetExecutionExecutor records the Executor of an attempt registered
	// before attempts recorded it, finished or not, and the Work's
	// LatestExecutor when it is the latest. PutExecution keeps a recorded
	// executor when an update leaves it empty, and registering an attempt
	// sets its Work's LatestExecutor. It is write-once: the
	// same executor again is nil, another is ErrConflict, an empty one
	// ErrInvalid, and an unknown attempt ErrNotFound.
	SetExecutionExecutor(execution domain.ID, executor string) error
	// PublishFinishedArtifacts records refs as the artifacts of a finished
	// attempt's last turn, published after it finished: the attempt was
	// fenced (interrupted, cancelled, timed out) while that turn ran, and
	// its process wrote them once it stopped. As a turn's result does
	// through PutExecution, refs become the attempt's Artifacts and the
	// ones it held move to EarlierArtifacts (unless refs repeats them), so
	// the record keeps every one (PublishedArtifacts). The attempt's state,
	// outcome, lease and finish are unchanged; its revision and UpdatedAt
	// advance. It applies whatever the Work's state, since a cancel ends
	// the Work with the attempt. Refs the attempt already holds as its
	// Artifacts are nil and change nothing. An attempt not finished, no
	// refs or an invalid ref (empty ID, negative size, a duplicate) is
	// ErrInvalid, and an unknown attempt ErrNotFound.
	PublishFinishedArtifacts(execution domain.ID, refs []domain.ArtifactRef) error
	// ReferencedArtifacts returns the IDs among ids that some attempt
	// references, as one of its Artifacts or EarlierArtifacts, each once and
	// in byte order. Content no attempt references is an orphan (a
	// publication interrupted before its result was recorded, or content
	// never attached), which collection may delete once it is old enough.
	// Its cost follows len(ids), not the
	// attempts retained. More than MaxReferenceQuery IDs, or an empty one,
	// is ErrInvalid.
	ReferencedArtifacts(ids []string) ([]string, error)
	// Claim enters running on both Work and Execution and advances the lease epoch.
	Claim(domain.ID, string, time.Duration) (domain.LeaseRef, error)
	// Renew extends the lease to now+ttl, keeping the stored expiry when
	// that is later (the clock stepped back): it never shortens a lease.
	Renew(domain.LeaseRef, time.Duration) error
	Release(domain.LeaseRef) error
	ListTimers() ([]domain.Timer, error)
	Timer(domain.ID) (domain.Timer, error)
	DeleteTimer(domain.ID, domain.Revision) error
	PutTimer(domain.Timer, domain.Revision) error
	// FireTimer accepts the due event and deletes or replaces the timer atomically.
	// Replacement keeps the timer ID with a later FireAt and a new event identity.
	FireTimer(domain.ID, domain.Revision, *domain.Timer) error
	ListWaits(domain.ID) ([]domain.Wait, error)
	// ActiveWaits lists every Wait still active, in ID order, at a cost
	// that follows the active set.
	ActiveWaits() ([]domain.Wait, error)
	Wait(domain.ID) (domain.Wait, error)
	// Timeout Timer must exist at registration; history remains valid after firing/deletion.
	PutWait(domain.Wait, domain.Revision) error
	ListScopes() ([]domain.Scope, error)
	Scope(domain.ID) (domain.Scope, error)
	PutScope(domain.Scope, domain.Revision) error
	// Namespace is mandatory. Keys are compared as bytes: List returns the
	// keys that begin with prefix byte for byte (no character in prefix is
	// a wildcard, so "%" and "_" match only themselves), in byte order,
	// never a collation's.
	Get(namespace, key string) ([]byte, error)
	Put(namespace, key string, value []byte) error
	Delete(namespace, key string) error
	List(namespace, prefix string) ([]Entry, error)
	// AppendAudit adds r to the audit ledger, committing with the rest of
	// the transaction, and returns it as stored. Storage numbers it one
	// past the last record (1 for the first), so the ledger has no gaps;
	// sets At to the Guard's Now, Prev to the last record's Hash ("" for
	// the first) and Hash to the record's Digest; and ignores a writer's
	// values for them. An empty Action or Outcome is ErrInvalid. Records
	// are never changed or removed.
	AppendAudit(r domain.AuditRecord) (domain.AuditRecord, error)
	// AuditRecords returns the ledger in sequence order, exclusively
	// after sequence; zero limit = all. A record is visible only once
	// every record before it is, so an "after" cursor never skips one.
	AuditRecords(after uint64, limit int) ([]domain.AuditRecord, error)
}

// InputTarget is what an Event can be input to: an Event targets it when
// it names Work (Event.WorkID), names one of Work's recorded attempts
// (Event.ExecutionID), or carries one of Keys (Event.CorrelationKey).
// Keys are given rather than read from Work, so a caller can ask for the
// input of Work it has not stored yet, or with keys it is about to attach.
type InputTarget struct {
	Work domain.ID
	Keys []domain.CorrelationKey
}

type Entry struct {
	Key   string
	Value []byte
}

// Artifacts do not participate in coordination transactions. See contract
// docs, and ArtifactStore for a backend's whole artifact store.
type ArtifactRef = domain.ArtifactRef
type ArtifactWriter interface {
	io.WriteCloser
	// Commit closes, publishes immutable content and returns its durable
	// reference, with the content's size and SHA-256. Close without Commit
	// aborts; unsuccessful Commit never yields a valid ref.
	Commit(context.Context) (ArtifactRef, error)
}

// Artifacts is what every participant uses to publish and read content.
type Artifacts interface {
	Create(context.Context, string) (ArtifactWriter, error)
	// Open streams committed content. The ref must be the one Commit
	// returned: a different size, media type or digest is ErrInvalid, and
	// an unknown or deleted ID ErrNotFound. Uncommitted content is never
	// readable. The reader verifies the size and digest as it goes and
	// returns ErrCorrupt in place of io.EOF when they don't match, so only
	// a read to EOF proves the content.
	Open(context.Context, ArtifactRef) (io.ReadCloser, error)
	// Delete removes committed content. It is idempotent: an absent ID is
	// nil. A ref that doesn't match the stored content is ErrInvalid and
	// removes nothing.
	Delete(context.Context, ArtifactRef) error
}
