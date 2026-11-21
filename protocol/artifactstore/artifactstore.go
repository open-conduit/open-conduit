// Package artifactstore runs a storage.ArtifactBackend in its own process,
// so an artifact store (S3, GCS, anything else) can be installed without
// changing or rebuilding Open Conduit.
//
// A provider is an executable whose main calls Serve with its backend:
//
//	func main() {
//		err := artifactstore.Serve(context.Background(), os.Stdin, os.Stdout, myBackend{})
//		if err != nil {
//			log.Fatal(err)
//		}
//	}
//
// The host starts it through Command, named by the state directory's
// storage.json, and speaks JSON-RPC over its stdin and stdout; stderr is
// its log. The provider inherits the host's environment, which is how it
// gets cloud credentials. It runs with the host's privileges.
//
// The host doesn't take a provider's word for content: it computes the size
// and digest of everything it writes and reads, and refuses what doesn't
// match its reference.
package artifactstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

// Version is the provider protocol's version.
const Version = 1

// maxChunk is the most content one message carries. Base64 in JSON keeps it
// well inside protocol.MaxMessageBytes.
const maxChunk = 1 << 20

// Methods the host calls on a provider.
const (
	methodInitialize   = "artifactstore.initialize"
	methodCreate       = "artifactstore.create"
	methodWrite        = "artifactstore.write"
	methodCommit       = "artifactstore.commit"
	methodAbort        = "artifactstore.abort"
	methodOpen         = "artifactstore.open"
	methodRead         = "artifactstore.read"
	methodCloseReader  = "artifactstore.close_reader"
	methodDelete       = "artifactstore.delete"
	methodList         = "artifactstore.list"
	methodImportBegin  = "artifactstore.import_begin"
	methodImportWrite  = "artifactstore.import_write"
	methodImportFinish = "artifactstore.import_finish"
	methodImportAbort  = "artifactstore.import_abort"
)

// Error codes for the storage errors the shared protocol has no code for.
const (
	codeCorrupt = "artifact_corrupt"
	codeInUse   = "artifact_in_use"
)

type wireRef struct {
	ID        string `json:"id"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256,omitempty"`
}

func toWire(r storage.ArtifactRef) wireRef {
	return wireRef{ID: r.ID, Size: r.Size, MediaType: r.MediaType, SHA256: r.SHA256}
}

func (r wireRef) ref() storage.ArtifactRef {
	return storage.ArtifactRef{ID: r.ID, Size: r.Size, MediaType: r.MediaType, SHA256: r.SHA256}
}

type initializeParams struct {
	Version  int    `json:"version"`
	Location string `json:"location"`
}
type initializeResult struct {
	Version int `json:"version"`
}
type createParams struct {
	MediaType string `json:"media_type"`
}
type handle struct {
	Handle string `json:"handle"`
}
type chunkParams struct {
	Handle string `json:"handle"`
	Chunk  []byte `json:"chunk"`
}
type readResult struct {
	Chunk []byte `json:"chunk,omitempty"`
	EOF   bool   `json:"eof,omitempty"`
}
type listParams struct {
	After string `json:"after,omitempty"`
	Limit int    `json:"limit"`
}
type listItem struct {
	Ref         wireRef   `json:"ref"`
	PublishedAt time.Time `json:"published_at"`
}
type listResult struct {
	Items []listItem `json:"items"`
}

// encodeError gives a storage error a code that survives the wire.
func encodeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrCorrupt):
		return protocol.Errorf(codeCorrupt, "artifact content is corrupt")
	case errors.Is(err, storage.ErrInUse):
		return protocol.Errorf(codeInUse, "artifact store is in use")
	case errors.Is(err, storage.ErrNotFound):
		return protocol.Errorf(protocol.CodeNotFound, "artifact not found")
	case errors.Is(err, storage.ErrConflict):
		return protocol.Errorf(protocol.CodeConflict, "artifact already exists")
	case errors.Is(err, storage.ErrInvalid):
		return protocol.Errorf(protocol.CodeInvalid, "invalid artifact request")
	}
	return err
}

