// Package backup composes a coordination backend's consistent snapshot with
// the artifacts it references, held from retention while they are copied.
// It works over the storage lifecycle and artifact contracts
// (storage.Backend, storage.Database, storage.ArtifactStore), never a
// particular backend; a bundle's own artifacts are a file store, as part
// of the bundle format. Backups contain private runtime state and must
// remain owner-only.
package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/storage"
)

type entry struct {
	Ref    domain.ArtifactRef
	SHA256 string
}
type manifest struct {
	Version uint32
	// Backend names the storage backend whose snapshot the bundle holds;
	// empty is SQLite, which every bundle held before backends had names
	// (left out for it, so older binaries still read SQLite bundles).
	Backend        string `json:",omitempty"`
	Schema         uint32
	Build          string
	CreatedAt      time.Time
	DatabaseSHA256 string
	Artifacts      []entry
	// Omitted lists references whose content retention expired. Their bytes
	// are not in the bundle; restore accepts them only on expired Executions.
	Omitted []domain.ArtifactRef `json:",omitempty"`
}

// Expired reports whether retention has expired an Execution's content. The
// runtime owns the retention ledger, so callers supply the lookup; nil means
// no Execution has expired.
type Expired func(tx storage.Tx, id domain.ID) (bool, error)

func (e Expired) of(tx storage.Tx, id domain.ID) (bool, error) {
	if e == nil {
		return false, nil
	}
	return e(tx, id)
}

// Retention is what Create needs of the runtime's retention ledger, which
// the runtime owns: Expired, and a retention hold (runtime.HoldRetention)
// that keeps retention from deleting content while Create copies it. Hold
// commits or renews the hold id until until; Release removes it.
type Retention struct {
	Expired Expired
	Hold    func(tx storage.Tx, id string, until time.Time) error
	Release func(tx storage.Tx, id string) error
	// HoldFor is how long each commit or renewal of the hold lasts; zero
	// is DefaultHoldFor. Create renews it every third of that while it
	// copies, so it lapses that long after a crashed backup.
	HoldFor time.Duration
	// Now is the clock the hold's expiry is set on, which must be the one
	// retention checks it against (the scheduler's); nil is time.Now.
	Now func() time.Time
	// Renewals, when set, is when to renew the hold, in place of a ticker
	// every third of HoldFor: a test drives it.
	Renewals <-chan time.Time
}

func (r Retention) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// DefaultHoldFor is how long a backup's retention hold lasts unless
// renewed.
const DefaultHoldFor = 15 * time.Minute

// hold commits a new retention hold, renews it until the returned stop is
// called, then releases it. ctx is cancelled with the renewal's error when
// one fails, since the hold may then lapse under the copy.
func (r Retention) hold(ctx context.Context, db storage.Database) (context.Context, func(), error) {
	if r.Hold == nil || r.Release == nil {
		return nil, nil, storage.ErrInvalid
	}
	period := r.HoldFor
	if period <= 0 {
		period = DefaultHoldFor
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, nil, err
	}
	id := "backup/" + hex.EncodeToString(b[:])
	put := func(ctx context.Context) error {
		now := r.now()
		return db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return r.Hold(tx, id, now.Add(period)) })
	}
	if err := put(ctx); err != nil {
		return nil, nil, err
	}
	held, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		renewals := r.Renewals
		if renewals == nil {
			tick := time.NewTicker(period / 3)
			defer tick.Stop()
			renewals = tick.C
		}
		for {
			select {
			case <-done:
				return
			case <-held.Done():
				return
			case <-renewals:
				if err := put(held); err != nil {
					cancel(fmt.Errorf("backup: renewing the retention hold: %w", err))
					return
				}
			}
		}
	}()
	stop := func() {
		close(done)
		<-renewed
		cancel(nil)
		// A hold that isn't released lapses at its expiry, which only
		// delays retention, so a failure here doesn't fail the backup.
		_ = db.Transact(context.WithoutCancel(ctx), storage.Guard{Now: r.now()}, func(tx storage.Tx) error { return r.Release(tx, id) })
	}
	return held, stop, nil
}

