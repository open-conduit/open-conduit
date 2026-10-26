package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/examples/runner"
	"github.com/open-conduit/open-conduit/storage"
)

func noteDelivery(id, rev, text string) extension.Delivery {
	body, _ := json.Marshal(map[string]any{"note": notes.Note{ID: id, Rev: rev, Text: text, At: extensionNoteTime}})
	return extension.Delivery{ID: id + "-" + rev, ReceivedAt: extensionNoteTime, Body: body}
}

func registerNotesIngress(t *testing.T, e *extEnv, r *extproc.Remote) (*runtime.Ingress, string) {
	t.Helper()
	ing, err := runtime.NewIngress(e.store, e.clock.Now, runtime.IngressOptions{PageTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	decl := notesSource(t, r)
	if err := ing.Register(context.Background(), decl.Name, decl.Source.Extension(), r.Ingress(decl), time.Minute); err != nil {
		t.Fatal(err)
	}
	return ing, decl.Name
}

// Durable acceptance survives a subprocess crash: what was accepted stays,
// a delivery in flight at the crash is not accepted (so not acknowledged),
// and the restarted process accepts its retry exactly once. A partial
// response (half a line, then death) is a transport failure, never a result.
func TestExternalIngressDurableAcceptanceAcrossCrash(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	e := newExtEnv(t)
	o := e.options(notes.Name)
	r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), o)
	ing, name := registerNotesIngress(t, e, r)
	ctx := context.Background()
	if _, err := ing.Push(ctx, name, noteDelivery("a", "r1", "hello world")); err != nil {
		t.Fatal(err)
	}
	before := eventCount(t, e.store, e.clock.Now())
	for _, mode := range []string{"exit", "partial"} {
		gen := r.Process().Generation()
		if err := notes.ArmFault(o.StateDir, notes.Fault{Method: "ingress.push", Mode: mode}); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := ing.Push(ctx, name, noteDelivery("c", "r1", "in flight")); err == nil {
			t.Fatalf("%s: push across a crash acknowledged", mode)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("%s: crash detection hung", mode)
		}
		if n := eventCount(t, e.store, e.clock.Now()); n != before {
			t.Fatalf("%s: crash accepted %d events", mode, n-before)
		}
		awaitRestart(t, r, gen)
	}
	if _, found := eventByDelivery(t, e.store, e.clock.Now(), "a/r1.s2"); !found {
		t.Fatal("event accepted before the crash was lost")
	}
	if _, err := ing.Push(ctx, name, noteDelivery("c", "r1", "in flight")); err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if _, err := ing.Push(ctx, name, noteDelivery("c", "r1", "in flight")); err != nil {
		t.Fatalf("duplicate retry: %v", err)
	}
	if n := eventCount(t, e.store, e.clock.Now()); n != before+1 {
		t.Fatalf("retried delivery accepted %d times", n-before)
	}
	// A reconcile page lost to a crash advances nothing; the next scan
	// completes from the last complete checkpoint.
	if err := notes.ArmFault(o.StateDir, notes.Fault{Method: "ingress.reconcile", Mode: "exit"}); err != nil {
		t.Fatal(err)
	}
	gen := r.Process().Generation()
	res, err := ing.Reconcile(ctx, name)
	if err == nil && res.Complete {
		t.Fatal("reconcile across a crash completed")
	}
	awaitRestart(t, r, gen)
	for i := 0; i < 5 && !res.Complete; i++ {
		if res, err = ing.Reconcile(ctx, name); err != nil {
			t.Fatalf("reconcile after restart: %v", err)
		}
	}
	if !res.Complete {
		t.Fatalf("reconcile did not complete: %+v", res)
	}
	if r.Process().Restarts() != 3 {
		t.Fatalf("restarts %d, want 3", r.Process().Restarts())
	}
	log, err := os.ReadFile(filepath.Join(o.LogDir, notes.Name+".log"))
	if err != nil {
		t.Fatalf("stderr log: %v", err)
	}
	_ = log // captured (may be empty for a clean crash); its bound is unit-tested
}

