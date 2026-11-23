package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// ErrCorrupt is content whose bytes don't match its reference: a read that
// reaches the end with the wrong size or digest, or an import whose source
// doesn't match the ref it was given.
var ErrCorrupt = errors.New("artifact content is corrupt")

// ArtifactBackend is one kind of artifact storage's lifecycle. A location
// names one instance of it in the backend's own terms (a directory, a
// bucket and prefix); the host passes it through without interpreting it.
//
// Nothing in the contract rests on a process-local lock. A backend may let
// several stores, in several processes or on several hosts, open one
// location at once (an object store does), or refuse a second owner with
// ErrInUse (the file store, which owns a local directory). Either way an
// unfinished write a dead process left is never readable or listed, and
// the backend clears it up itself.
type ArtifactBackend interface {
	// Open opens the store at location, creating it when it is absent. A
	// location the backend cannot resolve the same way from any process (a
	// relative path) is refused with ErrInvalid.
	Open(ctx context.Context, location string) (ArtifactStore, error)
}

// ArtifactStore is an open artifact store: the Artifacts every participant
// uses, and what the host needs to back it up, restore it and collect what
// nothing references.
type ArtifactStore interface {
	Artifacts
	// List returns committed content whose ID sorts after after (byte
	// order), in ID order, at most limit (1 to 1000) entries. Content a
	// writer hasn't committed, or an aborted or failed one left, is never
	// listed, nor is deleted content. An empty page is the end. Pages can
	// be read while content is published and deleted: content committed
	// before the first page and not deleted before the last is listed
	// exactly once, and content published meanwhile may or may not be.
	List(ctx context.Context, after string, limit int) ([]StoredArtifact, error)
	// Import publishes source as exactly ref, ID and all, into a store that
	// doesn't hold that ID (ErrConflict if it does): how a restore puts
	// back the content its records reference. The source must hold exactly
	// ref.Size bytes; when ref has a digest they must match it, and when it
	// has none (a ref from before digests) the store records the digest of
	// what it read. A source that runs short or long or doesn't match is
	// ErrCorrupt, and publishes nothing. A backend may refuse an ID it
	// could not have issued with ErrInvalid.
	Import(ctx context.Context, ref ArtifactRef, source io.Reader) error
	// Close aborts this store's unfinished writers and releases it.
	Close() error
}

// ArtifactReach is an ArtifactStore that says whether another host can reach
// what it holds. Over state several hosts share, a successor on another host
// can read artifacts only from a store that can. shared is the answer, and
// stated is false when the store can't say, which claims neither.
type ArtifactReach interface {
	SharesLocation() (shared, stated bool)
}

// StoredArtifact is a committed artifact as List reports it.
type StoredArtifact struct {
	Ref ArtifactRef
	// PublishedAt is when the content was committed (or imported), by the
	// backend's clock.
	PublishedAt time.Time
}

// ValidDigest reports whether d is a SHA-256 digest as an ArtifactRef
// carries it: 64 lower-case hex characters.
func ValidDigest(d string) bool {
	if len(d) != 64 {
		return false
	}
	for i := 0; i < len(d); i++ {
		if c := d[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

var artifactBackends = struct {
	sync.Mutex
	m map[string]ArtifactBackend
}{m: map[string]ArtifactBackend{}}

// RegisterArtifactBackend makes an artifact backend selectable by name, as
// RegisterBackend does a coordination backend. Registering a name twice
// panics.
func RegisterArtifactBackend(name string, b ArtifactBackend) {
	artifactBackends.Lock()
	defer artifactBackends.Unlock()
	if name == "" || b == nil {
		panic("storage: RegisterArtifactBackend needs a name and a backend")
	}
	if _, dup := artifactBackends.m[name]; dup {
		panic(fmt.Sprintf("storage: artifact backend %q registered twice", name))
	}
	artifactBackends.m[name] = b
}

// LookupArtifactBackend returns the artifact backend registered under name.
func LookupArtifactBackend(name string) (ArtifactBackend, error) {
	artifactBackends.Lock()
	defer artifactBackends.Unlock()
	if b, ok := artifactBackends.m[name]; ok {
		return b, nil
	}
	names := make([]string, 0, len(artifactBackends.m))
	for n := range artifactBackends.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("storage: no artifact backend %q is built into this binary (it has %v)", name, names)
}
