package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// OpRead returns a delivery of the calling Work in full.
const OpRead = "webhook.delivery.read"

// ReadInput is webhook.delivery.read's input.
type ReadInput struct {
	// Event is the delivery's Event ID, as the rendered turn names it; the
	// Work's latest delivery when empty.
	Event string `json:"event,omitempty"`
}

// ReadOutput is webhook.delivery.read's output: the delivery's payload,
// body included, whatever its size.
type ReadOutput struct {
	Event      string    `json:"event"`
	ReceivedAt time.Time `json:"received_at"`
	Payload
}

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func registerTools(ext *sdk.Extension, now func() time.Time) {
	ext.Operation(OpRead, sdk.Handler(func(ctx context.Context, a extension.Authority, in ReadInput) (ReadOutput, error) {
		return read(ctx, a, in, now)
	}),
		sdk.Describe("Read a webhook delivery of this Work in full, body included."),
		sdk.Guidance("Read a webhook delivery this Work received, in full. Your turn shows each delivery with its body cut at 8 KiB; when it says the body was cut, call this with the `event` the turn names to get the whole body (`body` for JSON, `text` otherwise). Without `event` it returns the Work's latest delivery. Only this Work's own deliveries can be read."))
}

// read finds the delivery among the calling Work's own Events, so a Work
// can never read another's.
func read(ctx context.Context, a extension.Authority, in ReadInput, now func() time.Time) (ReadOutput, error) {
	var found *domain.AcceptedEvent
	err := sdk.NewStore(sdk.HostFrom(ctx)).Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		w, err := tx.Work(a.WorkID)
		if err != nil || w.ScopeID != a.ScopeID {
			return extension.ErrUnauthorized
		}
		events, err := tx.WorkEvents(w.ID, 0, 0)
		if err != nil {
			return err
		}
		for i := range events {
			e := events[i]
			if e.Event.Type != TypeDelivery || in.Event != "" && string(e.Event.ID) != in.Event {
				continue
			}
			found = &e
		}
		return nil
	})
	if err != nil {
		return ReadOutput{}, err
	}
	if found == nil {
		if in.Event != "" {
			return ReadOutput{}, refuse("unknown_delivery", "this Work has no webhook delivery %s", in.Event)
		}
		return ReadOutput{}, refuse("no_delivery", "this Work has no webhook delivery")
	}
	var p Payload
	if found.Event.Data.Schema != SchemaDelivery || json.Unmarshal(found.Event.Data.Data, &p) != nil {
		return ReadOutput{}, refuse("unreadable_delivery", "the delivery's data is unreadable")
	}
	return ReadOutput{Event: string(found.Event.ID), ReceivedAt: found.Event.ReceivedAt, Payload: p}, nil
}