// In-transaction methods are bounded and cannot call back.
func TestExternalInTransactionTimeoutAndReentrancy(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	e := newExtEnv(t)
	o := e.options(notes.Name)
	r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), o)
	next := r.Recurrence(notes.Recurrence)
	fired := domain.Timer{ID: "t", FireAt: extensionNoteTime}
	if occ, nt, err := next(nil, fired, extensionNoteTime); err != nil || occ == nil || nt == nil || !nt.FireAt.Equal(extensionNoteTime.Add(24*time.Hour)) {
		t.Fatalf("recurrence: %v %v %v", occ, nt, err)
	}
	if err := notes.ArmFault(o.StateDir, notes.Fault{Method: "recurrence.next", Mode: "hang"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, _, err := next(nil, fired, extensionNoteTime); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung in-transaction call: %v, want a deadline", err)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Fatalf("in-transaction deadline took %v", d)
	}
	if err := notes.ArmFault(o.StateDir, notes.Fault{Method: "recurrence.next", Mode: "reenter"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := next(nil, fired, extensionNoteTime); protocol.ErrorCode(err) != protocol.CodeReentrant {
		t.Fatalf("callback from an in-transaction method: %v, want reentrant_call", err)
	}
	// The process survived both and still serves.
	if _, _, err := next(nil, fired, extensionNoteTime); err != nil {
		t.Fatal(err)
	}
	if r.Process().Restarts() != 0 {
		t.Fatal("a timeout restarted the process")
	}
}

// A hung emit.commit aborts the emission transaction (nothing accepted, no
// record); a crash in it likewise; the retry with the same request ID
// commits exactly one Event with its record.
func TestExternalEmissionCommitTimeoutAndCrash(t *testing.T) {
	r := fixtureRemote(t)
	ops := append(allToolbeltOperations(), "fixture.report")
	h := newExternalToolbeltHarness(t, "persistent", r.Registration(), ops)
	ctx := extension.WithCallRequestID(context.Background(), "report-1")
	call := extension.ToolCall{Operation: "fixture.report", Input: domain.Payload{Schema: "fixture.input.v1", Data: []byte(`{"n":1}`)}}
	stateDir := r.Process().Options().StateDir
	before := eventCount(t, h.f.Store, h.f.Clock.Now())
	for _, mode := range []string{"hang", "exit"} {
		gen := r.Process().Generation()
		if err := notes.ArmFault(stateDir, notes.Fault{Method: "emit.commit", Mode: mode}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.belt.Call(ctx, h.token, call); err == nil {
			t.Fatalf("%s: emission committed across a failed commit", mode)
		}
		if n := eventCount(t, h.f.Store, h.f.Clock.Now()); n != before {
			t.Fatalf("%s: aborted emission accepted %d events", mode, n-before)
		}
		if mode == "exit" {
			awaitRestart(t, r, gen)
		}
	}
	out, err := h.belt.Call(ctx, h.token, call)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	var res struct {
		Event string `json:"event"`
		Added bool   `json:"added"`
	}
	if err := json.Unmarshal(out.Output.Data, &res); err != nil || !res.Added {
		t.Fatalf("emission output %s: %v", out.Output.Data, err)
	}
	if n := eventCount(t, h.f.Store, h.f.Clock.Now()); n != before+1 {
		t.Fatalf("emission accepted %d events", n-before)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		v, err := tx.Get("fixture.reports", res.Event)
		if err != nil || string(v) != `{"n":1}` {
			t.Fatalf("committed record %q %v", v, err)
		}
		return nil
	})
}

// Protocol mismatches are deterministic refusals, never restarted.
func TestExternalProtocolMismatch(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	t.Run("no_common_version", func(t *testing.T) {
		e := newExtEnv(t)
		o := e.options(notes.Name)
		o.Versions = []int{2}
		_, err := extproc.Load(context.Background(), e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), o)
		if !errors.Is(err, extproc.ErrRefused) || !errors.Is(err, protocol.ErrIncompatible) {
			t.Fatalf("version mismatch: %v", err)
		}
	})
	t.Run("manifest_differs_from_file", func(t *testing.T) {
		e := newExtEnv(t)
		m := notes.Manifest(notes.Options{Release: 2})
		m.Version = "9.9.9"
		_, err := extproc.Load(context.Background(), e.install(m, notesPath), e.deps(), e.options(notes.Name))
		if !errors.Is(err, extproc.ErrRefused) || !strings.Contains(err.Error(), "differs") {
			t.Fatalf("manifest mismatch: %v", err)
		}
	})
	t.Run("kv_downgrade", func(t *testing.T) {
		e := newExtEnv(t)
		r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), e.options(notes.Name))
		_ = r.Process().Close()
		_, err := extproc.Load(context.Background(), e.install(notes.Manifest(notes.Options{Release: 1}), notesPath), e.deps(), e.options(notes.Name))
		if !errors.Is(err, extproc.ErrRefused) || !errors.Is(err, protocol.ErrKVDowngrade) {
			t.Fatalf("KV downgrade: %v", err)
		}
	})
}

