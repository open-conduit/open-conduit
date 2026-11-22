package sqlite

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// These tests SIGKILL a real process in the middle of a commit. The child is
// this test binary, re-run with crashChildEnv set. The parent watches for the
// files SQLite creates inside the commit and kills as soon as it sees one, so
// no test sleeps or depends on how fast the disk is.
const crashChildEnv = "OPEN_CONDUIT_SQLITE_CRASH_CHILD"

const (
	crashBatch  = 64
	crashRounds = 60
	crashWithin = 3
	// crashPatience is how many commits one round watches before moving on.
	crashPatience = 32
	rebuildKills  = 6
)

func crashEvent(sequence uint64) domain.AcceptedEvent {
	month := time.Month(sequence/(crashBatch*4)%12 + 1)
	e := archiveRecord(domain.ID("crash-"+strconv.FormatUint(sequence, 10)), sequence, time.Date(2026, month, 1, 0, 0, 0, 0, time.UTC))
	e.Event.Data = domain.Payload{Schema: "test.crash.v1", Data: json.RawMessage(fmt.Sprintf(`{"padding":%q}`, strings.Repeat("x", 16<<10)))}
	return e
}

func crashBatchOf(n uint64) []domain.AcceptedEvent {
	out := make([]domain.AcceptedEvent, 0, crashBatch)
	for i := range uint64(crashBatch) {
		out = append(out, crashEvent(n*crashBatch+i+1))
	}
	return out
}

