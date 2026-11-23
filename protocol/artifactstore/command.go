package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	// startTimeout bounds starting a provider and opening its store.
	startTimeout = 30 * time.Second
	// callTimeout bounds one call, so a hung provider fails the operation
	// rather than holding it for ever.
	callTimeout = 2 * time.Minute
	// stopTimeout is how long a provider has to exit once its stdin closes.
	stopTimeout = 5 * time.Second
	// restartBackoff is how long a failed start is remembered, so a broken
	// provider isn't started again for every call.
	restartBackoff = time.Second
	// maxConcurrent stays under what a provider's connection will serve at
	// once (protocol.MaxInflight).
	maxConcurrent = protocol.MaxInflight / 2
)

// Command is the storage.ArtifactBackend served by the provider executable
// at path, which must be absolute: the daemon and the offline commands
// resolve it the same way from any working directory.
func Command(path string) storage.ArtifactBackend { return command{path: path} }

type command struct{ path string }

// Open starts the provider and opens location in it.
func (c command) Open(ctx context.Context, location string) (storage.ArtifactStore, error) {
	if !filepath.IsAbs(c.path) {
		return nil, fmt.Errorf("artifact provider command %q is not an absolute path: %w", c.path, storage.ErrInvalid)
	}
	s := &client{path: c.path, location: location, slots: make(chan struct{}, maxConcurrent)}
	if _, err := s.current(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// client is an open store in a provider process. A provider that dies is
// started again on the next call; what was in flight in it fails, since a
// writer's or reader's handle means nothing to the new process.
type client struct {
	path, location string
	slots          chan struct{}

	mu       sync.Mutex
	proc     *process
	closed   bool
	failedAt time.Time
	failed   error
}

type process struct {
	conn   *jsonrpc.Conn
	exited chan struct{}
	// shared is what the provider said about its store's reach, if anything.
	shared *bool
}

// SharesLocation is what the running provider stated when it started.
func (s *client) SharesLocation() (shared, stated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil || s.proc.shared == nil {
		return false, false
	}
	return *s.proc.shared, true
}

func (s *client) current(ctx context.Context) (*process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("artifact provider: %w", os.ErrClosed)
	}
	if s.proc != nil {
		select {
		case <-s.proc.conn.Done():
			fmt.Fprintf(os.Stderr, "open-conduit: artifact provider %s exited; starting it again\n", s.path)
			s.proc = nil
		default:
			return s.proc, nil
		}
	}
	if s.failed != nil && time.Since(s.failedAt) < restartBackoff {
		return nil, s.failed
	}
	p, err := start(ctx, s.path, s.location)
	if err != nil {
		s.failed, s.failedAt = err, time.Now()
		return nil, err
	}
	s.failed, s.proc = nil, p
	return p, nil
}

func start(ctx context.Context, path, location string) (*process, error) {
	// Not CommandContext: the process outlives the call that starts it.
	cmd := exec.Command(path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start artifact provider %s: %w", path, err)
	}
	refuse := func(context.Context, string, json.RawMessage) (any, error) {
		return nil, protocol.Errorf(protocol.CodeMethodNotFound, "the host serves no methods to an artifact provider")
	}
	p := &process{conn: jsonrpc.NewConn(stdout, stdin, refuse, jsonrpc.Options{}), exited: make(chan struct{})}
	go func() {
		defer close(p.exited)
		<-p.conn.Done()
		kill := time.AfterFunc(stopTimeout, func() { _ = cmd.Process.Kill() })
		defer kill.Stop()
		_ = cmd.Wait()
	}()
	cx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	var res InitializeResult
	err = p.conn.Call(cx, MethodInitialize, InitializeParams{Version: Version, Location: location}, &res)
	if err == nil && res.Version != Version {
		err = protocol.Errorf(protocol.CodeUnsupportedVersion, "artifact provider answered with another version")
	}
	if err != nil {
		_ = p.conn.Close()
		<-p.exited
		return nil, decodeError(err)
	}
	p.shared = res.Shared
	return p, nil
}

// call runs one method on p.
func (s *client) call(ctx context.Context, p *process, method string, params, out any) error {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	cx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return decodeError(p.conn.Call(cx, method, params, out))
}

func (s *client) Create(ctx context.Context, mediaType string) (storage.ArtifactWriter, error) {
	p, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	var h Handle
	if err := s.call(ctx, p, MethodCreate, CreateParams{MediaType: mediaType}, &h); err != nil {
		return nil, err
	}
	return &clientWriter{s: s, p: p, id: h.Handle, mediaType: mediaType, hash: sha256.New()}, nil
}

func (s *client) Open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	p, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	var h Handle
	if err := s.call(ctx, p, MethodOpen, toWire(ref), &h); err != nil {
		return nil, err
	}
	return &clientReader{s: s, p: p, id: h.Handle, ref: ref, hash: sha256.New()}, nil
}

