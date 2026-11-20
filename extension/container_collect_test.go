package extension_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// collectingContainers is a fake provider that collects
// (extension.ContainerCollector): each collect is recorded, and collectErr
// fails it.
type collectingContainers struct {
	*fakeContainers
	collected  []string
	collectErr error
}

func (c *collectingContainers) Collect(_ context.Context, ref extension.ContainerRef) error {
	if c.collectErr != nil {
		return c.collectErr
	}
	c.collected = append(c.collected, ref.ID)
	return nil
}

// TestContainerCollect (#725): an exited container is collected before
// Adopt returns its exit and before any removal; a collect that fails
// keeps the container everywhere (not adopted, not removed) and is an
// error, so the attempt is never settled as the run's clean exit.
func TestContainerCollect(t *testing.T) {
	ctx := context.Background()
	cfg := containerConfig(testImage, "")
	owner := extension.ResourceOwner{ExecutionID: "x"}
	keepNone := func(ids []domain.ID) (map[domain.ID]extension.ContainerKeep, error) {
		return map[domain.ID]extension.ContainerKeep{}, nil
	}
	exited := func(t *testing.T) (*containerHost, *collectingContainers, *extension.ContainerPin) {
		h, f := newContainerHost(t), &collectingContainers{fakeContainers: newFakeContainers()}
		if _, err := h.bind(f, cfg, nil).Launch(ctx, h.needs(), h.launch()); err != nil {
			t.Fatal(err)
		}
		f.start("c1", h.now.Add(1e9))
		f.containers["c1"].Running, f.containers["c1"].ExitCode = false, 3
		f.containers["c1"].Ref.Labels[extension.ContainerLabelHost] = "host-a"
		pin := h.pins[0]
		return h, f, &pin
	}

	t.Run("adopt_collects_before_returning_the_exit", func(t *testing.T) {
		h, f, pin := exited(t)
		exit, err := h.bind(f, cfg, pin).Adopt(ctx, false)
		if err != nil || !exit.Exited || exit.ExitCode != 3 {
			t.Fatalf("adopt: %+v %v", exit, err)
		}
		if len(f.collected) != 1 || f.collected[0] != "c1" {
			t.Errorf("collected %v, want c1 before its exit was returned", f.collected)
		}
	})
	t.Run("adopt_of_a_live_container_collects_nothing", func(t *testing.T) {
		h, f, pin := exited(t)
		f.containers["c1"].Running = true
		if _, err := h.bind(f, cfg, pin).Adopt(ctx, true); err != nil || len(f.collected) != 0 {
			t.Fatalf("adopt: %v, collected %v", err, f.collected)
		}
	})
	t.Run("removals_collect_first", func(t *testing.T) {
		_, f, _ := exited(t)
		if err := extension.RemoveContainers(ctx, f, owner); err != nil {
			t.Fatal(err)
		}
		if len(f.collected) != 1 || len(f.removed) != 1 {
			t.Errorf("collected %v, removed %v", f.collected, f.removed)
		}
		_, f, _ = exited(t)
		if n, err := extension.ReconcileHostContainers(ctx, f, extension.ContainerHosts{Host: "host-a"}, keepNone); err != nil || n != 1 || len(f.collected) != 1 {
			t.Errorf("reconcile removed %d: %v, collected %v", n, err, f.collected)
		}
		h, f, _ := exited(t)
		h.pins = nil
		if _, err := h.bind(f, cfg, nil).Launch(ctx, h.needs(), h.launch()); err != nil {
			t.Fatal(err)
		}
		if len(f.collected) != 1 || f.collected[0] != "c1" || !contains(f.removed, "c1") {
			t.Errorf("the next launch collected %v and removed %v, want c1 collected then removed", f.collected, f.removed)
		}
	})
	t.Run("reconcile_stops_a_running_container_before_collecting_it", func(t *testing.T) {
		_, f, _ := exited(t)
		f.containers["c1"].Running = true
		if _, err := extension.ReconcileHostContainers(ctx, f, extension.ContainerHosts{Host: "host-a"}, keepNone); err != nil {
			t.Fatal(err)
		}
		if !contains(f.stopped, "c1") || len(f.collected) != 1 {
			t.Errorf("stopped %v, collected %v", f.stopped, f.collected)
		}
	})

	t.Run("a_failed_collect_keeps_the_container", func(t *testing.T) {
		h, f, pin := exited(t)
		f.collectErr = errors.New("remote unreachable")
		exit, err := h.bind(f, cfg, pin).Adopt(ctx, false)
		if !errors.Is(err, extension.ErrNotAdoptable) || !errors.Is(err, extension.ErrContainerUncollected) || exit.Exited {
			t.Errorf("adopt with a failed collect: %+v %v, want not adopted and uncollected", exit, err)
		}
		if err := extension.RemoveContainers(ctx, f, owner); !errors.Is(err, extension.ErrContainerUncollected) {
			t.Errorf("remove with a failed collect: %v", err)
		}
		if n, err := extension.ReconcileHostContainers(ctx, f, extension.ContainerHosts{Host: "host-a"}, keepNone); n != 0 || !errors.Is(err, extension.ErrContainerUncollected) {
			t.Errorf("reconcile with a failed collect removed %d: %v", n, err)
		}
		h.pins = nil
		if _, err := h.bind(f, cfg, nil).Launch(ctx, h.needs(), h.launch()); !errors.Is(err, extension.ErrSandboxUnavailable) || !errors.Is(err, extension.ErrContainerUncollected) {
			t.Errorf("launch beside an uncollected container: %v", err)
		}
		if err := extension.RemoveContainer(ctx, f, extension.ContainerRef{ID: "c1"}); !errors.Is(err, extension.ErrContainerUncollected) {
			t.Errorf("a detach's removal with a failed collect: %v", err)
		}
		if len(f.removed) != 0 || f.containers["c1"] == nil || len(h.pins) != 0 {
			t.Errorf("removed %v, pinned %v: the uncollected container must stay and nothing launch", f.removed, h.pins)
		}
		// Once it can be collected, it is, and goes.
		f.collectErr = nil
		if err := extension.RemoveContainers(ctx, f, owner); err != nil || f.containers["c1"] != nil {
			t.Errorf("remove once collectable: %v", err)
		}
	})

	t.Run("a_provider_that_doesnt_collect_is_unchanged", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		f.containers["c1"].Running, f.containers["c1"].ExitCode = false, 3
		if exit, err := h.bind(f, cfg, pin).Adopt(ctx, false); err != nil || exit.ExitCode != 3 {
			t.Fatalf("adopt: %+v %v", exit, err)
		}
		if err := extension.CollectContainer(ctx, f, extension.ContainerRef{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
	})
}

// notCollecting is an adapter that implements Collect but whose provider
// doesn't collect (extension.ContainerCollecting false).
type notCollecting struct{ *collectingContainers }

func (notCollecting) Collecting() bool { return false }

// TestRetryWaitsBehindAnUncollectedContainer (#725 review): a recovery
// whose collect failed fails its attempt, and the retry is another
// execution of the same Work, in the same workspace. Its launch collects
// what the earlier execution left first and is refused while that fails,
// so the earlier run's work never arrives over the retry's.
func TestRetryWaitsBehindAnUncollectedContainer(t *testing.T) {
	ctx := context.Background()
	cfg := containerConfig(testImage, "")
	h, f := newContainerHost(t), &collectingContainers{fakeContainers: newFakeContainers()}
	if _, err := h.bind(f, cfg, nil).Launch(ctx, h.needs(), h.launch()); err != nil {
		t.Fatal(err)
	}
	if got := f.created[0][extension.ContainerLabelWork]; got != "w" {
		t.Fatalf("the container's work label is %q, want the Work's", got)
	}
	f.start("c1", h.now.Add(time.Second))
	f.containers["c1"].Running = false
	f.collectErr = errors.New("remote unreachable")
	retry := func(p extension.ContainerProvider) error {
		_, err := extension.BindContainer(extension.ContainerBinding{Provider: p, Owner: extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x-retry"},
			Config: cfg, Host: "host-a", Segment: 1, Protected: []string{h.state}, Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot,
			Toolbelt: h.toolbelt, Records: h.records, Home: h.home, Now: func() time.Time { return h.now },
			Pin: func(p extension.ContainerPin) error { h.pins = append(h.pins, p); return nil }}).Launch(ctx, h.needs(), h.launch())
		return err
	}
	if err := retry(f); !errors.Is(err, extension.ErrSandboxUnavailable) || !errors.Is(err, extension.ErrContainerUncollected) {
		t.Fatalf("a retry launched beside the Work's uncollected container: %v", err)
	}
	if len(f.created) != 1 || f.containers["c1"] == nil {
		t.Fatalf("created %d, c1 %v: nothing may launch, and the container stays", len(f.created), f.containers["c1"])
	}
	// Once it collects, the earlier run's work is back before the retry
	// runs; the container itself is left to the removal paths.
	f.collectErr = nil
	if err := retry(f); err != nil {
		t.Fatalf("retry once collectable: %v", err)
	}
	if len(f.collected) != 1 || f.collected[0] != "c1" || f.containers["c1"] == nil {
		t.Errorf("collected %v, c1 kept %v; want c1 collected before the retry and left for removal", f.collected, f.containers["c1"] != nil)
	}
	// Another Work's container, and a provider that doesn't collect, are
	// never asked.
	for id := range f.containers {
		if id != "c1" {
			delete(f.containers, id)
		}
	}
	f.collected, f.collectErr = nil, errors.New("must not be asked")
	f.containers["c1"].Ref.Labels[extension.ContainerLabelWork] = "another-work"
	if err := retry(f); err != nil {
		t.Errorf("another Work's uncollected container held this one's launch: %v", err)
	}
	f.containers["c1"].Ref.Labels[extension.ContainerLabelWork] = "w"
	if err := retry(notCollecting{f}); err != nil {
		t.Errorf("a provider that doesn't collect was gated on collect: %v", err)
	}
}
