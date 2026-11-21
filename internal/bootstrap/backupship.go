package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
)

// Shipping bundles off the host. After a scheduled bundle is published the
// host puts its files to every configured destination, the manifest last,
// so a bundle cut short is never listed as complete. A shipment runs apart
// from the backup, and a destination that stops making progress is given up
// on, so neither a slow nor a hung one holds up the local bundle. The outcome
// is a diagnostic per destination and a telemetry event. What a destination
// keeps, for how long and how it encrypts it are its owner's policy.

// shipStallLimit is how long a destination may go without making progress
// before the shipment gives up on it. It bounds silence, not the transfer, so
// a large bundle on a slow link still lands. It sits above the 10 minutes the
// bundled S3 type allows one request.
const shipStallLimit = 15 * time.Minute

var errShipStalled = errors.New("the destination stopped responding")

// withinStall runs one destination call, cancelling it if progress isn't
// called for limit.
func withinStall(ctx context.Context, limit time.Duration, call func(ctx context.Context, progress func()) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(limit, func() { cancel(errShipStalled) })
	defer timer.Stop()
	err := call(ctx, func() { timer.Reset(limit) })
	if err != nil && errors.Is(context.Cause(ctx), errShipStalled) {
		return errShipStalled
	}
	return err
}

// progressReader reports each read of a file being uploaded.
type progressReader struct {
	r        io.Reader
	progress func()
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.progress()
	return n, err
}

// shipTarget is one destination the host can reach, or the reason it can't.
type shipTarget struct {
	cfg config.BackupDestination
	d   extension.BackupDestination
	err error
}

func destinationTarget(c config.BackupDestination) extension.BackupTarget {
	raw := c.Config
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return extension.BackupTarget{Name: c.Name, Config: domain.Payload{Schema: extension.BackupDestinationConfigSchema, Data: raw}}
}

func (d *daemon) shipTargets() []shipTarget {
	if d.backups.targets != nil {
		return d.backups.targets()
	}
	if d.configs == nil {
		return nil
	}
	s, _ := d.configs.Current()
	if s == nil {
		return nil
	}
	var out []shipTarget
	for _, c := range s.Settings.Backup.Destinations {
		t := shipTarget{cfg: c, err: fmt.Errorf("no running package provides the backup destination type %q", c.Type)}
		for _, x := range d.extensionPackages() {
			if x.r == nil {
				continue
			}
			if v, ok := x.r.BackupDestination(c.Type); ok {
				t.d, t.err = v, nil
				break
			}
		}
		out = append(out, t)
	}
	return out
}

