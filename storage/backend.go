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
// relative path) is refused with ErrInvalid.
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
	// current, or newer (which Open refuses).
	OpenPrior(ctx context.Context, location string) (Prior, error)
	// Restore publishes the snapshot at a previously absent destination,
	// never over existing state. Every Execution lease in it is fenced (see
	// Database.Fence) before the restored state can be opened, so no lease
	// holder from before the snapshot can commit to it.
	Restore(ctx context.Context, snapshot, destination string, now time.Time) error
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
	// Claim advances the epoch. It is how restore, and a standby taking
	// over the state, end the authority of whoever held it before.
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
