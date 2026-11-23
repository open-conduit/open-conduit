package storage

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrInstanceHeld refuses to acquire state another instance holds under
	// a lease that has not expired. Nothing changed; a standby tries again.
	ErrInstanceHeld = errors.New("coordination state is held by another instance")
	// ErrInstanceFenced refuses a write from an instance that no longer
	// holds the state, or one that names no holder while another holds it.
	// Nothing was committed. It is never transient: the instance has lost
	// its authority and must stop acting on the state.
	ErrInstanceFenced = errors.New("instance lease superseded or not held")
)

// InstanceRef is one tenure of the instance lease: who holds the state, and
// the epoch that tenure was granted. Every acquisition has a higher epoch
// than the one before it, so a reference can never become valid again.
type InstanceRef struct {
	Owner string
	Epoch uint64
}

// InstanceLeaser is a Database with an instance lease, the one claim on the
// whole state that makes a daemon its active instance. A standby blocks on
// AcquireInstance, and a holder puts its reference in the Guard of every
// transaction (Holding does that).
//
// Safety rests on the epoch alone. The backend compares a transaction's
// reference with the stored one inside that transaction, so a paused holder
// that wakes after a takeover commits nothing, however wrong any clock is.
// Expiry only decides when a successor may acquire, so clock error can delay
// a takeover or start one early, and never lets two instances commit. The
// backend judges expiry against its own clock, never a caller's.
//
// While a holder is recorded, even past its expiry, a write must name it: a
// Guard without Instance is refused with ErrInstanceFenced, so a path that
// forgot its reference fails closed. Reads (View) are not fenced. On state no
// instance holds, a Guard without Instance is the trusted control plane, as
// it is for a backend's single owner.
//
// It is separate from Database so that adding it breaks no backend built
// outside this repository. The lifecycle suite requires it.
type InstanceLeaser interface {
	// AcquireInstance takes the lease for ttl under a higher epoch. It
	// returns ErrInstanceHeld while a different owner holds an unexpired
	// lease. The same owner may acquire at once, which supersedes its own
	// earlier tenure: that is a restart on the same host.
	//
	// When the last holder was a different owner, the same transaction
	// fences every Execution lease (see Database.Fence), because what proved
	// those attempts went with that owner's host. An owner must therefore
	// name something stable that can prove its own attempts again, and be
	// unique among the instances sharing the state.
	//
	// It refuses state stored at newer versions than this binary writes
	// (ErrSchemaNewer, ErrRecordsNewer), so a standby that opened the state
	// before a newer binary raised it can't take it over.
	AcquireInstance(ctx context.Context, owner string, ttl time.Duration) (InstanceRef, error)
	// RenewInstance extends the lease to the backend's now+ttl, never
	// shortening it. A holder past its expiry may still renew if nobody has
	// acquired since. A superseded or released reference is ErrInstanceFenced.
	RenewInstance(ctx context.Context, ref InstanceRef, ttl time.Duration) error
	// ReleaseInstance gives the lease up so a successor need not wait for
	// it to expire, keeping the epoch and the record of who held it.
	// Releasing a tenure already released is nil, and a superseded one is
	// ErrInstanceFenced.
	ReleaseInstance(ctx context.Context, ref InstanceRef) error
}

// SharedBackend is a Backend whose state at a location several hosts can reach
// at once, so a standby on another host can take it over. Where SharesLocation
// is true, Open and Inspect take no exclusive ownership (a standby must be able
// to open the state while another instance runs on it), the Database must be
// an InstanceLeaser, and that lease is what guarantees one writer. A host
// refuses a shared location whose Database has no lease.
//
// A backend that doesn't implement it keeps Open's exclusive ownership, which
// already guarantees one writer, so its daemon doesn't hold the lease and an
// idle one writes nothing. It can't offer automatic takeover: a replica of its
// state is a recovery point, not state the old host has been fenced from.
type SharedBackend interface {
	Backend
	SharesLocation(location string) bool
	// ViewInPlace calls read once over the latest committed state, as a
	// standby may every few seconds for as long as it waits: it reads the
	// state where it is, without copying it, taking ownership or changing
	// anything, and a holder's writes don't make it fail. Absent state is
	// os.ErrNotExist, and state stored at newer versions than this binary
	// writes is ErrSchemaNewer or ErrRecordsNewer. State at older versions
	// is read as it stands.
	ViewInPlace(ctx context.Context, location string, read func(Tx) error) error
}

// Holding is s with every transaction made as the holder ref, whatever Guard
// its caller built, so no call site can leave the reference out. It keeps s
// a Viewer when it is one. Compose it inside Retry or outside it: a fenced
// transaction is never retried.
func Holding(s Store, ref InstanceRef) Store {
	h := holding{inner: s, ref: ref}
	if v, ok := s.(Viewer); ok {
		return holdingViewer{h, v}
	}
	return h
}

type holding struct {
	inner Store
	ref   InstanceRef
}

func (h holding) Transact(ctx context.Context, g Guard, fn func(Tx) error) error {
	ref := h.ref
	g.Instance = &ref
	return h.inner.Transact(ctx, g, fn)
}

type holdingViewer struct {
	holding
	v Viewer
}

func (h holdingViewer) View(ctx context.Context, fn func(Tx) error) error { return h.v.View(ctx, fn) }
