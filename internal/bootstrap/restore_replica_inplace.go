package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// RestoreReplicaInPlace restores a replica into stateDir itself, which may be
// an empty mounted volume, under its daemon lock. It never replaces a
// database. A replica is rewritten by its replicator and has no identity to
// restore "once" by, so what decides is whether the directory holds state: a
// restart must keep what the daemon committed since, even when the replica
// beside it has changed or gone. With o.Once a directory holding state is left
// as it is, and without it that is refused.
//
// The state is staged and swapped in as a bundle's is, so a restore that stops
// part way is finished by the next one, and it is drained before it is staged.
func RestoreReplicaInPlace(ctx context.Context, replica, stateDir string, o cli.ReplicaOptions) (cli.ReplicaInfo, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return cli.ReplicaInfo{}, err
	}
	report := &cli.InPlaceReport{StateDir: stateDir}
	info := cli.ReplicaInfo{Path: stateDir, InPlace: report}
	policy, err := restorePolicy(o.RestoreOptions)
	if err != nil {
		return info, err
	}
	target, choice, err := restoreTarget(o.RestoreOptions)
	if err != nil {
		return info, err
	}
	if err := checkReplicaTarget(o, target); err != nil {
		return info, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return info, err
	}
	lock, err := management.AcquireLock(stateDir)
	if errors.Is(err, localfs.ErrNotLocal) {
		return info, err
	}
	if err != nil {
		return info, fmt.Errorf("%w: stop the daemon first", err)
	}
	defer lock.Close()
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
	if report.RemovedStages, err = removeStaleStages(stateDir); err != nil {
		return info, err
	}
	current, err := coordinationOf(stateDir)
	if err != nil {
		return info, err
	}
	switch present, err := current.backend.Present(ctx, current.location); {
	case err != nil:
		return info, err
	case present && resumed != nil:
		// This run put the state in place, so it does what the interrupted
		// one didn't get to.
		return finishReplicaInPlace(ctx, lock, stateDir, o, policy, info)
	case present && !o.Once:
		return info, fmt.Errorf("%s already holds state, and a replica restore never replaces it: restore into an empty directory: %w", stateDir, os.ErrExist)
	case present:
		report.AlreadyRestored = true
		return info, nil
	}
	previous, err := readChoice(stateDir)
	if err != nil {
		return info, err
	}
	if previous.Location != "" && filepath.Clean(previous.Location) != filepath.Join(stateDir, "conduit.db") {
		return info, fmt.Errorf("%s names a database outside the state directory (%s), and a replica restores into the directory: remove that choice or restore into another directory", storageFile, previous.Location)
	}
	if err := keepsStorage(stateDir, previous, o.RestoreOptions); err != nil {
		return info, err
	}
	// Copies already in the directory are what survived: without this, the
	// swap would move them aside and record their content as lost.
	if o.ArtifactBackend == "" && o.ArtifactsFrom == "" {
		o.ArtifactsFrom = existingDir(filepath.Join(stateDir, "artifacts"))
	}
	if o.ArchiveFrom == "" {
		o.ArchiveFrom = existingDir(filepath.Join(stateDir, backup.ArchiveDir))
	}
	digest, err := fileDigest(ctx, replica)
	if err != nil {
		return info, err
	}
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405Z")
	stage := filepath.Join(stateDir, ".restore-"+stamp)
	source := backup.ReplicaSource{Database: replica, Artifacts: o.ArtifactsFrom, Archive: o.ArchiveFrom}
	// The drain's origin is the state directory, where the stage is going.
	restored, err := backup.RestoreReplica(ctx, source, stage, now, retentionExpired, sqlite.Name, restoreBackends, target, drainedReplica(ctx, withStartConfig(ctx, recordChoiceWith("", choice), o.RestoreOptions, policy), stateDir, now))
	if err != nil {
		return info, err
	}
	info.Artifacts, info.MissingArtifacts = restored.Artifacts, restored.MissingArtifacts
	info.ArchivedEvents, info.MissingEvents = restored.ArchivedEvents, restored.MissingEvents
	if err := restoreStopped(); err != nil {
		return info, err
	}
	swap, err := prepareSwap(ctx, stateDir, stage, stamp, restoredRecord{DatabaseSHA256: digest, Bundle: replica, RestoredAt: now})
	if err != nil {
		_ = os.RemoveAll(stage)
		return info, err
	}
	if err := runSwap(stateDir, swap); err != nil {
		return info, fmt.Errorf("the in-place restore stopped part way: %w (run it again to finish)", err)
	}
	report.MovedAside, report.MovedItems = filepath.Join(stateDir, swap.Aside), swap.Moved
	return finishReplicaInPlace(ctx, lock, stateDir, o, policy, info)
}

// finishReplicaInPlace reconciles a directory a replica was just swapped into
// and reports on it, ending with a state check. Unlike a bundle's, its drain
// stays until the operator ends it.
func finishReplicaInPlace(ctx context.Context, lock *management.Lock, stateDir string, o cli.ReplicaOptions, policy *config.Policy, info cli.ReplicaInfo) (cli.ReplicaInfo, error) {
	info.TokenFile = filepath.Join(stateDir, stagedToken)
	_, store, err := openState(ctx, stateDir)
	if err != nil {
		return info, err
	}
	if err := store.View(ctx, func(tx storage.Tx) error {
		_, on, err := runtime.DrainInTx(tx)
		info.Drained = on
		return err
	}); err != nil {
		store.Close()
		return info, err
	}
	if err := forgetLaterRuns(ctx, lock, stateDir, store, info.InPlace); err != nil {
		store.Close()
		return info, err
	}
	if info.Scopes, err = scopeIDs(ctx, store); err != nil {
		store.Close()
		return info, err
	}
	info.ConfigGeneration, info.ConfigProblems, err = restoredConfig(ctx, store, restoreConfigPath(o.RestoreOptions), policy)
	info.StartConfig = o.StartConfig
	store.Close()
	if err != nil {
		return info, err
	}
	check, err := checkState(ctx, stateDir)
	if err != nil {
		return info, err
	}
	info.InPlace.Check = &check
	return info, nil
}

// existingDir is path when it is a directory, else "".
func existingDir(path string) string {
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		return path
	}
	return ""
}

// fileDigest is the digest of a file, recorded as what a replica restore read.
func fileDigest(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, readerContext{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readerContext stops a long read when ctx ends.
type readerContext struct {
	ctx context.Context
	r   io.Reader
}

func (r readerContext) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
