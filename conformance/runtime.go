package conformance

import (
	"context"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// RuntimeDriver is a test adapter, not the production management API. Stage 02
// implements this adapter over the real scheduler/router and runs RuntimeCases.
// Methods return only after coordination commits; no executor I/O occurs in Tx.
// Time advances explicitly. Dispatch is deliberately separate from acceptance.
type RuntimeDriver interface {
	Accept(domain.Event) error
	Route(domain.ID) error
	Start(domain.ID) (domain.LeaseRef, error)
	Wait(domain.LeaseRef, domain.Wait, bool) error
	Wake(domain.ID) error
	Complete(domain.LeaseRef) error
	Fail(domain.LeaseRef) error
	Retry(domain.ID, domain.ID, bool) error
	Cancel(domain.ID) error
	Recover(domain.ID, bool) (domain.LeaseRef, error)
	Derive(domain.Event) error
}

type RuntimeRoute struct {
	Route   domain.Route
	Handler extension.RouteHandler
}

type RuntimeFixture struct {
	Driver  RuntimeDriver
	Store   storage.Store
	Clock   *Clock
	Restart func() RuntimeDriver
}

type RuntimeOptions struct {
	Routes              []RuntimeRoute
	Capabilities        domain.Capabilities
	Fallback            string
	MaxDepth, PerMinute int
	MaxAttempts         int
}

type RuntimeFactory func(RuntimeOptions) RuntimeFixture

type RuntimeCase struct {
	Name  string
	Check func(RuntimeFactory) error
}

// RuntimeRead is inspection through the public storage contract; fixtures must
// not expose private scheduler state as their evidence.
func RuntimeRead(f RuntimeFixture, fn func(storage.Tx) error) error {
	return f.Store.Transact(context.Background(), storage.Guard{Now: f.Clock.Now()}, fn)
}

const RuntimeLease = time.Minute
