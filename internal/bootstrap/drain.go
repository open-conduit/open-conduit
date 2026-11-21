package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// drainOrigin names where this daemon runs. A drain records it, and a daemon
// that starts with a different one has been moved. A host with no name still
// tells a move by its path; one that keeps both stays drained until
// `drain end`, the safe way to be wrong.
func drainOrigin(stateDir string) string {
	host, _ := os.Hostname()
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	return host + ":" + filepath.Clean(stateDir)
}

// daemonDrain serves the management API's drain routes.
type daemonDrain struct{ d *daemon }

func (x daemonDrain) status(ctx context.Context, change func(storage.Tx) error) (runtime.DrainStatus, error) {
	var out runtime.DrainStatus
	err := x.d.coord.Transact(ctx, storage.Guard{Now: x.d.now()}, func(tx storage.Tx) error {
		if change != nil {
			if err := change(tx); err != nil {
				return err
			}
		}
		var err error
		out, err = runtime.DrainStatusInTx(tx)
		return err
	})
	return out, err
}

func (x daemonDrain) DrainStatus(ctx context.Context) (runtime.DrainStatus, error) {
	return x.status(ctx, nil)
}

func (x daemonDrain) BeginDrain(ctx context.Context, reason string) (runtime.DrainStatus, error) {
	out, err := x.status(ctx, func(tx storage.Tx) error {
		_, err := runtime.BeginDrainInTx(tx, x.d.now(), drainOrigin(x.d.o.StateDir), reason)
		return err
	})
	if err == nil {
		x.d.log.Info("drain begun", "reason", reason, "safe_to_move", out.Safe)
	}
	return out, err
}

func (x daemonDrain) EndDrain(ctx context.Context) (runtime.DrainStatus, error) {
	out, err := x.status(ctx, func(tx storage.Tx) error {
		_, err := runtime.EndDrainInTx(tx)
		return err
	})
	if err == nil {
		x.d.log.Info("drain ended")
	}
	return out, err
}

// endRestoredDrain ends a drain a bundle was taken under: restoring it is the
// deliberate step the drain led up to, wherever it is restored.
func endRestoredDrain(ctx context.Context, store storage.Store) error {
	now := time.Now().UTC()
	return store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		ended, err := runtime.EndDrainInTx(tx)
		if err != nil || !ended {
			return err
		}
		_, err = tx.AppendAudit(domain.AuditRecord{Actor: "system:restore", Action: "core.drain.ended_by_restore", Outcome: domain.AuditOK})
		return err
	})
}