// bundleFiles are a bundle's regular files, in the order they must be put:
// the data, then the signature, then the manifest that completes it.
func bundleFiles(dir string) ([]extension.BackupFile, error) {
	var files []extension.BackupFile
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("backup: %s is not a regular file", p)
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !extension.ValidBackupPath(rel) {
			return fmt.Errorf("backup: %q can't be shipped", rel)
		}
		files = append(files, extension.BackupFile{Path: rel, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	rank := func(p string) int {
		switch p {
		case extension.BackupManifest:
			return 2
		case "manifest.sig":
			return 1
		}
		return 0
	}
	sort.SliceStable(files, func(a, b int) bool {
		if ra, rb := rank(files[a].Path), rank(files[b].Path); ra != rb {
			return ra < rb
		}
		return files[a].Path < files[b].Path
	})
	if len(files) == 0 || files[len(files)-1].Path != extension.BackupManifest {
		return nil, errors.New("backup: the bundle has no manifest")
	}
	return files, nil
}

// shipResult is a destination's latest shipment: the bundle, and what went
// wrong when it didn't land.
type shipResult struct {
	bundle  string
	problem string
}

func (d *daemon) recordShip(name string, r shipResult) {
	b := &d.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shipped == nil {
		b.shipped = map[string]shipResult{}
	}
	b.shipped[name] = r
}

// backupCheck is doctor's view of whether bundles are leaving the host, and
// whether they are signed. It calls no destination.
func (d *daemon) backupCheck(context.Context) runtime.Check {
	const name = "backup_destinations"
	if d.o.BackupInterval < 0 {
		return runtime.Check{Name: name, Status: runtime.CheckInfo, Code: "backups_off", Detail: "scheduled backups are off, so nothing is shipped"}
	}
	var configured []string
	if d.configs != nil {
		if s, _ := d.configs.Current(); s != nil {
			for _, c := range s.Settings.Backup.Destinations {
				configured = append(configured, c.Name)
			}
		}
	}
	unsigned := d.reportedCode("backup_signing", "")
	if len(configured) == 0 {
		if unsigned != "" {
			return runtime.Check{Name: name, Status: runtime.CheckWarn, Code: unsigned, Detail: "scheduled bundles are being published unsigned: the signing key can't be used"}
		}
		return runtime.Check{Name: name, Status: runtime.CheckInfo, Code: "none_configured", Detail: "no backup destination is configured, so bundles stay on this host"}
	}
	b := &d.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	var failed, waiting, landed []string
	for _, c := range configured {
		r, ok := b.shipped[c]
		switch {
		case !ok:
			waiting = append(waiting, c)
		case r.problem != "":
			failed = append(failed, c+": "+r.problem)
		default:
			landed = append(landed, c+": "+r.bundle)
		}
	}
	switch {
	case len(failed) > 0:
		return runtime.Check{Name: name, Status: runtime.CheckWarn, Code: "ship_failed", Detail: fmt.Sprintf("%d of %d backup destination(s) don't hold the latest bundle", len(failed), len(configured)), Items: failed}
	case unsigned != "":
		return runtime.Check{Name: name, Status: runtime.CheckWarn, Code: unsigned, Detail: "bundles are shipping, but unsigned: the signing key can't be used", Items: landed}
	case len(waiting) > 0:
		return runtime.Check{Name: name, Status: runtime.CheckInfo, Code: "not_yet_shipped", Detail: "no shipment to these destinations since the daemon started", Items: waiting}
	}
	return runtime.Check{Name: name, Status: runtime.CheckOK, Code: "shipped", Detail: "every backup destination holds the latest bundle", Items: landed}
}

// shipFailure says how far a shipment got.
type shipFailure struct {
	stored, total int
	err           error
}

func (e *shipFailure) Error() string {
	return fmt.Sprintf("%d of %d files stored, so the bundle is incomplete there and is not listed for restore: %v", e.stored, e.total, e.err)
}
func (e *shipFailure) Unwrap() error { return e.err }

// firstMissing is the index of the first file stored doesn't hold at its
// size, or -1 when it holds them all.
func firstMissing(files, stored []extension.BackupFile) int {
	sizes := make(map[string]int64, len(stored))
	for _, f := range stored {
		sizes[f.Path] = f.Size
	}
	for i, f := range files {
		if got, ok := sizes[f.Path]; !ok || got != f.Size {
			return i
		}
	}
	return -1
}

// storedFiles asks a destination what it holds of one bundle.
func storedFiles(ctx context.Context, t shipTarget, name string, limit time.Duration) ([]extension.BackupFile, error) {
	var stored []extension.BackupFile
	err := withinStall(ctx, limit, func(ctx context.Context, _ func()) error {
		var err error
		stored, err = t.d.Files(ctx, destinationTarget(t.cfg), name)
		return err
	})
	return stored, err
}

// putBundle stores a bundle and checks that the destination holds every
// file at its size before calling it shipped.
func putBundle(ctx context.Context, t shipTarget, dir, name string, limit time.Duration) (int, int64, error) {
	files, err := bundleFiles(dir)
	if err != nil {
		return 0, 0, err
	}
	target := destinationTarget(t.cfg)
	var total int64
	for i, f := range files {
		r, err := os.Open(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return 0, 0, &shipFailure{i, len(files), err}
		}
		err = withinStall(ctx, limit, func(ctx context.Context, progress func()) error {
			return t.d.Put(ctx, target, name, f, progressReader{r, progress})
		})
		_ = r.Close()
		if err != nil {
			return 0, 0, &shipFailure{i, len(files), fmt.Errorf("%s: %w", f.Path, err)}
		}
		total += f.Size
	}
	stored, err := storedFiles(ctx, t, name, limit)
	if err != nil {
		return 0, 0, &shipFailure{len(files), len(files), fmt.Errorf("checking what was stored: %w", err)}
	}
	if i := firstMissing(files, stored); i >= 0 {
		return 0, 0, &shipFailure{i, len(files), fmt.Errorf("%s is missing or the wrong size at the destination", files[i].Path)}
	}
	return len(files), total, nil
}

// holdsBundle asks whether a destination already has this bundle whole. It
// costs one question about one bundle, however many the destination keeps.
func holdsBundle(ctx context.Context, t shipTarget, dir, name string, limit time.Duration) (bool, error) {
	files, err := bundleFiles(dir)
	if err != nil {
		return false, err
	}
	stored, err := storedFiles(ctx, t, name, limit)
	if err != nil {
		return false, err
	}
	return firstMissing(files, stored) < 0, nil
}

// claimShipment takes the newest bundle for shipping when a shipment is due
// and none is in flight. The claim keeps pruning off it.
func (d *daemon) claimShipment(now time.Time) (string, bool) {
	b := &d.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shipping != "" || b.shipDue.IsZero() || now.Before(b.shipDue) {
		return "", false
	}
	names, _, err := scheduledBundles(d.o.BackupDir)
	if err != nil || len(names) == 0 {
		b.shipDue = time.Time{}
		return "", false
	}
	b.shipping = names[len(names)-1]
	return b.shipping, true
}

// shipBundle ships a claimed bundle to every destination that doesn't
// already hold it, and releases the claim. A failure is tried again after
// backupRetry, with whatever bundle is then the newest; a bundle published
// meanwhile is owed at once.
func (d *daemon) shipBundle(ctx context.Context, name string) {
	b := &d.backups
	defer func() {
		b.mu.Lock()
		b.shipping = ""
		b.mu.Unlock()
	}()
	limit := b.shipStall
	if limit <= 0 {
		limit = shipStallLimit
	}
	dir := filepath.Join(d.o.BackupDir, name)
	failed := false
	for _, t := range d.shipTargets() {
		subject := domain.ID(t.cfg.Name)
		err := t.err
		var files int
		var size int64
		held := false
		if err == nil {
			held, err = holdsBundle(ctx, t, dir, name, limit)
		}
		if err == nil && !held {
			files, size, err = putBundle(ctx, t, dir, name, limit)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failed = true
			d.report("backup_destination", subject, "ship_failed", fmt.Sprintf("bundle %s to %s: %v", name, t.cfg.Name, err))
			props := map[string]any{"destination": t.cfg.Name, "type": t.cfg.Type, "bundle": name, "outcome": "failed"}
			var partial *shipFailure
			if errors.As(err, &partial) {
				props["files_stored"], props["files"] = partial.stored, partial.total
			}
			d.capture(ctx, "backup.shipped", props)
			problem := "the destination can't be reached"
			if partial != nil {
				problem = fmt.Sprintf("%d of %d files stored", partial.stored, partial.total)
			}
			d.recordShip(t.cfg.Name, shipResult{bundle: name, problem: "bundle " + name + ": " + problem})
			continue
		}
		d.clear("backup_destination", subject)
		d.recordShip(t.cfg.Name, shipResult{bundle: name})
		if !held {
			d.log.Info("backup shipped", "destination", t.cfg.Name, "bundle", name, "files", files, "bytes", size)
			d.capture(ctx, "backup.shipped", map[string]any{"destination": t.cfg.Name, "type": t.cfg.Type, "bundle": name, "outcome": "stored", "files": files, "bytes": size})
		}
	}
	now := d.now()
	names, _, _ := scheduledBundles(d.o.BackupDir)
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case failed:
		b.shipDue = now.Add(min(backupRetry, d.o.BackupInterval))
	case len(names) > 0 && names[len(names)-1] != name:
		b.shipDue = now
	default:
		b.shipDue = time.Time{}
	}
}

// offlineDestination starts the package that serves a named destination in
// the operator's config file.
func offlineDestination(configPath, name string) (extension.BackupDestination, extension.BackupTarget, func(), error) {
	settings, err := fileBackupSettings(configPath)
	if err != nil {
		return nil, extension.BackupTarget{}, nil, err
	}
	i := slices.IndexFunc(settings.Destinations, func(d config.BackupDestination) bool { return d.Name == name })
	if i < 0 {
		return nil, extension.BackupTarget{}, nil, fmt.Errorf("settings.backup.destinations in %s has no destination %q", configPath, name)
	}
	c := settings.Destinations[i]
	data, err := config.ReadFile(configPath)
	if err != nil {
		return nil, extension.BackupTarget{}, nil, err
	}
	reg := registry(map[string]extension.Executor{})
	pkgs := offlineExternal(&reg, configPath, "", data)
	for _, x := range pkgs.installed {
		if !slices.ContainsFunc(x.m.Provides.BackupDestinations, func(t protocol.BackupDestinationDecl) bool { return t.Type == c.Type }) {
			continue
		}
		r, err := x.remote()
		if err != nil {
			pkgs.close()
			return nil, extension.BackupTarget{}, nil, fmt.Errorf("starting the package for backup destination %s: %w", name, err)
		}
		v, ok := r.BackupDestination(c.Type)
		if !ok {
			break
		}
		return v, destinationTarget(c), pkgs.close, nil
	}
	pkgs.close()
	return nil, extension.BackupTarget{}, nil, fmt.Errorf("no installed or bundled package provides the backup destination type %q", c.Type)
}

// ListDestination names the complete bundles a destination holds.
func ListDestination(ctx context.Context, name string, o cli.RestoreOptions) (cli.DestinationBundles, error) {
	v, t, closePackages, err := offlineDestination(o.ConfigPath, name)
	if err != nil {
		return cli.DestinationBundles{}, err
	}
	defer closePackages()
	bundles, err := v.List(ctx, t)
	if err != nil {
		return cli.DestinationBundles{}, err
	}
	kept := make([]string, 0, len(bundles))
	for _, b := range bundles {
		if extension.ValidBackupBundle(b) {
			kept = append(kept, b)
		}
	}
	sort.Strings(kept)
	return cli.DestinationBundles{Destination: name, Bundles: kept}, nil
}

// FetchBundle downloads a bundle from a destination into an absent directory.
func FetchBundle(ctx context.Context, name, bundle, destination string, o cli.RestoreOptions) (cli.BackupInfo, error) {
	v, t, closePackages, err := offlineDestination(o.ConfigPath, name)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer closePackages()
	return fetchBundle(ctx, v, t, bundle, destination, o)
}

// fetchBundle authenticates as it goes. Whoever can write to a destination
// can plant a bundle there, so a fetch needs a trust key or an explicit
// --accept-unsigned even where a local restore doesn't. The manifest and
// signature come first and are checked before any other file is fetched,
// and the whole bundle is verified before it is published at destination.
func fetchBundle(ctx context.Context, v extension.BackupDestination, t extension.BackupTarget, bundle, destination string, o cli.RestoreOptions) (cli.BackupInfo, error) {
	if !extension.ValidBackupBundle(bundle) {
		return cli.BackupInfo{}, fmt.Errorf("%q is not a bundle name", bundle)
	}
	trust, err := restoreTrust(o)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	if len(trust.Keys) == 0 && !o.AcceptUnsigned {
		return cli.BackupInfo{}, fmt.Errorf("%w: a bundle from a destination needs a trust key (settings.backup.trust_keys or --trust-key), or --accept-unsigned to take it unauthenticated", backup.ErrNotAuthentic)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return cli.BackupInfo{}, err
		}
		return cli.BackupInfo{}, errors.New("backup: destination already exists")
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return cli.BackupInfo{}, err
	}
	stage, err := os.MkdirTemp(parent, ".conduit-snapshot-")
	if err != nil {
		return cli.BackupInfo{}, err
	}
	defer os.RemoveAll(stage)
	files, err := v.Files(ctx, t, bundle)
	if err != nil {
		return cli.BackupInfo{}, err
	}
	seen := make(map[string]bool, len(files))
	var first, rest []extension.BackupFile
	for _, f := range files {
		if !fetchablePath(f.Path) || f.Size < 0 || seen[f.Path] {
			return cli.BackupInfo{}, fmt.Errorf("backup: the destination lists an unexpected file %q", f.Path)
		}
		seen[f.Path] = true
		if f.Path == extension.BackupManifest || f.Path == "manifest.sig" {
			first = append(first, f)
		} else {
			rest = append(rest, f)
		}
	}
	if !seen[extension.BackupManifest] {
		return cli.BackupInfo{}, fmt.Errorf("backup: bundle %s is incomplete at the destination (no manifest)", bundle)
	}
	for _, f := range first {
		if err := fetchFile(ctx, v, t, bundle, f, stage); err != nil {
			return cli.BackupInfo{}, err
		}
	}
	if _, err := backup.Authenticate(stage, trust); err != nil {
		return cli.BackupInfo{}, err
	}
	for _, f := range rest {
		if err := fetchFile(ctx, v, t, bundle, f, stage); err != nil {
			return cli.BackupInfo{}, err
		}
	}
	// A destination stores files, not directories, so the artifact store's
	// empty directories are made again.
	store, err := artifacts.OpenBundle(filepath.Join(stage, "artifacts"))
	if err != nil {
		return cli.BackupInfo{}, err
	}
	if err := store.Close(); err != nil {
		return cli.BackupInfo{}, err
	}
	info, err := VerifyBundle(ctx, stage, o)
	if err != nil {
		return info, err
	}
	if err := os.Rename(stage, destination); err != nil {
		return info, err
	}
	info.Path = destination
	return info, nil
}

