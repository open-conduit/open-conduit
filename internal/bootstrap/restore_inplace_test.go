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
	if _, err := BackupState(ctx, state, bundle, "test"); err != nil {
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
	info, err := RestoreInPlace(ctx, f.bundle, f.state, cli.RestoreOptions{ConfigPath: config})
	if err != nil {
		t.Fatal(err)
	}
	r := info.InPlace
	if r == nil || r.AlreadyRestored {
		t.Fatalf("report = %+v", r)
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
