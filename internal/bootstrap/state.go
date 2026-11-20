package bootstrap

import (
	"context"
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
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// The offline state operations behind `backup`, `state check` and
// `auth recover`. Every state operation takes the directory's exclusive
// daemon lock first, so none runs beside a daemon owning the directory.

// StateOps are the offline state operations for the operator CLI.
func StateOps() *cli.StateOps {
	return &cli.StateOps{Backup: BackupState, Verify: VerifyBundle, Restore: RestoreState, Check: CheckState, RecoverOperator: RecoverOperator}
}

// locked opens a stopped instance's state directory under its daemon lock.
func locked(stateDir string) (*management.Lock, error) {
	c, err := coordinationOf(stateDir)
	if err != nil {
		return nil, err
	}
	if present, err := c.backend.Present(context.Background(), c.location); err != nil {
		return nil, err
	} else if !present {
		return nil, fmt.Errorf("no Open Conduit database in %s", stateDir)
	}
	lock, err := management.AcquireLock(stateDir)
	if err != nil {
		return nil, fmt.Errorf("%w: stop the daemon first (or use open-conduit doctor while it runs)", err)
	}
	return lock, nil
}

// BackupState publishes a verified bundle of a stopped instance's database
// and referenced artifacts at an absent destination.
func BackupState(ctx context.Context, stateDir, destination, build string) (cli.BackupInfo, error) {
	lock, err := locked(stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer lock.Close()
	c, store, err := openState(ctx, stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer store.Close()
	_, files, err := openArtifacts(ctx, stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer files.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return cli.BackupInfo{}, err
	}
	if err := backup.Create(ctx, destination, c.name, store, files, build, retention(time.Now)); err != nil {
		return cli.BackupInfo{}, err
	}
	return bundleInfo(destination)
}

// retentionExpired tells the backup package which Executions' content the
// runtime's retention sweep has expired.
func retentionExpired(tx storage.Tx, id domain.ID) (bool, error) {
	r, err := runtime.RetentionInTx(tx, id)
	return r.Expired, err
}

// retention is the runtime's retention ledger as a backup uses it: what has
// expired, and the hold that keeps the sweep from deleting content while
// the backup copies it, its expiry set on now, the clock the sweep reads.
func retention(now func() time.Time) backup.Retention {
	return backup.Retention{Expired: retentionExpired, Hold: runtime.HoldRetention, Release: runtime.ReleaseRetention, Now: now}
}

func bundleInfo(bundle string) (cli.BackupInfo, error) {
	info, err := backup.Inspect(bundle)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	return cli.BackupInfo{Path: bundle, Backend: info.Backend, Schema: info.Schema, Build: info.Build, CreatedAt: info.CreatedAt, Artifacts: info.Artifacts, OmittedArtifacts: info.Omitted}, nil
}

// VerifyBundle restores a bundle into a private temporary directory, checks
// the result and removes it.
func VerifyBundle(ctx context.Context, bundle string) (cli.BackupInfo, error) {
	info, err := bundleInfo(bundle)
	if err != nil {
		return info, err
	}
	tmp, err := os.MkdirTemp("", "oc-verify-")
	if err != nil {
		return info, err
	}
	defer os.RemoveAll(tmp)
	dest := filepath.Join(tmp, "state")
	// Verification restores into the file store in its own temporary
	// directory, never into a store the operator uses.
	if err := backup.Restore(ctx, bundle, dest, time.Now().UTC(), retentionExpired, restoreBackends, backup.ArtifactTarget{Backend: artifacts.Backend}, recordChoice); err != nil {
		return info, err
	}
	_, store, err := openState(ctx, dest)
	if err != nil {
		return info, err
	}
	defer store.Close()
	info.Scopes, err = scopeIDs(ctx, store)
	return info, err
}

func scopeIDs(ctx context.Context, store storage.Store) ([]string, error) {
	var out []string
	err := store.Transact(ctx, storage.Guard{Now: time.Now().UTC()}, func(tx storage.Tx) error {
		out = nil
		scopes, err := tx.ListScopes()
		for _, s := range scopes {
			out = append(out, string(s.ID))
		}
		return err
	})
	return out, err
}

// RestoreState publishes a new state directory from a bundle (never over an
// existing one), fences every backed-up lease and writes a fresh operator
// credential, revoking the bundle's.
func RestoreState(ctx context.Context, bundle, destination string, o cli.RestoreOptions) (cli.BackupInfo, error) {
	target, choice, err := restoreTarget(o)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	info, err := bundleInfo(bundle)
	if err != nil {
		return info, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return info, err
	}
	if err := backup.Restore(ctx, bundle, destination, time.Now().UTC(), retentionExpired, restoreBackends, target, recordChoiceWith(choice)); err != nil {
		return info, err
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return info, err
	}
	token := filepath.Join(destination, "private", "operator.token")
	if err := RecoverOperator(ctx, destination, token); err != nil {
		return info, err
	}
	info.TokenFile = token
	_, store, err := openState(ctx, destination)
	if err != nil {
		return info, err
	}
	defer store.Close()
	if info.Scopes, err = scopeIDs(ctx, store); err != nil {
		return info, err
	}
	info.ConfigGeneration, info.ConfigProblems, err = restoredConfig(ctx, store, o.ConfigPath)
	return info, err
}

// restoredConfig reads the committed config generation a restore brought
// back and validates it as the daemon's OpenConfig will at start, against
// the packages installed beside configPath (the bundle carries the
// generation, not the packages). It returns the generation and every
// reason the daemon would refuse it, packages that couldn't start
// included. A check that fails outright is reported as a problem too: the
// directory is already published, and the operator still needs its token.
func restoredConfig(ctx context.Context, store storage.Store, configPath string) (uint64, []cli.ConfigEntry, error) {
	gen, normalized, err := management.StoredGeneration(ctx, store, func() time.Time { return time.Now().UTC() })
	if err != nil || gen == 0 {
		return 0, nil, err
	}
	diags, _, err := checkAgainstPackages(configPath, "", normalized, func(reg config.Registry) (*config.Snapshot, error) {
		return config.Validate(normalized, reg)
	})
	if err != nil {
		return gen, []cli.ConfigEntry{{Code: "unchecked", Message: "the restored generation could not be checked: " + err.Error()}}, nil
	}
	out := make([]cli.ConfigEntry, 0, len(diags))
	for _, d := range diags {
		out = append(out, cli.ConfigEntry{Path: d.Path, Code: d.Code, Message: d.Message})
	}
	return gen, out, nil
}

// RecoverOperator is the locked local credential reset (B05): it revokes
// every operator credential and atomically writes a fresh token file.
func RecoverOperator(ctx context.Context, stateDir, tokenPath string) error {
	lock, err := locked(stateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	_, store, err := openState(ctx, stateDir)
	if err != nil {
		return err
	}
	defer store.Close()
	return management.RecoverOperator(ctx, store, func() time.Time { return time.Now().UTC() }, tokenPath)
}

// CheckState diagnoses a stopped instance's state directory without
// changing it: the database is examined through a private copy.
func CheckState(ctx context.Context, stateDir string) (cli.StateReport, error) {
	r := cli.StateReport{StateDir: stateDir, OK: true}
	add := func(name, status, code, msg string) {
		r.Checks = append(r.Checks, cli.StateCheck{Name: name, Status: status, Code: code, Message: msg})
		if status == "fail" {
			r.OK = false
		}
	}
	lock, err := locked(stateDir)
	if err != nil {
		add("lock", "fail", "in_use_or_unsafe", err.Error())
		return r, nil
	}
	defer lock.Close()
	add("lock", "ok", "stopped", "no daemon owns the directory; permissions are owner-only")
	if _, err := os.Lstat(filepath.Join(stateDir, "management.sock")); err == nil {
		add("socket", "info", "stale_socket", "a socket from an unclean stop remains; the next start removes it after its ownership checks")
	}
	c, err := coordinationOf(stateDir)
	if err != nil {
		add("database", "fail", "storage_unavailable", err.Error())
		return r, nil
	}
	// An inspection changes nothing, not even the record format a newer
	// binary would read.
	store, err := c.backend.Inspect(ctx, c.location)
	if err != nil {
		code, remedy := "open_failed", "restore the latest verified bundle into a new state directory (backup restore)"
		msg := err.Error()
		switch {
		case errors.Is(err, storage.ErrRecordsNewer):
			code, remedy = "records_newer", "a newer Open Conduit wrote these records and this binary would drop the fields it doesn't know: run that binary, or restore the bundle taken before the upgrade"
		case errors.Is(err, storage.ErrInUse):
			code, remedy = "in_use", "another daemon or command owns this coordination state (a storage.json in another state directory may name the same database): stop it first"
		case errors.Is(err, storage.ErrSchemaNewer):
			code, remedy = "schema_newer", "a newer Open Conduit migrated this database: run that binary, or restore the bundle taken before the upgrade"
		case strings.Contains(msg, "explicit migration required"), strings.Contains(msg, "metadata"):
			code = "schema_unrecognized"
		case strings.Contains(msg, "integrity"), strings.Contains(msg, "foreign key"), strings.Contains(msg, "malformed"), strings.Contains(msg, "not a database"):
			code = "corrupt"
		}
		add("database", "fail", code, remedy)
		return r, nil
	}
	defer store.Close()
	// Open runs only quick_check; an offline check can afford the full one.
	if err := store.IntegrityCheck(ctx); err != nil {
		add("database", "fail", "corrupt", "restore the latest verified bundle into a new state directory (backup restore)")
		return r, nil
	}
	add("database", "ok", "schema_1", fmt.Sprintf("schema %d, integrity and foreign keys pass", store.SchemaVersion()))
	art, files, ferr := openArtifacts(ctx, stateDir)
	scopes := map[string]*cli.ScopeState{}
	scope := func(id domain.ID) *cli.ScopeState {
		s := scopes[string(id)]
		if s == nil {
			s = &cli.ScopeState{ID: string(id), Works: map[string]int{}}
			scopes[string(id)] = s
		}
		return s
	}
	var refs, missing, timerCount int
	err = store.View(ctx, func(tx storage.Tx) error {
		scopes, refs, missing, timerCount = map[string]*cli.ScopeState{}, 0, 0, 0
		ss, err := tx.ListScopes()
		if err != nil {
			return err
		}
		for _, s := range ss {
			scope(s.ID)
		}
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		workScope := map[domain.ID]domain.ID{}
		for _, w := range works {
			workScope[w.ID] = w.ScopeID
			s := scope(w.ScopeID)
			s.Works[string(w.State)]++
			if w.State == domain.WorkRunning || w.State == domain.WorkWaiting {
				s.DrainBlockedWork = append(s.DrainBlockedWork, string(w.ID))
			}
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			for _, x := range xs {
				if !x.State.Terminal() {
					s.LiveExecutions++
				}
				if ferr != nil {
					continue
				}
				// Retention released expired content on purpose; a backup omits it.
				if retention, err := runtime.RetentionInTx(tx, x.ID); err != nil {
					return err
				} else if retention.Expired {
					continue
				}
				for _, ref := range x.AllArtifacts() {
					refs++
					rc, err := files.Open(ctx, ref)
					if err != nil {
						missing++
						continue
					}
					rc.Close()
				}
			}
			ws, err := tx.ListWaits(w.ID)
			if err != nil {
				return err
			}
			for _, wt := range ws {
				if wt.State == domain.WaitActive {
					s.ActiveWaits++
				}
			}
		}
		timers, err := tx.ListTimers()
		if err != nil {
			return err
		}
		timerCount = len(timers)
		return nil
	})
	if err != nil {
		add("records", "fail", "unreadable_records", "coordination records could not be read: restore the latest verified bundle")
		return r, nil
	}
	switch {
	case ferr != nil:
		add("artifacts", "fail", "artifacts_unavailable", fmt.Sprintf("the %s artifact store could not be opened: %v", art.name, ferr))
	case missing > 0:
		add("artifacts", "warn", "artifacts_missing", fmt.Sprintf("%s store: %d of %d referenced artifacts are missing; a backup refuses missing content until they are restored", art.name, missing, refs))
		files.Close()
	default:
		add("artifacts", "ok", "complete", fmt.Sprintf("%s store: %d referenced artifacts present", art.name, refs))
		files.Close()
	}
	add("timers", "info", "durable", fmt.Sprintf("%d durable timers (schedule slots, waits' timeouts, retries) resume on start", timerCount))
	ids := make([]string, 0, len(scopes))
	for id := range scopes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := scopes[id]
		sort.Strings(s.DrainBlockedWork)
		s.Drained = s.LiveExecutions == 0 && s.ActiveWaits == 0 && len(s.DrainBlockedWork) == 0
		r.Scopes = append(r.Scopes, *s)
	}
	return r, nil
}
