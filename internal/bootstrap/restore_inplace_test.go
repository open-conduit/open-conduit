package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// inPlaceFixture is a stopped state directory with a bundle taken when it
// held execution x-before, and a later timeline after it: execution
// x-after, toolbelt run directories for both, an executor's and an
// extension's private state, and the directory's own keys.
type inPlaceFixture struct {
	state, bundle, oldToken string
}

func newInPlaceFixture(t *testing.T) inPlaceFixture {
	t.Helper()
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	record := func(id domain.ID) {
		store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		if err := store.Transact(ctx, storage.Guard{Now: at}, func(tx storage.Tx) error {
			if err := tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0); err != nil && !errors.Is(err, storage.ErrConflict) {
				return err
			}
			event := domain.Event{ID: "e-" + id, Type: "fixture.changed", Source: "fixture", DeliveryID: string(id), OccurredAt: at, ReceivedAt: at}
			if _, _, err := tx.AcceptEvent(event); err != nil {
				return err
			}
			work := "w-" + id
			if err := tx.PutWork(domain.Work{ID: work, ScopeID: "eng", Kind: "fixture.work", State: domain.WorkReady}, 0); err != nil {
				return err
			}
			return tx.PutExecution(domain.Execution{ID: id, WorkID: work, ExecutorBinding: "run", Executor: "fixture", Attempt: 1, State: domain.ExecutionPending, TriggerID: event.ID}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	record("x-before")
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := BackupState(ctx, state, bundle, "test", cli.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	// The later timeline.
	record("x-after")
	for _, id := range []domain.ID{"x-before", "x-after"} {
		write(t, filepath.Join(state, "runs", runKey(id), "execution"), string(id))
	}
	write(t, filepath.Join(state, "executors", "shell", "runs", "abc", "status.json"), "{}")
	write(t, filepath.Join(state, "extensions", "slack", "cache.json"), "{}")
	write(t, filepath.Join(state, "private", "cursor.key"), "cursor")
	if err := RecoverOperator(ctx, state, filepath.Join(state, stagedToken)); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(state, stagedToken))
	if err != nil {
		t.Fatal(err)
	}
	return inPlaceFixture{state: state, bundle: bundle, oldToken: string(old)}
}

// runKey is the toolbelt run directory's name for an execution
// (management's endpointKey).
func runKey(id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func executions(t *testing.T, state string) []domain.ID {
	t.Helper()
	ctx := context.Background()
	_, store, err := openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var ids []domain.ID
	if err := store.View(ctx, func(tx storage.Tx) error {
		ids = nil
		xs, err := tx.ListExecutions("")
		for _, x := range xs {
			ids = append(ids, x.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}

func asides(t *testing.T, state string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(state, "before-restore-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestRestoreInPlace: an in-place restore puts the bundle's database back
// in the daemon's own directory, moves the replaced database and token
// into before-restore-*, removes the run directory of the execution the
// restored database doesn't know (and keeps the one it does), leaves and
// reports the executor's and extension's private state, keeps the
// directory's other keys, writes a fresh token, and passes its state
// check. serve --restore's Once then leaves the directory as it is, while
// a second explicit restore (the negative control) restores again.
func TestRestoreInPlace(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	config := filepath.Join(t.TempDir(), "config.json")
	// An archive from after the bundle could hold sequences the restored
	// log will issue again, so it never stays in place.
	laterArchive := filepath.Join(f.state, backup.ArchiveDir, "index.db")
	if err := os.MkdirAll(filepath.Dir(laterArchive), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(laterArchive, []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ConfigPath: config})
	if err != nil {
		t.Fatal(err)
	}
	r := info.InPlace
	if r == nil || r.AlreadyRestored {
		t.Fatalf("report = %+v", r)
	}
	if exists(laterArchive) || !exists(filepath.Join(r.MovedAside, backup.ArchiveDir, "index.db")) {
		t.Fatalf("the later archive was not moved aside: %v", r.MovedItems)
	}
	if got := executions(t, f.state); !slices.Equal(got, []domain.ID{"x-before"}) {
		t.Fatalf("restored executions = %v, want the bundle's", got)
	}
	if got := asides(t, f.state); len(got) != 1 || got[0] != r.MovedAside {
		t.Fatalf("before-restore directories = %v, report %q", got, r.MovedAside)
	}
	for _, item := range []string{"conduit.db", stagedToken} {
		if !slices.Contains(r.MovedItems, item) || !exists(filepath.Join(r.MovedAside, item)) {
			t.Fatalf("%s was not moved aside: %v", item, r.MovedItems)
		}
	}
	// The replaced database is the later timeline's, intact.
	if got := executions(t, r.MovedAside); !slices.Equal(got, []domain.ID{"x-after", "x-before"}) {
		t.Fatalf("moved-aside executions = %v", got)
	}
	if !slices.Equal(r.RemovedRuns, []string{"x-after"}) || exists(filepath.Join(f.state, "runs", runKey("x-after"))) || !exists(filepath.Join(f.state, "runs", runKey("x-before"))) {
		t.Fatalf("removed runs = %v", r.RemovedRuns)
	}
	var later []string
	for _, e := range r.LaterTimeline {
		later = append(later, strings.TrimPrefix(e.Path, f.state+string(filepath.Separator)))
	}
	if !slices.Contains(later, filepath.Join("executors", "shell")) || !slices.Contains(later, filepath.Join("extensions", "slack")) {
		t.Fatalf("later timeline = %v", later)
	}
	if !exists(filepath.Join(f.state, "executors", "shell", "runs", "abc")) || !exists(filepath.Join(f.state, "private", "cursor.key")) {
		t.Fatal("the restore deleted state it only reports")
	}
	token, err := os.ReadFile(info.TokenFile)
	if err != nil || info.TokenFile != filepath.Join(f.state, stagedToken) || string(token) == f.oldToken {
		t.Fatalf("token file %s: %v (unchanged: %v)", info.TokenFile, err, string(token) == f.oldToken)
	}
	if r.Check == nil || !r.Check.OK {
		t.Fatalf("state check = %+v", r.Check)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.state, ".restore-*")); len(matches) != 0 || exists(filepath.Join(f.state, restoreJournal)) {
		t.Fatalf("stage or journal left: %v", matches)
	}

	again, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ConfigPath: config, Once: true})
	if err != nil || again.InPlace == nil || !again.InPlace.AlreadyRestored || len(asides(t, f.state)) != 1 {
		t.Fatalf("once: %+v, %v, %d before-restore directories", again.InPlace, err, len(asides(t, f.state)))
	}
	if _, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ConfigPath: config}); err != nil || len(asides(t, f.state)) != 2 {
		t.Fatalf("explicit restore: %v, %d before-restore directories", err, len(asides(t, f.state)))
	}
}

// TestRestoreInPlaceFinishesAnInterruptedSwap: a swap that stops after
// moving part of the replaced state aside leaves a journal; the daemon and
// the offline commands refuse the directory, and running the restore again
// finishes the swap without restoring a second time.
func TestRestoreInPlaceFinishesAnInterruptedSwap(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	config := filepath.Join(t.TempDir(), "config.json")
	swapInterrupted = func(i int) error {
		if i == 0 {
			return errors.New("power cut")
		}
		return nil
	}
	_, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ConfigPath: config})
	swapInterrupted = func(int) error { return nil }
	if err == nil || !strings.Contains(err.Error(), "power cut") {
		t.Fatalf("interrupted restore: %v", err)
	}
	if err := restoreInterrupted(f.state); err == nil {
		t.Fatal("an interrupted swap is not refused")
	}
	if r, _ := CheckState(ctx, f.state); r.OK {
		t.Fatal("state check passed a half-swapped directory")
	}
	info, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ConfigPath: config})
	if err != nil {
		t.Fatal(err)
	}
	if got := asides(t, f.state); len(got) != 1 || info.InPlace.MovedAside != got[0] {
		t.Fatalf("before-restore directories = %v, report %q", got, info.InPlace.MovedAside)
	}
	if got := executions(t, f.state); !slices.Equal(got, []domain.ID{"x-before"}) {
		t.Fatalf("restored executions = %v", got)
	}
	if err := restoreInterrupted(f.state); err != nil || info.InPlace.Check == nil || !info.InPlace.Check.OK {
		t.Fatalf("after finishing: %v, check %+v", err, info.InPlace.Check)
	}
}

// TestRestoreInPlaceRefusesARunningDaemon: the restore takes the daemon
// lock first, so it never touches a directory a daemon owns.
func TestRestoreInPlaceRefusesARunningDaemon(t *testing.T) {
	f := newInPlaceFixture(t)
	lock, err := locked(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := RestoreInPlace(context.Background(), f.bundle, f.state, cli.RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "stop the daemon") {
		t.Fatalf("restore under a held lock: %v", err)
	}
	if len(asides(t, f.state)) != 0 {
		t.Fatal("the refused restore moved state aside")
	}
}

// TestRestoreInPlaceKeepsAnExternalStore: a state directory whose
// storage.json names an artifact store outside it is not moved onto the
// directory's own file store unasked. The restore refuses before staging,
// and --artifacts files (the negative control) moves it there on purpose,
// reporting the old store as left in place. A database outside the
// directory always refuses, since the restored one goes in the directory.
func TestRestoreInPlaceKeepsAnExternalStore(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	external := filepath.Join(t.TempDir(), "artifacts")
	write(t, filepath.Join(f.state, storageFile), `{"backend":"sqlite","artifacts":{"backend":"files","location":"`+external+`"}}`)
	_, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "--artifacts") {
		t.Fatalf("an external artifact store: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.state, ".restore-*")); len(matches) != 0 || len(asides(t, f.state)) != 0 {
		t.Fatalf("the refused restore staged or moved: %v", matches)
	}
	info, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ArtifactBackend: "files"})
	if err != nil {
		t.Fatal(err)
	}
	var left bool
	for _, e := range info.InPlace.LaterTimeline {
		left = left || e.Path == external
	}
	if !left {
		t.Fatalf("the replaced store is not reported: %+v", info.InPlace.LaterTimeline)
	}

	write(t, filepath.Join(f.state, storageFile), `{"backend":"sqlite","location":"`+filepath.Join(t.TempDir(), "conduit.db")+`"}`)
	if _, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ArtifactBackend: "files"}); err == nil || !strings.Contains(err.Error(), "outside the state directory") {
		t.Fatalf("an external database: %v", err)
	}
}

