// Package backup composes consistent SQLite snapshots and pinned artifacts.
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
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

type entry struct {
	Ref    domain.ArtifactRef
	SHA256 string
}
type manifest struct {
	Version        uint32
	Schema         uint32
	Build          string
	CreatedAt      time.Time
	DatabaseSHA256 string
	Artifacts      []entry
	// Omitted lists references whose content retention expired. Their bytes
	// are not in the bundle; restore accepts them only on expired Executions.
	Omitted []domain.ArtifactRef `json:",omitempty"`
}

// Create publishes a complete immutable bundle at a previously absent path.
// Pinning precedes the database snapshot so retention cannot remove captured refs.
// References of Executions whose retention expired are recorded as omitted
// rather than copied, since their content may already be gone.
func Create(ctx context.Context, destination string, db *sqlite.Store, files *artifacts.Store, build string) error {
	return publish(ctx, destination, func(stage string) error {
		pin, err := files.Pin()
		if err != nil {
			return err
		}
		defer pin.Close()
		database := filepath.Join(stage, "conduit.db")
		if err := db.Backup(ctx, database); err != nil {
			return err
		}
		snapshot, err := sqlite.Open(ctx, database)
		if err != nil {
			return err
		}
		refs := map[string]domain.ArtifactRef{}
		live := map[string]bool{}
		err = snapshot.Transact(ctx, storage.Guard{Now: time.Now().UTC()}, func(tx storage.Tx) error {
			xs, err := tx.ListExecutions("")
			if err != nil {
				return err
			}
			for _, x := range xs {
				retention, err := runtime.RetentionInTx(tx, x.ID)
				if err != nil {
					return err
				}
				for _, ref := range x.Artifacts {
					if old, ok := refs[ref.ID]; ok && old != ref {
						return storage.ErrConflict
					}
					refs[ref.ID] = ref
					// A ref shared with an unexpired Execution must be copied.
					live[ref.ID] = live[ref.ID] || !retention.Expired
				}
			}
			return nil
		})
		closeErr := snapshot.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
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

// Restore verifies all bundle bytes before publishing a new isolated state
// directory. The SQLite restore fences old lease authority. No daemon may own
// destination; an existing path is rejected, never overwritten.
func Restore(ctx context.Context, bundle, destination string, now time.Time) error {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return err
	}
	database := filepath.Join(bundle, "conduit.db")
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
		if err := sqlite.Restore(ctx, database, filepath.Join(stage, "conduit.db"), now); err != nil {
			return err
		}
		restored, err := sqlite.Open(ctx, filepath.Join(stage, "conduit.db"))
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
				retention, err := runtime.RetentionInTx(tx, x.ID)
				if err != nil {
					return err
				}
				for _, ref := range x.Artifacts {
					if captured[ref.ID] == ref {
						continue
					}
					if retention.Expired && omitted[ref.ID] == ref {
						continue
					}
					return fmt.Errorf("backup: referenced artifact omitted or changed")
				}
			}
			return nil
		})
	})
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
	return Info{Schema: m.Schema, Build: m.Build, CreatedAt: m.CreatedAt, Artifacts: len(m.Artifacts), Omitted: len(m.Omitted)}, nil
}