// decodeError turns a provider's answer back into the storage error.
func decodeError(err error) error {
	if err == nil {
		return nil
	}
	code := protocol.ErrorCode(err)
	var op *extension.OperationError
	if errors.As(err, &op) {
		code = op.Code
	}
	switch code {
	case codeCorrupt:
		return fmt.Errorf("artifact provider: %w", storage.ErrCorrupt)
	case codeInUse:
		return fmt.Errorf("artifact provider: %w", storage.ErrInUse)
	case protocol.CodeNotFound:
		return fmt.Errorf("artifact provider: %w", storage.ErrNotFound)
	case protocol.CodeConflict:
		return fmt.Errorf("artifact provider: %w", storage.ErrConflict)
	case protocol.CodeInvalid:
		return fmt.Errorf("artifact provider: %w", storage.ErrInvalid)
	}
	return fmt.Errorf("artifact provider: %w", err)
}

// Serve runs backend as a provider over in and out until the host closes
// the connection or ctx ends. It closes whatever the host left open.
func Serve(ctx context.Context, in io.Reader, out io.Writer, backend storage.ArtifactBackend) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{ctx: ctx, backend: backend, writers: map[string]storage.ArtifactWriter{}, readers: map[string]*serverReader{}, imports: map[string]*serverImport{}}
	defer s.shutdown()
	conn := jsonrpc.NewConn(in, out, s.handle, jsonrpc.Options{})
	defer conn.Close()
	select {
	case <-conn.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type server struct {
	ctx     context.Context
	backend storage.ArtifactBackend

	mu      sync.Mutex
	store   storage.ArtifactStore
	next    int
	writers map[string]storage.ArtifactWriter
	readers map[string]*serverReader
	imports map[string]*serverImport
}

type serverReader struct {
	r   io.ReadCloser
	err error // what the last read returned after its bytes
}

type serverImport struct {
	w    *io.PipeWriter
	done chan error
}

func (s *server) id() string {
	s.next++
	return strconv.Itoa(s.next)
}

func (s *server) opened() (storage.ArtifactStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return nil, protocol.Errorf(protocol.CodeInvalid, "provider is not initialized")
	}
	return s.store, nil
}

func (s *server) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.writers {
		_ = w.Close()
	}
	for _, r := range s.readers {
		_ = r.r.Close()
	}
	for _, i := range s.imports {
		_ = i.w.CloseWithError(errors.New("artifact provider: import aborted"))
		<-i.done
	}
	if s.store != nil {
		_ = s.store.Close()
	}
}

func (s *server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	out, err := s.serve(ctx, method, raw)
	return out, encodeError(err)
}

