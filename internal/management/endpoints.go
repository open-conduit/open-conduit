package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// Endpoints gives each execution its own toolbelt endpoint in a host-owned
// run directory, StateDir/runs/<key>/:
//
//   - toolbelt.sock serves only that execution's toolbelt calls
//     (Server.ExecutionHandler);
//   - token/token holds its current toolbelt credential, written then
//     renamed inside token/, so a directory mount sees a rotated token;
//   - execution names the execution, so a restarted daemon can tell which
//     run directories belong to terminal executions.
//
// When the run directory's socket path would pass the platform limit, the
// socket goes in an owner-only directory keyed by execution under a short
// root (/tmp/open-conduit-<uid>) instead; the token stays in the run dir.
type Endpoints struct {
	root, short string
	handler     func(domain.ID) http.Handler

	mu     sync.Mutex
	served map[domain.ID]*endpoint
}

type endpoint struct {
	dir, socket string
	srv         *http.Server
}

const (
	endpointSocket = "toolbelt.sock"
	endpointTokens = "token"
	endpointToken  = "token"
	endpointID     = "execution"
)

// NewEndpoints prepares StateDir/runs. The caller must hold the daemon lock.
// handler returns the toolbelt-only handler for one execution; short
// overrides the fallback socket root (empty uses /tmp/open-conduit-<uid>).
func NewEndpoints(_ *Lock, stateDir, short string, handler func(domain.ID) http.Handler) (*Endpoints, error) {
	if !filepath.IsAbs(stateDir) || handler == nil {
		return nil, errors.New("endpoints need an absolute state directory and a handler")
	}
	if short == "" {
		short = fmt.Sprintf("/tmp/open-conduit-%d", os.Getuid())
	}
	root := filepath.Join(stateDir, "runs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(root); err != nil {
		return nil, err
	}
	return &Endpoints{root: root, short: short, handler: handler, served: map[domain.ID]*endpoint{}}, nil
}

// endpointKey is short so the socket path fits the platform limit; the run
// directory's execution file resolves the rare collision.
func endpointKey(id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// Dir is the execution's run directory.
func (e *Endpoints) Dir(id domain.ID) string { return filepath.Join(e.root, endpointKey(id)) }

// Serve writes the execution's current token into its token directory and
// serves its socket, if it is not served already. It returns the endpoint
// (unix:///…) and the token file. Every claim, recovery and resume calls it
// with that attempt's fresh token, so a restarted daemon serves the socket
// again for an adopted attempt and the token is rotated in place.
func (e *Endpoints) Serve(_ context.Context, id domain.ID, token string) (string, string, error) {
	if id == "" || token == "" || strings.ContainsAny(token, "\n\x00") {
		return "", "", errors.New("toolbelt endpoint needs an execution and a token")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	dir := e.Dir(id)
	if err := e.claim(dir, id); err != nil {
		return "", "", err
	}
	tokens := filepath.Join(dir, endpointTokens)
	file := filepath.Join(tokens, endpointToken)
	if err := writePrivate(file, []byte(token+"\n")); err != nil {
		return "", "", err
	}
	if ep := e.served[id]; ep != nil {
		return "unix://" + ep.socket, file, nil
	}
	socket, err := e.socketPath(id, dir)
	if err != nil {
		return "", "", err
	}
	l, err := listenUnix(socket)
	if err != nil {
		return "", "", fmt.Errorf("toolbelt socket %s: %w", socket, err)
	}
	ep := &endpoint{dir: dir, socket: socket, srv: &http.Server{Handler: e.handler(id), ReadHeaderTimeout: 5 * time.Second}}
	go func() { _ = ep.srv.Serve(l) }()
	e.served[id] = ep
	return "unix://" + socket, file, nil
}

// claim makes the owner-only run directory and its token directory, and
// refuses one that names another execution.
func (e *Endpoints) claim(dir string, id domain.ID) error {
	if err := os.MkdirAll(filepath.Join(dir, endpointTokens), 0o700); err != nil {
		return err
	}
	for _, d := range []string{dir, filepath.Join(dir, endpointTokens)} {
		if err := checkPrivateDir(d); err != nil {
			return err
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, endpointID))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return writePrivate(filepath.Join(dir, endpointID), []byte(id))
	case err != nil:
		return err
	case string(b) != string(id):
		return fmt.Errorf("run directory %s belongs to another execution", dir)
	}
	return nil
}

// socketPath is the run directory's socket, or a short owner-only directory
// keyed by execution when that path is too long for a Unix socket.
func (e *Endpoints) socketPath(id domain.ID, dir string) (string, error) {
	socket := filepath.Join(dir, endpointSocket)
	if len(socket) <= maxSocketPath {
		return socket, nil
	}
	short := filepath.Join(e.short, endpointKey(id), endpointSocket)
	if !filepath.IsAbs(short) || len(short) > maxSocketPath {
		return "", fmt.Errorf("toolbelt socket path %s is longer than the platform's %d-byte limit, and so is the fallback %s; use a shorter state directory", socket, maxSocketPath, short)
	}
	for _, d := range []string{e.short, filepath.Dir(short)} {
		if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("toolbelt socket directory %s: %w", d, err)
		}
		if err := checkPrivateDir(d); err != nil {
			return "", fmt.Errorf("toolbelt socket directory %s: %w", d, err)
		}
	}
	return short, nil
}

// Close stops serving the execution's socket and removes its run directory
// (and its fallback socket directory). Its token is revoked already: an
// execution's tokens never outlive its authority.
func (e *Endpoints) Close(id domain.ID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.close(id)
}

func (e *Endpoints) close(id domain.ID) error {
	if ep := e.served[id]; ep != nil {
		_ = ep.srv.Close()
		delete(e.served, id)
	}
	short := filepath.Join(e.short, endpointKey(id))
	if info, err := os.Lstat(short); err == nil && info.IsDir() && checkPrivateDir(short) == nil {
		_ = os.RemoveAll(short)
	}
	return os.RemoveAll(e.Dir(id))
}

// Reconcile closes every endpoint, served or only left on disk by an earlier
// daemon, whose execution live does not report as nonterminal.
func (e *Endpoints) Reconcile(live func([]domain.ID) (map[domain.ID]bool, error)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := map[domain.ID]bool{}
	for id := range e.served {
		ids[id] = true
	}
	entries, err := os.ReadDir(e.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if b, err := os.ReadFile(filepath.Join(e.root, entry.Name(), endpointID)); err == nil && len(b) > 0 {
			ids[domain.ID(b)] = true
		}
	}
	if len(ids) == 0 {
		return nil
	}
	list := make([]domain.ID, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	keep, err := live(list)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range list {
		if !keep[id] {
			errs = append(errs, e.close(id))
		}
	}
	return errors.Join(errs...)
}

// Shutdown stops serving every socket and keeps the run directories: the
// next daemon serves them again when it adopts their attempts.
func (e *Endpoints) Shutdown(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, ep := range e.served {
		_ = ep.srv.Shutdown(ctx)
		delete(e.served, id)
	}
}

// writePrivate writes a 0600 file through a temporary file in the same
// directory and a rename, so a reader never sees a partial credential.
func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
