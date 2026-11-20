package sqlite

import (
	"context"

	"github.com/open-conduit/open-conduit/storage"
)

// rerunTransact runs a transaction under storage.Retry with its first run
// aborted: the callback runs to completion, the run is rolled back with
// storage.ErrTransient, and Retry runs the callback again over the same
// state, which this time commits. Built with -tags rerun, every Transact
// and View goes through it, so a callback that is not safe to rerun (one
// that accumulates into state outside it, keeps a flag from an earlier run
// or runs an effect inside the transaction) fails its own tests the way it
// would under a real transient abort in the daemon.
func rerunTransact(ctx context.Context, g storage.Guard, inner func(context.Context, storage.Guard, func(storage.Tx) error) error, fn func(storage.Tx) error) error {
	return storage.Retry(&abortFirst{transact: inner}, rerunPolicy).Transact(ctx, g, fn)
}

// rerunView is rerunTransact for a View.
func rerunView(ctx context.Context, inner func(context.Context, func(storage.Tx) error) error, fn func(storage.Tx) error) error {
	s := storage.Retry(&abortFirst{view: inner}, rerunPolicy).(storage.Viewer)
	return s.View(ctx, fn)
}

var rerunPolicy = storage.RetryPolicy{Attempts: 2}

// abortFirst is one transaction's store: its first run aborts once the
// callback has succeeded, every later run is the inner store's own.
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
