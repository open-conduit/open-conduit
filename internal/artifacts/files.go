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

	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/storage"
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// prefixPattern names a fan-out directory: objects/<first two hex
// characters of the ID>/<ID>, so a listing page reads only the prefix
// directories it covers rather than every object.
var prefixPattern = regexp.MustCompile(`^[a-f0-9]{2}$`)
var ErrClosed = errors.New("artifact store or writer closed")

// ErrCorrupt is the contract's storage.ErrCorrupt.
var ErrCorrupt = storage.ErrCorrupt

// metadata is an object's meta.json. Ref carries the digest from record
// format 7 on; SHA256 repeats it, and is the only digest an object
// published before then has.
type metadata struct {
	Ref         storage.ArtifactRef
	SHA256      string
	PublishedAt time.Time
}

// matches reports whether ref names this object exactly. A ref without a
// digest (recorded before digests) matches on its other fields, and is
// verified against the digest the object stored.
func (m metadata) matches(ref storage.ArtifactRef) bool {
	if ref.SHA256 == "" {
		ref.SHA256 = m.Ref.SHA256
	}
	return ref == m.Ref
}

// Store is an exclusively opened artifact directory. Nothing outside it
// relies on its locks: retention and collection go through the
// storage.ArtifactStore contract, and a backup holds content from
// retention in coordination (runtime.HoldRetention), not here.
type Store struct {
	root   string
	gate   sync.RWMutex
	mu     sync.Mutex
	active map[string]*writer
	lock   *os.File
	closed bool
	// readOnly is a store OpenReadOnly opened: no lock, no writes.
	readOnly bool
	// fault is an internal test seam at flush/publication boundaries.
	fault func(string) error
}

// Backend is the file store as a storage.ArtifactBackend, registered as
// "files". A location is the absolute path of the store's directory.
var Backend storage.ArtifactBackend = backend{}

// BackendName is the name Backend is registered under.
const BackendName = "files"

func init() { storage.RegisterArtifactBackend(BackendName, Backend) }

type backend struct{}

