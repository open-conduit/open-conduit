package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// collectingRuntime is a sharedRuntime whose provider collects
// (extension.ContainerCollector) and whose runs have exited; collectErr
// fails every collect.
type collectingRuntime struct {
	*sharedRuntime
	collectErr error
	collected  []string
}

func (r *collectingRuntime) List(ctx context.Context, o extension.ResourceOwner) ([]extension.ContainerState, error) {
	states, err := r.sharedRuntime.List(ctx, o)
	for i := range states {
		states[i].Running = false
	}
	return states, err
}

func (r *collectingRuntime) Stop(context.Context, extension.ContainerRef, time.Duration) error {
	return nil
}

func (r *collectingRuntime) Collect(_ context.Context, ref extension.ContainerRef) error {
	if r.collectErr != nil {
		return r.collectErr
	}
	r.collected = append(r.collected, ref.ID)
	return nil
}

// TestReconcileKeepsAnUncollectedContainer (#725): reconcile collects an
// ended run's container before removing it. One it can't collect stays,
// and doctor's resources check says so until a later pass collects it.
func TestReconcileKeepsAnUncollectedContainer(t *testing.T) {
	rt := &collectingRuntime{sharedRuntime: newSharedRuntime(), collectErr: errors.New("remote unreachable")}
	d := containerDaemon(t, t.TempDir(), map[string]extension.ContainerProvider{"remote": rt}, nil)
	rt.launch(d, "ended-run", "x-gone")
	d.reconcileContainersPass(context.Background())
	if len(rt.removed) != 0 {
		t.Fatalf("reconcile removed %v, whose output it couldn't collect", rt.removed)
	}
	check := d.resourceCheck(context.Background())
	if check.Status != runtime.CheckWarn || check.Code != "container_uncollected" || len(check.Items) == 0 || !strings.Contains(strings.Join(check.Items, "\n"), "ended-run") {
		t.Fatalf("doctor's resources check is %+v, want the uncollected container warned of", check)
	}
	rt.collectErr = nil
	d.reconcileContainersPass(context.Background())
	if len(rt.collected) != 1 || len(rt.removed) != 1 || rt.removed[0] != "ended-run" {
		t.Fatalf("once collectable: collected %v, removed %v", rt.collected, rt.removed)
	}
	if check := d.resourceCheck(context.Background()); check.Code == "container_uncollected" {
		t.Fatalf("doctor still warns once the container was collected: %+v", check)
	}
}
