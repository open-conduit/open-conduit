package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/storage"
)

// An in-place restore puts a bundle back into the daemon's own state
// directory under its lock. The bundle is restored into a stage inside the
// directory (so every rename stays on one filesystem), complete with its
// fresh operator token and a record of the bundle, before anything is
// touched. A journal then names the stage, the before-restore-* directory
// and every item to move, and the swap moves the replaced items aside and
// the staged ones in. A swap that stops part way is finished by the next
// in-place restore; until then the daemon and the other offline commands
// refuse the directory.

const (
	// restoreJournal is the in-progress swap's journal.
	restoreJournal = "restore-in-progress.json"
	// restoredFile records the bundle the directory was last restored
	// from, so serve --restore restores a bundle once.
	restoredFile = "restored.json"
	// restoreIntent records a named database a restore is about to write,
	// until the swap has made it the directory's own.
	restoreIntent = "restore-database.json"
	// stagedToken is the operator token's path in a state directory.
	stagedToken = "private/operator.token"
)

// replaced are what an in-place restore moves aside whenever present: the
// coordination state (SQLite's database and its side files), the file
// store, the storage choice, the restore record and the operator token.
// Anything else the stage holds is moved aside too.
var replaced = []string{"conduit.db", "conduit.db-wal", "conduit.db-shm", "conduit.db-journal", "conduit.db.lock", "artifacts", storageFile, restoredFile, stagedToken}

// swapJournal is restoreJournal. Paths are relative to the state directory
// (Aside's items) or the stage (In).
type swapJournal struct {
	Stage string   `json:"stage"`
	Aside string   `json:"aside"`
	Moved []string `json:"moved"`
	In    []string `json:"in"`
	// Phase is "aside" while the replaced items move out, then "in".
	Phase string `json:"phase"`
}

// restoredRecord is restoredFile.
type restoredRecord struct {
	DatabaseSHA256  string    `json:"database_sha256"`
	BundleCreatedAt time.Time `json:"bundle_created_at"`
	Bundle          string    `json:"bundle"`
	RestoredAt      time.Time `json:"restored_at"`
}

// databaseIntent is restoreIntent. A restore can't take a database back
// from a location outside the directory, so what it wrote there before
// stopping has to be told apart from state that was already there.
type databaseIntent struct {
	Location       string `json:"location"`
	DatabaseSHA256 string `json:"database_sha256"`
	Bundle         string `json:"bundle"`
}

func readIntent(stateDir string) (databaseIntent, bool, error) {
	var intent databaseIntent
	data, err := os.ReadFile(filepath.Join(stateDir, restoreIntent))
	if errors.Is(err, os.ErrNotExist) {
		return intent, false, nil
	}
	if err != nil {
		return intent, false, err
	}
	if err := json.Unmarshal(data, &intent); err != nil {
		return intent, false, fmt.Errorf("%s: %w", restoreIntent, err)
	}
	return intent, true, nil
}

// settleIntent drops the intent once the directory's storage choice names
// its location: that database is now the live one, and must never again be
// described as a leftover.
func settleIntent(stateDir string) error {
	intent, ok, err := readIntent(stateDir)
	if err != nil || !ok {
		return err
	}
	choice, err := readChoice(stateDir)
	if err != nil || choice.Location != intent.Location {
		return err
	}
	return os.Remove(filepath.Join(stateDir, restoreIntent))
}

// claimDatabase records that this restore is about to write location. When
// an earlier restore recorded the same location and state is there, that
// state is what the earlier restore left, so it refuses and says so. It
// isn't carried on from: the leftover may hold part of the steps after the
// write, and a restore doesn't delete a database. A leftover at another
// location is reported.
func claimDatabase(ctx context.Context, stateDir, location, digest, bundle, backendName string, report *cli.InPlaceReport) error {
	backend, err := restoreBackends(backendName)
	if err != nil {
		return err
	}
	prior, ok, err := readIntent(stateDir)
	if err != nil {
		return err
	}
	if ok {
		present, err := backend.Present(ctx, prior.Location)
		switch {
		case err != nil:
			return err
		case present && prior.Location == location:
			return fmt.Errorf("%s holds the database an earlier restore of %s wrote before it stopped. Nothing uses it, so unless you have put state there since, it is safe to clear: clear it and run the restore again", location, prior.Bundle)
		case present:
			report.LaterTimeline = append(report.LaterTimeline, cli.LaterEntry{Path: prior.Location, Note: "the database an earlier restore wrote before it stopped: nothing uses it, so it is safe to clear"})
		}
	}
	data, err := json.Marshal(databaseIntent{Location: location, DatabaseSHA256: digest, Bundle: bundle})
	if err != nil {
		return err
	}
	return writeFileDurable(filepath.Join(stateDir, restoreIntent), append(data, '\n'))
}