// TestCrashChild is the process the crash tests kill.
func TestCrashChild(t *testing.T) {
	mode := os.Getenv(crashChildEnv)
	if mode == "" {
		t.Skip("run by the crash tests")
	}
	ctx := context.Background()
	location := os.Getenv(crashChildEnv + "_PATH")
	switch mode {
	case "archive":
		next, err := strconv.ParseUint(os.Getenv(crashChildEnv+"_NEXT"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		a, err := ArchiveBackend.Open(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		for n := next; ; n++ {
			if err = a.PutEvents(ctx, crashBatchOf(n)); err != nil {
				t.Fatal(err)
			}
			fmt.Printf("acked %d\n", n)
		}
	case "rebuild":
		s, err := Open(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("rebuilding")
		if err = s.Rebuild(ctx); err != nil {
			t.Fatal(err)
		}
		fmt.Println("rebuilt")
		// Held open so only the parent's kill ends it.
		select {}
	}
}

func startCrashChild(t *testing.T, mode, path string, env ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.v=false")
	cmd.Env = append(append(os.Environ(), crashChildEnv+"="+mode, crashChildEnv+"_PATH="+path), env...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, bufio.NewScanner(out)
}

func superJournals(dir string) map[string]bool {
	found, _ := filepath.Glob(filepath.Join(dir, "index.db-mj*"))
	out := make(map[string]bool, len(found))
	for _, f := range found {
		out[f] = true
	}
	return out
}

func newSuperJournals(dir string, stale map[string]bool) []string {
	var out []string
	for f := range superJournals(dir) {
		if !stale[f] {
			out = append(out, f)
		}
	}
	return out
}

// journalIsHot says SQLite must roll this journal back: until a journal is
// synced its first byte is zero, and SQLite ignores it.
func journalIsHot(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var first [1]byte
	n, _ := f.Read(first[:])
	return n == 1 && first[0] != 0
}

// archiveTorn is the first way the index and the month files disagree: an
// entry with no record, or a record with no entry.
func archiveTorn(ctx context.Context, dir string) error {
	index, err := openArchivePeriod(filepath.Join(dir, "index.db"))
	if err != nil {
		return err
	}
	defer index.Close()
	listed := map[string]map[uint64]bool{}
	rows, err := index.QueryContext(ctx, "SELECT sequence, period FROM records")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sequence uint64
		var period string
		if err = rows.Scan(&sequence, &period); err != nil {
			return err
		}
		if listed[period] == nil {
			listed[period] = map[uint64]bool{}
		}
		listed[period][sequence] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	months, err := filepath.Glob(filepath.Join(dir, "events-*.db"))
	if err != nil {
		return err
	}
	for _, path := range months {
		period := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "events-"), ".db")
		held, err := archiveMonthSequences(ctx, path)
		if err != nil {
			return fmt.Errorf("%s: %w", period, err)
		}
		for sequence := range held {
			if !listed[period][sequence] {
				return fmt.Errorf("%s holds record %d with no index entry", period, sequence)
			}
		}
		for sequence := range listed[period] {
			if !held[sequence] {
				return fmt.Errorf("index lists %d in %s, which has no such record", sequence, period)
			}
		}
		delete(listed, period)
	}
	for period := range listed {
		return fmt.Errorf("index lists month %s, which has no file", period)
	}
	return nil
}

func archiveMonthSequences(ctx context.Context, path string) (map[uint64]bool, error) {
	db, err := openArchivePeriod(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// A month created by a commit that rolled back has no table yet.
	var tables int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='events'").Scan(&tables); err != nil {
		return nil, err
	}
	held := map[uint64]bool{}
	if tables == 0 {
		return held, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT sequence FROM events")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sequence uint64
		if err = rows.Scan(&sequence); err != nil {
			return nil, err
		}
		held[sequence] = true
	}
	return held, rows.Err()
}

// A kill during the commit across the index and a month leaves both or
// neither, and everything PutEvents acknowledged survives.
func TestArchiveCommitSurvivesAKill(t *testing.T) {
	if testing.Short() {
		t.Skip("kills a real process repeatedly")
	}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "archive")
	var next uint64
	var acknowledged int64 = -1
	inCommit, hot := 0, 0
	for round := 0; round < crashRounds && hot < crashWithin; round++ {
		// SQLite leaves a super-journal behind when the kill lands before any
		// file refers to it, which is harmless, so only a new one counts.
		stale := superJournals(dir)
		cmd, out := startCrashChild(t, "archive", dir, crashChildEnv+"_NEXT="+strconv.FormatUint(next, 10))
		acks := make(chan int64, 1024)
		go func() {
			defer close(acks)
			for out.Scan() {
				if v, ok := strings.CutPrefix(out.Text(), "acked "); ok {
					n, _ := strconv.ParseInt(v, 10, 64)
					acks <- n
				}
			}
		}()
		// Let it commit at least once, so a round makes progress.
		first, ok := <-acks
		if !ok {
			t.Fatal("child exited before its first commit")
		}
		acknowledged = max(acknowledged, first)
		committed := 0
	watch:
		for {
			select {
			case n, ok := <-acks:
				if !ok {
					t.Fatal("child exited on its own")
				}
				acknowledged = max(acknowledged, n)
				committed++
			default:
			}
			// The riskiest moment: the journals are synced and the files are
			// being overwritten, so only the super-journal decides the outcome.
			if len(newSuperJournals(dir, stale)) > 0 && journalIsHot(filepath.Join(dir, "index.db-journal")) {
				break watch
			}
			if committed >= crashPatience {
				break watch
			}
		}
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		for n := range acks {
			acknowledged = max(acknowledged, n)
		}
		_ = cmd.Wait()
		// A super-journal left behind means the kill landed inside the commit.
		if len(newSuperJournals(dir, stale)) > 0 {
			inCommit++
			if journalIsHot(filepath.Join(dir, "index.db-journal")) {
				hot++
			}
		}
		a, err := ArchiveBackend.Open(ctx, dir)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		if err = archiveTorn(ctx, dir); err != nil {
			a.Close()
			t.Fatalf("round %d: %v", round, err)
		}
		for n := max(0, acknowledged-7); n <= acknowledged; n++ {
			for _, e := range []domain.AcceptedEvent{crashEvent(uint64(n)*crashBatch + 1), crashEvent(uint64(n+1) * crashBatch)} {
				got, err := a.Event(ctx, e.Event.ID)
				if err != nil {
					a.Close()
					t.Fatalf("round %d: acknowledged %s: %v", round, e.Event.ID, err)
				}
				if !storage.SameAcceptedEvent(got, e) {
					a.Close()
					t.Fatalf("round %d: %s came back changed", round, e.Event.ID)
				}
			}
		}
		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
		next = uint64(acknowledged + 1)
	}
	t.Logf("%d kills inside a commit, %d of them with a journal SQLite had to roll back", inCommit, hot)
	if hot < crashWithin {
		t.Fatalf("%d kills landed inside a commit and %d needed a rollback", inCommit, hot)
	}
}

// The check above catches each way a commit could tear.
func TestArchiveTornCheckCatchesATear(t *testing.T) {
	ctx := context.Background()
	for name, tear := range map[string]string{
		"record without an entry": "INSERT INTO events(sequence,id,record) VALUES(999,'stray','{}')",
		"entry without a record":  "DELETE FROM events WHERE sequence=1",
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "archive")
			a, err := ArchiveBackend.Open(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.PutEvents(ctx, crashBatchOf(0)); err != nil {
				t.Fatal(err)
			}
			if err = a.Close(); err != nil {
				t.Fatal(err)
			}
			if err = archiveTorn(ctx, dir); err != nil {
				t.Fatalf("intact archive: %v", err)
			}
			month, err := filepath.Glob(filepath.Join(dir, "events-*.db"))
			if err != nil || len(month) != 1 {
				t.Fatalf("months %v, %v", month, err)
			}
			db, err := sql.Open("sqlite", month[0])
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.ExecContext(ctx, tear); err != nil {
				t.Fatal(err)
			}
			if err = archiveTorn(ctx, dir); err == nil {
				t.Fatal("the check missed the tear")
			}
		})
	}
}

