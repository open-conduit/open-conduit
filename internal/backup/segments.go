package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// A segment is a directory beside the bundles, named by the digest of the one
// file it holds. It has no manifest, so a destination never lists it as a
// bundle, while storing and reading it work as for any bundle's files.
//
// A new bundle starts from the newest bundle's segments, which are disjoint
// by construction, keeps those whose every record still equals the archive's,
// and writes the rest to one new segment. Comparing rather than trusting
// matters twice over: the directory may be writable by others, and after a
// restore to an earlier point a kept segment can hold sequences the restored
// daemon has since issued to different Events. Segments follow what was newly
// archived, not sequence ranges, because an old Event can be retired late.

const segmentPrefix = "event-segment-"

var segmentName = regexp.MustCompile(`^event-segment-[0-9a-f]{64}$`)

// segmentEntry is one segment a manifest names.
type segmentEntry struct {
	SHA256  string
	Records uint64
}

func (s segmentEntry) name() string { return segmentPrefix + s.SHA256 }

// check refuses a description a restore must not act on. A digest becomes a
// path, and the manifest of an unsigned bundle is whatever someone wrote.
func (m *archiveManifest) check() error {
	if len(m.Segments) == 0 {
		return fmt.Errorf("backup: the event archive names no segments: %w", storage.ErrInvalid)
	}
	seen := make(map[string]bool, len(m.Segments))
	for _, s := range m.Segments {
		if !segmentName.MatchString(s.name()) || seen[s.SHA256] {
			return fmt.Errorf("backup: the event archive names an invalid segment %q: %w", s.SHA256, storage.ErrInvalid)
		}
		seen[s.SHA256] = true
	}
	return nil
}

// SegmentNames are the stored names of the segments a bundle needs.
func SegmentNames(bundle string) ([]string, error) {
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if m.Archive == nil {
		return nil, nil
	}
	if err := m.Archive.check(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m.Archive.Segments))
	for _, s := range m.Archive.Segments {
		names = append(names, s.name())
	}
	return names, nil
}

// HoldsSegment reports whether parent has an intact copy of a named segment.
func HoldsSegment(ctx context.Context, parent, name string) bool {
	digest, ok := strings.CutPrefix(name, segmentPrefix)
	if !ok || !segmentName.MatchString(name) {
		return false
	}
	got, err := digestFile(ctx, filepath.Join(parent, name, ArchiveFile))
	return err == nil && got == digest
}

// AdoptSegment publishes the segment file in dir as name in parent, once its
// bytes match the digest the name carries.
func AdoptSegment(ctx context.Context, parent, name, dir string) error {
	digest, ok := strings.CutPrefix(name, segmentPrefix)
	if !ok || !segmentName.MatchString(name) {
		return fmt.Errorf("backup: %q is not a segment name: %w", name, storage.ErrInvalid)
	}
	got, err := digestFile(ctx, filepath.Join(dir, ArchiveFile))
	if err != nil {
		return err
	}
	if got != digest {
		return fmt.Errorf("backup: event archive digest mismatch in segment %s", name)
	}
	return publishSegment(ctx, parent, name, dir)
}

// publishSegment moves dir to name in parent. The name is the content, so an
// intact copy already there is the same segment; a damaged one is replaced.
func publishSegment(ctx context.Context, parent, name, dir string) error {
	if HoldsSegment(ctx, parent, name) {
		return os.RemoveAll(dir)
	}
	target := filepath.Join(parent, name)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	if err := os.Rename(dir, target); err != nil {
		return err
	}
	return syncParent(parent)
}

// PruneSegments removes the segments in dir that no bundle there names. A
// bundle it can't read might need any of them, so it then removes nothing.
func PruneSegments(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	var segments []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if segmentName.MatchString(e.Name()) {
			segments = append(segments, e.Name())
			continue
		}
		bundle := filepath.Join(dir, e.Name())
		if _, err := os.Lstat(filepath.Join(bundle, "manifest.json")); errors.Is(err, os.ErrNotExist) {
			continue
		}
		names, err := SegmentNames(bundle)
		if err != nil {
			return nil, fmt.Errorf("backup: reading %s: %w", e.Name(), err)
		}
		for _, n := range names {
			needed[n] = true
		}
	}
	var removed []string
	for _, s := range segments {
		if needed[s] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, s)); err != nil {
			return removed, err
		}
		removed = append(removed, s)
	}
	return removed, nil
}