// DefaultBackend is the backend a manifest that names none was made with.
const DefaultBackend = "sqlite"

// snapshotName is the coordination snapshot's file in a bundle. It keeps
// the name SQLite bundles always had, whatever the backend.
const snapshotName = "conduit.db"

// Create publishes a complete immutable bundle at a previously absent path.
// A retention hold commits before the database snapshot and lasts until the
// copy is done, so retention cannot remove captured refs (Retention).
// References of Executions whose retention expired are recorded as omitted
// rather than copied, since their content may already be gone. backend is
// the name db's Backend is registered under; Restore looks it up again.
// Content a reference names that is missing anyway fails the backup, which
// then publishes nothing.
func Create(ctx context.Context, destination string, backend string, db storage.Database, files storage.ArtifactStore, build string, retention Retention) error {
	if backend == "" || files == nil {
		return storage.ErrInvalid
	}
	expired := retention.Expired
	_, err := publish(ctx, destination, func(stage string) (err error) {
		ctx, release, err := retention.hold(ctx, db)
		if err != nil {
			return err
		}
		defer func() {
			// A failed renewal cancels the copy; say why, not just that.
			if cause := context.Cause(ctx); err != nil && cause != nil && !errors.Is(cause, context.Canceled) {
				err = cause
			}
			release()
		}()
		database := filepath.Join(stage, snapshotName)
		var refs map[string]domain.ArtifactRef
		var live map[string]bool
		// References are read from the snapshot's own state, not the live
		// database, so the bundle copies exactly what its snapshot names.
		err = db.Snapshot(ctx, database, func(tx storage.Tx) error {
			refs, live = map[string]domain.ArtifactRef{}, map[string]bool{}
			xs, err := tx.ListExecutions("")
			if err != nil {
				return err
			}
			for _, x := range xs {
				gone, err := expired.of(tx, x.ID)
				if err != nil {
					return err
				}
				for _, ref := range x.AllArtifacts() {
					if old, ok := refs[ref.ID]; ok && old != ref {
						return storage.ErrConflict
					}
					refs[ref.ID] = ref
					// A ref shared with an unexpired Execution must be copied.
					live[ref.ID] = live[ref.ID] || !gone
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		output, err := artifacts.Open(filepath.Join(stage, "artifacts"))
		if err != nil {
			return err
		}
		defer output.Close()
		m := manifest{Version: 1, Schema: db.SchemaVersion(), Build: build, CreatedAt: retention.now()}
		// Sorting makes manifests reproducible apart from their creation time.
		for _, ref := range sortedRefs(refs) {
			if !live[ref.ID] {
				m.Omitted = append(m.Omitted, ref)
				continue
			}
			r, err := files.Open(ctx, ref)
			if err != nil {
				return err
			}
			h := sha256.New()
			err = output.Import(ctx, ref, io.TeeReader(r, h))
			readCloseErr := r.Close()
			if err != nil {
				return err
			}
			if readCloseErr != nil {
				return readCloseErr
			}
			m.Artifacts = append(m.Artifacts, entry{ref, hex.EncodeToString(h.Sum(nil))})
		}
		if backend != DefaultBackend {
			m.Backend = backend
		}
		m.DatabaseSHA256, err = digestFile(ctx, database)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		return writeDurable(filepath.Join(stage, "manifest.json"), append(data, '\n'))
	})
	return err
}

// Backends finds a storage backend by the name a bundle records
// (storage.LookupBackend).
type Backends func(name string) (storage.Backend, error)

// Record writes into a restored state directory, before it is published,
// whatever its caller needs to open it with backend (the composition
// root's storage choice). The directory is published with it or not at
// all. nil records nothing.
type Record func(dir, backend string) error

// ArtifactTarget is the artifact store a restore imports into: Backend
// opened at Location, or, when Location is empty, at the restored
// directory's artifacts (the file store's default place).
type ArtifactTarget struct {
	Backend  storage.ArtifactBackend
	Location string
}

// Restore verifies all bundle bytes before publishing a new isolated state
// directory. The bundle's backend restores its snapshot to conduit.db in
// that directory and fences old lease authority, and its artifacts are
// imported into the store target opens. That store must hold nothing yet,
// as the directory must not exist, so a restore never mixes a bundle's
// content into a live store; when a restore into a location outside the
// directory fails before the directory is published, what it imported
// there is deleted again. Each
// is checked against its reference's digest, or, for a reference recorded
// before references carried one, the digest the manifest vouches for. No
// daemon may own destination; an existing path is rejected, never
// overwritten.
func Restore(ctx context.Context, bundle, destination string, now time.Time, expired Expired, backends Backends, target ArtifactTarget, record Record) (err error) {
	if now.IsZero() || target.Backend == nil {
		return storage.ErrInvalid
	}
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return err
	}
	backend, err := backends(m.backend())
	if err != nil {
		return err
	}
	database := filepath.Join(bundle, snapshotName)
	hash, err := digestFile(ctx, database)
	if err != nil {
		return err
	}
	if hash != m.DatabaseSHA256 {
		return fmt.Errorf("backup: database digest mismatch")
	}
	// Read-only: verifying or restoring a bundle never changes it, so a
	// bundle taken by an older binary stays one that binary can restore.
	input, err := artifacts.OpenReadOnly(filepath.Join(bundle, "artifacts"))
	if err != nil {
		return err
	}
	defer input.Close()
	// A store outside the directory is opened and checked here, and emptied
	// again when the restore fails before the directory is published; one
	// inside it goes with the staged directory. Once the directory is
	// published its storage.json names the store, so the content stays even
	// if a later step (syncing the parent) fails.
	var external storage.ArtifactStore
	var imported []domain.ArtifactRef
	var published bool
	if target.Location != "" {
		if external, err = openEmpty(ctx, target.Backend, target.Location); err != nil {
			return err
		}
		defer external.Close()
		defer func() {
			if err == nil || published {
				return
			}
			for _, ref := range imported {
				_ = external.Delete(context.WithoutCancel(ctx), ref)
			}
		}()
	}
	published, err = publish(ctx, destination, func(stage string) error {
		output := external
		if output == nil {
			staged, err := openEmpty(ctx, target.Backend, filepath.Join(stage, "artifacts"))
			if err != nil {
				return err
			}
			defer staged.Close()
			output = staged
		}
		seen := map[string]bool{}
		for _, item := range m.Artifacts {
			if seen[item.Ref.ID] {
				return fmt.Errorf("backup: duplicate artifact")
			}
			seen[item.Ref.ID] = true
			// A ref recorded with a digest must name the bytes the
			// manifest vouches for, and is the check: reading and Import
			// both verify the bytes against it. One from before digests
			// is checked against the manifest's.
			if item.Ref.SHA256 != "" && item.Ref.SHA256 != item.SHA256 {
				return fmt.Errorf("backup: artifact digest mismatch")
			}
			r, err := input.Open(ctx, item.Ref)
			if err != nil {
				return err
			}
			h := sha256.New()
			err = output.Import(ctx, item.Ref, io.TeeReader(r, h))
			closeErr := r.Close()
			if err == nil {
				imported = append(imported, item.Ref)
			}
			if errors.Is(err, storage.ErrCorrupt) {
				return fmt.Errorf("backup: artifact digest mismatch: %w", err)
			}
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if item.Ref.SHA256 == "" && hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
				return fmt.Errorf("backup: artifact digest mismatch")
			}
		}
		if err := backend.Restore(ctx, database, filepath.Join(stage, snapshotName), now); err != nil {
			return err
		}
		if record != nil {
			if err := record(stage, m.backend()); err != nil {
				return err
			}
		}
		restored, err := backend.Open(ctx, filepath.Join(stage, snapshotName))
		if err != nil {
			return err
		}
		defer restored.Close()
		omitted := map[string]domain.ArtifactRef{}
		for _, ref := range m.Omitted {
			if seen[ref.ID] {
				return fmt.Errorf("backup: duplicate artifact")
			}
			seen[ref.ID] = true
			omitted[ref.ID] = ref
		}
		return restored.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			xs, err := tx.ListExecutions("")
			if err != nil {
				return err
			}
			captured := map[string]domain.ArtifactRef{}
			for _, item := range m.Artifacts {
				captured[item.Ref.ID] = item.Ref
			}
			for _, x := range xs {
				gone, err := expired.of(tx, x.ID)
				if err != nil {
					return err
				}
				for _, ref := range x.AllArtifacts() {
					if captured[ref.ID] == ref {
						continue
					}
					if gone && omitted[ref.ID] == ref {
						continue
					}
					return fmt.Errorf("backup: referenced artifact omitted or changed")
				}
			}
			return nil
		})
	})
	return err
}