// restoreInterrupted refuses a state directory an in-place restore left
// part way through its swap.
func restoreInterrupted(stateDir string) error {
	if _, err := os.Lstat(filepath.Join(stateDir, restoreJournal)); err == nil {
		return fmt.Errorf("an in-place restore of %s stopped part way (%s): run open-conduit backup restore BUNDLE --in-place again to finish it", stateDir, restoreJournal)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RestoreInPlace restores a bundle into stateDir itself, under its daemon
// lock: it verifies and stages the bundle, moves the state it replaces into
// a timestamped before-restore-* directory, renames the staged state into
// place, removes the toolbelt run directories of executions the restored
// database doesn't know, and reports what else stays from after the
// bundle, ending with a state check. The restored directory has a fresh
// operator token; the bundle's credentials are revoked. With o.Once, a
// directory already restored from this bundle is left as it is.
func RestoreInPlace(ctx context.Context, bundle, stateDir string, o cli.RestoreOptions) (cli.BackupInfo, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	target, choice, err := restoreTarget(o)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	info, err := bundleInfo(bundle)
	if err != nil {
		return info, err
	}
	digest, err := bundleDigest(bundle)
	if err != nil {
		return info, err
	}
	lock, err := management.AcquireLock(stateDir)
	if errors.Is(err, localfs.ErrNotLocal) {
		return info, err // Stopping a daemon wouldn't help.
	}
	if err != nil {
		return info, fmt.Errorf("%w: stop the daemon first", err)
	}
	defer lock.Close()
	report := &cli.InPlaceReport{StateDir: stateDir}
	info.InPlace = report

	// Finish a swap an earlier restore left part way. When it was this
	// bundle's, it needs no second restore.
	resumed, err := finishSwap(stateDir)
	if err != nil {
		return info, fmt.Errorf("finishing the interrupted in-place restore: %w", err)
	}
	if resumed != nil {
		report.MovedAside, report.MovedItems = filepath.Join(stateDir, resumed.Aside), resumed.Moved
	}
	if err := settleIntent(stateDir); err != nil {
		return info, err
	}
	// A stage no journal names is from a restore that stopped before its
	// swap began: nothing else makes one, and the lock is held.
	if report.RemovedStages, err = removeStaleStages(stateDir); err != nil {
		return info, err
	}
	if rec, ok := readRestored(stateDir); ok && rec.DatabaseSHA256 == digest && (o.Once || resumed != nil) {
		if resumed == nil {
			report.AlreadyRestored = true
			return info, nil
		}
		return finishInPlace(ctx, lock, stateDir, o, info)
	}

	// Checked only now: a restart of `serve --restore` that restores nothing
	// must not stop serving because the trust keys have since rotated.
	checked, trust, err := authenticated(bundle, o)
	if err != nil {
		return info, err
	}
	info.Authenticity, info.SigningKey = checked.Authenticity, checked.SigningKey

	// What the replaced storage choice names outside the directory is
	// never swapped away silently.
	previous, err := readChoice(stateDir)
	if err != nil {
		return info, err
	}
	if err := keepsStorage(stateDir, previous, o); err != nil {
		return info, err
	}

	if o.DatabaseLocation != "" {
		if err := claimDatabase(ctx, stateDir, o.DatabaseLocation, digest, bundle, info.Backend, report); err != nil {
			return info, err
		}
	}
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405Z")
	stage := filepath.Join(stateDir, ".restore-"+stamp)
	if err := backup.Restore(ctx, bundle, stage, trust, now, retentionExpired, restoreBackends, target, withStartConfig(ctx, recordChoiceWith(o.DatabaseLocation, choice), o)); err != nil {
		// Refused before the write: the record must not outlive it, or
		// whatever is put at the location later would pass for a leftover.
		if o.DatabaseLocation != "" {
			if backend, lookup := restoreBackends(info.Backend); lookup == nil {
				if present, perr := backend.Present(ctx, o.DatabaseLocation); perr == nil && !present {
					_ = os.Remove(filepath.Join(stateDir, restoreIntent))
				}
			}
		}
		return info, err
	}
	if err := restoreStopped(); err != nil {
		return info, err
	}
	swap, err := prepareSwap(ctx, stateDir, stage, stamp, restoredRecord{DatabaseSHA256: digest, BundleCreatedAt: info.CreatedAt, Bundle: bundle, RestoredAt: now})
	if err != nil {
		_ = os.RemoveAll(stage)
		if o.DatabaseLocation != "" {
			return info, fmt.Errorf("%s now holds the restored database but the restore stopped before using it, so clear it before restoring there again: %w", o.DatabaseLocation, err)
		}
		return info, err
	}
	if err := runSwap(stateDir, swap); err != nil {
		return info, fmt.Errorf("the in-place restore stopped part way: %w (run it again to finish)", err)
	}
	if err := settleIntent(stateDir); err != nil {
		return info, err
	}
	report.MovedAside, report.MovedItems = filepath.Join(stateDir, swap.Aside), swap.Moved
	aside := filepath.Join(stateDir, swap.Aside, storageFile)
	// The operator chose a new store (keepsStorage): the replaced one
	// stays where it is, named by the storage.json moved aside.
	if l := previous.Location; o.DatabaseLocation != "" && l != "" && filepath.Clean(l) != filepath.Join(stateDir, "conduit.db") {
		report.LaterTimeline = append(report.LaterTimeline, cli.LaterEntry{Path: l, Note: "the replaced database, which " + aside + " names: left where it is"})
	}
	if a := previous.Artifacts; a != nil && a.Location != "" && a.Location != filepath.Join(stateDir, "artifacts") {
		report.LaterTimeline = append(report.LaterTimeline, cli.LaterEntry{Path: a.Location, Note: "the replaced artifact store, which " + aside + " names: left where it is"})
	}
	return finishInPlace(ctx, lock, stateDir, o, info)
}

// keepsStorage refuses an in-place restore that would move the daemon off
// a store its storage.json names outside the state directory without
// being asked. An external database needs the operator to name the empty
// location the bundle's database goes to; an external or non-default artifact store
// needs the operator to name the store the bundle's artifacts go to (an
// empty one), or --artifacts files to move to the directory's own.
func keepsStorage(stateDir string, previous storageChoice, o cli.RestoreOptions) error {
	if o.DatabaseLocation == "" && previous.Location != "" && filepath.Clean(previous.Location) != filepath.Join(stateDir, "conduit.db") {
		return fmt.Errorf("%s names a database outside the state directory (%s): name the empty location the bundle's database goes to (--database-location LOC)", storageFile, previous.Location)
	}
	a := previous.Artifacts
	if a == nil || o.ArtifactBackend != "" {
		return nil
	}
	if a.Backend == artifacts.BackendName && (a.Location == "" || filepath.Clean(a.Location) == filepath.Join(stateDir, "artifacts")) {
		return nil
	}
	return fmt.Errorf("%s names the %s artifact store at %s: name the empty store the bundle's artifacts go to (--artifacts NAME --artifacts-location LOC), or --artifacts files to move them into the state directory", storageFile, a.Backend, a.Location)
}

// removeStaleStages removes the stages restores left in stateDir when they
// stopped before their swap began: .restore-* and backup.Restore's own
// .conduit-snapshot-*. The caller holds the lock and has finished any
// journalled swap.
func removeStaleStages(stateDir string) ([]string, error) {
	var removed []string
	for _, pattern := range []string{".restore-*", ".conduit-snapshot-*"} {
		matches, err := filepath.Glob(filepath.Join(stateDir, pattern))
		if err != nil {
			return removed, err
		}
		for _, m := range matches {
			if info, err := os.Lstat(m); err != nil || !info.IsDir() {
				continue
			}
			if err := os.RemoveAll(m); err != nil {
				return removed, err
			}
			removed = append(removed, m)
		}
	}
	return removed, nil
}

// bundleDigest is the digest of a bundle's coordination snapshot.
func bundleDigest(bundle string) (string, error) {
	i, err := backup.Inspect(bundle)
	return i.DatabaseSHA256, err
}

// prepareSwap completes the stage (its operator token and restore record)
// and lists what the swap moves.
func prepareSwap(ctx context.Context, stateDir, stage, stamp string, rec restoredRecord) (swapJournal, error) {
	if err := os.Chmod(stage, 0o700); err != nil {
		return swapJournal{}, err
	}
	if err := recoverOperator(ctx, stage, filepath.Join(stage, stagedToken)); err != nil {
		return swapJournal{}, err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return swapJournal{}, err
	}
	if err := writeFileDurable(filepath.Join(stage, restoredFile), append(data, '\n')); err != nil {
		return swapJournal{}, err
	}
	aside := "before-restore-" + stamp
	for n := 2; ; n++ {
		if _, err := os.Lstat(filepath.Join(stateDir, aside)); errors.Is(err, os.ErrNotExist) {
			break
		}
		aside = fmt.Sprintf("before-restore-%s-%d", stamp, n)
	}
	swap := swapJournal{Stage: filepath.Base(stage), Aside: aside, Phase: "aside"}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return swap, err
	}
	in := map[string]bool{stagedToken: true}
	for _, e := range entries {
		// The stage's private directory holds only its token; the state
		// directory's keeps its other keys.
		if e.Name() != "private" {
			in[e.Name()] = true
		}
	}
	for name := range in {
		swap.In = append(swap.In, name)
	}
	sort.Strings(swap.In)
	moved := map[string]bool{}
	for _, name := range append(append([]string{}, replaced...), swap.In...) {
		if moved[name] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(stateDir, name)); err == nil {
			moved[name] = true
			swap.Moved = append(swap.Moved, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return swap, err
		}
	}
	sort.Strings(swap.Moved)
	return swap, nil
}

// runSwap writes the journal and carries the swap out.
func runSwap(stateDir string, swap swapJournal) error {
	if err := writeJournal(stateDir, swap); err != nil {
		return err
	}
	return continueSwap(stateDir, swap)
}

// restoreStopped is a test seam: an error stops the restore where a crash
// would leave a named database written and no swap journal.
var restoreStopped = func() error { return nil }

// swapInterrupted is a test seam: an error stops the swap after it moved
// the replaced item at index i aside.
var swapInterrupted = func(i int) error { return nil }

// finishSwap finishes the swap a journal in stateDir describes, if any.
func finishSwap(stateDir string) (*swapJournal, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, restoreJournal))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var swap swapJournal
	if err := json.Unmarshal(data, &swap); err != nil {
		return nil, fmt.Errorf("%s: %w", restoreJournal, err)
	}
	for _, p := range append(append([]string{swap.Stage, swap.Aside}, swap.Moved...), swap.In...) {
		if !filepath.IsLocal(p) {
			return nil, fmt.Errorf("%s: %q is not a path in the state directory", restoreJournal, p)
		}
	}
	return &swap, continueSwap(stateDir, swap)
}

