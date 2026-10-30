package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

const authNamespace = "core.management.auth"

// ErrCredentialExists prevents implicit replacement of an operator credential.
var ErrCredentialExists = errors.New("operator credential already exists")

type operatorRecord struct {
	TokenID   string
	Revoked   bool
	CreatedAt time.Time
	RevokedAt time.Time
}

// TokenFile is the private operator credential file shape.
type TokenFile struct {
	TokenID string `json:"token_id"`
	Token   string `json:"token"`
}

func newTokenID(prefix string) (string, error) {
	b, err := runtime.NewBearer()
	if err != nil {
		return "", err
	}
	return prefix + b[:22], nil
}

// issueOperatorInTx stores only the digest of a fresh operator bearer.
func issueOperatorInTx(tx storage.Tx, now time.Time) (TokenFile, error) {
	token, err := runtime.NewBearer()
	if err != nil {
		return TokenFile{}, err
	}
	id, err := newTokenID("otok_")
	if err != nil {
		return TokenFile{}, err
	}
	digest := runtime.TokenDigest(token)
	b, _ := json.Marshal(operatorRecord{TokenID: id, CreatedAt: now})
	if err = tx.Put(authNamespace, "operator/"+digest, b); err != nil {
		return TokenFile{}, err
	}
	b, _ = json.Marshal(digest)
	return TokenFile{TokenID: id, Token: token}, tx.Put(authNamespace, "operator_id/"+id, b)
}

func revokeAllOperatorsInTx(tx storage.Tx, now time.Time) error {
	rows, err := tx.List(authNamespace, "operator/")
	if err != nil {
		return err
	}
	for _, row := range rows {
		var r operatorRecord
		if err = json.Unmarshal(row.Value, &r); err != nil {
			return err
		}
		if r.Revoked {
			continue
		}
		r.Revoked, r.RevokedAt = true, now
		b, _ := json.Marshal(r)
		if err = tx.Put(authNamespace, row.Key, b); err != nil {
			return err
		}
	}
	return nil
}

// WriteTokenFile atomically writes an owner-only credential file.
func WriteTokenFile(path string, f TokenFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(append(b, '\n')); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ReadTokenFile reads and checks a private credential file.
func ReadTokenFile(path string) (TokenFile, error) {
	var f TokenFile
	info, err := os.Lstat(path)
	if err != nil {
		return f, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return f, fmt.Errorf("unsafe credential file permissions")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err = decodeStrict(b, &f); err != nil || f.TokenID == "" || f.Token == "" {
		return TokenFile{}, fmt.Errorf("invalid credential file")
	}
	return f, nil
}

// BootstrapOperator creates the first operator credential under the daemon
// lock. It never replaces an existing private file or active credential.
func BootstrapOperator(ctx context.Context, store storage.Store, now func() time.Time, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return ErrCredentialExists
	}
	var file TokenFile
	err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		rows, err := tx.List(authNamespace, "operator/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var r operatorRecord
			if err = json.Unmarshal(row.Value, &r); err == nil && !r.Revoked {
				return ErrCredentialExists
			}
		}
		file, err = issueOperatorInTx(tx, now())
		return err
	})
	if err != nil {
		return err
	}
	return WriteTokenFile(path, file)
}

// RecoverOperator is the locked local recovery/rotation path for a lost
// credential or response: it revokes every operator credential (including any
// ambiguous replacement) and writes a fresh private file.
func RecoverOperator(ctx context.Context, store storage.Store, now func() time.Time, path string) error {
	var file TokenFile
	err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		if err := revokeAllOperatorsInTx(tx, now()); err != nil {
			return err
		}
		var err error
		file, err = issueOperatorInTx(tx, now())
		return err
	})
	if err != nil {
		return err
	}
	return WriteTokenFile(path, file)
}

// Lock is the exclusive per-state-directory daemon lock.
type Lock struct{ f *os.File }

// AcquireLock refuses unsafe state directories and concurrent instances.
func AcquireLock(stateDir string) (*Lock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(stateDir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "daemon.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state directory is in use by another instance")
	}
	return &Lock{f}, nil
}
func (l *Lock) Close() error { return l.f.Close() }

func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe directory permissions: directories must be owner-only 0700")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("unsafe directory ownership")
	}
	return nil
}

// Listen binds the owner-only Unix socket. The caller must hold the daemon
// lock; a stale socket is removed only after ownership checks.
func Listen(_ *Lock, socket string) (net.Listener, error) {
	if !filepath.IsAbs(socket) || len(socket) > maxSocketPath {
		return nil, fmt.Errorf("socket path must be an absolute path shorter than the platform limit; use --socket")
	}
	return listenUnix(socket)
}

// maxSocketPath leaves room under the smallest platform limit for a Unix
// socket path (104 bytes on macOS, including the terminating NUL).
const maxSocketPath = 100

// listenUnix binds an owner-only socket in an owner-only directory, removing
// a stale socket left at the path only when this user owns it.
func listenUnix(socket string) (net.Listener, error) {
	if err := checkPrivateDir(filepath.Dir(socket)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socket); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if info.Mode()&os.ModeSocket == 0 || !ok || int(st.Uid) != os.Getuid() {
			return nil, fmt.Errorf("refusing to replace unexpected file at socket path")
		}
		if err = os.Remove(socket); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(socket, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