func (m manifest) backend() string {
	if m.Backend == "" {
		return DefaultBackend
	}
	return m.Backend
}

func readManifest(path string) (manifest, error) {
	var m manifest
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return m, fmt.Errorf("backup: invalid manifest size or type")
	}
	d := json.NewDecoder(io.LimitReader(f, 16<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return m, fmt.Errorf("backup: trailing manifest data")
	}
	if m.Version != 1 || m.Schema != 1 || m.CreatedAt.IsZero() || len(m.DatabaseSHA256) != 64 {
		return m, fmt.Errorf("backup: unsupported or incomplete manifest")
	}
	return m, nil
}

func publish(ctx context.Context, destination string, create func(string) error) (published bool, err error) {
	destination, err = filepath.Abs(destination)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("backup: destination already exists")
	}
	parent := filepath.Dir(destination)
	stage, err := os.MkdirTemp(parent, ".conduit-snapshot-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	if err := create(stage); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := syncDirectory(stage); err != nil {
		return false, err
	}
	if err := os.Rename(stage, destination); err != nil {
		return false, err
	}
	return true, syncParent(parent)
}

func sortedRefs(refs map[string]domain.ArtifactRef) []domain.ArtifactRef {
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]domain.ArtifactRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, refs[id])
	}
	return out
}

func digestFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// syncParent syncs the directory a bundle or state directory was published
// into; a test seam for a failure after the rename.
var syncParent = syncDirectory

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Info summarizes a bundle's manifest.
type Info struct {
	// Backend names the storage backend the snapshot is for.
	Backend   string
	Schema    uint32
	Build     string
	CreatedAt time.Time
	Artifacts int
	// Omitted counts references whose expired content the bundle leaves out.
	Omitted int
	// DatabaseSHA256 is the snapshot's digest, which names the bundle's
	// coordination state: an in-place restore records it, so a
	// restore-then-serve entry point restores a bundle once.
	DatabaseSHA256 string
}

// Inspect reads and checks a bundle's manifest. It does not verify the
// bundle's bytes; Restore does.
func Inspect(bundle string) (Info, error) {
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return Info{}, err
	}
	return Info{Backend: m.backend(), Schema: m.Schema, Build: m.Build, CreatedAt: m.CreatedAt, Artifacts: len(m.Artifacts), Omitted: len(m.Omitted), DatabaseSHA256: m.DatabaseSHA256}, nil
}

// openEmpty opens the store a restore imports into and refuses one that
// already holds content.
func openEmpty(ctx context.Context, b storage.ArtifactBackend, location string) (storage.ArtifactStore, error) {
	s, err := b.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	held, err := s.List(ctx, "", 1)
	if err == nil && len(held) > 0 {
		err = fmt.Errorf("backup: the artifact store at %s already holds content: restore into an empty one", location)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