func routeInput(a domain.AcceptedEvent) extension.RouteInput {
	return extension.RouteInput{Event: a, Scope: domain.Scope{ID: extensionScope, Name: "notes"}, Route: domain.Route{Name: "notes", Handler: "notes", Binding: "run"}}
}

// Old persisted events and declared version mismatches have deterministic
// outcomes: older readable payloads are upcast by the extension, retired or
// newer ones are answered "no match" by the host without a call, a payload
// change re-observes items under new identities, and breaking that rule is
// a deterministic conflict.
func TestExternalSchemaEvolution(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	ctx := context.Background()
	e := newExtEnv(t)
	start := func(release int, stable bool) (*extproc.Remote, *runtime.Ingress, string) {
		r := e.load(e.install(notes.Manifest(notes.Options{Release: release, StableIdentity: stable}), notesPath), e.deps(), e.options(notes.Name))
		ing, name := registerNotesIngress(t, e, r)
		return r, ing, name
	}
	r1, ing, name := start(1, false)
	if _, err := ing.Reconcile(ctx, name); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _ = ing.Reconcile(ctx, name)
	}
	v1, found := eventByDelivery(t, e.store, e.clock.Now(), "a/r1.s1")
	if !found || v1.Event.Data.Schema != "notes.note.v1" {
		t.Fatalf("release 1 event %+v", v1)
	}
	if err := r1.Task(ctx, "notes-index"); err != nil {
		t.Fatal(err)
	}
	peek := func(ns, key string) string {
		var v []byte
		_ = e.store.Transact(ctx, storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error { v, _ = tx.Get(ns, key); return nil })
		return string(v)
	}
	if got := peek(notes.IndexNS, "a"); got != "hello world" {
		t.Fatalf("release 1 KV layout %q", got)
	}
	_ = r1.Process().Close()

	// Release 2 reads [1,2]: it migrates KV 1→2 at configure and upcasts the
	// persisted v1 event itself.
	r2, ing2, name2 := start(2, false)
	if got := peek(notes.IndexNS, "a"); got != `{"text":"hello world"}` {
		t.Fatalf("KV not migrated: %q", got)
	}
	if got := peek("core.extensions", "notes/kv_version"); got != "2" {
		t.Fatalf("recorded KV layout %q", got)
	}
	d, err := r2.RouteHandler("notes").Match(ctx, routeInput(v1))
	if err != nil || !d.Match || d.Kind != notes.Kind {
		t.Fatalf("v1 event under release 2: %+v %v", d, err)
	}
	if text, ok := r2.Render(ctx, v1.Event); !ok || text != "note a: hello world" {
		t.Fatalf("upcast render %q %v", text, ok)
	}
	out, err := r2.Registration().Operations[0].Handler(ctx, extension.Authority{}, domain.Payload{Schema: "notes.lookup.request.v1", Data: []byte(`{"id":"a"}`)})
	if err != nil || !strings.Contains(string(out.Data), "hello world") {
		t.Fatalf("lookup over migrated KV: %s %v", out.Data, err)
	}
	// The payload change is a new observation version: one re-observation
	// per item, no conflict with the v1 canonical identities.
	for i := 0; i < 4; i++ {
		if _, err := ing2.Reconcile(ctx, name2); err != nil {
			t.Fatal(err)
		}
	}
	v2, found := eventByDelivery(t, e.store, e.clock.Now(), "a/r1.s2")
	if !found || v2.Event.Data.Schema != "notes.note.v2" {
		t.Fatalf("release 2 re-observation %+v", v2)
	}
	_ = r2.Process().Close()

	// Release 3 reads [2,3]: the persisted v1 event is retired, so the host
	// answers no match without a call; v2 is upcast.
	r3, _, _ := start(3, false)
	if d, err := r3.RouteHandler("notes").Match(ctx, routeInput(v1)); err != nil || d.Match {
		t.Fatalf("retired v1 event under release 3: %+v %v", d, err)
	}
	if r3.SchemaSkips() != 1 {
		t.Fatalf("schema skips %d", r3.SchemaSkips())
	}
	if d, err := r3.RouteHandler("notes").Match(ctx, routeInput(v2)); err != nil || !d.Match {
		t.Fatalf("v2 event under release 3: %+v %v", d, err)
	}
	if _, ok := r3.Render(ctx, v1.Event); ok {
		t.Fatal("retired payload rendered by the extension")
	}
	// A payload newer than the running release (written before a
	// downgrade) is likewise a host-side no-match.
	newer := v2
	newer.Event.Data.Schema = "notes.note.v4"
	if d, err := r3.RouteHandler("notes").Match(ctx, routeInput(newer)); err != nil || d.Match {
		t.Fatalf("newer payload: %+v %v", d, err)
	}
	// Breaking the identity rule (observation identities that ignore the
	// payload schema) is a deterministic conflict across the upgrade: the
	// page fails and nothing is accepted.
	t.Run("stable_identity_conflicts", func(t *testing.T) {
		e := newExtEnv(t)
		run := func(release int) (int, int) {
			r := e.load(e.install(notes.Manifest(notes.Options{Release: release, StableIdentity: true}), notesPath), e.deps(), e.options(notes.Name))
			ing, name := registerNotesIngress(t, e, r)
			failures := 0
			for i := 0; i < 3; i++ {
				if res, err := ing.Reconcile(ctx, name); err != nil || res.ErrorCode != "" {
					failures++
				}
			}
			_ = r.Process().Close()
			return failures, eventCount(t, e.store, e.clock.Now())
		}
		if failures, n := run(1); failures != 0 || n != 2 {
			t.Fatalf("release 1: %d failures, %d events", failures, n)
		}
		if failures, n := run(2); failures != 3 || n != 2 {
			t.Fatalf("release 2 with stable identities: %d failures, %d events", failures, n)
		}
	})
	// An extension cannot write anything but its current version.
	svc, err := extproc.NewService(notes.Manifest(notes.Options{Release: 3}), e.deps())
	if err != nil {
		t.Fatal(err)
	}
	h := pipeHost(t, svc)
	_, _, err = h.Accept(ctx, protocol.EventAcceptParams{ScopeID: string(extensionScope), Parent: string(v2.Event.ID), Event: protocol.Event{Type: notes.Indexed, Source: "notes/x", DeliveryID: "old-write", Data: protocol.Payload{Schema: "notes.note.v2", Data: []byte(`{}`)}}})
	if protocol.ErrorCode(err) != protocol.CodeSchema {
		t.Fatalf("writing an old payload version: %v", err)
	}
}

