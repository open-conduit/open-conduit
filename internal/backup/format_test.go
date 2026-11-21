package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// formatBundle publishes a bundle of an empty database at this binary's
// record format.
func formatBundle(t *testing.T, build string) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, err := artifacts.Open(filepath.Join(root, "live-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	bundle := filepath.Join(root, "bundle")
	if err := Create(ctx, bundle, nil, DefaultBackend, db, files, build, kvRetention()); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func lookup(string) (storage.Backend, error) { return sqlite.Backend, nil }

// The manifest names the record format, and Inspect gives the build back
// without the label.
func TestManifestRecordsTheFormat(t *testing.T) {
	info, err := Inspect(formatBundle(t, "v2 (abc)"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Records != sqlite.RecordFormat || info.Build != "v2 (abc)" {
		t.Fatalf("records %d, build %q", info.Records, info.Build)
	}
}

// A bundle from a newer record format is refused from its manifest: no
// private copy is made and the destination stays absent. The same bundle
// restores on the binary that wrote it, the negative control.
func TestNewerBundleIsRefusedBeforeAnythingIsCopied(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	bundle := formatBundle(t, "v2")
	parent := t.TempDir()
	destination := filepath.Join(parent, "restored")
	target := Target{Artifacts: artifacts.Backend}

	restore := sqlite.SimulateRecordFormat(sqlite.RecordFormat - 1)
	err := CheckFormat(bundle, lookup)
	if !errors.Is(err, storage.ErrRecordsNewer) {
		restore()
		t.Fatalf("check: %v", err)
	}
	err = Restore(ctx, bundle, destination, Trust{AcceptUnsigned: true}, now, nil, lookup, target, nil)
	restore()
	if !errors.Is(err, storage.ErrRecordsNewer) {
		t.Fatalf("restore: %v", err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("a refused restore left %v (%v)", entries, err)
	}

	if err := Restore(ctx, bundle, destination, Trust{AcceptUnsigned: true}, now, nil, lookup, target, nil); err != nil {
		t.Fatalf("the binary that wrote it: %v", err)
	}
}

// A bundle from before manifests named a format is left to the backend's own
// check.
func TestBundleWithoutAFormatIsNotRefusedFromItsManifest(t *testing.T) {
	if _, records := parseRecords("v1 (old)"); records != 0 {
		t.Fatalf("records %d", records)
	}
	if err := refuseNewer(manifest{Build: "v1 (old)"}, sqlite.Backend); err != nil {
		t.Fatal(err)
	}
}
