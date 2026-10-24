// Package extension defines public integration contracts, not a wire protocol.
package extension

import (
	"context"
	"time"

	"github.com/alsiola/open-conduit/domain"
)

// Source identifies an account/resource namespace within one configured ingress.
// It must survive restarts and credential rotation; secrets never form identity.
type Source struct {
	Namespace string
	ScopeID   domain.ID
}
type EventFamily struct {
	Type        string
	ReDerivable bool
	// Loss describes what cannot be recovered when push is unavailable.
	Loss string
}
type IngressSpec struct{ Families []EventFamily }
type Delivery struct {
	ID          string
	ReceivedAt  time.Time
	ContentType string
	Body        []byte
}

// Observation is opaque state with a canonical event for this version. Push and
// reconcile MUST produce the same Source/DeliveryID for the same change.
// Version is an equality token, never an ordered counter. ID identifies an item.
type Observation struct {
	ID, Version string
	Event       domain.Event
}
type PushResult struct {
	Observations []Observation
	// Events is reserved for explicitly declared non-re-derivable families.
	Events []domain.Event
}
type Scan struct {
	Source Source
	// ScanID identifies a stable snapshot generation; Cursor is page continuation.
	ScanID, Cursor, Checkpoint string
}
type Snapshot struct {
	ScanID       string
	Observations []Observation
	NextCursor   string
	// Complete certifies exhaustive coverage of this source, across all pages.
	// False with no NextCursor is a partial scan, never proof of absence.
	Complete bool
	// Checkpoint is usable only after this page and its changes commit.
	Checkpoint string
}
type Ingress interface {
	Spec() IngressSpec
	Push(context.Context, Source, Delivery) (PushResult, error)
	Reconcile(context.Context, Scan) (Snapshot, error)
}