func (backend) Open(ctx context.Context, location string) (storage.ArtifactStore, error) {
	if !filepath.IsAbs(location) {
		return nil, fmt.Errorf("artifacts: location %q is not an absolute path: %w", location, storage.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return Open(location)
}

// OpenReadOnly opens an existing store to read it without changing
// anything in it: no directory is created, no object is moved to the
// fan-out layout, no pending stream is removed and no lock is taken. It is
// how a bundle's own artifacts are read (verify, restore), since a
// published bundle is immutable and an older binary must still be able to
// restore it. It finds an object in the fan-out layout or the flat one an
// older store used. Create, Import and Delete return storage.ErrReadOnly;
// List needs the fan-out layout.
func OpenReadOnly(root string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{root, filepath.Join(root, "objects")} {
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("unsafe artifact directory permissions")
		}
	}
	return &Store{root: root, readOnly: true, active: map[string]*writer{}}, nil
}

// Open creates private subdirectories and acquires an exclusive process
// lock, then removes the unpublished streams a process that held it before
// left behind: the lock proves no other process still owns them. It refuses
// a root on a network filesystem, where that lock proves nothing.
func Open(root string) (*Store, error) {
	if err := localfs.Require(root, "artifact store"); err != nil {
		return nil, err
	}
	return open(root)
}

// OpenBundle is Open for the store inside a backup bundle being written. A
// bundle has one writer and its digests catch a bad copy, so unlike live
// state it may be written to a mounted network volume.
func OpenBundle(root string) (*Store, error) { return open(root) }

func open(root string) (*Store, error) {
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
	s := &Store{root: root, lock: lock, active: map[string]*writer{}}
	if err = s.fanOut(); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err = s.recover(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
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
func (s *Store) objects() string { return filepath.Join(s.root, "objects") }

// objectDir is where a valid ID's committed object lives.
func (s *Store) objectDir(id string) string { return filepath.Join(s.objects(), id[:2], id) }

// locate is where a valid ID's object is to be read: objectDir, or, in a
// read-only store not yet fanned out, the flat objects/<ID>.
func (s *Store) locate(id string) string {
	dir := s.objectDir(id)
	if !s.readOnly {
		return dir
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return filepath.Join(s.objects(), id)
	}
	return dir
}

// prefixes lists the fan-out directories in name order, checking each.
func (s *Store) prefixes() ([]string, error) {
	entries, err := os.ReadDir(s.objects())
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !prefixPattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("artifacts: unexpected %q in objects: %w", entry.Name(), storage.ErrInvalid)
		}
		if err = privateDir(filepath.Join(s.objects(), entry.Name())); err != nil {
			return nil, err
		}
		out = append(out, entry.Name())
	}
	return out, nil
}

// fanOut moves objects a store published before the fan-out layout, at
// objects/<ID>, to objects/<prefix>/<ID>. Each move is one rename, so a
// store interrupted partway is finished by the next open. It runs at Open,
// under the lock.
func (s *Store) fanOut() error {
	entries, err := os.ReadDir(s.objects())
	if err != nil {
		return err
	}
	moved := map[string]bool{}
	for _, entry := range entries {
		id := entry.Name()
		if !idPattern.MatchString(id) {
			continue // a prefix directory; prefixes checks the rest
		}
		info, err := os.Lstat(filepath.Join(s.objects(), id))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifacts: object %s: %w", id, storage.ErrInvalid)
		}
		if err = privateDir(filepath.Join(s.objects(), id[:2])); err != nil {
			return err
		}
		if err = os.Rename(filepath.Join(s.objects(), id), s.objectDir(id)); err != nil {
			return err
		}
		moved[id[:2]] = true
	}
	if len(moved) == 0 {
		return nil
	}
	for prefix := range moved {
		if err = syncDir(filepath.Join(s.objects(), prefix)); err != nil {
			return err
		}
	}
	return syncDir(s.objects())
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

// writable is available, and refuses a read-only store.
func (s *Store) writable() error {
	if s.readOnly {
		return storage.ErrReadOnly
	}
	return s.available()
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
	if err := s.writable(); err != nil {
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
	mu    sync.Mutex
	store *Store
	file  *os.File
	dir   string
	hash  hash.Hash
	ref   storage.ArtifactRef
	// expect is the digest an Import must arrive at; empty for Create.
	expect   string
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
				_ = syncDir(filepath.Dir(published))
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
	digest := hex.EncodeToString(w.hash.Sum(nil))
	if w.expect != "" && digest != w.expect {
		return ref, ErrCorrupt
	}
	w.ref.SHA256 = digest
	m := metadata{Ref: w.ref, SHA256: digest, PublishedAt: time.Now().UTC()}
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
	destination := s.objectDir(w.ref.ID)
	if err = privateDir(filepath.Dir(destination)); err != nil {
		return ref, err
	}
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
	if err = syncDir(filepath.Dir(destination)); err != nil {
		return ref, err
	}
	if err = syncDir(s.objects()); err != nil {
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
	dir := s.locate(id)
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
	switch m.Ref.SHA256 {
	case "":
		m.Ref.SHA256 = m.SHA256 // published before refs carried digests
	case m.SHA256:
	default:
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
	if !m.matches(ref) {
		return nil, storage.ErrInvalid
	}
	path := filepath.Join(s.locate(ref.ID), "data")
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
	return &reader{file: f, ctx: ctx, hash: sha256.New(), expected: m.Ref.SHA256, size: ref.Size}, nil
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
	if err := s.writable(); err != nil {
		return err
	}
	m, err := s.readMetadata(ref.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !m.matches(ref) {
		return storage.ErrInvalid
	}
	if err = os.RemoveAll(s.objectDir(ref.ID)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.objectDir(ref.ID)))
}

// Import restores a selected immutable ID into a new isolated artifact store
// (storage.ArtifactStore.Import). It reads the source to its end; a short or
// long source, a digest other than ref's, or a source read error publishes
// nothing.
func (s *Store) Import(ctx context.Context, ref storage.ArtifactRef, source io.Reader) error {
	if s.readOnly {
		return storage.ErrReadOnly
	}
	if ref.Size < 0 || ref.Size == int64(^uint64(0)>>1) || (ref.SHA256 != "" && !storage.ValidDigest(ref.SHA256)) {
		return storage.ErrInvalid
	}
	if _, err := s.readMetadata(ref.ID); err == nil {
		return storage.ErrConflict
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	w, err := s.create(ctx, ref.ID, ref.MediaType)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return storage.ErrConflict // another import of the ID is under way
		}
		return err
	}
	w.expect = ref.SHA256
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

// List pages committed objects in ID order (storage.ArtifactStore.List).
// It reads only the fan-out directories from the cursor's prefix on, and
// stops at the page's end, so a page costs the objects of the prefixes it
// spans (about 1/256 of the store each), not the whole store.
func (s *Store) List(ctx context.Context, after string, limit int) ([]storage.StoredArtifact, error) {
	if limit < 1 || limit > 1000 {
		return nil, storage.ErrInvalid
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	prefixes, err := s.prefixes()
	if err != nil {
		return nil, err
	}
	out := []storage.StoredArtifact{}
	for _, prefix := range prefixes {
		// IDs in a prefix directory all sort after after when the prefix
		// does, and none does when it sorts before after's own prefix.
		if len(after) >= 2 && prefix < after[:2] {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.objects(), prefix))
		if err != nil {
			return nil, err
		}
		// ReadDir sorts by name, which for hex IDs is byte order.
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			id := entry.Name()
			if !idPattern.MatchString(id) || id[:2] != prefix {
				return nil, fmt.Errorf("artifacts: unexpected %q in objects/%s: %w", id, prefix, storage.ErrInvalid)
			}
			if id <= after {
				continue
			}
			m, err := s.readMetadata(id)
			if err != nil {
				return nil, fmt.Errorf("artifacts: object %s: %w", id, err)
			}
			out = append(out, storage.StoredArtifact{Ref: m.Ref, PublishedAt: m.PublishedAt})
			if len(out) == limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// SharesLocation states that the store is on this host: Open refuses a
// directory on a network filesystem.
func (s *Store) SharesLocation() (shared, stated bool) { return false, true }

// recover removes the unpublished streams in pending/ that are not this
// store's own active writers: those a process that held the store before
// left, which Open runs once it holds the lock that proves no other process
// still owns them.
func (s *Store) recover(ctx context.Context) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	pending := filepath.Join(s.root, "pending")
	entries, err := os.ReadDir(pending)
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
		s.mu.Lock()
		_, active := s.active[id]
		s.mu.Unlock()
		if active {
			continue
		}
		path := filepath.Join(pending, id)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return storage.ErrInvalid
		}
		if err = os.RemoveAll(path); err != nil {
			return err
		}
	}
	return syncDir(pending)
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
	if s.lock != nil {
		result = errors.Join(result, syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
	}
	return result
}

var _ storage.ArtifactStore = (*Store)(nil)
