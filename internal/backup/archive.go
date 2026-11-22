package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// A bundle names the event archive's records rather than holding them. They
// live in segments: immutable files of whole records, one JSON object a line
// in sequence order, each stored once beside the bundles under a name made
// from its digest (segments.go). The signed manifest lists the digests a
// bundle needs, so it vouches for every record, and kept bundles share what
// they have in common instead of each holding all of history.
//
// A bundle's records stop at the snapshot's latest sequence. The daemon goes
// on archiving while a backup runs, and a restored daemon issues sequences
// after the restored log's: a later record left in its archive would then
// share a sequence with a different new Event.

const (
	// ArchiveFile is the file of records in a segment.
	ArchiveFile = "events.ndjson"
	// ArchiveDir is the event archive's directory in a state directory.
	ArchiveDir = "archive"
	// archivePage is how many records one archive read or write handles.
	archivePage = 500
)

// archiveManifest is the manifest's description of a bundle's records.
type archiveManifest struct {
	Records uint64
	// Through is the snapshot's latest sequence, past which no record goes.
	Through uint64
	// Segments together hold the records, each of them in exactly one.
	Segments []segmentEntry
}

// retiredCover walks a snapshot's tombstones beside records in sequence
// order, checking each tombstone's record is among them. It holds one page,
// so its memory doesn't follow how much history has been retired.
type retiredCover struct {
	tx    storage.Tx
	page  []domain.AcceptedEvent
	after uint64
	ended bool
}

// next is the first tombstone not yet matched, nil when none is left.
//
// A tombstone a replica restore recorded as lost is passed over: no archive
// can hold its record any more.
func (c *retiredCover) next() (*domain.AcceptedEvent, error) {
	for {
		if len(c.page) == 0 && !c.ended {
			page, err := c.tx.Tombstones(c.after, archivePage)
			if err != nil {
				return nil, err
			}
			c.page, c.ended = page, len(page) < archivePage
			if len(page) > 0 {
				c.after = page[len(page)-1].Sequence
			}
		}
		if len(c.page) == 0 {
			return nil, nil
		}
		lost, err := eventLost(c.tx, string(c.page[0].Event.ID))
		if err != nil {
			return nil, err
		}
		if !lost {
			return &c.page[0], nil
		}
		c.page = c.page[1:]
	}
}

func (c *retiredCover) see(a domain.AcceptedEvent) error {
	head, err := c.next()
	if err != nil || head == nil {
		return err
	}
	switch {
	case head.Sequence < a.Sequence:
		return missingRecord(head)
	case head.Sequence == a.Sequence:
		if head.Event.ID != a.Event.ID {
			return fmt.Errorf("backup: the event archive holds a different Event at sequence %d than the database retired: %w", a.Sequence, storage.ErrConflict)
		}
		c.page = c.page[1:]
	}
	return nil
}

func (c *retiredCover) done() error {
	head, err := c.next()
	if err != nil || head == nil {
		return err
	}
	return missingRecord(head)
}

func missingRecord(a *domain.AcceptedEvent) error {
	return fmt.Errorf("backup: retired Event %s (sequence %d) is not in the event archive, so its payload would be lost: %w", a.Event.ID, a.Sequence, storage.ErrInvalid)
}

// writeBundleArchive makes sure segments in parent hold events' records
// through the latest sequence of the snapshot tx reads, writing only those
// the newest bundle's segments don't, and fails unless the records cover
// every tombstone. With no records it returns nil, so the bundle stays one an
// older binary reads. A snapshot's read can run again after a transient
// failure; a segment is named by its content, so a second run lands on the
// same files.
func writeBundleArchive(ctx context.Context, parent string, events storage.EventArchive, tx storage.Tx) (_ *archiveManifest, err error) {
	cover := retiredCover{tx: tx}
	if events == nil {
		return nil, cover.done()
	}
	through, err := tx.LatestSequence()
	if err != nil {
		return nil, err
	}
	kept, err := reusableSegments(ctx, parent, events, through)
	if err != nil {
		return nil, err
	}
	held, err := openSegments(parent, kept)
	if err != nil {
		return nil, err
	}
	defer held.close()
	var fresh *segmentWriter
	defer func() {
		if fresh != nil {
			fresh.abort()
		}
	}()
	m := &archiveManifest{Through: through}
	err = walkArchive(ctx, events, through, func(a domain.AcceptedEvent) error {
		if err := cover.see(a); err != nil {
			return err
		}
		m.Records++
		switch in, err := held.take(a.Sequence); {
		case err != nil:
			return err
		case in:
			return nil
		}
		if fresh == nil {
			if fresh, err = newSegmentWriter(parent); err != nil {
				return err
			}
		}
		line, err := encodeRecord(a)
		if err != nil {
			return err
		}
		return fresh.add(line)
	})
	if err != nil {
		return nil, err
	}
	if err := cover.done(); err != nil {
		return nil, err
	}
	if m.Records == 0 {
		return nil, nil
	}
	m.Segments = kept
	if fresh != nil {
		written, err := fresh.finish(ctx)
		fresh = nil
		if err != nil {
			return nil, err
		}
		m.Segments = append(m.Segments, written)
	}
	if m.Segments, err = compactSegments(ctx, parent, m.Segments); err != nil {
		return nil, err
	}
	return m, nil
}

