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
	Transact(context.Context, Guard, func(Tx) error) error
}

// Viewer is a Store that can also read outside its transactions. View
// invokes fn once over a consistent snapshot of committed state. It is
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
func (readOnly) PutWork(domain.Work, domain.Revision) error           { return ErrReadOnly }
func (readOnly) PutExecution(domain.Execution, domain.Revision) error { return ErrReadOnly }
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
func (readOnly) Delete(string, string) error                               { return ErrReadOnly }

type Tx interface {
	AcceptEvent(domain.Event) (domain.AcceptedEvent, bool, error)
	Event(domain.ID) (domain.AcceptedEvent, error)
	// Events returns acceptance order, exclusively after sequence; zero limit = all.
	Events(after uint64, limit int) ([]domain.AcceptedEvent, error)
	// UnroutedEvents is Events restricted to events not yet marked routed.
	// Its cost follows the unrouted backlog, not retained history.
	UnroutedEvents(after uint64, limit int) ([]domain.AcceptedEvent, error)
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
	// LatestSequence is the last accepted sequence, zero before any event.
	LatestSequence() (uint64, error)
	// EventByDelivery finds an event by its delivery identity.
	EventByDelivery(source, deliveryID string) (domain.AcceptedEvent, error)
	// MarkRouted commits alongside route effects; replay with identical args is safe.
	MarkRouted(eventID, workID domain.ID, disposition string) error
	// SetEventScope records the Scope an Event was accepted into. It is
	// write-once: the same scope again is nil, another is ErrConflict, an
	// empty one ErrInvalid, and an unknown Event or Scope ErrNotFound.
	SetEventScope(eventID, scopeID domain.ID) error
	ListWorks() ([]domain.Work, error)
	// LiveWorks is ListWorks restricted to Work that has not finished
	// (State not Terminal), in ID order. Like UnroutedEvents, its cost
	// follows the live set, not the history retained.
	LiveWorks() ([]domain.Work, error)
	Work(domain.ID) (domain.Work, error)
	WorkByCorrelation(domain.CorrelationKey) (domain.Work, error)
	// Put methods require expected=0 for create, otherwise the exact current revision.
	// Backend assigns revision/timestamps. Work correlations are exclusively owned.
	PutWork(domain.Work, domain.Revision) error
	// Empty work ID lists all attempts for recovery.
	ListExecutions(domain.ID) ([]domain.Execution, error)
	// LiveExecutions lists every attempt that has not finished (State not
	// Terminal: pending, running or parked), in ID order, at a cost that
	// follows the live set.
	LiveExecutions() ([]domain.Execution, error)
	Execution(domain.ID) (domain.Execution, error)
	PutExecution(domain.Execution, domain.Revision) error
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
	// Namespace is mandatory; prefix scans return keys in lexical order.
	Get(namespace, key string) ([]byte, error)
	Put(namespace, key string, value []byte) error
	Delete(namespace, key string) error
	List(namespace, prefix string) ([]Entry, error)
}
type Entry struct {
	Key   string
	Value []byte
}

// Artifacts do not participate in coordination transactions. See contract docs.
type ArtifactRef = domain.ArtifactRef
type ArtifactWriter interface {
	io.WriteCloser
	// Commit closes, publishes immutable content and returns its durable reference.
	// Close without Commit aborts; unsuccessful Commit never yields a valid ref.
	Commit(context.Context) (ArtifactRef, error)
}
type Artifacts interface {
	Create(context.Context, string) (ArtifactWriter, error)
	Open(context.Context, ArtifactRef) (io.ReadCloser, error)
	Delete(context.Context, ArtifactRef) error
}