func (s *client) Delete(ctx context.Context, ref storage.ArtifactRef) error {
	p, err := s.current(ctx)
	if err != nil {
		return err
	}
	return s.call(ctx, p, MethodDelete, toWire(ref), nil)
}

func (s *client) List(ctx context.Context, after string, limit int) ([]storage.StoredArtifact, error) {
	p, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	var res ListResult
	if err := s.call(ctx, p, MethodList, ListParams{After: after, Limit: limit}, &res); err != nil {
		return nil, err
	}
	out := make([]storage.StoredArtifact, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, storage.StoredArtifact{Ref: it.Ref.ref(), PublishedAt: it.PublishedAt})
	}
	return out, nil
}

// Import streams source to the provider, checking it here as well: a
// source that doesn't match ref is ErrCorrupt whatever the provider says.
func (s *client) Import(ctx context.Context, ref storage.ArtifactRef, source io.Reader) error {
	p, err := s.current(ctx)
	if err != nil {
		return err
	}
	var h Handle
	if err := s.call(ctx, p, MethodImportBegin, toWire(ref), &h); err != nil {
		return err
	}
	abort := func(cause error) error {
		_ = s.call(context.WithoutCancel(ctx), p, MethodImportAbort, h, nil)
		return cause
	}
	sum, size, buf := sha256.New(), int64(0), make([]byte, MaxChunk)
	for {
		n, rerr := source.Read(buf)
		if n > 0 {
			size += int64(n)
			if size > ref.Size {
				return abort(fmt.Errorf("import %s runs long: %w", ref.ID, storage.ErrCorrupt))
			}
			_, _ = sum.Write(buf[:n])
			if err := s.call(ctx, p, MethodImportWrite, ChunkParams{Handle: h.Handle, Chunk: buf[:n]}, nil); err != nil {
				// The import has already answered (a conflict, say).
				_ = s.call(context.WithoutCancel(ctx), p, MethodImportAbort, h, nil)
				return err
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return abort(rerr)
		}
	}
	if size != ref.Size || (ref.SHA256 != "" && hex.EncodeToString(sum.Sum(nil)) != ref.SHA256) {
		return abort(fmt.Errorf("import %s doesn't match its ref: %w", ref.ID, storage.ErrCorrupt))
	}
	return s.call(ctx, p, MethodImportFinish, h, nil)
}

// Close stops the provider, which aborts its unfinished writers.
func (s *client) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	p := s.proc
	s.proc = nil
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	_ = p.conn.Close()
	<-p.exited
	return nil
}

// clientWriter counts and hashes what it sends, so a commit is only
// accepted for the content the host actually wrote.
type clientWriter struct {
	s         *client
	p         *process
	id        string
	mediaType string
	hash      hash.Hash
	size      int64
	err       error
	done      bool
}

func (w *clientWriter) Write(b []byte) (int, error) {
	if w.done {
		return 0, fmt.Errorf("artifact writer: %w", os.ErrClosed)
	}
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(b) > 0 {
		chunk := b[:min(len(b), MaxChunk)]
		if err := w.s.call(context.Background(), w.p, MethodWrite, ChunkParams{Handle: w.id, Chunk: chunk}, nil); err != nil {
			w.err = err
			return written, err
		}
		_, _ = w.hash.Write(chunk)
		w.size += int64(len(chunk))
		written += len(chunk)
		b = b[len(chunk):]
	}
	return written, nil
}

