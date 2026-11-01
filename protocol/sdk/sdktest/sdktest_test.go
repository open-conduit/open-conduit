package sdktest_test

import (
	"bytes"
	"context"
	"errors"
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
