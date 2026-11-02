package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

var updateRecordFields = flag.Bool("update-record-fields", false, "rewrite testdata/record_fields.txt (needs a RecordFormat bump when the fields changed)")

var formatNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// asBinary runs fn as a binary that writes records in format.
func asBinary(t *testing.T, format uint32, fn func()) {
	t.Helper()
	saved := recordFormat
	recordFormat = format
	defer func() { recordFormat = saved }()
	fn()
}

func putScope(t *testing.T, path string) {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Transact(context.Background(), storage.Guard{Now: formatNow}, func(tx storage.Tx) error {
		sc, err := tx.Scope("scope")
		if errors.Is(err, storage.ErrNotFound) {
			return tx.PutScope(domain.Scope{ID: "scope", Name: "before"}, 0)
		}
		if err != nil {
			return err
		}
		return tx.PutScope(sc, sc.Revision)
	}); err != nil {
		t.Fatal(err)
	}
}

func stateFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(path + suffix)
		if err == nil {
			out[suffix] = b
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return out
}

// An older binary refuses a database a newer one wrote, without changing it,
// and points at a restore.
func TestOlderBinaryRefusesNewerRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conduit.db")
	asBinary(t, RecordFormat+1, func() { putScope(t, path) })
	before := stateFiles(t, path)
	_, err := Open(context.Background(), path)
	if !errors.Is(err, ErrRecordsNewer) || !strings.Contains(err.Error(), "backup restore") {
		t.Fatalf("older binary opened newer records: %v", err)
	}
	if after := stateFiles(t, path); !reflect.DeepEqual(before, after) {
		t.Fatal("refused database was modified")
	}
	// The newer binary itself still opens it.
	asBinary(t, RecordFormat+1, func() {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	})
}

// Negative control: the refusal is what keeps the fields. A binary that opens
// newer records anyway rewrites them through its own structs and loses
// whatever it doesn't know.
func TestRewriteWithoutTheRefusalDropsUnknownFields(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	putScope(t, path)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A newer binary's field, as it would sit in the record.
	if _, err = s.db.ExecContext(ctx, `UPDATE scopes SET record=json_set(CAST(record AS TEXT),'$.FutureField','kept') WHERE id='scope'`); err != nil {
		t.Fatal(err)
	}
	err = s.Transact(ctx, storage.Guard{Now: formatNow}, func(tx storage.Tx) error {
		sc, err := tx.Scope("scope")
		if err != nil {
			return err
		}
		sc.Name = "after"
		return tx.PutScope(sc, sc.Revision)
	})
	if err != nil {
		t.Fatal(err)
	}
	var record []byte
	if err = s.db.QueryRowContext(ctx, "SELECT record FROM scopes WHERE id='scope'").Scan(&record); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(record, []byte("FutureField")) {
		t.Fatal("rewrite kept an unknown field: the downgrade refusal would be unnecessary")
	}
}

// Opening raises the stored format to the binary's and never lowers it.
func TestOpenRecordsTheHighestFormat(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	putScope(t, path)
	stored := func() string {
		db, err := readonlyDB(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var v string
		if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='record_format'").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := stored(); got != fmt.Sprint(RecordFormat) {
		t.Fatalf("format %s recorded, want %d", got, RecordFormat)
	}
	// A database from before the entry existed opens as format 1.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "DELETE FROM metadata WHERE key='record_format'"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	asBinary(t, RecordFormat+2, func() { putScope(t, path) })
	if got := stored(); got != fmt.Sprint(RecordFormat+2) {
		t.Fatalf("format %s after a newer binary, want %d", got, RecordFormat+2)
	}
}

// A snapshot of newer records is refused at restore, before anything is made.
func TestRestoreRefusesNewerRecords(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "snapshot.db")
	asBinary(t, RecordFormat+1, func() {
		putScope(t, filepath.Join(dir, "conduit.db"))
		s, err := Open(ctx, filepath.Join(dir, "conduit.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err = s.Backup(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	})
	restored := filepath.Join(dir, "restored.db")
	if err := Restore(ctx, snapshot, restored, formatNow); !errors.Is(err, ErrRecordsNewer) {
		t.Fatalf("restore accepted newer records: %v", err)
	}
	if _, err := os.Lstat(restored); !os.IsNotExist(err) {
		t.Fatalf("refused restore published: %v", err)
	}
}

// storedRecords are the record types the store encodes as JSON.
var storedRecords = []any{domain.Scope{}, domain.Work{}, domain.AcceptedEvent{}, domain.Execution{}, domain.Timer{}, domain.Wait{}}

// recordFields lists every JSON field path of the given types.
func recordFields(values ...any) []string {
	var out []string
	marshaler := reflect.TypeFor[json.Marshaler]()
	var walk func(prefix string, t reflect.Type, seen map[reflect.Type]bool)
	walk = func(prefix string, t reflect.Type, seen map[reflect.Type]bool) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
			if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
				return
			}
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler) || seen[t] {
			return
		}
		seen[t] = true
		defer delete(seen, t)
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if tag == "-" {
				continue
			}
			if f.Anonymous && tag == "" {
				walk(prefix, f.Type, seen)
				continue
			}
			name := f.Name
			if tag != "" {
				name = tag
			}
			out = append(out, prefix+name+" "+f.Type.Kind().String())
			walk(prefix+name+".", f.Type, seen)
		}
	}
	for _, v := range values {
		t := reflect.TypeOf(v)
		walk(t.Name()+".", t, map[reflect.Type]bool{})
	}
	slices.Sort(out)
	return out
}

// TestRecordFieldsArePinned ties the stored record fields to RecordFormat, so
// a field change can't ship without the bump that stops older binaries
// rewriting the records.
func TestRecordFieldsArePinned(t *testing.T) {
	golden := filepath.Join("testdata", "record_fields.txt")
	got := recordFields(storedRecords...)
	header := fmt.Sprintf("format %d", RecordFormat)
	want := append([]string{header}, got...)
	b, err := os.ReadFile(golden)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(b) > 0 && slices.Equal(lines, want) {
		return
	}
	pinnedHeader := ""
	if len(b) > 0 {
		pinnedHeader = lines[0]
	}
	fieldsChanged := len(b) == 0 || !slices.Equal(lines[1:], got)
	if fieldsChanged && pinnedHeader == header {
		t.Fatalf("the stored record fields changed but RecordFormat is still %d: bump RecordFormat in store.go, then run go test ./internal/storage/sqlite -run TestRecordFieldsArePinned -update-record-fields", RecordFormat)
	}
	if !*updateRecordFields {
		t.Fatalf("%s is out of date (pinned %q, now %q): run go test ./internal/storage/sqlite -run TestRecordFieldsArePinned -update-record-fields", golden, pinnedHeader, header)
	}
	if err = os.WriteFile(golden, []byte(strings.Join(want, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Negative control: the pinned list notices a field a record gains.
func TestRecordFieldsNoticeANewField(t *testing.T) {
	type grown struct {
		domain.Scope
		Added string
	}
	before, after := recordFields(domain.Scope{}), recordFields(grown{})
	for i := range after {
		after[i] = strings.Replace(after[i], "grown.", "Scope.", 1)
	}
	if slices.Equal(before, after) || !slices.Contains(after, "Scope.Added string") {
		t.Fatalf("an added field went unnoticed: %v", after)
	}
}
