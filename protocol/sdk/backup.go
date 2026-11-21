package sdk

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
)

// backupUpload is one Put in progress: the host's chunks go into the pipe
// its reader takes them from.
type backupUpload struct {
	w      *io.PipeWriter
	cancel context.CancelFunc
	done   chan error
	next   int64
	size   int64
}

// backupUploads are the Puts in progress, by the host's upload ID.
type backupUploads struct {
	mu sync.Mutex
	m  map[string]*backupUpload
}

func (b *backupUploads) take(id string) *backupUpload {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.m[id]
	delete(b.m, id)
	return u
}

func (b *backupUploads) get(id string) *backupUpload {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[id]
}

func (u *backupUpload) stop(err error) {
	_ = u.w.CloseWithError(err)
	u.cancel()
	<-u.done
}

func (s *Server) backupTarget(p protocol.BackupTargetParams) (extension.BackupDestination, extension.BackupTarget, error) {
	d, ok := s.ext.BackupDestinations[p.Type]
	if !ok {
		return nil, extension.BackupTarget{}, protocol.Errorf(protocol.CodeNotFound, "no backup destination of type "+p.Type)
	}
	return d, extension.BackupTarget{Name: p.Name, Config: p.Config.Domain()}, nil
}

func (s *Server) backupDestination(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "backup_destination.write":
		var p protocol.BackupWriteParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return struct{}{}, s.backupWrite(ctx, p)
	case "backup_destination.abort":
		var p protocol.BackupAbortParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if u := s.ext.backups.take(p.Upload); u != nil {
			u.stop(context.Canceled)
		}
		return struct{}{}, nil
	case "backup_destination.list":
		var p protocol.BackupListParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		d, t, err := s.backupTarget(p.Target)
		if err != nil {
			return nil, err
		}
		bundles, err := d.List(ctx, t)
		if err != nil {
			return nil, err
		}
		return protocol.BackupListResult{Bundles: bundles}, nil
	case "backup_destination.files":
		var p protocol.BackupFilesParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		d, t, err := s.backupTarget(p.Target)
		if err != nil {
			return nil, err
		}
		files, err := d.Files(ctx, t, p.Bundle)
		if err != nil {
			return nil, err
		}
		out := protocol.BackupFilesResult{Files: make([]protocol.BackupFileEntry, 0, len(files))}
		for _, f := range files {
			out.Files = append(out.Files, protocol.BackupFileEntry{Path: f.Path, Size: f.Size})
		}
		return out, nil
	}
	var p protocol.BackupReadParams
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	if p.Length <= 0 || p.Length > protocol.MaxChunkBytes || p.Offset < 0 {
		return nil, protocol.Errorf(protocol.CodeInvalid, "a read is 1 to MaxChunkBytes bytes from a non-negative offset")
	}
	d, t, err := s.backupTarget(p.Target)
	if err != nil {
		return nil, err
	}
	chunk, eof, err := d.Read(ctx, t, p.Bundle, p.Path, p.Offset, p.Length)
	if err != nil {
		return nil, err
	}
	return protocol.BackupReadResult{Chunk: chunk, EOF: eof}, nil
}

// backupWrite feeds one chunk to its Put, starting it on the first. The Put
// outlives each call, so it runs on its own context until the final chunk
// or an abort.
func (s *Server) backupWrite(ctx context.Context, p protocol.BackupWriteParams) error {
	if len(p.Chunk) > protocol.MaxChunkBytes || p.Upload == "" || p.Size < 0 {
		return protocol.Errorf(protocol.CodeInvalid, "an invalid backup chunk")
	}
	b := &s.ext.backups
	u := b.get(p.Upload)
	if u == nil {
		if p.Offset != 0 {
			return protocol.Errorf(protocol.CodeConflict, "no backup upload in progress under that ID")
		}
		d, t, err := s.backupTarget(p.Target)
		if err != nil {
			return err
		}
		r, w := io.Pipe()
		putCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		u = &backupUpload{w: w, cancel: cancel, done: make(chan error, 1), size: p.Size}
		b.mu.Lock()
		if b.m == nil {
			b.m = map[string]*backupUpload{}
		}
		b.m[p.Upload] = u
		b.mu.Unlock()
		go func() {
			err := d.Put(putCtx, t, p.Bundle, extension.BackupFile{Path: p.Path, Size: p.Size}, r)
			// A Put that stops early must not leave the host's write blocked.
			_ = r.CloseWithError(errOr(err, io.ErrClosedPipe))
			u.done <- err
		}()
	}
	fail := func(err error) error {
		if b.take(p.Upload) != nil {
			u.stop(err)
		}
		return err
	}
	if p.Offset != u.next || u.next+int64(len(p.Chunk)) > u.size {
		return fail(protocol.Errorf(protocol.CodeConflict, "backup chunk out of order or past the file's size"))
	}
	if len(p.Chunk) > 0 {
		if _, err := u.w.Write(p.Chunk); err != nil {
			b.take(p.Upload)
			u.cancel()
			if putErr := <-u.done; putErr != nil {
				return putErr
			}
			return err
		}
		u.next += int64(len(p.Chunk))
	}
	if !p.Final {
		return nil
	}
	if u.next != u.size {
		return fail(protocol.Errorf(protocol.CodeConflict, "backup file ended before its size"))
	}
	b.take(p.Upload)
	_ = u.w.Close()
	err := <-u.done
	u.cancel()
	return err
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
