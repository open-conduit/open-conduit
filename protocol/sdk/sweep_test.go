package sdk_test

import (
	"context"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// heldExecutor holds state for a fixed set of executions and sweeps it.
type heldExecutor struct {
	extension.Executor
	held []domain.ID
}

func (e *heldExecutor) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	got, err := extension.KnownExecutions(ctx, known, e.held)
	if err != nil {
		return extension.StateSweepReport{}, err
	}
	var report extension.StateSweepReport
	var kept []domain.ID
	for _, id := range e.held {
		if got[id] {
			kept = append(kept, id)
		} else {
			report.Removed = append(report.Removed, id)
		}
	}
	e.held = kept
	return report, nil
}

// executor.sweep reaches the executor's SweepState, whose known calls are
// answered by the host over executor.known.
func TestExecutorSweepAsksTheHost(t *testing.T) {
	ctx := context.Background()
	x := &heldExecutor{held: []domain.ID{"kept", "later"}}
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "runner", Version: "1.0.0", Command: []string{"runner"}, Provides: protocol.Provides{
		Executors: []protocol.ExecutorDecl{{Name: "runner", Capabilities: protocol.Capabilities{Toolbelt: extension.ToolbeltCLI}, Controls: protocol.Controls{Sweep: true}}}}}
	h := sdktest.New(t, &sdk.Extension{Manifest: m, Executors: map[string]extension.Executor{"runner": x}}, sdktest.WithExecutions(domain.Execution{ID: "kept"}))
	var out protocol.StateSweepReport
	if err := h.Call(ctx, "executor.sweep", protocol.ExecutorSweepParams{Executor: "runner", Sweep: "s1"}, &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Removed, []string{"later"}) || !slices.Equal(x.held, []domain.ID{"kept"}) {
		t.Fatalf("removed %v, held %v", out.Removed, x.held)
	}
}
