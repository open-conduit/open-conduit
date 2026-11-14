package integration_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
)

// Regressions for defects found by the 02-09 kill drills.

// TestToolbeltTypeOnlyWait: a Wait subscribes on its Work's keys (B04), so a
// type-only `os wait` subscribes on every key the Work owns. Previously the
// declaration was accepted but registration failed at the host handshake.
// Work without keys is given its own key, core.work/<its ID>, and
// subscribes on that; where it came from doesn't change.
func TestToolbeltTypeOnlyWait(t *testing.T) {
	ctx := context.Background()
	script := `if os wait --type operator.reply --timeout 3600 --request-id wait > /dev/null; then echo declared; else echo refused; exit 3; fi`
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": script}))
	h := newShellHost(t, state, bin, conformance.NewClock(), "daemon", false)
	c := h.operator()

	root := h.submit("operator.requested", "root", "typed")
	h.runOnce(root.RoutedWorkID)
	w, err := c.Work(ctx, root.RoutedWorkID)
	if err != nil || w.State != "waiting" {
		t.Fatalf("type-only wait not registered: %+v %v", w, err)
	}
	waits, err := c.Waits(ctx, url.Values{"work_id": {w.ID}, "state": {"active"}})
	if err != nil || len(waits.Items) != 1 || len(waits.Items[0].Match.Keys) != 1 || waits.Items[0].Match.Keys[0] != (client.Key{Source: "ops", ExternalID: "typed"}) || waits.Items[0].Match.Types[0] != "operator.reply" {
		t.Fatalf("wait predicate: %+v %v", waits.Items, err)
	}

	created, err := c.CreateWork(ctx, client.WorkRequest{ScopeID: "ops", Kind: "fixture.keyless", Binding: "run"}, "keyless")
	if err != nil {
		t.Fatal(err)
	}
	h.runOnce(created.Work.ID)
	own := client.Key{Source: domain.LineageSource, ExternalID: created.Work.ID}
	w, err = c.Work(ctx, created.Work.ID)
	if err != nil || w.State != "waiting" || len(w.CorrelationKeys) != 1 || w.CorrelationKeys[0] != own {
		t.Fatalf("keyless wait not registered on the Work's own key: %+v %v", w, err)
	}
	waits, err = c.Waits(ctx, url.Values{"work_id": {w.ID}, "state": {"active"}})
	if err != nil || len(waits.Items) != 1 || len(waits.Items[0].Match.Keys) != 1 || waits.Items[0].Match.Keys[0] != own {
		t.Fatalf("keyless wait predicate: %+v %v", waits.Items, err)
	}
	page, err := c.Works(ctx, url.Values{"source": {"fixture"}})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != w.ID {
		t.Fatalf("the keyless Work left its source once it waited: %+v %v", page.Items, err)
	}
}

// TestDeclarationSurvivesRecovery: a toolbelt declaration recorded under one
// epoch must still apply when a restarted host recovers the same segment under
// a new epoch. Previously the declaration was ignored after recovery, so a
// declared wait completed the Work and a declared outcome was replaced by the
// raw exit status.
func TestDeclarationSurvivesRecovery(t *testing.T) {
	cases := []struct {
		name, command string
		check         func(t *testing.T, c *client.Client, work string, x client.Execution)
	}{
		{"wait", `os wait --type operator.reply --timeout 3600 --request-id wait > /dev/null; touch "$HOME/declared"`, func(t *testing.T, c *client.Client, work string, x client.Execution) {
			w, err := c.Work(context.Background(), work)
			if err != nil || w.State != "waiting" || x.Outcome == nil || x.Outcome.Kind != "waiting" {
				t.Fatalf("declared wait lost across recovery: work %+v execution %+v %v", w, x.Outcome, err)
			}
		}},
		{"outcome", `os outcome completed --summary declared-summary --request-id done > /dev/null; touch "$HOME/declared"`, func(t *testing.T, c *client.Client, work string, x client.Execution) {
			if x.State != "completed" || x.Outcome == nil || x.Outcome.Summary != "declared-summary" {
				t.Fatalf("declared outcome lost across recovery: %s %+v", x.State, x.Outcome)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			state, bin := shellState(t, shellConfig(t, map[string]any{"command": tc.command}))
			clock := conformance.NewClock()
			first := newShellHost(t, state, bin, clock, "daemon-1", false)
			root := first.submit("operator.requested", "root", "declared")
			a, err := first.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(state, "declared")
			waitWithin(t, "the declaration", 20*time.Second, func() bool { return fileExists(marker) })
			// The host dies after the declaration, before observing the result.
			first.close()
			clock.Advance(conformance.RuntimeLease + time.Second)
			second := newShellHost(t, state, bin, clock, "daemon-2", false)
			recovered, err := second.scheduler.Recover(ctx, a.Lease.ExecutionID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Lease.Epoch <= a.Lease.Epoch {
				t.Fatal("recovery did not advance the epoch")
			}
			second.observe(recovered)
			x, err := second.operator().Execution(ctx, string(a.Lease.ExecutionID))
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, second.operator(), root.RoutedWorkID, x)
		})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
