// Package backup composes a coordination backend's consistent snapshot with
// pinned artifacts. It works over the storage lifecycle contract
// (storage.Backend, storage.Database), never a particular backend.
// Backups contain private runtime state and must remain owner-only.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/storage"
)

type entry struct {
	Ref    domain.ArtifactRef
	SHA256 string
}
type manifest struct {
	Version uint32
	// Backend names the storage backend whose snapshot the bundle holds;
	// empty is SQLite, which every bundle held before backends had names
	// (left out for it, so older binaries still read SQLite bundles).
	Backend        string `json:",omitempty"`
	Schema         uint32
	Build          string
	CreatedAt      time.Time
	DatabaseSHA256 string
	Artifacts      []entry
	// Omitted lists references whose content retention expired. Their bytes
	// are not in the bundle; restore accepts them only on expired Executions.
	Omitted []domain.ArtifactRef `json:",omitempty"`
}

// Expired reports whether retention has expired an Execution's content. The
// runtime owns the retention ledger, so callers supply the lookup; nil means
// no Execution has expired.
type Expired func(tx storage.Tx, id domain.ID) (bool, error)

func (e Expired) of(tx storage.Tx, id domain.ID) (bool, error) {
	if e == nil {
		return false, nil
	}
	return e(tx, id)
}

// DefaultBackend is the backend a manifest that names none was made with.
const DefaultBackend = "sqlite"

// snapshotName is the coordination snapshot's file in a bundle. It keeps
// the name SQLite bundles always had, whatever the backend.
const snapshotName = "conduit.db"

// Create publishes a complete immutable bundle at a previously absent path.
// Pinning precedes the database snapshot so retention cannot remove captured refs.
// References of Executions whose retention expired are recorded as omitted
// rather than copied, since their content may already be gone. backend is
// the name db's Backend is registered under; Restore looks it up again.
func Create(ctx context.Context, destination string, backend string, db storage.Database, files *artifacts.Store, build string, expired Expired) error {
	if backend == "" {
		return storage.ErrInvalid
	}
	return publish(ctx, destination, func(stage string) error {
		pin, err := files.Pin()
		if err != nil {
			return err
		}
		defer pin.Close()
		database := filepath.Join(stage, snapshotName)
		var refs map[string]domain.ArtifactRef
		var live map[string]bool
		// References are read from the snapshot's own state, not the live
		// database, so the bundle copies exactly what its snapshot names.
		err = db.Snapshot(ctx, database, func(tx storage.Tx) error {
			refs, live = map[string]domain.ArtifactRef{}, map[string]bool{}
			xs, err := tx.ListExecutions("")
			if err != nil {
				return err
			}
			for _, x := range xs {
				gone, err := expired.of(tx, x.ID)
				if err != nil {
					return err
				}
				for _, ref := range x.AllArtifacts() {
					if old, ok := refs[ref.ID]; ok && old != ref {
						return storage.ErrConflict
					}
					refs[ref.ID] = ref
					// A ref shared with an unexpired Execution must be copied.
					live[ref.ID] = live[ref.ID] || !gone
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		output, err := artifacts.Open(filepath.Join(stage, "artifacts"))
		if err != nil {
			return err
		}
		defer output.Close()
		m := manifest{Version: 1, Schema: db.SchemaVersion(), Build: build, CreatedAt: time.Now().UTC()}
		// Sorting makes manifests reproducible apart from their creation time.
		for _, ref := range sortedRefs(refs) {
			if !live[ref.ID] {
				m.Omitted = append(m.Omitted, ref)
				continue
			}
			r, err := pin.Open(ctx, ref)
			if err != nil {
				return err
			}
			h := sha256.New()
			err = output.Import(ctx, ref, io.TeeReader(r, h))
			readCloseErr := r.Close()
			if err != nil {
				return err
			}
			if readCloseErr != nil {
				return readCloseErr
			}
			m.Artifacts = append(m.Artifacts, entry{ref, hex.EncodeToString(h.Sum(nil))})
		}
		if backend != DefaultBackend {
			m.Backend = backend
		}
		m.DatabaseSHA256, err = digestFile(ctx, database)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		return writeDurable(filepath.Join(stage, "manifest.json"), append(data, '\n'))
	})
}

// Backends finds a storage backend by the name a bundle records
// (storage.LookupBackend).
type Backends func(name string) (storage.Backend, error)

// Record writes into a restored state directory, before it is published,
// whatever its caller needs to open it with backend (the composition
// root's storage choice). The directory is published with it or not at
// all. nil records nothing.
type Record func(dir, backend string) error

// Restore verifies all bundle bytes before publishing a new isolated state
// directory. The bundle's backend restores its snapshot to conduit.db in
// that directory and fences old lease authority. No daemon may own
// destination; an existing path is rejected, never overwritten.
func Restore(ctx context.Context, bundle, destination string, now time.Time, expired Expired, backends Backends, record Record) error {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return err
	}
	backend, err := backends(m.backend())
	if err != nil {
		return err
	}
	database := filepath.Join(bundle, snapshotName)
	hash, err := digestFile(ctx, database)
	if err != nil {
		return err
	}
	if hash != m.DatabaseSHA256 {
		return fmt.Errorf("backup: database digest mismatch")
	}
	input, err := artifacts.Open(filepath.Join(bundle, "artifacts"))
	if err != nil {
		return err
	}
	defer input.Close()
	pin, err := input.Pin()
	if err != nil {
		return err
	}
	defer pin.Close()
	return publish(ctx, destination, func(stage string) error {
		output, err := artifacts.Open(filepath.Join(stage, "artifacts"))
		if err != nil {
			return err
		}
		defer output.Close()
		seen := map[string]bool{}
		for _, item := range m.Artifacts {
			if seen[item.Ref.ID] {
				return fmt.Errorf("backup: duplicate artifact")
			}
			seen[item.Ref.ID] = true
			r, err := pin.Open(ctx, item.Ref)
			if err != nil {
				return err
			}
			h := sha256.New()
			err = output.Import(ctx, item.Ref, io.TeeReader(r, h))
			closeErr := r.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
				return fmt.Errorf("backup: artifact digest mismatch")
			}
		}
		if err := backend.Restore(ctx, database, filepath.Join(stage, snapshotName), now); err != nil {
			return err
		}
		if record != nil {
			if err := record(stage, m.backend()); err != nil {
				return err
			}
		}
		restored, err := backend.Open(ctx, filepath.Join(stage, snapshotName))
		if err != nil {
			return err
		}
		defer restored.Close()
		omitted := map[string]domain.ArtifactRef{}
		for _, ref := range m.Omitted {
			if seen[ref.ID] {
				return fmt.Errorf("backup: duplicate artifact")
			}
			seen[ref.ID] = true
			omitted[ref.ID] = ref
		}
		return restored.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			xs, err := tx.ListExecutions("")
			if err != nil {
				return err
			}
			captured := map[string]domain.ArtifactRef{}
			for _, item := range m.Artifacts {
				captured[item.Ref.ID] = item.Ref
			}
			for _, x := range xs {
				gone, err := expired.of(tx, x.ID)
				if err != nil {
					return err
				}
				for _, ref := range x.AllArtifacts() {
					if captured[ref.ID] == ref {
						continue
					}
					if gone && omitted[ref.ID] == ref {
						continue
					}
					return fmt.Errorf("backup: referenced artifact omitted or changed")
				}
			}
			return nil
		})
	})
}

