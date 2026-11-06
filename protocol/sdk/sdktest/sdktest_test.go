package sdktest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
	"github.com/open-conduit/open-conduit/storage"
)

func manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"acme"}, Provides: protocol.Provides{
		Families: []protocol.Family{{Type: "acme.thing.changed", ReDerivable: true}},
		Tasks:    []protocol.TaskDecl{{Name: "acme-sync", IntervalSeconds: 60}},
	}}
}

// task serves one task whose body the test supplies.
func task(fn func(ctx context.Context, h *sdk.Host) error) *sdk.Extension {
	return &sdk.Extension{Manifest: manifest(), Tasks: map[string]func(context.Context, *sdk.Host) error{"acme-sync": fn}}
}

func TestKVVersionsAndConflicts(t *testing.T) {
	var conflict, foreign error
	h := sdktest.New(t, task(func(ctx context.Context, h *sdk.Host) error {
		_, version, err := h.Get(ctx, "acme.things", "a")
		if err != nil {
			return err
		}
		if err := h.Commit(ctx, protocol.KVCommitParams{Checks: []protocol.KVCheck{{Namespace: "acme.things", Key: "a", Version: version}}, Puts: []protocol.KVPut{{Namespace: "acme.things", Key: "a", Value: []byte("2")}}}); err != nil {
			return err
		}
		// The same version again is stale now.
		conflict = h.Commit(ctx, protocol.KVCommitParams{Checks: []protocol.KVCheck{{Namespace: "acme.things", Key: "a", Version: version}}, Puts: []protocol.KVPut{{Namespace: "acme.things", Key: "a", Value: []byte("3")}}})
		foreign = h.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{{Namespace: "other", Key: "a", Value: []byte("x")}}})
		return nil
	}), sdktest.WithKV("acme.things", "a", []byte("1")))
	if err := h.RunTask(context.Background(), "acme-sync"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(conflict, storage.ErrConflict) {
		t.Fatalf("stale version: %v", conflict)
	}
	if protocol.ErrorCode(foreign) != protocol.CodeUnauthorized {
		t.Fatalf("foreign namespace: %v", foreign)
	}
	if got := h.KV("acme.things"); string(got["a"]) != "2" {
		t.Fatalf("kv %q", got)
	}
	if len(h.Commits()) != 1 {
		t.Fatalf("commits %+v", h.Commits())
	}
}

func TestStateIsScoped(t *testing.T) {
	clock := conformance.NewClock()
	var works []domain.Work
	var events []domain.AcceptedEvent
	var secrets map[string]string
	var outside error
	h := sdktest.New(t, task(func(ctx context.Context, h *sdk.Host) error {
		var err error
		if works, err = h.Works(ctx, ""); err != nil {
			return err
		}
		if events, err = h.Events(ctx, 0, 10); err != nil {
			return err
		}
		if secrets, err = h.Secrets(ctx, "s1", "token", "absent"); err != nil {
			return err
		}
		_, outside = h.Secrets(ctx, "elsewhere", "token")
		return nil
	}), sdktest.WithClock(clock),
		sdktest.WithWorks(domain.Work{ID: "w1", ScopeID: "s1", Kind: "acme.thing"}),
		sdktest.WithEvents("s1", domain.Event{Type: "acme.thing.changed", Source: "acme/s1", DeliveryID: "d1"}),
		sdktest.WithSecrets("s1", map[string]string{"token": "t0ken"}))
	if err := h.RunTask(context.Background(), "acme-sync"); err != nil {
		t.Fatal(err)
	}
	if len(works) != 1 || works[0].ID != "w1" {
		t.Fatalf("works %+v", works)
	}
	if len(events) != 1 || events[0].Sequence != 1 || !events[0].Event.ReceivedAt.Equal(clock.Now()) {
		t.Fatalf("events %+v", events)
	}
	if len(secrets) != 1 || secrets["token"] != "t0ken" {
		t.Fatalf("secrets %v", secrets)
	}
	if protocol.ErrorCode(outside) != protocol.CodeUnauthorized {
		t.Fatalf("unconfigured scope: %v", outside)
	}
}

func TestAcceptRequiresAParentInScope(t *testing.T) {
	var root, wrongScope, added error
	h := sdktest.New(t, task(func(ctx context.Context, h *sdk.Host) error {
		e := protocol.Event{Type: "acme.thing.changed", Source: "acme/derived", DeliveryID: "x"}
		_, _, root = h.Accept(ctx, protocol.EventAcceptParams{ScopeID: "s1", Event: e})
		_, _, wrongScope = h.Accept(ctx, protocol.EventAcceptParams{ScopeID: "s2", Parent: "p1", Event: e})
		_, _, added = h.Accept(ctx, protocol.EventAcceptParams{ScopeID: "s1", Parent: "p1", Event: e, Records: []protocol.KVPut{{Namespace: "acme", Key: "x", Value: []byte("1")}}})
		return nil
	}), sdktest.WithEvents("s1", domain.Event{ID: "p1", Type: "acme.thing.changed", Source: "acme/s1", DeliveryID: "d1"}), sdktest.WithScopes(domain.Scope{ID: "s2"}))
	if err := h.RunTask(context.Background(), "acme-sync"); err != nil {
		t.Fatal(err)
	}
	if protocol.ErrorCode(root) != protocol.CodeUnauthorized || protocol.ErrorCode(wrongScope) != protocol.CodeUnauthorized || added != nil {
		t.Fatalf("root %v, wrong scope %v, added %v", root, wrongScope, added)
	}
	if events := h.Events(); len(events) != 2 || events[1].Event.CausationID != "p1" || string(h.KV("acme")["x"]) != "1" {
		t.Fatalf("events %+v kv %q", events, h.KV("acme"))
	}
}

