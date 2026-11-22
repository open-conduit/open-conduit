package bootstrap

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// The offline state operations behind `backup`, `state check` and
// `auth recover`. Every state operation takes the directory's exclusive
// daemon lock first, so none runs beside a daemon owning the directory.

// StateOps are the offline state operations for the operator CLI.
func StateOps() *cli.StateOps {
	return &cli.StateOps{ListDestination: ListDestination, FetchBundle: FetchBundle, Keygen: backup.GenerateKey, Backup: BackupState, Verify: VerifyBundle, Restore: RestoreState, RestoreInPlace: RestoreInPlace, Check: CheckState, RecoverOperator: RecoverOperator}
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
	if errors.Is(err, localfs.ErrNotLocal) {
		return nil, err // Stopping a daemon wouldn't help.
	}
	if err != nil {
		return nil, fmt.Errorf("%w: stop the daemon first (or use open-conduit doctor while it runs)", err)
	}
	if err := restoreInterrupted(stateDir); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// BackupState publishes a verified bundle of a stopped instance's database
// and referenced artifacts at an absent destination.
func BackupState(ctx context.Context, stateDir, destination, build string, o cli.BackupOptions) (cli.BackupInfo, error) {
	key, err := offlineSigningKey(ctx, o)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	lock, err := locked(stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer lock.Close()
	_, files, err := openArtifacts(ctx, stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer files.Close()
	events, closeEvents, err := openEventArchive(ctx, stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer closeEvents()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return cli.BackupInfo{}, err
	}
	// A newer binary's bundle of a database it hasn't started on yet holds
	// it as it stands, and leaves its format for the daemon's first start.
	pending, err := coordinationOf(stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	prior, err := pending.backend.OpenPrior(ctx, pending.location)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	if prior != nil {
		defer prior.Close()
		if err := backup.CreatePrior(ctx, destination, key, pending.name, prior, files, events, build, retentionExpired, time.Now(), lastBuild); err != nil {
			return cli.BackupInfo{}, err
		}
		return bundleInfo(destination)
	}
	c, store, err := openState(ctx, stateDir)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer store.Close()
	if err := backup.CreateWithArchive(ctx, destination, key, c.name, store, files, events, build, retention(time.Now)); err != nil {
		return cli.BackupInfo{}, err
	}
	return bundleInfo(destination)
}

// openEventArchive opens the state directory's event archive for a backup,
// nil when it has none, without creating one. Its close is always callable.
func openEventArchive(ctx context.Context, stateDir string) (storage.EventArchive, func(), error) {
	path := filepath.Join(stateDir, backup.ArchiveDir)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, func() {}, nil
	} else if err != nil {
		return nil, nil, err
	}
	events, err := sqlite.ArchiveBackend.Open(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	return events, func() { _ = events.Close() }, nil
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
	out := cli.BackupInfo{Path: bundle, Backend: info.Backend, Schema: info.Schema, RecordFormat: info.Records, Build: info.Build, CreatedAt: info.CreatedAt, Artifacts: info.Artifacts, OmittedArtifacts: info.Omitted}
	if p := info.PreUpgrade; p != nil {
		out.PreUpgrade = &cli.PreUpgradeInfo{RecordFormat: p.Records, Schema: p.Schema, FromBuild: p.FromBuild}
	}
	return out, nil
}

// VerifyBundle restores a bundle into a private temporary directory, checks
// the result and removes it.
func VerifyBundle(ctx context.Context, bundle string, o cli.RestoreOptions) (cli.BackupInfo, error) {
	info, trust, err := authenticated(bundle, o)
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
	if err := backup.Restore(ctx, bundle, dest, trust, time.Now().UTC(), retentionExpired, restoreBackends, backup.Target{Artifacts: artifacts.Backend, EventArchive: sqlite.ArchiveBackend}, recordChoice); err != nil {
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
	info, trust, err := authenticated(bundle, o)
	if err != nil {
		return info, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return info, err
	}
	if err := backup.Restore(ctx, bundle, destination, trust, time.Now().UTC(), retentionExpired, restoreBackends, target, withStartConfig(ctx, recordChoiceWith(o.DatabaseLocation, choice), o)); err != nil {
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
	if err := endRestoredDrain(ctx, store); err != nil {
		return info, err
	}
	if info.Scopes, err = scopeIDs(ctx, store); err != nil {
		return info, err
	}
	info.ConfigGeneration, info.ConfigProblems, err = restoredConfig(ctx, store, restoreConfigPath(o))
	info.StartConfig = o.StartConfig
	return info, err
}

// restoreConfigPath is the config file whose packages the restored
// generation is checked against.
func restoreConfigPath(o cli.RestoreOptions) string {
	if o.StartConfig != "" {
		return o.StartConfig
	}
	return o.ConfigPath
}

// withStartConfig publishes the operator's start config into a restore's
// stage, after the storage choice is recorded and before the directory is
// published: a config that is refused leaves nothing published, so a daemon
// can't start on the old machine's generation by accident.
func withStartConfig(ctx context.Context, record backup.Record, o cli.RestoreOptions) backup.Record {
	if o.StartConfig == "" {
		return record
	}
	return func(dir, backend string) error {
		if record != nil {
			if err := record(dir, backend); err != nil {
				return err
			}
		}
		_, store, err := openState(ctx, dir)
		if err != nil {
			return err
		}
		defer store.Close()
		data, err := config.ReadFile(o.StartConfig)
		if err != nil {
			return fmt.Errorf("reading the start config: %w", err)
		}
		var published error
		diags, _, err := checkAgainstPackages(o.StartConfig, "", data, func(reg config.Registry) (*config.Snapshot, error) {
			s, err := management.PublishStartConfig(ctx, store, func() time.Time { return time.Now().UTC() }, reg, o.StartConfig)
			var invalid *config.ValidationError
			if err != nil && !errors.As(err, &invalid) {
				published = err // A refusal, not a diagnostic: say it as it is.
			}
			return s, err
		})
		if published != nil {
			return fmt.Errorf("the start config can't be published over the bundle's state: %w", published)
		}
		if err != nil {
			return fmt.Errorf("checking the start config: %w", err)
		}
		if len(diags) > 0 {
			return fmt.Errorf("the start config %s doesn't validate here: %s: %s (%s)", o.StartConfig, diags[0].Path, diags[0].Message, diags[0].Code)
		}
		return nil
	}
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
	return recoverOperator(ctx, stateDir, tokenPath)
}

// recoverOperator is RecoverOperator for a caller holding the lock.
func recoverOperator(ctx context.Context, stateDir, tokenPath string) error {
	if err := refuseBeforeUpgrade(ctx, stateDir); err != nil {
		return err
	}
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
	lock, err := locked(stateDir)
	if err != nil {
		r := cli.StateReport{StateDir: stateDir}
		if errors.Is(err, localfs.ErrNotLocal) {
			r.Checks = append(r.Checks, cli.StateCheck{Name: "filesystem", Status: "fail", Code: filesystemCode(err), Message: err.Error()})
			return r, nil
		}
		r.Checks = append(r.Checks, cli.StateCheck{Name: "lock", Status: "fail", Code: "in_use_or_unsafe", Message: err.Error()})
		return r, nil
	}
	defer lock.Close()
	return checkState(ctx, stateDir)
}

// checkState is CheckState for a caller holding the lock.
func checkState(ctx context.Context, stateDir string) (cli.StateReport, error) {
	r := cli.StateReport{StateDir: stateDir, OK: true}
	add := func(name, status, code, msg string) {
		r.Checks = append(r.Checks, cli.StateCheck{Name: name, Status: status, Code: code, Message: msg})
		if status == "fail" {
			r.OK = false
		}
	}
	add("lock", "ok", "stopped", "no daemon owns the directory; permissions are owner-only")
	if fs, err := localfs.Detect(stateDir); err == nil {
		add("filesystem", "ok", "local", fs.Name)
	}
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
		code, remedy := "open_failed", "restore the latest verified bundle (backup restore BUNDLE --in-place)"
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
		add("database", "fail", "corrupt", "restore the latest verified bundle (backup restore BUNDLE --in-place)")
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
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			blocked := false
			for _, x := range xs {
				if !x.State.Terminal() {
					s.LiveExecutions++
				}
				blocked = blocked || runtime.InFlight(x)
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
			if blocked {
				s.DrainBlockedWork = append(s.DrainBlockedWork, string(w.ID))
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
		// A parked attempt and its Waits hold no process and travel with the
		// state, so only an attempt in flight keeps a scope from being drained.
		s.Drained = len(s.DrainBlockedWork) == 0
		r.Scopes = append(r.Scopes, *s)
	}
	return r, nil
}

// offlineSigningKey is the key `backup create` signs with: the config file's
// settings.backup.signing_key, nil when there is none or the operator asked
// for an unsigned bundle. Unlike a scheduled backup, an operator is there to
// see a failure, so a configured key that can't be had fails the command.
func offlineSigningKey(ctx context.Context, o cli.BackupOptions) (ed25519.PrivateKey, error) {
	if o.Unsigned {
		return nil, nil
	}
	settings, err := fileBackupSettings(o.ConfigPath)
	if err != nil || settings.SigningKey == "" {
		return nil, err
	}
	resolver := &config.Resolver{SecretsFile: config.SecretsPath(o.ConfigPath)}
	value, err := resolver.ResolveNamed(ctx, "backup_signing_key", settings.SigningKey)
	if err != nil {
		return nil, fmt.Errorf("resolving settings.backup.signing_key: %w (--unsigned publishes without it)", err)
	}
	return backup.ParsePrivateKey(value)
}

// fileBackupSettings reads settings.backup from the operator's config file.
// A missing file configures nothing. A file that can't be read or parsed is
// an error rather than no policy, so a damaged config can't turn the
// signature check off.
func fileBackupSettings(path string) (config.BackupSettings, error) {
	if path == "" {
		return config.BackupSettings{}, nil
	}
	data, err := config.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config.BackupSettings{}, nil
	}
	if err != nil {
		return config.BackupSettings{}, fmt.Errorf("reading the backup trust settings: %w", err)
	}
	doc, err := config.ParseFile(data)
	if err != nil {
		return config.BackupSettings{}, fmt.Errorf("reading the backup trust settings from %s: %w", path, err)
	}
	return doc.Settings.Backup, nil
}

// restoreTrust is the operator's policy for a bundle: the config file's
// trust keys and any given on the command line. It never reads the bundle.
func restoreTrust(o cli.RestoreOptions) (backup.Trust, error) {
	settings, err := fileBackupSettings(o.ConfigPath)
	if err != nil {
		return backup.Trust{}, err
	}
	trust := backup.Trust{AcceptUnsigned: o.AcceptUnsigned}
	for _, text := range append(slices.Clone(settings.TrustKeys), o.TrustKeys...) {
		key, err := backup.ParsePublicKey(text)
		if err != nil {
			return backup.Trust{}, err
		}
		trust.Keys = append(trust.Keys, key)
	}
	return trust, nil
}

// authenticated reads a bundle's manifest and applies the operator's trust
// policy, before anything is restored.
func authenticated(bundle string, o cli.RestoreOptions) (cli.BackupInfo, backup.Trust, error) {
	info, err := bundleInfo(bundle)
	if err != nil {
		return info, backup.Trust{}, err
	}
	trust, err := restoreTrust(o)
	if err != nil {
		return info, trust, err
	}
	found, err := backup.Authenticate(bundle, trust)
	if err != nil {
		return info, trust, err
	}
	info.Authenticity, info.SigningKey = found.Status, found.KeyID
	// Once the manifest is trusted, a bundle a newer binary wrote is refused
	// from it, before a stage is made or a byte is copied.
	if err := backup.CheckFormat(bundle, restoreBackends); err != nil {
		return info, trust, err
	}
	return info, trust, nil
}
