package backup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// A bundle holds the event archive as one file of whole records, one JSON
// object a line in sequence order. It is a format of the bundle, like its
// artifact store, so it doesn't depend on how a backend lays out an archive
// or what the bundle's filesystem supports, and one digest in the signed
// manifest covers it.
//
// It stops at the snapshot's latest sequence. The daemon goes on archiving
// while a backup runs, and a restored daemon issues sequences after the
// restored log's: a later record left in its archive would then share a
// sequence with a different new Event.

const (
	// ArchiveFile is the archive's file in a bundle.
	ArchiveFile = "events.ndjson"
	// ArchiveDir is the event archive's directory in a state directory.
	ArchiveDir = "archive"
	// archivePage is how many records one archive read or write handles.
	archivePage = 500
)

// archiveManifest is the manifest's description of ArchiveFile.
type archiveManifest struct {
	SHA256  string
	Records uint64
	// Through is the snapshot's latest sequence, past which no record goes.
	Through uint64
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
func (c *retiredCover) next() (*domain.AcceptedEvent, error) {
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
	return &c.page[0], nil
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

// writeBundleArchive writes events' records through the latest sequence of
// the snapshot tx reads to path, and fails unless they cover every
// tombstone. With no records it writes nothing and returns nil, so the
// bundle stays one an older binary reads.
func writeBundleArchive(ctx context.Context, path string, events storage.EventArchive, tx storage.Tx) (_ *archiveManifest, err error) {
	cover := retiredCover{tx: tx}
	if events == nil {
		return nil, cover.done()
	}
	through, err := tx.LatestSequence()
	if err != nil {
		return nil, err
	}
	// A snapshot's read can run again after a transient failure, so an
	// earlier attempt's file may be here. It says nothing about this one.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	h := sha256.New()
	buffered := bufio.NewWriter(io.MultiWriter(f, h))
	encoder := json.NewEncoder(buffered)
	encoder.SetEscapeHTML(false)
	m := &archiveManifest{Through: through}
	var after uint64
reading:
	for {
		page, err := events.Events(ctx, after, archivePage)
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			if a.Sequence > through {
				break reading
			}
			if err := cover.see(a); err != nil {
				return nil, err
			}
			if err := encoder.Encode(a); err != nil {
				return nil, err
			}
			m.Records++
			after = a.Sequence
		}
		if len(page) < archivePage {
			break
		}
	}
	if err := cover.done(); err != nil {
		return nil, err
	}
	if err := buffered.Flush(); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	closing := f
	f = nil
	if err := closing.Close(); err != nil {
		return nil, err
	}
	if m.Records == 0 {
		return nil, os.Remove(path)
	}
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	return m, nil
}

// restoreBundleArchive checks the private copy at path against its manifest
// and the tombstones of the snapshot tx reads, and imports it at location.
// Without an archive in the bundle, the snapshot must have no tombstones.
func restoreBundleArchive(ctx context.Context, path string, m *archiveManifest, backend storage.EventArchiveBackend, location string, tx storage.Tx) (err error) {
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
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := backend.Open(ctx, location)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	decoder := json.NewDecoder(bufio.NewReader(f))
	batch := make([]domain.AcceptedEvent, 0, archivePage)
	var previous, records uint64
	for {
		var a domain.AcceptedEvent
		err := decoder.Decode(&a)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
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
