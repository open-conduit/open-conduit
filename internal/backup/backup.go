// Package backup composes a coordination backend's consistent snapshot with
// the artifacts it references, held from retention while they are copied.
// It works over the storage lifecycle and artifact contracts
// (storage.Backend, storage.Database, storage.ArtifactStore), never a
// particular backend; a bundle's own artifacts are a file store, as part
// of the bundle format. Backups contain private runtime state and must
// remain owner-only.
package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	// Archive names the event archive segments the bundle needs. A bundle
	// with one is refused by a binary that can't restore it, as it should
	// be; one without stays readable by older binaries.
	Archive *archiveManifest `json:",omitempty"`
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
// then publishes nothing. With a key, the manifest is signed (sign.go); nil
// publishes an unsigned bundle.
func Create(ctx context.Context, destination string, key ed25519.PrivateKey, backend string, db storage.Database, files storage.ArtifactStore, build string, retention Retention) error {
	return CreateWithArchive(ctx, destination, key, backend, db, files, nil, build, retention)
}

// CreateWithArchive is Create for a daemon with an event archive: the bundle
// also names the archive's records up to the snapshot's latest sequence, in
// segments stored beside it (archive.go). Create refuses a snapshot with retired Events, whose payloads
// only the archive holds.
func CreateWithArchive(ctx context.Context, destination string, key ed25519.PrivateKey, backend string, db storage.Database, files storage.ArtifactStore, events storage.EventArchive, build string, retention Retention) error {
	if backend == "" || files == nil {
		return storage.ErrInvalid
	}
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
		stored, err := db.Stored(ctx)
		if err != nil {
			return err
		}
		return compose(ctx, stage, key, backend, db.Snapshot, db.SchemaVersion(), files, events, retention.Expired, retention.now(), func(storage.Tx) (string, error) { return recordsBuild(build, stored.Records), nil })
	})
	return err
}

// CreatePrior publishes a bundle of state an older binary left, at the
// versions it holds and without writing to it: unlike Create it commits no
// retention hold, so the caller must hold the state directory's lock, which
// keeps the retention sweep from running. Its manifest's Build carries the
// label Inspect reads as PreUpgrade. fromBuild reads, from the snapshot, the
// build that last ran on the state ("" when it recorded none).
func CreatePrior(ctx context.Context, destination string, key ed25519.PrivateKey, backend string, prior storage.Prior, files storage.ArtifactStore, build string, expired Expired, now time.Time, fromBuild func(storage.Tx) (string, error)) error {
	if backend == "" || files == nil || prior == nil {
		return storage.ErrInvalid
	}
	stored := prior.Stored()
	_, err := publish(ctx, destination, func(stage string) error {
		return compose(ctx, stage, key, backend, prior.Snapshot, stored.Schema, files, nil, expired, now.UTC(), func(tx storage.Tx) (string, error) {
			from, err := fromBuild(tx)
			if err != nil {
				return "", err
			}
			return preUpgradeBuild(build, stored, from), nil
		})
	})
	return err
}

