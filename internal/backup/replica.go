package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/storage"
)

// A replica is a copy of the coordination database some other system kept
// (a WAL shipper, say), so unlike a bundle nothing vouches that the content
// it references survived. Restoring one reports what is missing and records
// it as lost in the restored database. Without that record every later backup
// would refuse the state for the same missing content, on the host that has
// just lost its disk.

// lostNamespace holds what a replica restore found missing, by kind and ID.
const lostNamespace = "core.host.restore.lost"

func lostKey(kind, id string) string { return kind + "/" + id }

func isLost(tx storage.Tx, kind, id string) (bool, error) {
	_, err := tx.Get(lostNamespace, lostKey(kind, id))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ArtifactLost reports whether a replica restore recorded this artifact's
// content as lost.
func ArtifactLost(tx storage.Tx, id string) (bool, error) { return isLost(tx, "artifact", id) }

func eventLost(tx storage.Tx, id string) (bool, error) { return isLost(tx, "event", id) }

// ReplicaSource is what survived the host: the database replica and, when
// they were protected separately, a copy of its file artifact store and of
// its event archive directory. None of them is changed.
type ReplicaSource struct {
	Database, Artifacts, Archive string
}

// ReplicaReport is what a replica restore brought back and what it couldn't.
type ReplicaReport struct {
	Artifacts        int
	MissingArtifacts []string
	ArchivedEvents   int
	MissingEvents    []string
}

// RestoreReplica publishes a new state directory from a replica. The backend
// verifies and fences the database; each artifact and retired Event it
// references is then copied from the source, or reported and recorded as lost.
// With target.ArtifactLocation the restored directory keeps using that store,
// which already holds its content (an object store), and it is only checked.
func RestoreReplica(ctx context.Context, src ReplicaSource, destination string, now time.Time, expired Expired, name string, backends Backends, target Target, record Record) (ReplicaReport, error) {
	var report ReplicaReport
	if now.IsZero() || target.Artifacts == nil || target.EventArchive == nil || target.Database != "" {
		return report, storage.ErrInvalid
	}
	backend, err := backends(name)
	if err != nil {
		return report, err
	}
	restorer, ok := backend.(storage.ReplicaRestorer)
	if !ok {
		return report, fmt.Errorf("backup: the %s backend doesn't restore replicas: %w", name, storage.ErrInvalid)
	}
	_, err = publish(ctx, destination, func(stage string) error {
		report = ReplicaReport{}
		location := filepath.Join(stage, snapshotName)
		if err := restorer.RestoreReplica(ctx, src.Database, location, now); err != nil {
			return err
		}
		db, err := backend.Open(ctx, location)
		if err != nil {
			return err
		}
		err = bringBack(ctx, db, src, stage, now, expired, target, &report)
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
		if err != nil || record == nil {
			return err
		}
		return record(stage, name)
	})
	return report, err
}

func bringBack(ctx context.Context, db storage.Database, src ReplicaSource, stage string, now time.Time, expired Expired, target Target, report *ReplicaReport) error {
	refs := map[string]domain.ArtifactRef{}
	var retired []domain.AcceptedEvent
	if err := db.View(ctx, func(tx storage.Tx) error {
		refs, retired = map[string]domain.ArtifactRef{}, nil
		xs, err := tx.ListExecutions("")
		if err != nil {
			return err
		}
		for _, x := range xs {
			gone, err := expired.of(tx, x.ID)
			if err != nil {
				return err
			}
			if gone {
				continue
			}
			for _, ref := range x.AllArtifacts() {
				refs[ref.ID] = ref
			}
		}
		var after uint64
		for {
			page, err := tx.Tombstones(after, archivePage)
			if err != nil {
				return err
			}
			retired = append(retired, page...)
			if len(page) < archivePage {
				return nil
			}
			after = page[len(page)-1].Sequence
		}
	}); err != nil {
		return err
	}
	if err := bringBackArtifacts(ctx, refs, src.Artifacts, stage, target, report); err != nil {
		return err
	}
	if err := bringBackEvents(ctx, retired, src.Archive, stage, target.EventArchive, report); err != nil {
		return err
	}
	return db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		for _, id := range report.MissingArtifacts {
			if err := tx.Put(lostNamespace, lostKey("artifact", id), []byte("{}")); err != nil {
				return err
			}
		}
		for _, id := range report.MissingEvents {
			if err := tx.Put(lostNamespace, lostKey("event", id), []byte("{}")); err != nil {
				return err
			}
		}
		return nil
	})
}