func (m manifest) backend() string {
	if m.Backend == "" {
		return DefaultBackend
	}
	return m.Backend
}

func readManifest(path string) (manifest, error) {
	var m manifest
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return m, fmt.Errorf("backup: invalid manifest size or type")
	}
	d := json.NewDecoder(io.LimitReader(f, 16<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return m, fmt.Errorf("backup: trailing manifest data")
	}
	if m.Version != 1 || m.Schema != 1 || m.CreatedAt.IsZero() || len(m.DatabaseSHA256) != 64 {
		return m, fmt.Errorf("backup: unsupported or incomplete manifest")
	}
	return m, nil
}

func publish(ctx context.Context, destination string, create func(string) error) error {
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("backup: destination already exists")
	}
	parent := filepath.Dir(destination)
	stage, err := os.MkdirTemp(parent, ".conduit-snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := create(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return syncDirectory(parent)
}

func sortedRefs(refs map[string]domain.ArtifactRef) []domain.ArtifactRef {
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]domain.ArtifactRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, refs[id])
	}
	return out
}

func digestFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Info summarizes a bundle's manifest.
type Info struct {
	// Backend names the storage backend the snapshot is for.
	Backend   string
	Schema    uint32
	Build     string
	CreatedAt time.Time
	Artifacts int
	// Omitted counts references whose expired content the bundle leaves out.
	Omitted int
}

// Inspect reads and checks a bundle's manifest. It does not verify the
// bundle's bytes; Restore does.
func Inspect(bundle string) (Info, error) {
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return Info{}, err
	}
	return Info{Backend: m.backend(), Schema: m.Schema, Build: m.Build, CreatedAt: m.CreatedAt, Artifacts: len(m.Artifacts), Omitted: len(m.Omitted)}, nil
}
