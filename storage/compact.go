package storage

import "context"

// Space is how a database's file is used.
type Space struct {
	// TotalBytes is the file's size and FreeBytes how much of it holds no
	// records, which the database reuses before it grows.
	TotalBytes, FreeBytes int64
	// Incremental says Reclaim can return free space while the database is
	// in use. Without it, only Rebuild can.
	Incremental bool
}

// Compactor is a Database that can give free space back to the disk. It is
// optional: a backend without file space to manage has nothing to do here.
//
// Its methods run outside a transaction, so the instance lease's epoch check
// doesn't cover them. They must therefore change no record, whoever calls them
// and whatever commits meanwhile. On a shared location the host proves its
// tenure before Reclaim and doesn't call Rebuild, which would hold up the
// lease's renewal.
type Compactor interface {
	// Space reports how the file is used.
	Space(context.Context) (Space, error)
	// Reclaim returns up to bytes of free space to the disk and says how
	// much it returned. It holds up writes only briefly, and does nothing
	// on a database that isn't Incremental.
	Reclaim(ctx context.Context, bytes int64) (int64, error)
	// Rebuild rewrites the database so it is Incremental and holds no free
	// space. It holds up every write until it finishes and needs disk room
	// for a copy; a failure leaves the database as it was.
	Rebuild(context.Context) error
}