// continueSwap moves each replaced item aside, then each staged item in,
// skipping those already moved, so it can be run again after it stops.
func continueSwap(stateDir string, swap swapJournal) error {
	aside, stage := filepath.Join(stateDir, swap.Aside), filepath.Join(stateDir, swap.Stage)
	if swap.Phase == "aside" {
		for i, name := range swap.Moved {
			if err := move(filepath.Join(stateDir, name), filepath.Join(aside, name)); err != nil {
				return err
			}
			if err := swapInterrupted(i); err != nil {
				return err
			}
		}
		if err := syncTree(aside, swap.Moved); err != nil {
			return err
		}
		swap.Phase = "in"
		if err := writeJournal(stateDir, swap); err != nil {
			return err
		}
	}
	for _, name := range swap.In {
		if err := move(filepath.Join(stage, name), filepath.Join(stateDir, name)); err != nil {
			return err
		}
	}
	if err := syncTree(stateDir, swap.In); err != nil {
		return err
	}
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(stateDir, restoreJournal)); err != nil {
		return err
	}
	return syncDir(stateDir)
}

// move renames from to to, creating to's owner-only parent. One already
// moved (from gone, to present) is left as it is.
func move(from, to string) error {
	if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(to); err == nil {
			return nil
		}
		return fmt.Errorf("%s is missing", from)
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// syncTree syncs root and the directories names were moved into.
func syncTree(root string, names []string) error {
	dirs := map[string]bool{root: true}
	for _, n := range names {
		dirs[filepath.Dir(filepath.Join(root, n))] = true
	}
	for d := range dirs {
		if _, err := os.Lstat(d); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writeJournal(stateDir string, swap swapJournal) error {
	data, err := json.Marshal(swap)
	if err != nil {
		return err
	}
	return writeFileDurable(filepath.Join(stateDir, restoreJournal), append(data, '\n'))
}

// writeFileDurable writes an owner-only file atomically and syncs it and
// its directory.
func writeFileDurable(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func readRestored(stateDir string) (restoredRecord, bool) {
	var rec restoredRecord
	data, err := os.ReadFile(filepath.Join(stateDir, restoredFile))
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.DatabaseSHA256 == "" {
		return rec, false
	}
	return rec, true
}

// finishInPlace reconciles the restored directory with what stays from
// after the bundle, and reports on it.
func finishInPlace(ctx context.Context, lock *management.Lock, stateDir string, o cli.RestoreOptions, info cli.BackupInfo) (cli.BackupInfo, error) {
	report := info.InPlace
	info.TokenFile = filepath.Join(stateDir, stagedToken)
	_, store, err := openState(ctx, stateDir)
	if err != nil {
		return info, err
	}
	if err := endRestoredDrain(ctx, store); err != nil {
		store.Close()
		return info, err
	}
	removed, err := management.ForgetRuns(lock, stateDir, func(ids []domain.ID) (map[domain.ID]bool, error) {
		return knownExecutions(ctx, store, ids)
	})
	for _, id := range removed {
		report.RemovedRuns = append(report.RemovedRuns, string(id))
	}
	if err != nil {
		store.Close()
		return info, fmt.Errorf("removing the run directories of executions from after the bundle: %w", err)
	}
	report.LaterTimeline = append(report.LaterTimeline, laterTimeline(stateDir)...)
	if info.Scopes, err = scopeIDs(ctx, store); err != nil {
		store.Close()
		return info, err
	}
	info.ConfigGeneration, info.ConfigProblems, err = restoredConfig(ctx, store, restoreConfigPath(o))
	info.StartConfig = o.StartConfig
	store.Close()
	if err != nil {
		return info, err
	}
	check, err := checkState(ctx, stateDir)
	if err != nil {
		return info, err
	}
	report.Check = &check
	return info, nil
}

// knownExecutions reports which of ids the restored database holds.
func knownExecutions(ctx context.Context, store storage.Database, ids []domain.ID) (map[domain.ID]bool, error) {
	return knownIn(ctx, store, time.Now(), ids)
}

// laterTimeline reports the parts of the state directory an in-place
// restore leaves as they are, which may hold state from after the bundle,
// and what becomes of each. Their layouts are their owners' (an executor,
// a provider, an extension), so the restore never deletes them.
func laterTimeline(stateDir string) []cli.LaterEntry {
	var out []cli.LaterEntry
	each := func(dir, note string) {
		entries, err := os.ReadDir(filepath.Join(stateDir, dir))
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, cli.LaterEntry{Path: filepath.Join(stateDir, dir, e.Name()), Note: note})
			}
		}
	}
	each("executors", "the executor's private state, kept: when the daemon starts, an executor that sweeps its state removes the run directories of executions the restored database doesn't know, which doctor's executor_state check lists; another's stay until removed by hand. The restored executions recover or retry, and no process from before the bundle is adopted")
	each("resources", "kept: the provider reconciles its leases against the restored database when the daemon starts, so an owner the database doesn't know is reported unknown to it; what it keeps (a workspace with unpushed work, say), doctor reports")
	each("containers", "kept: the container provider reconciles its containers against the restored database when the daemon starts, removing those of executions it doesn't know")
	each("extensions", "the extension's private directory, a cache: kept, and rebuilt by the extension as it needs; durable extension state is in the restored database")
	for _, name := range []string{"homes", "logs"} {
		if _, err := os.Lstat(filepath.Join(stateDir, name)); err == nil {
			out = append(out, cli.LaterEntry{Path: filepath.Join(stateDir, name), Note: "kept as it is, including what was written after the bundle"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.Compare(out[i].Path, out[j].Path) < 0 })
	return out
}
