package runtime_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// Only the host says who did something: an Event from any source but
// core.manual or core.action loses the Actor it claims, while those two keep
// the one the host stamped.
func TestAcceptKeepsActorOnlyForHostSources(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases := []struct {
		source, want string
	}{
		{"github", ""},                             // an extension forging an operator
		{"core.timer", ""},                         // any other core source
		{runtime.ManualSource, "operator:alice"},   // stamped by the host
		{extension.ActionSource, "operator:alice"}, // the action's caller
	}
	err = store.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "s", Name: "s"}, 0); err != nil {
			return err
		}
		for i, c := range cases {
			e := domain.Event{ID: domain.ID("e" + string(rune('0'+i))), Type: "fixture.changed", Source: c.source, DeliveryID: "d", OccurredAt: ingressTime, ReceivedAt: ingressTime, Data: domain.Payload{Schema: "fixture.v1", Data: []byte(`{}`)}, Actor: "operator:alice"}
			a, _, err := runtime.AcceptInTx(tx, "s", e)
			if err != nil {
				return err
			}
			if a.Event.Actor != c.want {
				t.Errorf("source %s: actor %q, want %q", c.source, a.Event.Actor, c.want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestActorTravelsOnContext(t *testing.T) {
	if got := runtime.ActorFrom(context.Background()); got != "" {
		t.Fatalf("empty context has actor %q", got)
	}
	if got := runtime.ActorFrom(runtime.WithActor(context.Background(), "operator:bo")); got != "operator:bo" {
		t.Fatalf("actor %q", got)
	}
}