func TestArtifactsAndUndeclaredMethods(t *testing.T) {
	data := bytes.Repeat([]byte("a"), protocol.MaxChunkBytes+10)
	var ref domain.ArtifactRef
	var capture error
	h := sdktest.New(t, task(func(ctx context.Context, h *sdk.Host) error {
		var err error
		if ref, err = h.Artifact(ctx, "text/plain", data); err != nil {
			return err
		}
		capture = h.Capture(ctx, "", "acme_pass", sdktest.Start, nil) // no telemetry.capture permission
		return nil
	}))
	if err := h.RunTask(context.Background(), "acme-sync"); err != nil {
		t.Fatal(err)
	}
	arts := h.Artifacts()
	if len(arts) != 1 || arts[0].Ref != ref || !bytes.Equal(arts[0].Data, data) || ref.Size != int64(len(data)) {
		t.Fatalf("artifacts %+v", ref)
	}
	if protocol.ErrorCode(capture) != protocol.CodeUndeclared {
		t.Fatalf("capture without permission: %v", capture)
	}
}

// scanningTx is the negative control for the WorkEvents bound: a client
// that answers one Work's events by scanning the whole log through
// state.events and filtering on its own side, as sdk.Store did before
// state.work_events (#536).
type scanningTx struct{ storage.Tx }

func (t scanningTx) WorkEvents(work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	all, err := t.Events(after, 0)
	if err != nil {
		return nil, err
	}
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if (a.Event.WorkID == work || a.RoutedWorkID == work) && (limit <= 0 || len(out) < limit) {
			out = append(out, a)
		}
	}
	return out, nil
}

// workEventsRead reads work's events after a sequence through wrap(sdk.Store's
// transaction) over a log of 2,500 other events with work's three among
// them, and reports what came back and how many events the host sent.
func workEventsRead(t *testing.T, wrap func(storage.Tx) storage.Tx, after uint64, limit int) ([]domain.ID, int) {
	t.Helper()
	var events []domain.Event
	for i := range 2500 {
		work := domain.ID("other")
		if i == 10 || i == 1200 || i == 2400 {
			work = "thread"
		}
		events = append(events, domain.Event{ID: domain.ID(fmt.Sprintf("e-%d", i)), Type: "acme.thing.changed", Source: "acme", DeliveryID: fmt.Sprint(i), WorkID: work})
	}
	var got []domain.ID
	h := sdktest.New(t, task(func(ctx context.Context, h *sdk.Host) error {
		return sdk.NewStore(h).Transact(ctx, storage.Guard{}, func(tx storage.Tx) error {
			list, err := wrap(tx).WorkEvents("thread", after, limit)
			for _, a := range list {
				got = append(got, a.Event.ID)
			}
			return err
		})
	}), sdktest.WithEvents("eng", events...))
	if err := h.RunTask(context.Background(), "acme-sync"); err != nil {
		t.Fatal(err)
	}
	return got, h.EventsRead()
}

// sdk.Store reads one Work's events through state.work_events, so the host
// sends that Work's events and no others: the cost follows the Work, not
// the retained log (#536). The scanning client gets the same answer but
// fails the bound.
func TestStoreWorkEventsReadsOnlyTheWorksEvents(t *testing.T) {
	direct := func(tx storage.Tx) storage.Tx { return tx }
	for _, c := range []struct {
		after uint64
		limit int
		want  []domain.ID
	}{
		{0, 0, []domain.ID{"e-10", "e-1200", "e-2400"}},
		{11, 0, []domain.ID{"e-1200", "e-2400"}},
		{0, 2, []domain.ID{"e-10", "e-1200"}},
	} {
		got, read := workEventsRead(t, direct, c.after, c.limit)
		if !slices.Equal(got, c.want) {
			t.Fatalf("WorkEvents after %d limit %d: %v, want %v", c.after, c.limit, got, c.want)
		}
		if read > len(c.want) {
			t.Fatalf("WorkEvents after %d limit %d read %d events from the host, want at most %d", c.after, c.limit, read, len(c.want))
		}
	}
	got, read := workEventsRead(t, func(tx storage.Tx) storage.Tx { return scanningTx{tx} }, 0, 0)
	if want := []domain.ID{"e-10", "e-1200", "e-2400"}; !slices.Equal(got, want) {
		t.Fatalf("negative control answered %v, want %v", got, want)
	}
	if read <= 3 {
		t.Fatalf("negative control: scanning state.events read only %d events; the bound catches nothing", read)
	}
}
