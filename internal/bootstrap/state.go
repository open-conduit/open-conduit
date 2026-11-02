package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/posthog"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/migrate"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// 06-02: explicit Open Supervisor conversion and the offline state
// operations behind `migrate config`, `backup`, `state check` and
// `auth recover`. Every state operation takes the directory's exclusive
// daemon lock first, so none runs beside a daemon owning the directory.

// Migrate converts an Open Supervisor v1 config (internal/migrate) with the
// operator's files: project checkouts are read only when asked, and a
// missing time zone is taken from the machine (v1 read sweep and triage
// times in local time).
func Migrate(r cli.MigrateRequest) (cli.MigrateResult, error) {
	opts := migrate.Options{Home: r.Home, TimeZone: r.TimeZone, ShellJobs: r.ShellJobs, DetachedTimeoutSeconds: r.DetachedTimeoutSeconds}
	if opts.TimeZone == "" {
		opts.TimeZone = localZone()
	}
	if r.RepoFiles {
		opts.ReadFile = func(path string) ([]byte, bool) {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
				return nil, false
			}
			b, err := os.ReadFile(path)
			return b, err == nil
		}
	}
	res, err := migrate.Convert(r.Source, opts)
	if err != nil {
		return cli.MigrateResult{}, err
	}
	out := cli.MigrateResult{Config: res.Config, Scopes: res.Scopes}
	for _, it := range res.Items {
		out.Items = append(out.Items, cli.MigrateItem(it))
	}
	for _, s := range res.Secrets {
		out.Secrets = append(out.Secrets, cli.MigrateSecret(s))
	}
	return out, nil
}

// renames are the extension settings this release renamed, from each
// extension's own table.
var renames = migrate.Renames{posthog.Type: posthog.Renamed}

// Upgrade renames them in an Open Conduit config (`migrate upgrade`).
func Upgrade(config []byte) (cli.MigrateResult, error) {
	res, err := migrate.Upgrade(config, renames)
	if err != nil {
		return cli.MigrateResult{}, err
	}
	out := cli.MigrateResult{Config: res.Config, Scopes: res.Scopes, Items: []cli.MigrateItem{}, Secrets: []cli.MigrateSecret{}}
	for _, it := range res.Items {
		out.Items = append(out.Items, cli.MigrateItem(it))
	}
	return out, nil
}

// RenamedWarnings is one warning per renamed setting a config still uses,
// for `config check` and setup; the old names work for one release.
func RenamedWarnings(config []byte) []string {
	res, err := migrate.Upgrade(config, renames)
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range res.Items {
		out = append(out, it.Path+" is a renamed setting ("+it.Message+"); the old name works for one release: run open-conduit migrate upgrade")
	}
	return out
}

// localZone is the machine's IANA zone from TZ or /etc/localtime, else "".
func localZone() string {
	if tz := os.Getenv("TZ"); tz != "" && !strings.HasPrefix(tz, ":") {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(target, "zoneinfo/"); ok {
			if _, err := time.LoadLocation(zone); err == nil {
				return zone
			}
		}
	}
	return ""
}

// StateOps are the offline state operations for the operator CLI.
func StateOps() *cli.StateOps {
	return &cli.StateOps{Backup: BackupState, Verify: VerifyBundle, Restore: RestoreState, Check: CheckState, RecoverOperator: RecoverOperator}
}

// locked opens a stopped instance's state directory under its daemon lock.
func locked(stateDir string) (*management.Lock, error) {
	if _, err := os.Stat(filepath.Join(stateDir, "conduit.db")); err != nil {
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
	store, err := sqlite.Open(ctx, filepath.Join(stateDir, "conduit.db"))
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer store.Close()
	files, err := artifacts.Open(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer files.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return cli.BackupInfo{}, err
	}
	if err := backup.Create(ctx, destination, store, files, build, retentionExpired); err != nil {
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

func bundleInfo(bundle string) (cli.BackupInfo, error) {
	info, err := backup.Inspect(bundle)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	return cli.BackupInfo{Path: bundle, Schema: info.Schema, Build: info.Build, CreatedAt: info.CreatedAt, Artifacts: info.Artifacts, OmittedArtifacts: info.Omitted}, nil
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
	if err := backup.Restore(ctx, bundle, dest, time.Now().UTC(), retentionExpired); err != nil {
		return info, err
	}
	store, err := sqlite.Open(ctx, filepath.Join(dest, "conduit.db"))
	if err != nil {
		return info, err
	}
	defer store.Close()
	info.Scopes, err = scopeIDs(ctx, store)
	return info, err
}

func scopeIDs(ctx context.Context, store *sqlite.Store) ([]string, error) {
	var out []string
	err := store.Transact(ctx, storage.Guard{Now: time.Now().UTC()}, func(tx storage.Tx) error {
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
func RestoreState(ctx context.Context, bundle, destination string) (cli.BackupInfo, error) {
	info, err := bundleInfo(bundle)
	if err != nil {
		return info, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return info, err
	}
	if err := backup.Restore(ctx, bundle, destination, time.Now().UTC(), retentionExpired); err != nil {
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
	store, err := sqlite.Open(ctx, filepath.Join(destination, "conduit.db"))
	if err != nil {
		return info, err
	}
	defer store.Close()
	info.Scopes, err = scopeIDs(ctx, store)
	return info, err
}

// RecoverOperator is the locked local credential reset (B05): it revokes
// every operator credential and atomically writes a fresh token file.
func RecoverOperator(ctx context.Context, stateDir, tokenPath string) error {
	lock, err := locked(stateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	store, err := sqlite.Open(ctx, filepath.Join(stateDir, "conduit.db"))
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
	tmp, err := os.MkdirTemp("", "oc-check-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(tmp)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(filepath.Join(stateDir, "conduit.db"+suffix), filepath.Join(tmp, "conduit.db"+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			add("database", "fail", "unreadable", "the database could not be copied for inspection")
			return r, nil
		}
	}
	store, err := sqlite.Open(ctx, filepath.Join(tmp, "conduit.db"))
	if err != nil {
		code, remedy := "open_failed", "restore the latest verified bundle into a new state directory (backup restore)"
		msg := err.Error()
		switch {
		case errors.Is(err, sqlite.ErrRecordsNewer):
			code, remedy = "records_newer", "a newer Open Conduit wrote these records and this binary would drop the fields it doesn't know: run that binary, or restore the bundle taken before the upgrade"
		case strings.Contains(msg, "newer than supported"):
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
	add("database", "ok", "schema_1", fmt.Sprintf("schema %d, integrity and foreign keys pass", store.SchemaVersion()))
	files, ferr := artifacts.Open(filepath.Join(stateDir, "artifacts"))
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
	err = store.Transact(ctx, storage.Guard{Now: time.Now().UTC()}, func(tx storage.Tx) error {
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
				for _, ref := range x.Artifacts {
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
		add("artifacts", "fail", "artifacts_unavailable", "the artifact directory could not be opened")
	case missing > 0:
		add("artifacts", "warn", "artifacts_missing", fmt.Sprintf("%d of %d referenced artifacts are missing; a backup refuses missing content until they are restored", missing, refs))
		files.Close()
	default:
		add("artifacts", "ok", "complete", fmt.Sprintf("%d referenced artifacts present", refs))
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