func (s *server) serve(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == methodInitialize {
		var p initializeParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if p.Version != Version {
			return nil, protocol.Errorf(protocol.CodeUnsupportedVersion, "unsupported artifact provider version")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.store != nil {
			return nil, protocol.Errorf(protocol.CodeInvalid, "provider is already initialized")
		}
		// The store outlives this request.
		store, err := s.backend.Open(s.ctx, p.Location)
		if err != nil {
			return nil, err
		}
		s.store = store
		return initializeResult{Version: Version}, nil
	}
	store, err := s.opened()
	if err != nil {
		return nil, err
	}
	switch method {
	case methodCreate:
		var p createParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		w, err := store.Create(s.ctx, p.MediaType)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		id := s.id()
		s.writers[id] = w
		return handle{Handle: id}, nil
	case methodWrite:
		var p chunkParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		w, err := s.writer(p.Handle, false)
		if err != nil {
			return nil, err
		}
		_, err = w.Write(p.Chunk)
		return struct{}{}, err
	case methodCommit:
		var p handle
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		w, err := s.writer(p.Handle, true)
		if err != nil {
			return nil, err
		}
		ref, err := w.Commit(ctx)
		if err != nil {
			return nil, err
		}
		return toWire(ref), nil
	case methodAbort:
		var p handle
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		w, err := s.writer(p.Handle, true)
		if err != nil {
			return nil, err
		}
		return struct{}{}, w.Close()
	case methodOpen:
		var p wireRef
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		r, err := store.Open(s.ctx, p.ref())
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		id := s.id()
		s.readers[id] = &serverReader{r: r}
		return handle{Handle: id}, nil
	case methodRead:
		var p handle
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		r := s.readers[p.Handle]
		s.mu.Unlock()
		if r == nil {
			return nil, protocol.Errorf(protocol.CodeInvalid, "unknown reader")
		}
		return r.read()
	case methodCloseReader:
		var p handle
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		r := s.readers[p.Handle]
		delete(s.readers, p.Handle)
		s.mu.Unlock()
		if r == nil {
			return nil, protocol.Errorf(protocol.CodeInvalid, "unknown reader")
		}
		return struct{}{}, r.r.Close()
	case methodDelete:
		var p wireRef
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return struct{}{}, store.Delete(ctx, p.ref())
	case methodList:
		var p listParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		items, err := store.List(ctx, p.After, p.Limit)
		if err != nil {
			return nil, err
		}
		out := listResult{Items: make([]listItem, 0, len(items))}
		for _, it := range items {
			out.Items = append(out.Items, listItem{Ref: toWire(it.Ref), PublishedAt: it.PublishedAt})
		}
		return out, nil
	case methodImportBegin:
		var p wireRef
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		pr, pw := io.Pipe()
		imp := &serverImport{w: pw, done: make(chan error, 1)}
		go func() {
			err := store.Import(s.ctx, p.ref(), pr)
			// A write still in flight gets the import's own answer.
			_ = pr.CloseWithError(importEnded(err))
			imp.done <- err
		}()
		s.mu.Lock()
		defer s.mu.Unlock()
		id := s.id()
		s.imports[id] = imp
		return handle{Handle: id}, nil
	case methodImportWrite:
		var p chunkParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		imp := s.imports[p.Handle]
		s.mu.Unlock()
		if imp == nil {
			return nil, protocol.Errorf(protocol.CodeInvalid, "unknown import")
		}
		_, err := imp.w.Write(p.Chunk)
		return struct{}{}, err
	case methodImportFinish, methodImportAbort:
		var p handle
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		imp := s.imports[p.Handle]
		delete(s.imports, p.Handle)
		s.mu.Unlock()
		if imp == nil {
			return nil, protocol.Errorf(protocol.CodeInvalid, "unknown import")
		}
		if method == methodImportAbort {
			_ = imp.w.CloseWithError(errors.New("artifact provider: import aborted"))
			<-imp.done
			return struct{}{}, nil
		}
		_ = imp.w.Close()
		return struct{}{}, <-imp.done
	}
	return nil, protocol.Errorf(protocol.CodeMethodNotFound, "unknown method")
}

// importEnded is what a write to an import that has already returned sees.
func importEnded(err error) error {
	if err != nil {
		return err
	}
	return io.ErrClosedPipe
}

// writer finds a writer, and forgets it when the call ends it.
func (s *server) writer(id string, end bool) (storage.ArtifactWriter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.writers[id]
	if w == nil {
		return nil, protocol.Errorf(protocol.CodeInvalid, "unknown writer")
	}
	if end {
		delete(s.writers, id)
	}
	return w, nil
}

func (r *serverReader) read() (readResult, error) {
	if r.err != nil {
		if errors.Is(r.err, io.EOF) {
			return readResult{EOF: true}, nil
		}
		return readResult{}, r.err
	}
	buf := make([]byte, maxChunk)
	n, err := r.r.Read(buf)
	if err != nil && n == 0 {
		r.err = err
		if errors.Is(err, io.EOF) {
			return readResult{EOF: true}, nil
		}
		return readResult{}, err
	}
	// Bytes first: the error, or EOF, is the next read's answer.
	r.err = err
	return readResult{Chunk: buf[:n]}, nil
}