func absent(err error) bool {
	return errors.Is(err, storage.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

func bringBackArtifacts(ctx context.Context, refs map[string]domain.ArtifactRef, from, stage string, target Target, report *ReplicaReport) error {
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	found := func(id string, ok bool) {
		if ok {
			report.Artifacts++
		} else {
			report.MissingArtifacts = append(report.MissingArtifacts, id)
		}
	}
	if target.ArtifactLocation != "" {
		store, err := target.Artifacts.Open(ctx, target.ArtifactLocation)
		if err != nil {
			return err
		}
		defer store.Close()
		for _, id := range ids {
			r, err := store.Open(ctx, refs[id])
			if err != nil && !absent(err) {
				// An outage isn't a loss: recording one would be permanent.
				return fmt.Errorf("backup: checking artifact %s: %w", id, err)
			}
			if err == nil {
				r.Close()
			}
			found(id, err == nil)
		}
		return nil
	}
	output, err := openEmpty(ctx, target.Artifacts, filepath.Join(stage, "artifacts"))
	if err != nil {
		return err
	}
	defer output.Close()
	if from == "" {
		for _, id := range ids {
			found(id, false)
		}
		return nil
	}
	input, err := artifacts.OpenReadOnly(from)
	if err != nil {
		return fmt.Errorf("backup: the artifact copy at %s: %w", from, err)
	}
	defer input.Close()
	for _, id := range ids {
		r, err := input.Open(ctx, refs[id])
		if absent(err) {
			found(id, false)
			continue
		}
		if err != nil {
			return fmt.Errorf("backup: reading artifact %s: %w", id, err)
		}
		err = output.Import(ctx, refs[id], r)
		r.Close()
		if err != nil && !errors.Is(err, storage.ErrCorrupt) {
			return fmt.Errorf("backup: restoring artifact %s: %w", id, err)
		}
		found(id, err == nil)
	}
	return nil
}

// bringBackEvents copies the record of each Event the replica retired, and
// nothing else: a later record in the copy would share its sequence with a
// new Event once the restored daemon issues it again. The copy is opened from
// a private duplicate, because opening an archive can write to it.
func bringBackEvents(ctx context.Context, retired []domain.AcceptedEvent, from, stage string, backend storage.EventArchiveBackend, report *ReplicaReport) (err error) {
	if len(retired) == 0 {
		return nil
	}
	missing := func(a domain.AcceptedEvent) {
		report.MissingEvents = append(report.MissingEvents, string(a.Event.ID))
	}
	if from == "" {
		for _, a := range retired {
			missing(a)
		}
		return nil
	}
	private, err := os.MkdirTemp(filepath.Dir(stage), ".conduit-snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	if err := copyTree(ctx, from, private); err != nil {
		return fmt.Errorf("backup: the event archive copy at %s: %w", from, err)
	}
	in, err := backend.Open(ctx, private)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := backend.Open(ctx, filepath.Join(stage, ArchiveDir))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	batch := make([]domain.AcceptedEvent, 0, archivePage)
	for _, tombstone := range retired {
		a, err := in.Event(ctx, tombstone.Event.ID)
		if errors.Is(err, storage.ErrNotFound) || err == nil && a.Sequence != tombstone.Sequence {
			missing(tombstone)
			continue
		}
		if err != nil {
			return err
		}
		report.ArchivedEvents++
		if batch = append(batch, a); len(batch) == archivePage {
			if err := out.PutEvents(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		return out.PutEvents(ctx, batch)
	}
	return nil
}

// copyTree copies the regular files under from into to, owner-only.
func copyTree(ctx context.Context, from, to string) error {
	return filepath.WalkDir(from, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if e.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !e.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errors.Join(err, out.Close())
	})
}