func (w *clientWriter) Commit(ctx context.Context) (storage.ArtifactRef, error) {
	if w.done {
		return storage.ArtifactRef{}, fmt.Errorf("artifact writer: %w", os.ErrClosed)
	}
	w.done = true
	if w.err != nil {
		_ = w.s.call(context.WithoutCancel(ctx), w.p, MethodAbort, Handle{Handle: w.id}, nil)
		return storage.ArtifactRef{}, w.err
	}
	var res Ref
	if err := w.s.call(ctx, w.p, MethodCommit, Handle{Handle: w.id}, &res); err != nil {
		return storage.ArtifactRef{}, err
	}
	ref := res.ref()
	if ref.ID == "" || ref.Size != w.size || ref.MediaType != w.mediaType || ref.SHA256 != hex.EncodeToString(w.hash.Sum(nil)) {
		// Published under a ref that doesn't describe what was written:
		// take it back rather than record it.
		// The delete names what was really written, since a store refuses
		// a ref that doesn't match what it holds.
		if res.ID != "" {
			actual := Ref{ID: res.ID, Size: w.size, MediaType: w.mediaType, SHA256: hex.EncodeToString(w.hash.Sum(nil))}
			_ = w.s.call(context.WithoutCancel(ctx), w.p, MethodDelete, actual, nil)
		}
		return storage.ArtifactRef{}, fmt.Errorf("artifact provider returned a ref that doesn't match the content: %w", storage.ErrCorrupt)
	}
	return ref, nil
}

func (w *clientWriter) Close() error {
	if w.done {
		return fmt.Errorf("artifact writer: %w", os.ErrClosed)
	}
	w.done = true
	return w.s.call(context.Background(), w.p, MethodAbort, Handle{Handle: w.id}, nil)
}

// clientReader verifies the size and digest itself, so a read to EOF proves
// the content whatever the provider did.
type clientReader struct {
	s      *client
	p      *process
	id     string
	ref    storage.ArtifactRef
	hash   hash.Hash
	size   int64
	buf    []byte
	err    error
	closed bool
}

func (r *clientReader) Read(b []byte) (int, error) {
	if r.closed {
		return 0, fmt.Errorf("artifact reader: %w", os.ErrClosed)
	}
	if len(b) == 0 {
		return 0, nil
	}
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		var res ReadResult
		if err := r.s.call(context.Background(), r.p, MethodRead, Handle{Handle: r.id}, &res); err != nil {
			r.err = err
			return 0, err
		}
		if len(res.Chunk) > MaxChunk || (len(res.Chunk) == 0 && !res.EOF) {
			r.err = fmt.Errorf("artifact provider answered a read with %d bytes: %w", len(res.Chunk), storage.ErrInvalid)
			return 0, r.err
		}
		if len(res.Chunk) > 0 {
			r.size += int64(len(res.Chunk))
			if r.size > r.ref.Size {
				r.err = fmt.Errorf("artifact %s runs long: %w", r.ref.ID, storage.ErrCorrupt)
				return 0, r.err
			}
			_, _ = r.hash.Write(res.Chunk)
			r.buf = res.Chunk
		}
		if res.EOF {
			r.err = r.verify()
		}
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// verify is the answer at the end of the content: io.EOF only if it is the
// content the ref describes. A ref from before digests has only its size
// to check here; the provider checks it against what it stored.
func (r *clientReader) verify() error {
	if r.size != r.ref.Size {
		return fmt.Errorf("artifact %s is the wrong size: %w", r.ref.ID, storage.ErrCorrupt)
	}
	if r.ref.SHA256 != "" && hex.EncodeToString(r.hash.Sum(nil)) != r.ref.SHA256 {
		return fmt.Errorf("artifact %s doesn't match its digest: %w", r.ref.ID, storage.ErrCorrupt)
	}
	return io.EOF
}

func (r *clientReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.s.call(context.Background(), r.p, MethodCloseReader, Handle{Handle: r.id}, nil)
}
