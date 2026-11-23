package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

const (
	// instanceTTL is how long a holder that stops renewing keeps others out.
	// It bounds a takeover's wait and has no bearing on safety, which rests
	// on the lease's epoch.
	instanceTTL = 30 * time.Second
	// instanceRenewEvery leaves two renewals to fail before the lease lapses.
	instanceRenewEvery = instanceTTL / 3
	// instanceRetry is how often a daemon waiting for the lease asks again.
	instanceRetry = instanceTTL / 6
	// instanceReleaseTimeout bounds giving the lease up on the way out. A
	// release that fails only makes a successor wait for the expiry.
	instanceReleaseTimeout = 5 * time.Second
)

// instanceOwner names this host's use of a state directory. It is the same
// across restarts, so a restarted daemon or an offline command takes over its
// own crashed tenure at once and the Execution leases stay for it to adopt.
// Another host, or another state directory on one database, is another owner.
func instanceOwner(stateDir string) string {
	host, _ := os.Hostname()
	return host + ":" + drainOrigin(stateDir)
}

// heldDatabase is a Database held under the instance lease: every transaction
// names the tenure, and giving it up is part of closing. Only state at a
// shared location is held; any other keeps to Open's single owner.
type heldDatabase struct {
	storage.Database
	leaser storage.InstanceLeaser
	ref    storage.InstanceRef
	lost   atomic.Pointer[func(error)]
}

func (h *heldDatabase) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	ref := h.ref
	g.Instance = &ref
	err := h.Database.Transact(ctx, g, fn)
	if errors.Is(err, storage.ErrInstanceFenced) {
		h.fenced(err)
	}
	return err
}

func (h *heldDatabase) fenced(err error) {
	if lost := h.lost.Load(); lost != nil {
		(*lost)(err)
	}
}

// onLost sets what hears that this tenure was superseded.
func (h *heldDatabase) onLost(fn func(error)) { h.lost.Store(&fn) }

func (h *heldDatabase) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), instanceReleaseTimeout)
	defer cancel()
	// Superseded is fine: the state has already moved on.
	err := h.leaser.ReleaseInstance(ctx, h.ref)
	if errors.Is(err, storage.ErrInstanceFenced) {
		err = nil
	}
	return errors.Join(err, h.Database.Close())
}

// keep renews the tenure on each tick until ctx ends. A failed renewal is
// retried on the next tick, since an expired lease nobody has taken can still
// be renewed. Only a superseded tenure ends it.
func (h *heldDatabase) keep(ctx context.Context, ticks <-chan time.Time, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		err := h.leaser.RenewInstance(ctx, h.ref, instanceTTL)
		switch {
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, storage.ErrInstanceFenced):
			h.fenced(err)
			return
		default:
			log.Warn("renewing the instance lease failed; trying again", "error", err)
		}
	}
}

// rawDatabase is the backend's own Database under any lease, for the optional
// interfaces a backend implements beside it.
func rawDatabase(db storage.Database) storage.Database {
	if h, ok := db.(*heldDatabase); ok {
		return h.Database
	}
	return db
}

// shared reports whether c's location is state other hosts can reach, which
// is when the instance lease, not Open, guarantees one writer.
func (c coordination) shared() bool {
	b, ok := c.backend.(storage.SharedBackend)
	return ok && b.SharesLocation(c.location)
}

// holdInstance takes the lease on an open database at a shared location. wait
// decides what a live holder means: nil refuses, and otherwise it returns once
// it is worth asking again. A shared database with no lease is refused, since
// nothing would keep two instances from writing to it.
func holdInstance(ctx context.Context, db storage.Database, owner string, wait func(context.Context) error) (storage.Database, error) {
	leaser, ok := db.(storage.InstanceLeaser)
	if !ok {
		return nil, fmt.Errorf("a shared storage location needs a backend with an instance lease: %w", storage.ErrInvalid)
	}
	for {
		ref, err := leaser.AcquireInstance(ctx, owner, instanceTTL)
		if err == nil {
			return &heldDatabase{Database: db, leaser: leaser, ref: ref}, nil
		}
		if !errors.Is(err, storage.ErrInstanceHeld) {
			return nil, fmt.Errorf("instance lease: %w", err)
		}
		if wait == nil {
			return nil, fmt.Errorf("another instance is running on this state: %w", err)
		}
		if err := wait(ctx); err != nil {
			return nil, err
		}
	}
}

// standby is the wait of a daemon that found the state held: it stays out
// until the holder releases the lease or lets it expire.
func standby(log *slog.Logger, every time.Duration) func(context.Context) error {
	logged := false
	return func(ctx context.Context) error {
		if !logged {
			logged = true
			log.Info("another instance holds this state; waiting to take over")
		}
		//duty:instance_standby
		t := time.NewTimer(every)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
}

// openServing is openState for the daemon, which waits for a live holder
// instead of refusing.
func openServing(ctx context.Context, stateDir string, log *slog.Logger) (coordination, storage.Database, error) {
	return openHeld(ctx, stateDir, standby(log, instanceRetry))
}

func openHeld(ctx context.Context, stateDir string, wait func(context.Context) error) (coordination, storage.Database, error) {
	c, err := coordinationOf(stateDir)
	if err != nil {
		return c, nil, err
	}
	db, err := c.open(ctx)
	if err != nil {
		return c, nil, err
	}
	if !c.shared() {
		return c, db, nil
	}
	held, err := holdInstance(ctx, db, instanceOwner(stateDir), wait)
	if err != nil {
		return c, nil, errors.Join(err, db.Close())
	}
	return c, held, nil
}