// Restarts cannot bypass causation: chain depth lives in host storage.
func TestExternalCausationAcrossRestart(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	ctx := context.Background()
	e := newExtEnv(t)
	deps := e.deps()
	deps.Limits = runtime.CausalLimits{MaxDepth: 3, PerMinute: 1000}
	r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), deps, e.options(notes.Name))
	ing, name := registerNotesIngress(t, e, r)
	if _, err := ing.Push(ctx, name, noteDelivery("a", "r1", "root")); err != nil {
		t.Fatal(err)
	}
	derived := 0
	var err error
	for i := 0; i < 10; i++ {
		if err = r.Task(ctx, "notes-derive"); err != nil {
			break
		}
		derived++
	}
	if protocol.ErrorCode(err) != protocol.CodeCausalLimit || derived != 3 {
		t.Fatalf("chain stopped after %d derivations: %v", derived, err)
	}
	crash(t, r)
	if err := r.Task(ctx, "notes-derive"); protocol.ErrorCode(err) != protocol.CodeCausalLimit {
		t.Fatalf("restart reset the causal chain: %v", err)
	}
}

// The host binds every callback to the extension's identity: scopes,
// families, sources, causation and runs are never the extension's choice.
func TestExternalHostSurfaceScoping(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	ctx := context.Background()
	e := newExtEnv(t)
	r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), e.options(notes.Name))
	ing, name := registerNotesIngress(t, e, r)
	accepted, err := ing.Push(ctx, name, noteDelivery("a", "r1", "root"))
	if err != nil || len(accepted) != 1 {
		t.Fatal(err)
	}
	parent := string(accepted[0].Event.ID)
	var foreign domain.AcceptedEvent
	if err := e.store.Transact(ctx, storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		var err error
		foreign, _, err = runtime.AcceptInTx(tx, "other-scope", domain.Event{ID: "foreign", Type: "other.thing", Source: "other", DeliveryID: "foreign", OccurredAt: e.clock.Now(), ReceivedAt: e.clock.Now()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := extproc.NewService(notes.Manifest(notes.Options{Release: 2}), e.deps())
	if err != nil {
		t.Fatal(err)
	}
	h := pipeHost(t, svc)
	ok := protocol.Event{Type: notes.Indexed, Source: "notes/x", DeliveryID: "x"}
	cases := map[string]struct {
		p    protocol.EventAcceptParams
		code string
	}{
		"root":           {protocol.EventAcceptParams{ScopeID: string(extensionScope), Event: ok}, protocol.CodeUnauthorized},
		"foreign_scope":  {protocol.EventAcceptParams{ScopeID: "other-scope", Parent: string(foreign.Event.ID), Event: ok}, protocol.CodeUnauthorized},
		"foreign_parent": {protocol.EventAcceptParams{ScopeID: string(extensionScope), Parent: string(foreign.Event.ID), Event: ok}, protocol.CodeUnauthorized},
		"family":         {protocol.EventAcceptParams{ScopeID: string(extensionScope), Parent: parent, Event: protocol.Event{Type: "other.thing", Source: "notes/x", DeliveryID: "x"}}, protocol.CodeUndeclared},
		"source":         {protocol.EventAcceptParams{ScopeID: string(extensionScope), Parent: parent, Event: protocol.Event{Type: notes.Indexed, Source: "other/x", DeliveryID: "x"}}, protocol.CodeUnauthorized},
		"causation":      {protocol.EventAcceptParams{ScopeID: string(extensionScope), Parent: parent, Event: protocol.Event{Type: notes.Indexed, Source: "notes/x", DeliveryID: "x", CausationID: "forged"}}, protocol.CodeUnauthorized},
		"foreign_record": {protocol.EventAcceptParams{ScopeID: string(extensionScope), Parent: parent, Event: ok, Records: []protocol.KVPut{{Namespace: "github.notes", Key: "k", Value: []byte("v")}}}, protocol.CodeUnauthorized},
	}
	for name, c := range cases {
		if _, _, err := h.Accept(ctx, c.p); protocol.ErrorCode(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	// A derived event with its record commits atomically and once.
	p := protocol.EventAcceptParams{ScopeID: string(extensionScope), Parent: parent, Event: ok, Records: []protocol.KVPut{{Namespace: "notes.records", Key: "k", Value: []byte("v")}}}
	a, added, err := h.Accept(ctx, p)
	if err != nil || !added || a.Event.CausationID != domain.ID(parent) {
		t.Fatalf("derived accept: %+v %v %v", a, added, err)
	}
	if _, added, err := h.Accept(ctx, p); err != nil || added {
		t.Fatalf("replayed accept: %v %v", added, err)
	}
	// State reads see only configured scopes.
	var events []protocol.AcceptedEvent
	if err := h.Call(ctx, "state.events", protocol.StateParams{}, &events); err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Event.ID == "foreign" {
			t.Fatal("state.events leaked another scope's event")
		}
	}
	if err := h.Call(ctx, "state.event", protocol.StateParams{ID: "foreign"}, nil); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("state.event of another scope: %v", err)
	}
	if err := h.Call(ctx, "state.works", protocol.StateParams{ScopeID: "other-scope"}, nil); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("state.works of another scope: %v", err)
	}
	// Persistence and resources need a run the host opened for this
	// process; a guessed or stale run ID is fenced, and an extension
	// without executors cannot call them at all.
	if err := h.Call(ctx, "execution.persist_recovery", protocol.PersistRecoveryParams{RunID: "guessed"}, nil); protocol.ErrorCode(err) != protocol.CodeUndeclared {
		t.Fatalf("persist from a non-executor extension: %v", err)
	}
	runnerSvc, err := extproc.NewService(runner.Manifest(), e.deps())
	if err != nil {
		t.Fatal(err)
	}
	rh := pipeHost(t, runnerSvc)
	if err := rh.Call(ctx, "execution.persist_recovery", protocol.PersistRecoveryParams{RunID: "guessed", State: protocol.Payload{Schema: "x.v1"}}, nil); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("persist through an unknown run: %v", err)
	}
	if err := rh.Call(ctx, "resource.acquire", protocol.ResourceAcquireParams{RunID: "guessed"}, nil); protocol.ErrorCode(err) != protocol.CodeUndeclared {
		t.Fatalf("resources for an executor without the workspace capability: %v", err)
	}
	// ingress.deliver only reaches sources this extension registered.
	if _, err := h.Deliver(ctx, "github/other", protocol.Delivery{ID: "x"}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("delivery to a foreign source: %v", err)
	}
	// Methods outside the manifest are refused before any work.
	if err := h.Call(ctx, "timer.arm", protocol.TimerArmParams{}, nil); err == nil {
		t.Fatal("timer.arm without timers accepted")
	}
	if err := h.Call(ctx, "route.match", struct{}{}, nil); protocol.ErrorCode(err) != protocol.CodeMethodNotFound {
		t.Fatalf("host-to-extension method called on the host: %v", err)
	}
}

