package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

func writeChoice(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, storageFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Without a storage choice a state directory is SQLite at conduit.db.
func TestStateDefaultsToSQLite(t *testing.T) {
	dir := t.TempDir()
	c, err := coordinationOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.name != "sqlite" || c.location != filepath.Join(dir, "conduit.db") || c.chosen || c.present(dir) {
		t.Fatalf("default coordination: %+v", c)
	}
}

// storage.json selects a registered backend by name and hands it the
// location as written.
func TestStorageChoiceSelectsTheBackend(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "kept-here.db")
	writeChoice(t, dir, `{"backend": "sqlite", "location": "`+elsewhere+`"}`)
	c, db, err := openState(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if c.location != elsewhere || !c.present(dir) {
		t.Fatalf("chosen coordination: %+v", c)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("backend did not open the chosen location: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "conduit.db")); !os.IsNotExist(err) {
		t.Fatalf("default location used anyway: %v", err)
	}
}

// A choice this binary can't honour stops every entry point rather than
// falling back to SQLite, which would start an empty state beside the real
// one.
func TestStorageChoiceRefusesWhatItCannotHonour(t *testing.T) {
	for name, body := range map[string]string{
		"unknown_backend": `{"backend": "postgres"}`,
		"no_backend":      `{"location": "x"}`,
		"unknown_field":   `{"backend": "sqlite", "dsn": "x"}`,
		"trailing_data":   `{"backend": "sqlite"} {}`,
		"not_json":        `sqlite`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeChoice(t, dir, body)
			if _, _, err := openState(context.Background(), dir); err == nil || !strings.Contains(err.Error(), storageFile) {
				t.Fatalf("got %v, want a %s error", err, storageFile)
			}
			if _, err := locked(dir); err == nil {
				t.Fatal("offline state command accepted the directory")
			}
			if _, err := os.Stat(filepath.Join(dir, "conduit.db")); !os.IsNotExist(err) {
				t.Fatal("fell back to the default database")
			}
		})
	}
	dir := t.TempDir()
	writeChoice(t, dir, `{"backend": "postgres"}`)
	if _, err := coordinationOf(dir); err == nil || !strings.Contains(err.Error(), "sqlite") {
		t.Fatalf("unknown backend error should list the built-in ones: %v", err)
	}
}

// A bundle restores with the backend it names, and the restored directory
// records it unless it is the default.
func TestRestoredDirectoryRecordsItsBackend(t *testing.T) {
	dir := t.TempDir()
	if err := recordChoice(dir, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, storageFile)); !os.IsNotExist(err) {
		t.Fatal("default backend recorded")
	}
	if _, err := storage.LookupBackend("bootstrap-test"); err != nil {
		storage.RegisterBackend("bootstrap-test", fakeBackend{})
	}
	if err := recordChoice(dir, "bootstrap-test"); err != nil {
		t.Fatal(err)
	}
	c, err := coordinationOf(dir)
	if err != nil || c.name != "bootstrap-test" || c.location != filepath.Join(dir, "conduit.db") {
		t.Fatalf("recorded choice: %+v, %v", c, err)
	}
}

type fakeBackend struct{}

func (fakeBackend) Open(context.Context, string) (storage.Database, error) {
	return nil, storage.ErrInvalid
}
func (fakeBackend) Inspect(context.Context, string) (storage.Inspection, error) {
	return nil, storage.ErrInvalid
}
func (fakeBackend) Restore(context.Context, string, string, time.Time) error {
	return storage.ErrInvalid
}
