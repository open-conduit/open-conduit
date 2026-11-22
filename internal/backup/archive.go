package backup

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"slices"

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

// retiredEvent identifies a tombstone. The ID is hashed so that the list
// costs little however much history has been retired.
type retiredEvent struct {
	sequence, id uint64
}

func retiredID(id domain.ID) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return h.Sum64()
}

// retiredInSnapshot lists the snapshot's tombstones in sequence order.
func retiredInSnapshot(tx storage.Tx) ([]retiredEvent, error) {
	var out []retiredEvent
	var after domain.ID
	for {
		page, err := tx.Tombstones(after, archivePage)
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			out = append(out, retiredEvent{a.Sequence, retiredID(a.Event.ID)})
		}
		if len(page) < archivePage {
			break
		}
		after = page[len(page)-1].Event.ID
	}
	slices.SortFunc(out, func(a, b retiredEvent) int { return cmp.Compare(a.sequence, b.sequence) })
	return out, nil
}

// retiredCover checks, over records in sequence order, that each tombstone's
// record is among them.
type retiredCover struct {
	retired []retiredEvent
}

func (c *retiredCover) see(a domain.AcceptedEvent) error {
	if len(c.retired) > 0 && c.retired[0].sequence < a.Sequence {
		return c.missing()
	}
	if len(c.retired) > 0 && c.retired[0].sequence == a.Sequence {
		if c.retired[0].id != retiredID(a.Event.ID) {
			return fmt.Errorf("backup: the event archive holds a different Event at sequence %d than the database retired: %w", a.Sequence, storage.ErrConflict)
		}
		c.retired = c.retired[1:]
	}
	return nil
}

func (c *retiredCover) done() error {
	if len(c.retired) > 0 {
		return c.missing()
	}
	return nil
}

func (c *retiredCover) missing() error {
	return fmt.Errorf("backup: %d retired Events are not in the event archive, the first at sequence %d: their payloads would be lost: %w", len(c.retired), c.retired[0].sequence, storage.ErrInvalid)
}

// writeBundleArchive writes events' records through the snapshot's latest
// sequence to path, and fails unless they cover every tombstone. With no
// records it writes nothing and returns nil, so the bundle stays one an
// older binary reads.
func writeBundleArchive(ctx context.Context, path string, events storage.EventArchive, through uint64, retired []retiredEvent) (_ *archiveManifest, err error) {
	cover := retiredCover{retired: retired}
	if events == nil {
		return nil, cover.done()
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
// and the snapshot's tombstones and imports it at location. Without an
// archive in the bundle, the snapshot must have no tombstones.
func restoreBundleArchive(ctx context.Context, path string, m *archiveManifest, backend storage.EventArchiveBackend, location string, through uint64, retired []retiredEvent) (err error) {
	cover := retiredCover{retired: retired}
	if m == nil {
		return cover.done()
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
