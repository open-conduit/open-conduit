package reference

import (
	"context"

	"github.com/open-conduit/open-conduit/storage"
)

// Built with -tags rerun, every Memory Transact and View runs under
// storage.Retry with its first run aborted: the callback runs to
// completion, the run is discarded with storage.ErrTransient, and Retry
// runs the callback again over the same state, which this time commits.
// A callback that is not safe to rerun then fails its own tests, as with
// the SQLite backend's harness (internal/storage/sqlite/rerun.go).
var rerunPolicy = storage.RetryPolicy{Attempts: 2}

// abortFirst is one transaction's store: its first run aborts once the
// callback has succeeded, every later run is the Memory's own.
type abortFirst struct {
	transact func(context.Context, storage.Guard, func(storage.Tx) error) error
	view     func(context.Context, func(storage.Tx) error) error
	ran      bool
}

func (a *abortFirst) wrap(fn func(storage.Tx) error) func(storage.Tx) error {
	if a.ran {
		return fn
	}
	a.ran = true
	return func(tx storage.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return storage.ErrTransient
	}
}

func (a *abortFirst) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return a.transact(ctx, g, a.wrap(fn))
}

func (a *abortFirst) View(ctx context.Context, fn func(storage.Tx) error) error {
	return a.view(ctx, a.wrap(fn))
}
