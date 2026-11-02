package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMigrationFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	original := schema
	schema = append(append([]string{}, schema[:len(schema)-1]...), "INTENTIONALLY INVALID SQL", schema[len(schema)-1])
	defer func() { schema = original }()
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("migration failure accepted")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var tables, version int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if tables != 0 || version != 0 {
		t.Fatalf("partial migration: tables=%d version=%d", tables, version)
	}
	schema = original
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.SchemaVersion() != 1 {
		t.Fatal("schema not exposed")
	}
}
func TestNewerSchemaIsRefusedWithoutModification(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "future.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA user_version=99; CREATE TABLE future(id TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("newer database was modified")
	}
}
func TestUnversionedAndUnsafeFilesRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "unknown.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE old_state(id TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, path); err == nil || !strings.Contains(err.Error(), "unversioned") {
		t.Fatalf("unknown database reused: %v", err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, path); err == nil {
		t.Fatal("public database opened")
	}
	link := filepath.Join(dir, "link.db")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, link); err == nil {
		t.Fatal("symlinked database opened")
	}
}

// Outcomes stored under the old source-named turn schema are read as
// domain.TurnSchema once the store opens; any other schema is left alone.
// The rewrite runs once: a store that records it as done is not rescanned.
func TestOpenRewritesLegacyTurnSchemas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{"x1": "github.status.v1", "x2": "acme.result.v1", "x3": ".status.v1", "x4": "core.turn.v1"}
	// A store from before the rewrite has no record of it.
	stmts := []string{`DELETE FROM metadata WHERE key='rewrite_turn_schema'`, `INSERT INTO scopes VALUES ('s',NULL,'{}')`, `INSERT INTO works VALUES ('w','s',NULL,'{}')`}
	attempt := 0
	for id, schema := range rows {
		attempt++
		stmts = append(stmts, `INSERT INTO executions VALUES ('`+id+`','w',`+strconv.Itoa(attempt)+`,0,CAST('{"ID":"`+id+`","Outcome":{"Kind":"handoff","Data":{"Schema":"`+schema+`","Data":{"status":"review","n":12345678901234567890}}}}' AS BLOB))`)
	}
	for _, q := range stmts {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if db, err = sql.Open("sqlite", path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	want := map[string]string{"x1": "core.turn.v1", "x2": "acme.result.v1", "x3": ".status.v1", "x4": "core.turn.v1"}
	for id, w := range want {
		var schema, data string
		if err = db.QueryRow(`SELECT json_extract(CAST(record AS TEXT),'$.Outcome.Data.Schema'), json_extract(CAST(record AS TEXT),'$.Outcome.Data.Data') FROM executions WHERE id=?`, id).Scan(&schema, &data); err != nil {
			t.Fatal(err)
		}
		if schema != w || !strings.Contains(data, `"status":"review"`) || !strings.Contains(data, "12345678901234567890") {
			t.Errorf("%s: schema %q data %s, want %q", id, schema, data, w)
		}
	}
	// Once recorded, a later open does not scan again.
	if _, err = db.Exec(`INSERT INTO executions VALUES ('x5','w',9,0,CAST('{"ID":"x5","Outcome":{"Kind":"handoff","Data":{"Schema":"jira.status.v1","Data":{}}}}' AS BLOB))`); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var schema string
	if err = db.QueryRow(`SELECT json_extract(CAST(record AS TEXT),'$.Outcome.Data.Schema') FROM executions WHERE id='x5'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != "jira.status.v1" {
		t.Fatalf("the rewrite ran again on a store that recorded it: %q", schema)
	}
}
