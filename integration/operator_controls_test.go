package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/conformance/reference"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/internal/runtime"
)

// TestManagementOperatorControls covers the 03-05 controls through the real
// management server and CLI on SQLite: rebind repairs queued input onto
// another binding of the same scope, a hold keeps attempts from starting
// while input still queues and its release lets them start, attach needs a
// hold and an executor that offers it, and a scope reconcile makes the
// scope's ingress sources due. Every control is an accepted core.manual.*
// Event addressed to its Work or scope.
func TestManagementOperatorControls(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	// A second binding to rebind onto (the same fixture executor).
	cfg := strings.Replace(managementConfig, `"bindings": [{"name": "run",`, `"bindings": [{"name": "alt", "executor": "fixture", "requires": {"toolbelt": "cli"}, "operations": ["read_context"], "secret_names": [], "config": {}}, {"name": "run",`, 1)
	path := h.state + "/candidate.json"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustCLI(nil, "config", "apply", path, "--generation", "1", "--key", "apply-2")

	var created struct {
		Work    client.Work `json:"work"`
		EventID string      `json:"event_id"`
	}
	h.mustCLI(&created, "work", "create", "--scope-id", "ops", "--kind", "fixture.work", "--binding", "run", "--key", "create-1")
	id := created.Work.ID
	w := created.Work

	// Rebind: an unknown binding is refused; a valid one moves the queued
	// input and records the previous binding and generation.
	if _, errOut, code := h.cli("work", "rebind", id, "--revision", w.Revision, "--binding", "missing", "--key", "rb0"); code != 1 || !strings.Contains(errOut, "invalid_request") {
		t.Fatalf("unknown binding: %d %s", code, errOut)
	}
	var rebound client.ControlResult
	h.mustCLI(&rebound, "work", "rebind", id, "--revision", w.Revision, "--binding", "alt", "--reason", "repair", "--key", "rb1")
	inputs, err := h.router.PendingInputs(ctx, domain.ID(id))
	if err != nil || len(inputs) != 1 || inputs[0].Binding != "alt" {
		t.Fatalf("queued input after rebind: %+v %v", inputs, err)
	}
	var ev client.Event
	h.mustCLI(&ev, "events", "get", rebound.EventID)
	var rb struct {
		Binding    string   `json:"binding"`
		Previous   []string `json:"previous"`
		Generation string   `json:"generation"`
	}
	if ev.Data != nil {
		_ = json.Unmarshal(ev.Data.Data, &rb)
	}
	if ev.Type != "core.manual.rebind" || ev.WorkID != id || ev.Source != "core.manual" || rb.Binding != "alt" || len(rb.Previous) != 1 || rb.Previous[0] != "run" || rb.Generation == "" {
		t.Fatalf("rebind event: %+v %s", ev, ev.Data.Data)
	}
	if _, errOut, code := h.cli("work", "rebind", id, "--revision", w.Revision, "--binding", "run", "--key", "rb2"); code != 1 || !strings.Contains(errOut, "revision_conflict") {
		t.Fatalf("stale rebind: %s", errOut)
	}

	// Back onto run (this harness's scheduler pins generation 1, where alt
	// does not exist).
	h.mustCLI(&w, "work", "get", id)
	h.mustCLI(nil, "work", "rebind", id, "--revision", w.Revision, "--binding", "run", "--key", "rb3")

	// Hold: no attempt starts; input keeps queueing. Release starts it.
	h.mustCLI(&w, "work", "get", id)
	var held client.ControlResult
	h.mustCLI(&held, "work", "hold", id, "--revision", w.Revision, "--reason", "looking", "--key", "hold-1")
	h.mustCLI(&w, "work", "get", id)
	if w.Hold == nil || w.Hold.EventID != held.EventID || w.Hold.Reason != "looking" {
		t.Fatalf("hold not visible: %+v", w)
	}
	if _, errOut, code := h.cli("work", "hold", id, "--revision", w.Revision, "--key", "hold-2"); code != 1 || !strings.Contains(errOut, "hold_conflict") {
		t.Fatalf("double hold: %s", errOut)
	}
	if _, err = h.scheduler.Start(ctx, domain.ID(id)); !errors.Is(err, runtime.ErrNotEligible) {
		t.Fatalf("held Work started: %v", err)
	}
	// Attach needs an executor that offers it (this server has none).
	if _, errOut, code := h.cli("work", "attach", id); code != 1 || !strings.Contains(errOut, "attach_unsupported") {
		t.Fatalf("attach without support: %d %s", code, errOut)
	}
	h.mustCLI(&w, "work", "get", id)
	if w.Hold == nil {
		t.Fatal("attach must leave an existing hold in place")
	}
	var released client.ControlResult
	h.mustCLI(&released, "work", "release", id, "--revision", w.Revision, "--key", "release-1")
	attempt, err := h.scheduler.Start(ctx, domain.ID(id))
	if err != nil {
		t.Fatalf("released Work did not start: %v", err)
	}
	var x client.Execution
	h.mustCLI(&x, "executions", "get", string(attempt.Lease.ExecutionID))
	if x.ExecutorBinding != "run" {
		t.Fatalf("attempt binding %q, want the rebound run", x.ExecutorBinding)
	}
	<-h.tokens
	<-h.secrets
	var events client.Page[client.Event]
	h.mustCLI(&events, "events", "list", "--work-id", id)
	var types []string
	for _, e := range events.Items {
		types = append(types, e.Type)
	}
	if got := strings.Join(types, ","); got != "core.work.created,core.manual.rebind,core.manual.rebind,core.manual.hold,core.manual.release" {
		t.Fatalf("control history: %s", got)
	}

	// Scope reconcile makes the scope's registered sources due now.
	in, err := runtime.NewIngress(h.store, h.clock.Now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &reference.Ingress{Declaration: extension.IngressSpec{Families: []extension.EventFamily{{Type: "fixture.changed", ReDerivable: true}}},
		Pages: map[string]extension.Snapshot{"": {Complete: true, Checkpoint: "c1"}}}
	if err = in.Register(ctx, "fixture/ops", extension.Source{Namespace: "fixture/ops", ScopeID: "ops"}, adapter, time.Hour); err != nil {
		t.Fatal(err)
	}
	// A completed scan makes the next one due in an hour.
	if _, err = in.ReconcileDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if statuses, err := in.Inspect(ctx); err != nil || len(statuses) != 1 || !statuses[0].NextDue.After(h.clock.Now()) {
		t.Fatalf("scan did not complete: %+v %v", statuses, err)
	}
	var res client.ReconcileResult
	h.mustCLI(&res, "scopes", "reconcile", "ops", "--key", "rec-1")
	if len(res.Sources) != 1 || res.Sources[0] != "fixture/ops" || res.EventID == "" {
		t.Fatalf("reconcile: %+v", res)
	}
	statuses, err := in.Inspect(ctx)
	if err != nil || len(statuses) != 1 || statuses[0].NextDue.After(h.clock.Now()) {
		t.Fatalf("source not due: %+v %v", statuses, err)
	}
	h.mustCLI(&ev, "events", "get", res.EventID)
	if ev.Type != "core.manual.reconcile" || ev.ScopeID != "ops" {
		t.Fatalf("reconcile event: %+v", ev)
	}
	if _, errOut, code := h.cli("scopes", "reconcile", "ops", "--revision", "999", "--key", "rec-2"); code != 1 || !strings.Contains(errOut, "revision_conflict") {
		t.Fatalf("stale reconcile: %s", errOut)
	}
}