// Discovery validates offline and admits an extension set atomically.
func TestExternalDiscoveryAndConflicts(t *testing.T) {
	notesPath, runnerPath := extensionBinaries(t)
	e := newExtEnv(t)
	e.install(notes.Manifest(notes.Options{Release: 2}), notesPath)
	e.install(runner.Manifest(), runnerPath)
	bad := filepath.Join(e.root, "broken")
	_ = os.MkdirAll(bad, 0o755)
	_ = os.WriteFile(filepath.Join(bad, protocol.ManifestFile), []byte(`{"protocol":[1],"name":"broken","version":"1","command":["x"],"provides":{},"storage":"sqlite"}`), 0o644)
	misnamed := filepath.Join(e.root, "elsewhere")
	_ = os.MkdirAll(misnamed, 0o755)
	raw, _ := json.Marshal(runner.Manifest())
	_ = os.WriteFile(filepath.Join(misnamed, protocol.ManifestFile), raw, 0o644)
	found, errs := extproc.Discover(e.root)
	if len(found) != 2 || len(errs) != 2 {
		t.Fatalf("discovered %d, errors %v", len(found), errs)
	}
	if found, errs := extproc.Discover(filepath.Join(e.root, "missing")); found != nil || errs != nil {
		t.Fatal("a missing install root is not empty")
	}
	taken := extproc.Taken{}
	taken.Add("executor", "shell", "claude-code")
	taken.Add("family", "github.issue.opened")
	if err := extproc.CheckConflicts(found, taken); err != nil {
		t.Fatal(err)
	}
	taken.Add("executor", runner.Name)
	if err := extproc.CheckConflicts(found, taken); !errors.Is(err, extproc.ErrConflict) || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("first-party conflict: %v", err)
	}
	twice := []extproc.Found{found[0], found[0]}
	if err := extproc.CheckConflicts(twice, extproc.Taken{}); !errors.Is(err, extproc.ErrConflict) {
		t.Fatalf("duplicate claim: %v", err)
	}
}

