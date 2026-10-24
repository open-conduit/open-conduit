// Package storage specifies atomic coordination storage and separate artifacts.
package storage

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/alsiola/open-conduit/domain"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("revision or identity conflict")
	ErrInvalid   = errors.New("invalid record or transition")
	ErrLeaseLost = errors.New("lease expired or superseded")
	ErrClaimed   = errors.New("execution or work already claimed")
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
type Tx interface {
	AcceptEvent(domain.Event) (domain.AcceptedEvent, bool, error)
	Event(domain.ID) (domain.AcceptedEvent, error)
	// Events returns acceptance order, exclusively after sequence; zero limit = all.
	Events(after uint64, limit int) ([]domain.AcceptedEvent, error)
	// MarkRouted commits alongside route effects; replay with identical args is safe.
	MarkRouted(eventID, workID domain.ID, disposition string) error
	ListWorks() ([]domain.Work, error)
	Work(domain.ID) (domain.Work, error)
	WorkByCorrelation(domain.CorrelationKey) (domain.Work, error)
	// Put methods require expected=0 for create, otherwise the exact current revision.
	// Backend assigns revision/timestamps. Work correlations are exclusively owned.
	PutWork(domain.Work, domain.Revision) error
	// Empty work ID lists all attempts for recovery.
	ListExecutions(domain.ID) ([]domain.Execution, error)
	Execution(domain.ID) (domain.Execution, error)
	PutExecution(domain.Execution, domain.Revision) error
	// Claim enters running on both Work and Execution and advances the lease epoch.
	Claim(domain.ID, string, time.Duration) (domain.LeaseRef, error)
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
	Wait(domain.ID) (domain.Wait, error)
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