// Shared state is never swapped out of a directory: another instance may be
// running on it, and this host's lock can't see that.
func TestRestoreInPlaceRefusesToReplaceSharedState(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	write(t, filepath.Join(f.state, storageFile), `{"backend":"`+sharedTestBackend+`"}`)
	_, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{})
	if !errors.Is(err, storage.ErrInvalid) || !strings.Contains(err.Error(), "shared state") {
		t.Fatalf("restoring over shared state: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.state, ".restore-*")); len(matches) != 0 || len(asides(t, f.state)) != 0 {
		t.Fatalf("the refused restore staged or moved: %v", matches)
	}
	if !exists(filepath.Join(f.state, "conduit.db")) {
		t.Fatal("the shared database was moved")
	}
	// The negative control: the same directory on an unshared backend restores.
	write(t, filepath.Join(f.state, storageFile), `{"backend":"sqlite"}`)
	if _, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{}); err != nil {
		t.Fatalf("restoring unshared state: %v", err)
	}
}

// An in-place restore moves a daemon whose database is outside the state
// directory to the empty location the operator names, records it, and
// leaves the replaced database where it was.
func TestRestoreInPlaceToANamedDatabase(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	old := filepath.Join(t.TempDir(), "old.db")
	writeChoice(t, f.state, `{"backend": "sqlite", "location": "`+old+`"}`)
	db, err := sqlite.Backend.Open(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(t.TempDir(), "new.db")
	info, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{DatabaseLocation: named})
	if err != nil {
		t.Fatal(err)
	}
	c, err := coordinationOf(f.state)
	if err != nil || c.location != named {
		t.Fatalf("restored choice = %+v, %v; want %s", c, err, named)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("the replaced database moved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.state, "conduit.db")); !os.IsNotExist(err) {
		t.Fatalf("the directory has its own database as well: %v", err)
	}
	var reported bool
	for _, e := range info.InPlace.LaterTimeline {
		reported = reported || e.Path == old
	}
	if !reported {
		t.Fatalf("the report doesn't name the replaced database: %+v", info.InPlace.LaterTimeline)
	}
}

// A restore that dies after writing a named database and before its swap
// journal is told apart from state that was already there: the rerun says
// the leftover is safe to clear, and runs once it has been. A database the
// swap went on to adopt is never called a leftover.
func TestRestoreInPlaceAfterACrashWritingANamedDatabase(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	named := filepath.Join(t.TempDir(), "new.db")
	o := cli.RestoreOptions{DatabaseLocation: named, Once: true}
	restoreStopped = func() error { return errors.New("power cut") }
	_, err := RestoreInPlace(ctx, f.bundle, f.state, o)
	restoreStopped = func() error { return nil }
	if err == nil || !strings.Contains(err.Error(), "power cut") {
		t.Fatalf("stopped restore: %v", err)
	}
	if _, err := os.Stat(named); err != nil {
		t.Fatalf("the crash should leave the database written: %v", err)
	}
	for range 2 { // Every restart of serve --restore says the same.
		_, err = RestoreInPlace(ctx, f.bundle, f.state, o)
		if err == nil || !strings.Contains(err.Error(), "safe to clear") || !strings.Contains(err.Error(), named) {
			t.Fatalf("rerun over the leftover: %v", err)
		}
	}
	if len(asides(t, f.state)) != 0 {
		t.Fatal("the refused rerun moved state aside")
	}
	for _, suffix := range []string{"", "-wal", "-shm", ".lock"} {
		if err := os.Remove(named + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if _, err := RestoreInPlace(ctx, f.bundle, f.state, o); err != nil {
		t.Fatal(err)
	}
	if c, err := coordinationOf(f.state); err != nil || c.location != named {
		t.Fatalf("restored choice = %+v, %v", c, err)
	}
	if _, err := os.Lstat(filepath.Join(f.state, restoreIntent)); !os.IsNotExist(err) {
		t.Fatalf("the intent outlived the swap: %v", err)
	}
	if info, err := RestoreInPlace(ctx, f.bundle, f.state, o); err != nil || !info.InPlace.AlreadyRestored {
		t.Fatalf("restart after the restore: %+v, %v", info.InPlace, err)
	}
}

// A restore refused before it writes leaves no record, so state put at the
// location afterwards is never described as its leftover.
func TestRestoreInPlaceRefusedBeforeTheWriteLeavesNoIntent(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	named := filepath.Join(t.TempDir(), "new.db")
	database := filepath.Join(f.bundle, "conduit.db")
	if err := os.Chmod(database, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database, []byte("not the database the manifest names"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{DatabaseLocation: named}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("restore of a damaged bundle: %v", err)
	}
	if _, err := os.Lstat(named); !os.IsNotExist(err) {
		t.Fatalf("a refused restore wrote the location: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.state, restoreIntent)); !os.IsNotExist(err) {
		t.Fatalf("a refused restore left its intent: %v", err)
	}
}

// A location that held state before the restore ran is never called a
// leftover, however many times the restore is run against it, and naming
// it doesn't lose the record of a real leftover elsewhere.
func TestRestoreInPlaceNeverCallsExistingStateALeftover(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	occupied := filepath.Join(t.TempDir(), "kept.db")
	db, err := sqlite.Backend.Open(ctx, occupied)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	refused := func() {
		t.Helper()
		_, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{DatabaseLocation: occupied})
		if err == nil || !strings.Contains(err.Error(), "already holds state") || strings.Contains(err.Error(), "safe to clear") {
			t.Fatalf("restore onto existing state: %v", err)
		}
	}
	refused()
	refused()
	if _, err := os.Lstat(filepath.Join(f.state, restoreIntent)); !os.IsNotExist(err) {
		t.Fatalf("refusing existing state left a record: %v", err)
	}

	leftover := filepath.Join(t.TempDir(), "leftover.db")
	restoreStopped = func() error { return errors.New("power cut") }
	_, err = RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{DatabaseLocation: leftover})
	restoreStopped = func() error { return nil }
	if err == nil || !strings.Contains(err.Error(), "power cut") {
		t.Fatalf("stopped restore: %v", err)
	}
	refused()
	_, err = RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{DatabaseLocation: leftover})
	if err == nil || !strings.Contains(err.Error(), "safe to clear") {
		t.Fatalf("the real leftover's record was lost: %v", err)
	}
}

// A swap interrupted after the journal is finished by the rerun, which
// settles the intent, so the now-live database is not offered for clearing.
func TestRestoreInPlaceSettlesTheIntentWhenItFinishesASwap(t *testing.T) {
	ctx := context.Background()
	f := newInPlaceFixture(t)
	named := filepath.Join(t.TempDir(), "new.db")
	o := cli.RestoreOptions{DatabaseLocation: named}
	swapInterrupted = func(int) error { return errors.New("power cut") }
	_, err := RestoreInPlace(ctx, f.bundle, f.state, o)
	swapInterrupted = func(int) error { return nil }
	if err == nil || !strings.Contains(err.Error(), "power cut") {
		t.Fatalf("interrupted swap: %v", err)
	}
	if _, err := RestoreInPlace(ctx, f.bundle, f.state, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.state, restoreIntent)); !os.IsNotExist(err) {
		t.Fatalf("the intent outlived the finished swap: %v", err)
	}
	_, err = RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{DatabaseLocation: named})
	if err == nil || strings.Contains(err.Error(), "safe to clear") {
		t.Fatalf("restore onto the live database: %v", err)
	}
}

// TestRestoreInPlaceClearsStaleStages: stages a restore left when it was
// killed before its swap began are removed and reported by the next one,
// so repeated failures don't fill the volume with copies of the database.
func TestRestoreInPlaceClearsStaleStages(t *testing.T) {
	f := newInPlaceFixture(t)
	stale := []string{filepath.Join(f.state, ".restore-20200101T000000Z"), filepath.Join(f.state, ".conduit-snapshot-123")}
	for _, d := range stale {
		write(t, filepath.Join(d, "conduit.db"), "copy")
	}
	info, err := RestoreInPlace(context.Background(), f.bundle, f.state, cli.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(stale)
	got := slices.Clone(info.InPlace.RemovedStages)
	slices.Sort(got)
	if !slices.Equal(got, stale) {
		t.Fatalf("removed stages = %v, want %v", got, stale)
	}
	for _, d := range stale {
		if exists(d) {
			t.Fatalf("%s is left", d)
		}
	}
}
