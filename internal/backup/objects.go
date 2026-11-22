package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// A scheduled bundle names its artifacts rather than holding them. Each is an
// object: a directory beside the bundles, named by the digest of the one file
// it holds, stored once however many bundles need it. Like a segment it has no
// manifest, so a destination never lists it as a bundle. The signed manifest
// lists every object's digest, so it vouches for the bytes.
//
// One copy now stands behind every kept bundle, so a held object is hashed
// before it is reused, and a damaged one is replaced from the live store,
// which also repairs the older bundles that name it.

const (
	objectPrefix = "artifact-object-"
	// ObjectFile is the file of content in an object.
	ObjectFile = "content"
)

var objectName = regexp.MustCompile(`^artifact-object-[0-9a-f]{64}$`)

func (e entry) objectName() string { return objectPrefix + e.SHA256 }

// checkObjects refuses a list a restore must not act on. A digest becomes a
// path, and the manifest of an unsigned bundle is whatever someone wrote.
func checkObjects(entries []entry) error {
	for _, e := range entries {
		if !objectName.MatchString(e.objectName()) {
			return fmt.Errorf("backup: the manifest names an invalid artifact object %q: %w", e.SHA256, storage.ErrInvalid)
		}
	}
	return nil
}

// SharedPart is something a bundle needs that is stored beside it: an event
// archive segment or an artifact object. Name is its directory and File the
// one file in it.
type SharedPart struct {
	Name, File string
}

// SharedParts are the stored parts a bundle needs, each named once.
func SharedParts(bundle string) ([]SharedPart, error) {
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if err := checkObjects(m.SharedArtifacts); err != nil {
		return nil, err
	}
	var parts []SharedPart
	if m.Archive != nil {
		if err := m.Archive.check(); err != nil {
			return nil, err
		}
		for _, s := range m.Archive.Segments {
			parts = append(parts, SharedPart{s.name(), ArchiveFile})
		}
	}
	seen := make(map[string]bool, len(m.SharedArtifacts))
	for _, e := range m.SharedArtifacts {
		if name := e.objectName(); !seen[name] {
			seen[name] = true
			parts = append(parts, SharedPart{name, ObjectFile})
		}
	}
	return parts, nil
}

// partFile is the file a stored part's name says it holds, and that file's
// digest.
func partFile(name string) (file, digest string, ok bool) {
	switch {
	case segmentName.MatchString(name):
		return ArchiveFile, strings.TrimPrefix(name, segmentPrefix), true
	case objectName.MatchString(name):
		return ObjectFile, strings.TrimPrefix(name, objectPrefix), true
	}
	return "", "", false
}

// HoldsPart reports whether parent has an intact copy of a named part.
func HoldsPart(ctx context.Context, parent, name string) bool {
	file, digest, ok := partFile(name)
	if !ok {
		return false
	}
	got, err := digestFile(ctx, filepath.Join(parent, name, file))
	return err == nil && got == digest
}

// AdoptPart publishes the part's file in dir as name in parent, once its bytes
// match the digest the name carries.
func AdoptPart(ctx context.Context, parent, name, dir string) error {
	file, digest, ok := partFile(name)
	if !ok {
		return fmt.Errorf("backup: %q is not a stored part's name: %w", name, storage.ErrInvalid)
	}
	got, err := digestFile(ctx, filepath.Join(dir, file))
	if err != nil {
		return err
	}
	if got != digest {
		return fmt.Errorf("backup: digest mismatch in %s", name)
	}
	return publishPart(ctx, parent, name, dir)
}

// storeObject makes sure parent holds ref's content as an object. It opens the
// live content even when the object is held, so content that has gone from
// the store still fails the backup rather than passing unnoticed.
func storeObject(ctx context.Context, parent string, files storage.ArtifactStore, ref domain.ArtifactRef) (entry, error) {
	r, err := files.Open(ctx, ref)
	if err != nil {
		return entry{}, err
	}
	defer r.Close()
	if ref.SHA256 != "" && HoldsPart(ctx, parent, objectPrefix+ref.SHA256) {
		return entry{ref, ref.SHA256}, nil
	}
	dir, err := os.MkdirTemp(parent, ".conduit-snapshot-")
	if err != nil {
		return entry{}, err
	}
	defer os.RemoveAll(dir)
	out, err := os.OpenFile(filepath.Join(dir, ObjectFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return entry{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), r)
	if err == nil {
		err = out.Sync()
	}
	if err = errors.Join(err, out.Close()); err != nil {
		return entry{}, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if n != ref.Size || (ref.SHA256 != "" && digest != ref.SHA256) {
		return entry{}, fmt.Errorf("backup: artifact %s doesn't match its reference: %w", ref.ID, storage.ErrCorrupt)
	}
	if err := publishPart(ctx, parent, objectPrefix+digest, dir); err != nil {
		return entry{}, err
	}
	return entry{ref, digest}, nil
}

// openObject opens a bundle's object from beside it. The caller checks the
// bytes as it reads them.
func openObject(parent string, e entry) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(parent, e.objectName(), ObjectFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("backup: artifact object %s is missing: it is stored beside the bundle: %w", e.objectName(), err)
	}
	return f, err
}
