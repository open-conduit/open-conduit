package extension

import (
	"context"
	"io"
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
)

// BackupDestinationConfigSchema labels the config the host hands a backup
// destination: the operator's settings.backup.destinations[].config, an
// object passed on as written. Its shape is the providing package's own,
// which it checks through validate (kind backup_destination).
const BackupDestinationConfigSchema = "core.config.backup_destination.v1"

// BackupManifest is the file whose presence makes a stored bundle complete.
// The host puts it last, so a bundle cut short by a failure is never listed.
const BackupManifest = "manifest.json"

// BackupTarget is one configured destination.
type BackupTarget struct {
	// Name is the operator's name for the destination.
	Name string
	// Config is its config (schema BackupDestinationConfigSchema).
	Config domain.Payload
}

// BackupFile is one file of a bundle: its slash-separated path inside the
// bundle and its size in bytes.
type BackupFile struct {
	Path string
	Size int64
}

// BackupDestination stores backup bundles somewhere other than the host's
// disk. The bytes are opaque to it. The host authenticates what it fetches
// against the operator's trust keys, so a destination is never trusted for
// a bundle's integrity or origin. Retention, versioning, object lock and
// server-side encryption are the destination owner's policy.
type BackupDestination interface {
	// Put stores one file, reading r to its end. A file must become visible
	// only once all of it is stored, and putting a path again replaces it.
	Put(ctx context.Context, t BackupTarget, bundle string, f BackupFile, r io.Reader) error
	// List names the complete bundles: those holding BackupManifest.
	List(ctx context.Context, t BackupTarget) ([]string, error)
	// Files lists a bundle's stored files. A bundle the destination doesn't
	// hold has none: that is an empty list, not an error, because the host
	// asks before uploading.
	Files(ctx context.Context, t BackupTarget, bundle string) ([]BackupFile, error)
	// Read returns up to length bytes of a file from offset, and whether
	// they reach its end.
	Read(ctx context.Context, t BackupTarget, bundle, path string, offset int64, length int) ([]byte, bool, error)
}

var (
	backupBundleName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	backupPathPart   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
)

// ValidBackupBundle says whether name can name a stored bundle.
func ValidBackupBundle(name string) bool { return backupBundleName.MatchString(name) }

// ValidBackupPath says whether p is a safe relative path inside a bundle:
// slash-separated plain names, none of them "." or "..". What a destination
// reports is untrusted, and the host writes fetched files under these paths.
func ValidBackupPath(p string) bool {
	if p == "" || len(p) > 1024 {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || !backupPathPart.MatchString(part) {
			return false
		}
	}
	return true
}