// Executor restarts cannot bypass lease fencing: a crash at one epoch is
// re-adopted transparently at that epoch, recovery at a newer epoch fences
// the older observer, its persistence and its stop, and only the current
// lease completes the attempt.
func TestExternalExecutorLeaseFencingAcrossRestart(t *testing.T) {
	_, runnerPath := extensionBinaries(t)
	ctx := context.Background()
	e := newExtEnv(t)
	o := e.options(runner.Name)
	r := e.load(e.install(runner.Manifest(), runnerPath), e.deps(), o)
	exec, _ := r.Executor(runner.Name)
	f := routingFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	base := f.Driver.(*routingDriver)
	scheduler, err := runtime.NewScheduler(f.Store, f.Clock.Now, base.router, runtime.SchedulerOptions{Owner: "fixture-owner", LeaseDuration: conformance.RuntimeLease, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": exec}})
	if err != nil {
		t.Fatal(err)
	}
	key := toolbeltKey
	requireAccept(t, f, "trigger", &key)
	requireRoute(t, f.Driver, "trigger")
	first, err := scheduler.Start(ctx, "work/item")
	if err != nil {
		t.Fatal(err)
	}
	dir := runner.RunDir(o.StateDir, first.Lease.ExecutionID)
	if err := waitForFile(filepath.Join(dir, "ready")); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		_, err := first.Handle.Wait(wctx)
		firstDone <- err
	}()
	// A crash at the same epoch: the observer re-adopts and keeps waiting.
	crash(t, r)
	select {
	case err := <-firstDone:
		t.Fatalf("observer ended across a same-epoch crash: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// Recovery at a newer epoch after the lease expires.
	f.Clock.Advance(conformance.RuntimeLease + time.Second)
	second, err := scheduler.Recover(ctx, first.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Lease.Epoch <= first.Lease.Epoch {
		t.Fatalf("recovery epoch %d after %d", second.Lease.Epoch, first.Lease.Epoch)
	}
	if err := exec.(extension.StopExecutor).Stop(ctx, first.Lease.ExecutionID, first.Lease.Epoch); err == nil {
		t.Fatal("stop at a superseded epoch accepted")
	}
	// Another crash: the old observer's re-adoption must persist through
	// its superseded lease, which the scheduler fences.
	crash(t, r)
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("superseded observer returned a result")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("superseded observer was not fenced")
	}
	if err := os.WriteFile(filepath.Join(dir, "finish"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := second.Handle.Wait(wctx)
	if err != nil || res.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("current observer: %+v %v", res, err)
	}
	if err := scheduler.Complete(ctx, first.Lease, res); err == nil {
		t.Fatal("superseded lease completed the attempt")
	}
	if err := scheduler.Complete(ctx, second.Lease, res); err != nil {
		t.Fatal(err)
	}
}
