package extproc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

func sweepManifest(sweep bool) protocol.Manifest {
	return protocol.Manifest{Protocol: []int{1}, Name: "runner", Version: "1", Command: []string{"./runner"}, Provides: protocol.Provides{Executors: []protocol.ExecutorDecl{{Name: "runner", Capabilities: protocol.Capabilities{Toolbelt: extension.ToolbeltNone}, Controls: protocol.Controls{Sweep: sweep}}}}}
}

// executor.known is enabled only with the sweep control, answers only
// under a sweep in progress, from its oracle, and refuses an oversized
// batch.
func TestExecutorKnownAnswersOnlyARunningSweep(t *testing.T) {
	ctx := context.Background()
	s, err := NewService(sweepManifest(false), Deps{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if s.enabled["executor.known"] {
		t.Fatal("executor.known enabled without the sweep control")
	}
	if x, _ := (&Remote{m: sweepManifest(false), s: s}).Executor("runner"); x != nil {
		if _, err := x.(extension.StateSweeper).SweepState(ctx, nil); err != extension.ErrUnsupported {
			t.Fatalf("an undeclared sweep: %v", err)
		}
	}
	if s, err = NewService(sweepManifest(true), Deps{Now: time.Now}); err != nil {
		t.Fatal(err)
	}
	ask := func(sweep string, ids ...string) (any, error) {
		raw, _ := json.Marshal(protocol.ExecutorKnownParams{Sweep: sweep, Executions: ids})
		return s.Handle(ctx, "executor.known", raw)
	}
	if _, err := ask("none", "a"); err == nil {
		t.Fatal("executor.known answered with no sweep in progress")
	}
	s.sweeps["sw"] = func(_ context.Context, ids []domain.ID) (map[domain.ID]bool, error) {
		return map[domain.ID]bool{"a": true, "elsewhere": true}, nil
	}
	out, err := ask("sw", "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(protocol.ExecutorKnown).Known; len(got) != 1 || got[0] != "a" {
		t.Fatalf("known %v", got)
	}
	if _, err := ask("sw", make([]string, extension.MaxKnownBatch+1)...); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("an oversized batch: %v", err)
	}
}
