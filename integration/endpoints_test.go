package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// TestManagementExecutionEndpoints runs real scheduled attempts through the
// per-execution toolbelt endpoints: each attempt's socket in its run
// directory serves only its own token and only toolbelt calls, the token is
// rotated in its token directory on recovery, the socket is served again
// after a restart and goes away with the run directory once the attempt is
// terminal.
func TestManagementExecutionEndpoints(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	seen := map[domain.ID]extension.ExecutionContext{}
	handles := map[domain.ID]*schedulerHandle{}
	record := func(r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
		if err := r.PersistRecovery(p); err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		seen[r.Execution.ID] = r.Context
		if handles[r.Execution.ID] == nil {
			handles[r.Execution.ID] = &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
		}
		return handles[r.Execution.ID], nil
	}
	h.executor.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		return record(r, domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)})
	}
	h.executor.onRecover = func(_ context.Context, r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
		return record(r, p)
	}
	contextOf := func(id domain.ID) extension.ExecutionContext {
		mu.Lock()
		defer mu.Unlock()
		return seen[id]
	}
	start := func(delivery string) runtime.Attempt {
		t.Helper()
		var root client.Event
		h.mustCLI(&root, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", delivery)
		a, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	dial := func(endpoint, token string) *client.Client {
		t.Helper()
		c, err := client.New(endpoint, token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	readContext := func(c *client.Client) error {
		_, err := c.ToolbeltCall(ctx, client.ToolCall{Operation: "read_context"})
		return err
	}

	a, b := start("endpoint-a"), start("endpoint-b")
	ca, cb := contextOf(a.Lease.ExecutionID), contextOf(b.Lease.ExecutionID)
	runs := filepath.Join(h.state, "runs")
	for _, c := range []extension.ExecutionContext{ca, cb} {
		socket, ok := strings.CutPrefix(c.ToolbeltEndpoint, "unix://")
		if !ok || filepath.Dir(filepath.Dir(socket)) != runs || filepath.Base(socket) != "toolbelt.sock" {
			t.Fatalf("toolbelt endpoint is not in the attempt's run directory: %q", c.ToolbeltEndpoint)
		}
		if c.ToolbeltTokenFile != filepath.Join(filepath.Dir(socket), "token", "token") {
			t.Fatalf("token file is not in the run directory's token directory: %q", c.ToolbeltTokenFile)
		}
		if b, err := os.ReadFile(c.ToolbeltTokenFile); err != nil || strings.TrimSpace(string(b)) != c.ToolbeltToken {
			t.Fatalf("token file does not hold the attempt's token: %q %v", b, err)
		}
		for path, mode := range map[string]os.FileMode{socket: 0o600, filepath.Dir(socket): 0o700, filepath.Dir(c.ToolbeltTokenFile): 0o700, c.ToolbeltTokenFile: 0o600} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
				t.Fatalf("%s is not owner-only %o: %v %v", path, mode, info.Mode(), err)
			}
		}
	}
	if ca.ToolbeltEndpoint == cb.ToolbeltEndpoint {
		t.Fatal("two executions share a toolbelt endpoint")
	}

	// Each socket serves its own token, and refuses the other execution's.
	if err := readContext(dial(ca.ToolbeltEndpoint, ca.ToolbeltToken)); err != nil {
		t.Fatalf("own token on own socket: %v", err)
	}
	if err := readContext(dial(cb.ToolbeltEndpoint, cb.ToolbeltToken)); err != nil {
		t.Fatalf("own token on own socket: %v", err)
	}
	if err := readContext(dial(ca.ToolbeltEndpoint, cb.ToolbeltToken)); !isStatus(err, 403) {
		t.Fatalf("another execution's token was served: %v", err)
	}
	if err := readContext(dial(cb.ToolbeltEndpoint, ca.ToolbeltToken)); !isStatus(err, 403) {
		t.Fatalf("another execution's token was served: %v", err)
	}
	if _, err := dial(ca.ToolbeltEndpoint, ca.ToolbeltToken).ToolbeltCall(ctx, client.ToolCall{Operation: "read_context", ExecutionID: string(b.Lease.ExecutionID)}); !isStatus(err, 403) {
		t.Fatalf("a call asserting another execution was served: %v", err)
	}

	// It serves the guidance of the execution's granted operations too, under
	// the same assertion.
	if g, err := dial(ca.ToolbeltEndpoint, ca.ToolbeltToken).ToolbeltGuidance(ctx); err != nil || g == nil {
		t.Fatalf("own guidance on own socket: %q %v", g, err)
	}
	if _, err := dial(ca.ToolbeltEndpoint, cb.ToolbeltToken).ToolbeltGuidance(ctx); !isStatus(err, 403) {
		t.Fatalf("another execution's guidance was served: %v", err)
	}

	// Operator endpoints are not served on it, to either credential class.
	operatorToken, err := client.ReadTokenFile(filepath.Join(h.state, "private", "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	op := dial(ca.ToolbeltEndpoint, operatorToken.Token)
	if _, err = op.Health(ctx); !isStatus(err, 404) {
		t.Fatalf("operator endpoint served on an execution socket: %v", err)
	}
	if _, err = op.ValidateConfig(ctx, []byte(managementConfig)); !isStatus(err, 404) {
		t.Fatalf("operator endpoint served on an execution socket: %v", err)
	}
	if err = readContext(op); !isStatus(err, 403) {
		t.Fatalf("operator credential reached the toolbelt: %v", err)
	}
	if _, err = op.ToolbeltGuidance(ctx); !isStatus(err, 403) {
		t.Fatalf("operator credential reached the toolbelt guidance: %v", err)
	}
	if _, err = dial(ca.ToolbeltEndpoint, ca.ToolbeltToken).Execution(ctx, string(a.Lease.ExecutionID)); !isStatus(err, 404) {
		t.Fatalf("operator endpoint served to an execution token: %v", err)
	}

	// Recovery rotates the token in place: the file is replaced inside its
	// directory and the old token is refused on the same socket.
	h.clock.Advance(2 * time.Minute)
	recovered, err := h.scheduler.Recover(ctx, a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	ra := contextOf(a.Lease.ExecutionID)
	if ra.ToolbeltEndpoint != ca.ToolbeltEndpoint || ra.ToolbeltTokenFile != ca.ToolbeltTokenFile || ra.ToolbeltToken == ca.ToolbeltToken {
		t.Fatalf("recovery did not rotate the token in place: %+v", ra)
	}
	rotated, err := os.ReadFile(ra.ToolbeltTokenFile)
	if err != nil || strings.TrimSpace(string(rotated)) != ra.ToolbeltToken {
		t.Fatalf("rotated token not in the token file: %q %v", rotated, err)
	}
	if err = readContext(dial(ca.ToolbeltEndpoint, strings.TrimSpace(string(rotated)))); err != nil {
		t.Fatalf("rotated token from the file: %v", err)
	}
	if err = readContext(dial(ca.ToolbeltEndpoint, ca.ToolbeltToken)); err == nil {
		t.Fatal("pre-recovery token still served")
	}

	// A restart: the next daemon serves the socket again when it adopts the
	// attempt, with a fresh token in the same token directory.
	h.endpoints.Shutdown(ctx)
	if err = readContext(dial(ca.ToolbeltEndpoint, ra.ToolbeltToken)); err == nil {
		t.Fatal("socket still served after shutdown")
	}
	h.openEndpoints(h.server.ExecutionHandler)
	h.clock.Advance(2 * time.Minute)
	adoptedAttempt, err := h.scheduler.Recover(ctx, recovered.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.scheduler.Recover(ctx, b.Lease.ExecutionID); err != nil {
		t.Fatal(err)
	}
	adopted := contextOf(a.Lease.ExecutionID)
	if adopted.ToolbeltEndpoint != ca.ToolbeltEndpoint || adopted.ToolbeltToken == ra.ToolbeltToken {
		t.Fatalf("adoption after restart: %+v", adopted)
	}
	fromFile, _ := os.ReadFile(adopted.ToolbeltTokenFile)
	if err = readContext(dial(adopted.ToolbeltEndpoint, strings.TrimSpace(string(fromFile)))); err != nil {
		t.Fatalf("adopted attempt's socket after restart: %v", err)
	}

	// Terminal: the run directory and its socket go away; the live one stays.
	if err = h.scheduler.Complete(ctx, adoptedAttempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	if err = h.endpoints.Reconcile(func(ids []domain.ID) (map[domain.ID]bool, error) {
		live := map[domain.ID]bool{}
		err := h.store.Transact(ctx, storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
			for _, id := range ids {
				x, err := tx.Execution(id)
				if err != nil {
					return err
				}
				live[id] = !x.State.Terminal()
			}
			return nil
		})
		return live, err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(ca.ToolbeltTokenFile)); !os.IsNotExist(err) {
		t.Fatalf("terminal attempt's run directory kept: %v", err)
	}
	if err = readContext(dial(ca.ToolbeltEndpoint, adopted.ToolbeltToken)); err == nil {
		t.Fatal("terminal attempt's socket still served")
	}
	if err = readContext(dial(cb.ToolbeltEndpoint, contextOf(b.Lease.ExecutionID).ToolbeltToken)); err != nil {
		t.Fatalf("live attempt's socket closed with another's: %v", err)
	}
}
