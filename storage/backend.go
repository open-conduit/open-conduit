package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	// ErrSchemaNewer refuses state a newer binary migrated to a schema this
	// one does not know. Nothing was changed.
	ErrSchemaNewer = errors.New("stored schema is newer than this binary supports")
	// ErrRecordsNewer refuses state whose records a newer binary wrote: this
	// one would drop the fields it doesn't know from every record it
	// rewrites. Nothing was changed.
	ErrRecordsNewer = errors.New("stored records are newer than this binary writes")
	// ErrStateMoved refuses to raise state that has changed since it was
	// preserved, so the preserved copy would miss committed writes. Nothing
	// was changed. The caller preserves it again.
	ErrStateMoved = errors.New("stored state changed since it was preserved")
	// ErrInUse refuses state another Database or Inspection, in this
	// process or another, already owns.
	ErrInUse = errors.New("coordination state is in use by another owner")
)

// Backend is one kind of coordination state's lifecycle: how it is opened,
// inspected and restored. A location names one instance of that state in
// the backend's own terms (a file path, a connection string); the host
// passes it through without interpreting it.
//
// Open and Inspect refuse state a newer binary wrote (ErrSchemaNewer,
// ErrRecordsNewer) before they change anything at the location, so the
// newer binary, or a restore, can still use it.
//
// Open and Inspect take exclusive ownership of the state they open, held
// until Close, and refuse state another owner holds with ErrInUse. The
// ownership covers what the location names, wherever that is (a lock
// beside a database file, an advisory lock on a database server), so two
// hosts or state directories naming the same state cannot both own it.
// A location the backend cannot resolve the same way from any process (a
// relative path) is refused with ErrInvalid. A SharedBackend's shared location
// is the exception: the instance lease guarantees its one writer instead.
//
// Open raises the stored versions, and runs any one-time rewrite of stored
// records, only in a transaction that finds no live instance holder, and ends
// any expired holder's tenure in that transaction, so no instance runs on
// records it doesn't expect. With a live holder it returns ErrInstanceHeld and
// changes nothing.
type Backend interface {
	// Present reports whether state exists at location, without creating
	// or changing anything.
	Present(ctx context.Context, location string) (bool, error)
	// Open opens the state at location, creating it when it is absent.
	Open(ctx context.Context, location string) (Database, error)
	// Inspect opens the state at location to read it without changing it,
	// not even to record that this binary opened it. It refuses absent
	// state, and newer state as Open does.
	Inspect(ctx context.Context, location string) (Inspection, error)
	// OpenPrior opens state an older binary wrote, which Open would raise to
	// this binary's versions, without changing it, so it can be preserved
	// first. It takes the same ownership as Open, until Close. It returns
	// nil, and no error, when Open would raise nothing: the state is absent,
	// current, or newer (which Open refuses). When it would raise state a
	// live instance holds, it returns ErrInstanceHeld, since a copy taken now
	// would be stale by the time Open could raise it.
	OpenPrior(ctx context.Context, location string) (Prior, error)
	// Restore publishes the snapshot at a previously absent destination,
	// never over existing state. Every Execution lease in it is fenced (see
	// Database.Fence) and any instance tenure it recorded is ended before
	// the restored state can be opened, so no holder of either lease from
	// before the snapshot can commit to it.
	Restore(ctx context.Context, snapshot, destination string, now time.Time) error
	// ReadSnapshot calls read once over the state Restore would publish from
	// snapshot, raised to this binary's versions, and publishes nothing: the
	// snapshot is unchanged and no location is touched. It is how a restore
	// checks a snapshot before writing to a destination it could not take
	// back, such as a database on a server. read is read-only, as a View is.
	ReadSnapshot(ctx context.Context, snapshot string, now time.Time, read func(Tx) error) error
	// Writes is the versions this binary writes. State stored at a higher
	// version is what Open and Inspect refuse, so a bundle that records a
	// higher one can be refused from its manifest, before anything is copied.
	Writes() StoredVersion
}

// ReplicaRestorer is a Backend whose state can be continuously replicated by
// some other system, and restored from what that system kept.
type ReplicaRestorer interface {
	// RestoreReplica publishes the replica at a previously absent
	// destination, as Restore does a snapshot: verified, and with every
	// Execution lease fenced. The replica is not changed, and any log the
	// replicator left beside it is applied to a private copy first.
	RestoreReplica(ctx context.Context, replica, destination string, now time.Time) error
}

// Replication is what a backend can say about a continuous copy of its state.
type Replication string

const (
	// ReplicationNone says the state can't be continuously copied, so its
	// recovery point is the latest snapshot.
	ReplicationNone Replication = "none"
	// ReplicationUnobserved says another system can continuously copy the
	// state, but the backend can't see whether one is doing so.
	ReplicationUnobserved Replication = "unobserved"
	// ReplicationObserved says the backend sees a continuous copy, and how
	// far behind it is.
	ReplicationObserved Replication = "observed"
)