// olderDatabase is a database from before incremental mode with live records
// and enough free pages that a rebuild has real work to do.
func olderDatabase(t *testing.T, path string, kept int) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.ExecContext(ctx, "PRAGMA auto_vacuum=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "VACUUM"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	value := []byte(strings.Repeat("k", 16<<10))
	for i := range kept {
		if err = s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			return tx.Put("kept", strconv.Itoa(i), value)
		}); err != nil {
			t.Fatal(err)
		}
	}
	fillAndFree(t, s, 256)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
}

// A kill during the rebuild leaves the database whole, in its old mode or
// its new one, with every record, and a later rebuild still works.
func TestRebuildSurvivesAKill(t *testing.T) {
	if testing.Short() {
		t.Skip("kills a real process repeatedly")
	}
	ctx := context.Background()
	const kept = 128
	template := filepath.Join(t.TempDir(), "template.db")
	olderDatabase(t, template, kept)
	killedDuring := 0
	for round := range rebuildKills * 5 {
		if killedDuring >= rebuildKills {
			break
		}
		path := filepath.Join(t.TempDir(), "conduit.db")
		copyFile(t, template, path)
		cmd, out := startCrashChild(t, "rebuild", path)
		lines := make(chan string, 4)
		go func() {
			defer close(lines)
			for out.Scan() {
				lines <- out.Text()
			}
		}()
		for line := range lines {
			if line == "rebuilding" {
				break
			}
		}
		// VACUUM writes through the WAL, so a growing WAL is the rewrite.
		wal := path + "-wal"
		rebuiltFirst := false
	grow:
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatal("child exited on its own")
				}
				if line == "rebuilt" {
					rebuiltFirst = true
					break grow
				}
			default:
			}
			if info, err := os.Stat(wal); err == nil && info.Size() > 256<<10 {
				break grow
			}
		}
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		finished := rebuiltFirst
		for line := range lines {
			finished = finished || line == "rebuilt"
		}
		_ = cmd.Wait()
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		// Still in the old mode means the kill landed before the commit,
		// and SQLite rolled the rewrite back.
		if before, err := s.Space(ctx); err == nil && !finished && !before.Incremental {
			killedDuring++
		}
		if err = s.IntegrityCheck(ctx); err != nil {
			s.Close()
			t.Fatalf("round %d: %v", round, err)
		}
		checkKept := func() {
			t.Helper()
			for i := range kept {
				err := s.Transact(ctx, storage.Guard{Now: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}, func(tx storage.Tx) error {
					b, err := tx.Get("kept", strconv.Itoa(i))
					if err == nil && len(b) != 16<<10 {
						return errors.New("wrong length")
					}
					return err
				})
				if err != nil {
					s.Close()
					t.Fatalf("round %d: record %d: %v", round, i, err)
				}
			}
		}
		checkKept()
		if err = s.Rebuild(ctx); err != nil {
			s.Close()
			t.Fatalf("round %d: rebuild after the kill: %v", round, err)
		}
		space, err := s.Space(ctx)
		if err != nil || !space.Incremental {
			s.Close()
			t.Fatalf("round %d: after rebuilding: %+v, %v", round, space, err)
		}
		checkKept()
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if killedDuring < rebuildKills {
		t.Fatalf("only %d kills landed before a rebuild committed", killedDuring)
	}
}
