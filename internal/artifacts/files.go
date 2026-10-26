// Package artifacts publishes immutable files outside coordination transactions.
package artifacts

import (
	"context"
	"crypto/rand"
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
	"sync"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var ErrClosed = errors.New("artifact store or writer closed")
var ErrCorrupt = errors.New("artifact content is corrupt")

type metadata struct {
	Ref         storage.ArtifactRef
	SHA256      string
	PublishedAt time.Time
}

// Store is an exclusively opened artifact directory. Pin blocks Delete/Collect
// while a consistent coordination snapshot and its referenced files are copied.
type Store struct {
	root   string
	gate   sync.RWMutex
	mu     sync.Mutex
	active map[string]*writer
	lock   *os.File
	closed bool
	// fault is an internal test seam at flush/publication boundaries.
	fault func(string) error
}

// Open creates private subdirectories and acquires an exclusive process lock.
// Call Recover after opening to remove stale pre-publication streams.
func Open(root string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = privateDir(root); err != nil {
		return nil, err
	}
	for _, name := range []string{"pending", "objects"} {
		if err = privateDir(filepath.Join(root, name)); err != nil {
			return nil, err
		}
	}
	lockPath := filepath.Join(root, ".lock")
	if info, e := os.Lstat(lockPath); e == nil {
		if e = privateFile(info); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("artifact directory in use: %w", err)
	}
	if err = syncDir(root); err == nil {
		err = syncDir(filepath.Dir(root))
	}
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, err
	}
	return &Store{root: root, lock: lock, active: map[string]*writer{}}, nil
}
func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("unsafe artifact directory permissions")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("artifact directory owner mismatch")
	}
	return nil
}
func privateFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("unsafe artifact file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("artifact file owner mismatch")
	}
	return nil
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) stage(name string) error {
	if s.fault != nil {
		return s.fault(name)
	}
	return nil
}
func (s *Store) available() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}
func newID() (string, error) {
	var b [32]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (s *Store) Create(ctx context.Context, mediaType string) (storage.ArtifactWriter, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	return s.create(ctx, id, mediaType)
}
func (s *Store) create(ctx context.Context, id, mediaType string) (*writer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !idPattern.MatchString(id) || mediaType == "" || len(mediaType) > 4096 {
		return nil, storage.ErrInvalid
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, "pending", id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "data"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	w := &writer{store: s, file: f, dir: dir, hash: sha256.New(), ref: storage.ArtifactRef{ID: id, MediaType: mediaType}}
	s.mu.Lock()
	s.active[id] = w
	s.mu.Unlock()
	return w, nil
}

type writer struct {
	mu       sync.Mutex
	store    *Store
	file     *os.File
	dir      string
	hash     hash.Hash
	ref      storage.ArtifactRef
	closed   bool
	writeErr error
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	n, err := w.file.Write(p)
	if n > 0 {
		w.hash.Write(p[:n])
		w.ref.Size += int64(n)
	}
	if err != nil {
		w.writeErr = err
	}
	return n, err
}
func (w *writer) unregister() {
	w.store.mu.Lock()
	delete(w.store.active, w.ref.ID)
	w.store.mu.Unlock()
}
func (w *writer) abort() error {
	w.closed = true
	err := w.file.Close()
	if errors.Is(err, os.ErrClosed) {
		err = nil
	}
	remove := os.RemoveAll(w.dir)
	w.unregister()
	return errors.Join(err, remove)
}
func (w *writer) Close() error {
	w.store.gate.RLock()
	defer w.store.gate.RUnlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return w.abort()
}
func (w *writer) Commit(ctx context.Context) (ref storage.ArtifactRef, err error) {
	s := w.store
	s.gate.RLock()
	defer s.gate.RUnlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ref, ErrClosed
	}
	if err = s.available(); err != nil {
		w.abort()
		return ref, err
	}
	published := ""
	defer func() {
		if err != nil {
			w.abort()
			if published != "" {
				_ = os.RemoveAll(published)
				_ = syncDir(filepath.Join(s.root, "objects"))
			}
		}
	}()
	if err = ctx.Err(); err != nil {
		return ref, err
	}
	if w.writeErr != nil {
		return ref, w.writeErr
	}
	if err = s.stage("data_sync"); err != nil {
		return ref, err
	}
	if err = w.file.Sync(); err != nil {
		return ref, err
	}
	if err = w.file.Close(); err != nil {
		return ref, err
	}
	m := metadata{Ref: w.ref, SHA256: hex.EncodeToString(w.hash.Sum(nil)), PublishedAt: time.Now().UTC()}
	bytes, e := json.Marshal(m)
	if e != nil {
		return ref, e
	}
	f, e := os.OpenFile(filepath.Join(w.dir, "meta.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return ref, e
	}
	_, err = f.Write(bytes)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return ref, err
	}
	if err = syncDir(w.dir); err != nil {
		return ref, err
	}
	if err = ctx.Err(); err != nil {
		return ref, err
	}
	if err = s.stage("publish"); err != nil {
		return ref, err
	}
	destination := filepath.Join(s.root, "objects", w.ref.ID)
	// IDs are random and never reused. Existing immutable content is not replaced.
	if _, e = os.Lstat(destination); e == nil {
		return ref, storage.ErrConflict
	} else if !errors.Is(e, os.ErrNotExist) {
		return ref, e
	}
	if err = os.Rename(w.dir, destination); err != nil {
		return ref, err
	}
	published = destination
	if err = s.stage("directory_sync"); err != nil {
		return ref, err
	}
	if err = syncDir(filepath.Join(s.root, "objects")); err != nil {
		return ref, err
	}
	if err = syncDir(filepath.Join(s.root, "pending")); err != nil {
		return ref, err
	}
	if err = ctx.Err(); err != nil {
		return ref, err
	}
	w.closed = true
	w.unregister()
	return w.ref, nil
}

func (s *Store) Open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	return s.open(ctx, ref)
}
func (s *Store) readMetadata(id string) (metadata, error) {
	var m metadata
	if !idPattern.MatchString(id) {
		return m, storage.ErrInvalid
	}
	dir := filepath.Join(s.root, "objects", id)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return m, storage.ErrNotFound
	}
	if err != nil {
		return m, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return m, storage.ErrInvalid
	}
	path := filepath.Join(dir, "meta.json")
	info, err = os.Lstat(path)
	if err != nil {
		return m, err
	}
	if err = privateFile(info); err != nil {
		return m, err
	}
	if info.Size() > 65536 {
		return m, ErrCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 65536))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return m, ErrCorrupt
	}
	var extra any
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return m, ErrCorrupt
	}
	if m.Ref.ID != id || m.Ref.Size < 0 || m.Ref.MediaType == "" || !idPattern.MatchString(m.SHA256) || m.PublishedAt.IsZero() {
		return m, ErrCorrupt
	}
	return m, nil
}
func (s *Store) open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.readMetadata(ref.ID)
	if err != nil {
		return nil, err
	}
	if m.Ref != ref {
		return nil, storage.ErrInvalid
	}
	path := filepath.Join(s.root, "objects", ref.ID, "data")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err = privateFile(info); err != nil {
		return nil, err
	}
	if info.Size() != ref.Size {
		return nil, ErrCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &reader{file: f, ctx: ctx, hash: sha256.New(), expected: m.SHA256, size: ref.Size}, nil
}

