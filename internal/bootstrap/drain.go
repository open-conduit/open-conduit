package bootstrap

import (
	"context"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// drainOrigin is the state directory's real path, which a drain records as
// where it began. Nothing ends a drain for starting elsewhere.
func drainOrigin(stateDir string) string {
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	if real, err := filepath.EvalSymlinks(stateDir); err == nil {
		stateDir = real
	}
	return filepath.Clean(stateDir)
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