// walkArchive calls see with events' records through a sequence, in order.
func walkArchive(ctx context.Context, events storage.EventArchive, through uint64, see func(domain.AcceptedEvent) error) error {
	var after uint64
	for {
		page, err := events.Events(ctx, after, archivePage)
		if err != nil {
			return err
		}
		for _, a := range page {
			if a.Sequence > through {
				return nil
			}
			if err := see(a); err != nil {
				return err
			}
			after = a.Sequence
		}
		if len(page) < archivePage {
			return nil
		}
	}
}

// copyBundleSegments reads each segment the manifest names, from beside the
// bundle, once into a private file, and checks its digest: the bytes checked
// are the bytes restored. A segment that isn't there fails the restore rather
// than restoring part of history.
func copyBundleSegments(ctx context.Context, bundle, private string, m *archiveManifest) ([]string, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	parent, err := filepath.Abs(filepath.Dir(bundle))
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(m.Segments))
	for _, s := range m.Segments {
		path := filepath.Join(private, s.SHA256+".ndjson")
		hash, err := copyDigest(ctx, filepath.Join(parent, s.name(), ArchiveFile), path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("backup: event archive segment %s is missing: it is stored beside the bundle: %w", s.name(), err)
		}
		if err != nil {
			return nil, err
		}
		if hash != s.SHA256 {
			return nil, fmt.Errorf("backup: event archive digest mismatch in segment %s", s.name())
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// restoreBundleArchive checks the private segment copies at paths against
// their manifest and the tombstones of the snapshot tx reads, and imports
// them at location. Without an archive in the bundle, the snapshot must have
// no tombstones.
func restoreBundleArchive(ctx context.Context, paths []string, m *archiveManifest, backend storage.EventArchiveBackend, location string, tx storage.Tx) (err error) {
	cover := retiredCover{tx: tx}
	if m == nil {
		return cover.done()
	}
	through, err := tx.LatestSequence()
	if err != nil {
		return err
	}
	if m.Through != through {
		return fmt.Errorf("backup: the event archive was taken through sequence %d but the database is at %d", m.Through, through)
	}
	in, err := openSegmentPaths(paths)
	if err != nil {
		return err
	}
	defer in.close()
	out, err := backend.Open(ctx, location)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	batch := make([]domain.AcceptedEvent, 0, archivePage)
	var previous, records uint64
	for {
		line, err := in.next()
		if err != nil {
			return fmt.Errorf("backup: event archive: %w", err)
		}
		if line == nil {
			break
		}
		var a domain.AcceptedEvent
		if err := json.Unmarshal(line, &a); err != nil {
			return fmt.Errorf("backup: event archive: %w", err)
		}
		if a.Sequence <= previous || a.Sequence > through {
			return fmt.Errorf("backup: event archive record at sequence %d is out of order or past the snapshot", a.Sequence)
		}
		previous = a.Sequence
		if err := cover.see(a); err != nil {
			return err
		}
		records++
		if batch = append(batch, a); len(batch) == archivePage {
			if err := out.PutEvents(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if records != m.Records {
		return fmt.Errorf("backup: event archive holds %d records, the manifest says %d", records, m.Records)
	}
	if err := cover.done(); err != nil {
		return err
	}
	if len(batch) > 0 {
		return out.PutEvents(ctx, batch)
	}
	return nil
}