// RecoveryPoint is a backend's statement of how much committed state could be
// lost with the location that holds it. It reports what the backend knows and
// claims nothing it can't see. Whether that is enough is the operator's call.
type RecoveryPoint struct {
	Replication Replication
	// Lag is how far the copy is behind, for ReplicationObserved only.
	Lag time.Duration
	// Detail says it in the backend's own terms, for an operator.
	Detail string
}

// Valid reports whether the statement is one the contract allows.
func (p RecoveryPoint) Valid() bool {
	if p.Detail == "" || p.Lag < 0 {
		return false
	}
	switch p.Replication {
	case ReplicationNone, ReplicationUnobserved:
		return p.Lag == 0
	case ReplicationObserved:
		return true
	}
	return false
}

// RecoveryStater is a Backend that states its recovery point. Every backend
// should: the lifecycle conformance suite requires it. It is separate from
// Backend so that adding it breaks no backend built outside this repository.
type RecoveryStater interface {
	// RecoveryPoint states the recovery point of the state at location. It
	// changes nothing there, and may be called while a Database has it open.
	RecoveryPoint(ctx context.Context, location string) (RecoveryPoint, error)
}

// PriorRaiser is a Prior that can raise the state it holds, and only if that
// state is still what it held. The lifecycle suite requires it, and a host
// refuses a shared location whose Prior doesn't have it, since there nothing
// else stops another instance committing between a preservation and the raise.
// It is separate from Prior so that adding it breaks no backend built outside
// this repository.
type PriorRaiser interface {
	Prior
	// Raise raises the stored versions in one transaction, as Open would,
	// if nothing has committed since OpenPrior. Otherwise it changes nothing
	// and returns ErrStateMoved, and what was preserved is out of date. A
	// live holder is ErrInstanceHeld, and a holder past its expiry has its
	// tenure ended in the same transaction.
	Raise(ctx context.Context) error
}

// Database is open coordination state.
type Database interface {
	Store
	// Snapshot publishes a complete, verified copy of committed state as a
	// file at a previously absent path, and calls read once over exactly
	// the state it copied: no write committed after the copy is visible to
	// read. read is read-only, as a View is. A snapshot is restored with
	// the same Backend's Restore.
	Snapshot(ctx context.Context, path string, read func(Tx) error) error
	// Fence clears the owner and expiry of every Execution lease, keeping
	// its epoch. No earlier holder can commit afterwards, and the next
	// Claim advances the epoch. On a backend with an instance lease it is
	// a write like any other: while a holder is recorded it is refused with
	// ErrInstanceFenced and changes nothing, so a superseded instance can't
	// use it against its successor. A takeover needs no Fence, because
	// AcquireInstance fences the Execution leases itself.
	Fence(ctx context.Context, now time.Time) error
	Inspection
}

// Inspection is coordination state opened for reading.
type Inspection interface {
	Viewer
	// IntegrityCheck examines the whole of the stored state and reports
	// any corruption it finds.
	IntegrityCheck(ctx context.Context) error
	// SchemaVersion is the schema of the stored state.
	SchemaVersion() uint32
	// Stored is the versions the state is stored at.
	Stored(ctx context.Context) (StoredVersion, error)
	Close() error
}

// StoredVersion is the versions stored state was last written at.
type StoredVersion struct {
	// Schema is the stored schema, and Records the stored record format.
	Schema, Records uint32
}

// Prior is state an older binary wrote, held as that binary left it.
type Prior interface {
	// Stored is the versions the state is at.
	Stored() StoredVersion
	// Snapshot publishes a complete, verified copy at a previously absent
	// path, at the stored versions, and calls read once over exactly that
	// copy, as Database.Snapshot does. It changes nothing in the state.
	Snapshot(ctx context.Context, path string, read func(Tx) error) error
	Close() error
}

var backends = struct {
	sync.Mutex
	m map[string]Backend
}{m: map[string]Backend{}}

// RegisterBackend makes a backend selectable by name. A backend registers
// itself from its package's init, so building it into a binary is enough
// to offer it; the composition root selects one by name from the state's
// configuration. Registering a name twice panics.
func RegisterBackend(name string, b Backend) {
	backends.Lock()
	defer backends.Unlock()
	if name == "" || b == nil {
		panic("storage: RegisterBackend needs a name and a backend")
	}
	if _, dup := backends.m[name]; dup {
		panic(fmt.Sprintf("storage: backend %q registered twice", name))
	}
	backends.m[name] = b
}

// LookupBackend returns the backend registered under name.
func LookupBackend(name string) (Backend, error) {
	backends.Lock()
	defer backends.Unlock()
	if b, ok := backends.m[name]; ok {
		return b, nil
	}
	names := make([]string, 0, len(backends.m))
	for n := range backends.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("storage: no backend %q is built into this binary (it has %v)", name, names)
}