type reader struct {
	file       *os.File
	ctx        context.Context
	hash       hash.Hash
	expected   string
	size, read int64
}

func (r *reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.Read(p)
	if n > 0 {
		r.hash.Write(p[:n])
		r.read += int64(n)
	}
	if err == io.EOF {
		if r.read != r.size || hex.EncodeToString(r.hash.Sum(nil)) != r.expected {
			return n, ErrCorrupt
		}
	}
	return n, err
}
func (r *reader) Close() error { return r.file.Close() }
func (s *Store) Delete(ctx context.Context, ref storage.ArtifactRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !idPattern.MatchString(ref.ID) {
		return storage.ErrInvalid
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	if err := s.available(); err != nil {
		return err
	}
	m, err := s.readMetadata(ref.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Ref != ref {
		return storage.ErrInvalid
	}
	if err = os.RemoveAll(filepath.Join(s.root, "objects", ref.ID)); err != nil {
		return err
	}
	return syncDir(filepath.Join(s.root, "objects"))
}

// Pinned holds retention/deletion while a database backup snapshot and files are
// captured. Call Close after copying. Open must be used on this lease (rather
// than reentering Store.Open while a writer waits for the retention gate).
type Pinned struct {
	store  *Store
	mu     sync.Mutex
	closed bool
}

func (s *Store) Pin() (*Pinned, error) {
	s.gate.RLock()
	if err := s.available(); err != nil {
		s.gate.RUnlock()
		return nil, err
	}
	return &Pinned{store: s}, nil
}
func (p *Pinned) Open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	return p.store.open(ctx, ref)
}
func (p *Pinned) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.closed = true
	p.store.gate.RUnlock()
	return nil
}

