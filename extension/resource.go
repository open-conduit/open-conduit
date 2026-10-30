package extension

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// Resource modes an executor may request from a provider. A provider documents
// which it supports and rejects others with ErrUnsupported. An empty mode
// selects the provider configuration's default.
const (
	// ResourceShared is one long-lived resource shared by concurrent users (for
	// example a scope's main checkout). It is never reset on a user's behalf.
	ResourceShared = "shared"
	// ResourceWork is private to one Work and reused by each of its attempts
	// until the Work is terminal (for example a directory that keeps a Work's
	// local changes between attempts).
	ResourceWork = "work"
	// ResourceFresh is private to one execution, created for it and cleaned up
	// after it finishes (kept for inspection after a failure).
	ResourceFresh = "fresh"
)

var (
	// ErrResourceSetup means the resource exists but its setup failed. The
	// returned Resource is still valid for inspection; it is retained under the
	// provider's failure policy.
	ErrResourceSetup = errors.New("resource setup failed")
	// ErrResourceNotFound means no lease with that reference is owned by the
	// caller. Lookup never creates a resource.
	ErrResourceNotFound = errors.New("resource lease not found")
	// ErrResourceExhausted means the provider refused to create a resource
	// because a host capacity it guards (such as free disk space) is too low.
	// Nothing was created. Trying again is refused the same way until the
	// capacity is freed, so the message says what to free.
	ErrResourceExhausted = errors.New("resource capacity exhausted")
)

// ResourceOwner is the trusted identity a lease is acquired for. It is bound by
// the host from the attempt, never supplied by an executor.
type ResourceOwner struct {
	ScopeID, WorkID, ExecutionID domain.ID
}

// ResourceRequest is what an executor asks for. Params are provider-defined
// and opaque to core (for example a requested branch name). Log, when set,
// receives human-readable progress such as setup output or why a shared
// resource is behind; it never receives secrets from the provider.
//
// Sandboxed is set by the host when the binding runs its executor in a
// sandbox (B09). The provider then refuses, before it creates or runs
// anything, a resource that shares writable state outside its directory or
// runs code from it outside the sandbox (ErrUnsupported), and describes what
// a sandbox needs to use the one it hands out (Resource.Sandbox).
type ResourceRequest struct {
	Mode      string
	Params    domain.Payload
	Log       io.Writer
	Sandboxed bool
}

// Resource is an acquired, provider-defined resource. Ref is the opaque lease
// reference an executor may keep in its recovery metadata. Dir is the working
// location, if the resource has one. Env is the provider's environment layer,
// applied after the sanitized process baseline and before binding variables
// (B05); it is never persisted by the provider. Data describes the acquisition
// (for example how far a shared checkout is behind) for history, and Summary
// says the same in one human-readable line. Sandbox is what a process
// sandbox must allow and deny for the resource to be used in it (its
// directory writable, its own control files read-only); nil means it can't
// be used in one.
type Resource struct {
	Kind    string
	Ref     string
	Dir     string
	Env     map[string]string
	Data    domain.Payload
	Summary string
	Sandbox *SandboxNeeds
}

// ResourceAcquisition is the trusted request a provider receives: the bound
// owner, the binding's opaque resource config and the executor's request.
type ResourceAcquisition struct {
	Owner   ResourceOwner
	Config  domain.Payload
	Request ResourceRequest
}

// ResourceOwnerStatus is durable runtime state reported by the host for a
// lease owner. ExecutionDone means the attempt is terminal and its physical
// resource exclusion is confirmed, so nothing still runs in the resource.
type ResourceOwnerStatus struct {
	Known              bool
	ExecutionDone      bool
	ExecutionSucceeded bool
	WorkDone           bool
	WorkSucceeded      bool
}

// ResourceStatusFunc reports owner status from trusted durable state.
type ResourceStatusFunc func(context.Context, ResourceOwner) (ResourceOwnerStatus, error)

// ResourceLeaseReport describes one lease that needs operator attention.
type ResourceLeaseReport struct {
	Ref, Mode, State, Code string
	RetainUntil            time.Time
}

// ResourceReport summarizes one reconcile pass.
type ResourceReport struct {
	Active, Released, Removed, Retained int
	Attention                           []ResourceLeaseReport
}

// ResourceSpare is one spare resource a sweep found: why it was spare
// (Spare: finished_work, owner_unknown or orphaned) and, when the sweep kept
// it, why (Kept: uncommitted, unpushed, in_use, unreadable or remove_failed).
type ResourceSpare struct {
	Ref, Mode, Spare, Kept string
}

// ResourceSweepReport summarizes one sweep.
type ResourceSweepReport struct {
	Removed, Kept []ResourceSpare
}

// ResourceSweeper is optionally implemented by a provider that can find spare
// resources no lease release covers: those of finished Work it still keeps,
// those whose owner no record holds and those no lease holds at all. A sweep
// never removes one with unsaved changes or one a running process uses. The
// host calls it on a slow cadence (default every 6 hours) with the same
// trusted status as Reconcile.
type ResourceSweeper interface {
	Sweep(context.Context, ResourceStatusFunc) (ResourceSweepReport, error)
}

// ResourceProvider is an optional extension that supplies execution resources.
// Core never calls it and never inspects resources; a host binds it to an
// attempt (BindResources) and drives Reconcile from durable owner state.
//
// Acquire is idempotent per owner and mode: acquiring again (a resumed segment
// or a retried launch) returns the same lease and never duplicates the
// resource. A provider durably records a lease before creating anything, so a
// crash at any point leaves a lease Reconcile can find. Lookup returns an
// existing lease owned by the caller and never creates one. Reconcile releases
// leases whose owners finished, applies retention, retries failed cleanup and
// must be safe to call repeatedly and after restart.
type ResourceProvider interface {
	Acquire(context.Context, ResourceAcquisition) (Resource, error)
	Lookup(ctx context.Context, owner ResourceOwner, ref string) (Resource, error)
	Reconcile(context.Context, ResourceStatusFunc) (ResourceReport, error)
}

// ResourceBroker is what an executor sees in its ExecutionContext: a provider
// bound to the attempt's trusted owner and binding config.
type ResourceBroker interface {
	Acquire(context.Context, ResourceRequest) (Resource, error)
	Lookup(ctx context.Context, ref string) (Resource, error)
}

// BindResources binds a provider to one attempt. Hosts call it; executors only
// see the returned broker, so they cannot act for another owner.
func BindResources(p ResourceProvider, owner ResourceOwner, config domain.Payload) ResourceBroker {
	return boundResources{p, owner, config}
}

type boundResources struct {
	p      ResourceProvider
	owner  ResourceOwner
	config domain.Payload
}

func (b boundResources) Acquire(ctx context.Context, r ResourceRequest) (Resource, error) {
	return b.p.Acquire(ctx, ResourceAcquisition{Owner: b.owner, Config: b.config, Request: r})
}
func (b boundResources) Lookup(ctx context.Context, ref string) (Resource, error) {
	return b.p.Lookup(ctx, b.owner, ref)
}