// fetchablePath is a path a bundle holds: its manifest, signature, database
// or a file of its artifact store.
func fetchablePath(p string) bool {
	if !extension.ValidBackupPath(p) {
		return false
	}
	switch p {
	case extension.BackupManifest, "manifest.sig", "conduit.db":
		return true
	}
	return strings.HasPrefix(p, "artifacts/")
}

// fetchFile copies one file, holding the destination to the size it stated.
func fetchFile(ctx context.Context, v extension.BackupDestination, t extension.BackupTarget, bundle string, f extension.BackupFile, stage string) (err error) {
	path := filepath.Join(stage, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()
	var offset int64
	for {
		chunk, eof, err := v.Read(ctx, t, bundle, f.Path, offset, protocol.MaxChunkBytes)
		if err != nil {
			return fmt.Errorf("fetching %s: %w", f.Path, err)
		}
		if int64(len(chunk)) > f.Size-offset {
			return fmt.Errorf("backup: %s is larger than the destination stated", f.Path)
		}
		if _, err := out.Write(chunk); err != nil {
			return err
		}
		offset += int64(len(chunk))
		if eof {
			break
		}
		if len(chunk) == 0 {
			return fmt.Errorf("backup: %s stopped before its end", f.Path)
		}
	}
	if offset != f.Size {
		return fmt.Errorf("backup: %s is %d bytes, not the %d the destination stated", f.Path, offset, f.Size)
	}
	return out.Sync()
}