// Import restores a selected immutable ID into a new isolated artifact store.
// The caller must fully read the source; size and source read errors abort it.
func (s *Store) Import(ctx context.Context, ref storage.ArtifactRef, source io.Reader) error {
	if ref.Size < 0 || ref.Size == int64(^uint64(0)>>1) {
		return storage.ErrInvalid
	}
	w, err := s.create(ctx, ref.ID, ref.MediaType)
	if err != nil {
		return err
	}
	defer func() {
		if !w.closed {
			_ = w.Close()
		}
	}()
	n, err := io.Copy(w, io.LimitReader(source, ref.Size+1))
	if err != nil {
		return err
	}
	if n != ref.Size {
		return ErrCorrupt
	}
	_, err = w.Commit(ctx)
	return err
}

// Recover removes only stale unpublished streams. The exclusive directory lock
// proves another process cannot still own them; active writers are preserved.
func (s *Store) Recover(ctx context.Context) error { return s.cleanup(ctx, time.Time{}, nil, true) }

// Collect deletes aged committed objects absent from liveIDs and stale pending
// streams. The caller supplies a complete protected reference set; it must not
// race coordination attachment. Pending streams owned by this Store are kept.
func (s *Store) Collect(ctx context.Context, before time.Time, liveIDs map[string]bool) error {
	return s.cleanup(ctx, before, liveIDs, false)
}
func (s *Store) cleanup(ctx context.Context, before time.Time, live map[string]bool, recovery bool) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	if err := s.available(); err != nil {
		return err
	}
	for _, name := range []string{"pending", "objects"} {
		if recovery && name == "objects" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, name))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return err
			}
			id := entry.Name()
			if !idPattern.MatchString(id) {
				return storage.ErrInvalid
			}
			if name == "pending" {
				s.mu.Lock()
				_, active := s.active[id]
				s.mu.Unlock()
				if active {
					continue
				}
			} else if live[id] {
				continue
			}
			path := filepath.Join(s.root, name, id)
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return storage.ErrInvalid
			}
			when := info.ModTime()
			if name == "objects" {
				m, err := s.readMetadata(id)
				if err != nil {
					return err
				}
				when = m.PublishedAt
			}
			if !recovery && !when.Before(before) {
				continue
			}
			if err = os.RemoveAll(path); err != nil {
				return err
			}
		}
		if err = syncDir(filepath.Join(s.root, name)); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Close() error {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.closed = true
	writers := make([]*writer, 0, len(s.active))
	for _, w := range s.active {
		writers = append(writers, w)
	}
	s.mu.Unlock()
	var result error
	for _, w := range writers {
		w.mu.Lock()
		if !w.closed {
			result = errors.Join(result, w.abort())
		}
		w.mu.Unlock()
	}
	result = errors.Join(result, syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
	return result
}

var _ storage.Artifacts = (*Store)(nil)