// compose writes a bundle into stage from a snapshot. buildOf names its build
// from the snapshot's own state.
func compose(ctx context.Context, stage string, key ed25519.PrivateKey, backend string, snapshot func(context.Context, string, func(storage.Tx) error) error, schema uint32, files storage.ArtifactStore, events storage.EventArchive, expired Expired, now time.Time, buildOf func(storage.Tx) (string, error)) error {
	database := filepath.Join(stage, snapshotName)
	var refs map[string]domain.ArtifactRef
	var live map[string]bool
	var build string
	var archived *archiveManifest
	// References are read from the snapshot's own state, not the live
	// database, so the bundle copies exactly what its snapshot names.
	err := snapshot(ctx, database, func(tx storage.Tx) error {
		refs, live = map[string]domain.ArtifactRef{}, map[string]bool{}
		var err error
		if build, err = buildOf(tx); err != nil {
			return err
		}
		// The snapshot is taken by now. An Event is archived before it is
		// retired, so the archive holds what its tombstones stand for.
		if archived, err = writeBundleArchive(ctx, filepath.Dir(stage), events, tx); err != nil {
			return err
		}
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
	output, err := artifacts.OpenBundle(filepath.Join(stage, "artifacts"))
	if err != nil {
		return err
	}
	defer output.Close()
	m := manifest{Version: 1, Schema: schema, Build: build, CreatedAt: now, Archive: archived}
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
	data = append(data, '\n')
	if err := writeDurable(filepath.Join(stage, "manifest.json"), data); err != nil {
		return err
	}
	if key != nil {
		return writeSignature(stage, data, key)
	}
	return nil
}

// A pre-upgrade bundle's label lives in the manifest's Build. Released
// binaries decode manifests strictly and refuse an unknown field, so a new
// field would stop the older binary restoring the one bundle made for it.
// In Build it is signed with the manifest, and the older binary's backup
// verify prints it as it stands.
var preUpgradeLabel = regexp.MustCompile(`^(.*) pre-upgrade records=([0-9]+) schema=([0-9]+) from=(.*)$`)

// UnknownBuild is the origin of state no build recorded itself on.
const UnknownBuild = "unknown"

func preUpgradeBuild(build string, stored storage.StoredVersion, from string) string {
	if from == "" {
		from = UnknownBuild
	}
	return fmt.Sprintf("%s pre-upgrade records=%d schema=%d from=%s", build, stored.Records, stored.Schema, from)
}

// recordsLabel is how an ordinary bundle's Build names its record format,
// for the same reason a pre-upgrade bundle's label lives there: an older
// binary refuses a manifest with a field it doesn't know, and would then
// refuse every bundle, not only those it can't read.
var recordsLabel = regexp.MustCompile(`^(.*) records=([0-9]+)$`)

func recordsBuild(build string, records uint32) string {
	return fmt.Sprintf("%s records=%d", build, records)
}

// parseRecords splits a Build into the build and the record format it names
// (0 for a bundle from before bundles recorded one).
func parseRecords(build string) (string, uint32) {
	g := recordsLabel.FindStringSubmatch(build)
	if g == nil {
		return build, 0
	}
	records, err := strconv.ParseUint(g[2], 10, 32)
	if err != nil {
		return build, 0
	}
	return g[1], uint32(records)
}

// PreUpgrade is what a bundle taken before an upgrade says it holds.
type PreUpgrade struct {
	// Records and Schema are the versions the state was at.
	Records, Schema uint32
	// FromBuild is the build that last ran on it (UnknownBuild when none
	// recorded itself).
	FromBuild string
}

func parsePreUpgrade(build string) (string, *PreUpgrade) {
	g := preUpgradeLabel.FindStringSubmatch(build)
	if g == nil {
		return build, nil
	}
	records, err := strconv.ParseUint(g[2], 10, 32)
	if err != nil {
		return build, nil
	}
	schema, err := strconv.ParseUint(g[3], 10, 32)
	if err != nil {
		return build, nil
	}
	return g[1], &PreUpgrade{Records: uint32(records), Schema: uint32(schema), FromBuild: g[4]}
}

// Backends finds a storage backend by the name a bundle records
// (storage.LookupBackend).
type Backends func(name string) (storage.Backend, error)

// Record writes into a restored state directory, before it is published,
// whatever its caller needs to open it with backend (the composition
// root's storage choice). The directory is published with it or not at
// all. nil records nothing.
type Record func(dir, backend string) error

// Target is where a restore puts a bundle's state. Database is the location
// the bundle's backend restores to, which must hold no state; empty is
// conduit.db in the restored directory. Artifacts is the store to import
// into, opened at ArtifactLocation, or, when that is empty, at the restored
// directory's artifacts (the file store's default place).
type Target struct {
	Database         string
	Artifacts        storage.ArtifactBackend
	ArtifactLocation string
	// EventArchive is the backend a bundle's event archive is restored
	// into, at ArchiveDir in the restored directory. A bundle holding an
	// archive is refused without one.
	EventArchive storage.EventArchiveBackend
}

// Restore verifies all bundle bytes before publishing a new isolated state
// directory. The bundle's backend restores its snapshot to target.Database,
// or conduit.db in that directory, and fences old lease authority, and its
// artifacts are imported into the store target opens. The snapshot's
// references are checked before the database is written, so a bundle that
// fails leaves a named location untouched. A failure after the write (the
// caller's record, publishing the directory) can't take a named location
// back, since a backend offers no way to delete state: the error says so. That store must hold nothing yet,
// as the directory must not exist, so a restore never mixes a bundle's
// content into a live store; when a restore into a location outside the
// directory fails before the directory is published, what it imported
// there is deleted again. Each
// is checked against its reference's digest, or, for a reference recorded
// before references carried one, the digest the manifest vouches for. No
// daemon may own destination; an existing path is rejected, never
// overwritten. The bundle must pass trust first, checked on the same manifest
// bytes the restore then acts on.
func Restore(ctx context.Context, bundle, destination string, trust Trust, now time.Time, expired Expired, backends Backends, target Target, record Record) (err error) {
	if now.IsZero() || target.Artifacts == nil {
		return storage.ErrInvalid
	}
	m, raw, err := readManifestBytes(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return err
	}
	if _, err := trust.check(bundle, raw); err != nil {
		return err
	}
	backend, err := backends(m.backend())
	if err != nil {
		return err
	}
	if err := refuseNewer(m, backend); err != nil {
		return err
	}
	if target.Database != "" {
		switch present, err := backend.Present(ctx, target.Database); {
		case err != nil:
			return err
		case present:
			return fmt.Errorf("backup: %s already holds state: restore into an empty location (if an earlier restore to it stopped, clear what it left): %w", target.Database, os.ErrExist)
		}
	}
	// The bundle may be somewhere others can write, so its database is read
	// once, into a private copy hashed as it is written, and only that copy
	// is restored: the bytes checked are the bytes restored. Nothing beside
	// the bundle's database (a planted -wal) comes with it. The directory is
	// named like a stage, so one a crash leaves is cleared with them.
	parent, err := filepath.Abs(filepath.Dir(destination))
	if err != nil {
		return err
	}
	private, err := os.MkdirTemp(parent, ".conduit-snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	database := filepath.Join(private, snapshotName)
	hash, err := copyDigest(ctx, filepath.Join(bundle, snapshotName), database)
	if err != nil {
		return err
	}
	if hash != m.DatabaseSHA256 {
		return fmt.Errorf("backup: database digest mismatch")
	}
	// The archive's segments are read once into private copies too, for the
	// same reason.
	var archiveCopies []string
	if m.Archive != nil {
		if target.EventArchive == nil {
			return fmt.Errorf("backup: the bundle holds an event archive this restore has nowhere to put: %w", storage.ErrInvalid)
		}
		if archiveCopies, err = copyBundleSegments(ctx, bundle, private, m.Archive); err != nil {
			return err
		}
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
	var published, wrote bool
	if target.ArtifactLocation != "" {
		if external, err = openEmpty(ctx, target.Artifacts, target.ArtifactLocation); err != nil {
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
			staged, err := openEmpty(ctx, target.Artifacts, filepath.Join(stage, "artifacts"))
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
		omitted := map[string]domain.ArtifactRef{}
		for _, ref := range m.Omitted {
			if seen[ref.ID] {
				return fmt.Errorf("backup: duplicate artifact")
			}
			seen[ref.ID] = true
			omitted[ref.ID] = ref
		}
		captured := map[string]domain.ArtifactRef{}
		for _, item := range m.Artifacts {
			captured[item.Ref.ID] = item.Ref
		}
		err := backend.ReadSnapshot(ctx, database, now, func(tx storage.Tx) error {
			// Before a named database is written, so a bundle whose archive
			// doesn't stand behind its tombstones leaves that location alone.
			if err := restoreBundleArchive(ctx, archiveCopies, m.Archive, target.EventArchive, filepath.Join(stage, ArchiveDir), tx); err != nil {
				return err
			}
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
		if err != nil {
			return err
		}
		location := target.Database
		if location == "" {
			location = filepath.Join(stage, snapshotName)
		}
		if err := backend.Restore(ctx, database, location, now); err != nil {
			return err
		}
		wrote = target.Database != ""
		if record != nil {
			return record(stage, m.backend())
		}
		return nil
	})
	if err != nil && wrote && !published {
		return fmt.Errorf("backup: %s now holds the restored database but the restore failed after writing it; nothing uses it, so clear it before restoring there again: %w", target.Database, err)
	}
	return err
}

// records is the record format the manifest names, 0 when it names none.
func (m manifest) records() uint32 {
	if _, p := parsePreUpgrade(m.Build); p != nil {
		return p.Records
	}
	_, records := parseRecords(m.Build)
	return records
}

// refuseNewer refuses a bundle whose manifest names a record format this
// binary doesn't write, before any of it is copied. A bundle that names none
// is left to the backend's own check when it is opened.
func refuseNewer(m manifest, backend storage.Backend) error {
	writes := backend.Writes().Records
	if records := m.records(); records > writes {
		return fmt.Errorf("backup: %w (the bundle is at record format %d; this binary writes %d): restore it with the newer binary that made it", storage.ErrRecordsNewer, records, writes)
	}
	return nil
}

// CheckFormat refuses a bundle a newer binary wrote, from its manifest alone.
func CheckFormat(bundle string, backends Backends) error {
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return err
	}
	backend, err := backends(m.backend())
	if err != nil {
		return err
	}
	return refuseNewer(m, backend)
}

func (m manifest) backend() string {
	if m.Backend == "" {
		return DefaultBackend
	}
	return m.Backend
}

func readManifest(path string) (manifest, error) {
	m, _, err := readManifestBytes(path)
	return m, err
}

// readManifestBytes reads a manifest and the exact bytes it was parsed from,
// which are what a signature covers.
func readManifestBytes(path string) (manifest, []byte, error) {
	var m manifest
	f, err := os.Open(path)
	if err != nil {
		return m, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return m, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return m, nil, fmt.Errorf("backup: invalid manifest size or type")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		return m, nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, nil, err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return m, nil, fmt.Errorf("backup: trailing manifest data")
	}
	if m.Version != 1 || m.Schema != 1 || m.CreatedAt.IsZero() || len(m.DatabaseSHA256) != 64 {
		return m, nil, fmt.Errorf("backup: unsupported or incomplete manifest")
	}
	return m, raw, nil
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

// copyDigest copies source to an absent owner-only file and returns the
// digest of the bytes it wrote.
func copyDigest(ctx context.Context, source, destination string) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	h := sha256.New()
	w := io.MultiWriter(out, h)
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := in.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return "", err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if err := out.Close(); err != nil {
		return "", err
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
	Backend string
	Schema  uint32
	Build   string
	// Records is the record format the bundle holds, 0 for a bundle from
	// before bundles recorded one.
	Records   uint32
	CreatedAt time.Time
	Artifacts int
	// Omitted counts references whose expired content the bundle leaves out.
	Omitted int
	// DatabaseSHA256 is the snapshot's digest, which names the bundle's
	// coordination state: an in-place restore records it, so a
	// restore-then-serve entry point restores a bundle once.
	DatabaseSHA256 string
	// PreUpgrade is set on a bundle a daemon took before raising the stored
	// versions; Build is then the build that took it.
	PreUpgrade *PreUpgrade
}

// Inspect reads and checks a bundle's manifest. It does not verify the
// bundle's bytes; Restore does.
func Inspect(bundle string) (Info, error) {
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return Info{}, err
	}
	build, preUpgrade := parsePreUpgrade(m.Build)
	if preUpgrade == nil {
		build, _ = parseRecords(build)
	}
	return Info{PreUpgrade: preUpgrade, Records: m.records(), Backend: m.backend(), Schema: m.Schema, Build: build, CreatedAt: m.CreatedAt, Artifacts: len(m.Artifacts), Omitted: len(m.Omitted), DatabaseSHA256: m.DatabaseSHA256}, nil
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