// newestSegments are the segments the most recent bundle in parent names.
// Anything unreadable is passed over: this only saves writing records again.
func newestSegments(parent string) []segmentEntry {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var newest *manifest
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || segmentName.MatchString(e.Name()) {
			continue
		}
		m, err := readManifest(filepath.Join(parent, e.Name(), "manifest.json"))
		if err != nil || m.Archive == nil || m.Archive.check() != nil {
			continue
		}
		if newest == nil || m.CreatedAt.After(newest.CreatedAt) {
			newest = &m
		}
	}
	if newest == nil {
		return nil
	}
	return newest.Archive.Segments
}

// reusableSegments are the newest bundle's segments whose digest holds and
// whose every record equals the archive's at or below through.
func reusableSegments(ctx context.Context, parent string, events storage.EventArchive, through uint64) ([]segmentEntry, error) {
	candidates := newestSegments(parent)
	if len(candidates) == 0 {
		return nil, nil
	}
	type candidate struct {
		entry   segmentEntry
		cursor  *segmentCursor
		records uint64
		bad     bool
	}
	var open []*candidate
	defer func() {
		for _, c := range open {
			c.cursor.close()
		}
	}()
	for _, s := range candidates {
		cursor, err := openSegment(filepath.Join(parent, s.name(), ArchiveFile))
		if err != nil {
			continue
		}
		c := &candidate{entry: s, cursor: cursor}
		open = append(open, c)
		if cursor.advance() != nil {
			c.bad = true
		}
	}
	err := walkArchive(ctx, events, through, func(a domain.AcceptedEvent) error {
		line, err := encodeRecord(a)
		if err != nil {
			return err
		}
		claimed := false
		for _, c := range open {
			if c.bad || c.cursor.line == nil {
				continue
			}
			if c.cursor.seq < a.Sequence {
				c.bad = true // a record the archive doesn't hold
				continue
			}
			if c.cursor.seq > a.Sequence {
				continue
			}
			if claimed || !bytes.Equal(c.cursor.line, line) {
				c.bad = true
				continue
			}
			claimed = true
			c.records++
			if c.cursor.advance() != nil {
				c.bad = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var kept []segmentEntry
	for _, c := range open {
		if c.bad || c.cursor.line != nil || c.records != c.entry.Records || c.cursor.digest() != c.entry.SHA256 {
			continue
		}
		kept = append(kept, c.entry)
	}
	return kept, nil
}

// compactSegments merges the smallest segments until each is more than twice
// the size of the next, which keeps their number logarithmic in the records
// and rewrites a record only a logarithmic number of times. The inputs stay
// until no kept bundle names them.
func compactSegments(ctx context.Context, parent string, segments []segmentEntry) ([]segmentEntry, error) {
	out := slices.Clone(segments)
	slices.SortStableFunc(out, func(a, b segmentEntry) int {
		switch {
		case a.Records > b.Records:
			return -1
		case a.Records < b.Records:
			return 1
		}
		return strings.Compare(a.SHA256, b.SHA256)
	})
	for len(out) > 1 && out[len(out)-2].Records <= 2*out[len(out)-1].Records {
		merged, err := mergeSegments(ctx, parent, out[len(out)-2], out[len(out)-1])
		if err != nil {
			return nil, err
		}
		out = append(out[:len(out)-2], merged)
	}
	return out, nil
}

func mergeSegments(ctx context.Context, parent string, a, b segmentEntry) (_ segmentEntry, err error) {
	in, err := openSegments(parent, []segmentEntry{a, b})
	if err != nil {
		return segmentEntry{}, err
	}
	defer in.close()
	w, err := newSegmentWriter(parent)
	if err != nil {
		return segmentEntry{}, err
	}
	defer func() {
		if err != nil {
			w.abort()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return segmentEntry{}, err
		}
		line, err := in.next()
		if err != nil {
			return segmentEntry{}, err
		}
		if line == nil {
			break
		}
		if err := w.add(line); err != nil {
			return segmentEntry{}, err
		}
	}
	merged, err := w.finish(ctx)
	if err != nil {
		return segmentEntry{}, err
	}
	if merged.Records != a.Records+b.Records {
		return segmentEntry{}, fmt.Errorf("backup: merging event archive segments kept %d of %d records", merged.Records, a.Records+b.Records)
	}
	return merged, nil
}

// encodeRecord is a record's line in a segment.
func encodeRecord(a domain.AcceptedEvent) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(a); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// segmentWriter builds one segment in a private staging directory, named like
// a bundle's stage so one a crash leaves is cleared with them.
type segmentWriter struct {
	parent, dir string
	file        *os.File
	buffered    *bufio.Writer
	hash        hash.Hash
	records     uint64
}

func newSegmentWriter(parent string) (*segmentWriter, error) {
	dir, err := os.MkdirTemp(parent, ".conduit-snapshot-")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ArchiveFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	h := sha256.New()
	return &segmentWriter{parent: parent, dir: dir, file: f, hash: h, buffered: bufio.NewWriter(io.MultiWriter(f, h))}, nil
}

func (w *segmentWriter) add(line []byte) error {
	w.records++
	_, err := w.buffered.Write(line)
	return err
}

func (w *segmentWriter) abort() {
	w.file.Close()
	os.RemoveAll(w.dir)
}

// finish makes the segment durable and publishes it.
func (w *segmentWriter) finish(ctx context.Context) (_ segmentEntry, err error) {
	defer func() {
		if err != nil {
			os.RemoveAll(w.dir)
		}
	}()
	if err := w.buffered.Flush(); err != nil {
		w.file.Close()
		return segmentEntry{}, err
	}
	if err := w.file.Sync(); err != nil {
		w.file.Close()
		return segmentEntry{}, err
	}
	if err := w.file.Close(); err != nil {
		return segmentEntry{}, err
	}
	s := segmentEntry{SHA256: hex.EncodeToString(w.hash.Sum(nil)), Records: w.records}
	if err := publishSegment(ctx, w.parent, s.name(), w.dir); err != nil {
		return segmentEntry{}, err
	}
	return s, nil
}

// segmentCursor reads a segment's lines in order, hashing them as it goes.
type segmentCursor struct {
	file *os.File
	in   *bufio.Reader
	hash hash.Hash
	line []byte // nil once the segment has ended
	seq  uint64
}

func openSegment(path string) (*segmentCursor, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &segmentCursor{file: f, in: bufio.NewReader(f), hash: sha256.New()}, nil
}

func (c *segmentCursor) close() { c.file.Close() }

func (c *segmentCursor) digest() string { return hex.EncodeToString(c.hash.Sum(nil)) }

// advance moves to the next line. Records must rise in sequence.
func (c *segmentCursor) advance() error {
	line, err := c.in.ReadBytes('\n')
	if errors.Is(err, io.EOF) {
		if len(line) > 0 {
			c.line = nil
			return io.ErrUnexpectedEOF
		}
		c.line = nil
		return nil
	}
	if err != nil {
		c.line = nil
		return err
	}
	_, _ = c.hash.Write(line)
	var head struct{ Sequence uint64 }
	if err := json.Unmarshal(line, &head); err != nil {
		c.line = nil
		return err
	}
	if c.seq != 0 && head.Sequence <= c.seq {
		c.line = nil
		return fmt.Errorf("backup: event archive record at sequence %d is out of order", head.Sequence)
	}
	c.line, c.seq = line, head.Sequence
	return nil
}

// segmentSet reads several segments as one stream in sequence order.
type segmentSet struct {
	cursors []*segmentCursor
	started bool
}

func openSegments(parent string, segments []segmentEntry) (*segmentSet, error) {
	paths := make([]string, 0, len(segments))
	for _, s := range segments {
		paths = append(paths, filepath.Join(parent, s.name(), ArchiveFile))
	}
	return openSegmentPaths(paths)
}

func openSegmentPaths(paths []string) (*segmentSet, error) {
	set := &segmentSet{}
	for _, p := range paths {
		c, err := openSegment(p)
		if err != nil {
			set.close()
			return nil, err
		}
		set.cursors = append(set.cursors, c)
	}
	return set, nil
}

func (s *segmentSet) close() {
	for _, c := range s.cursors {
		c.close()
	}
}

func (s *segmentSet) start() error {
	if s.started {
		return nil
	}
	s.started = true
	for _, c := range s.cursors {
		if err := c.advance(); err != nil {
			return err
		}
	}
	return nil
}

// next is the lowest-sequence line left, nil at the end. The same sequence in
// two segments is returned twice, for the caller's order check to refuse.
func (s *segmentSet) next() ([]byte, error) {
	if err := s.start(); err != nil {
		return nil, err
	}
	var low *segmentCursor
	for _, c := range s.cursors {
		if c.line != nil && (low == nil || c.seq < low.seq) {
			low = c
		}
	}
	if low == nil {
		return nil, nil
	}
	line := low.line
	if err := low.advance(); err != nil {
		return nil, err
	}
	return line, nil
}

// take reports whether a segment holds sequence, moving past it.
func (s *segmentSet) take(sequence uint64) (bool, error) {
	if err := s.start(); err != nil {
		return false, err
	}
	for _, c := range s.cursors {
		if c.line != nil && c.seq == sequence {
			return true, c.advance()
		}
	}
	return false, nil
}
