package bootstrap

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/extension"
)

// Issue #342: a hung container runtime or a panicking projection must not
// stop the host loop or the daemon.

// hungProvider is a container provider whose List blocks until released.
type hungProvider struct {
	extension.ContainerProvider
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (p *hungProvider) List(ctx context.Context, _ extension.ResourceOwner) ([]extension.ContainerState, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-p.release
	return nil, nil
}

// TestContainerReconcileRunsOffTheLoop: the loop's reconcile step returns
// while the runtime it lists is wedged, and doesn't start a second pass
// beside the one still running. The control: the pass really is blocked
// in List until released.
func TestContainerReconcileRunsOffTheLoop(t *testing.T) {
	clock := conformance.NewClock()
	p := &hungProvider{release: make(chan struct{})}
	d := &daemon{o: Options{StateDir: t.TempDir(), ResourceInterval: time.Second}, now: clock.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)), reported: map[string]string{}}
	d.containers.providers = map[string]extension.ContainerProvider{"hung": p}
	returned := make(chan struct{})
	go func() {
		d.reconcileContainers(context.Background())
		clock.Advance(time.Minute)
		d.reconcileContainers(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop is held by a wedged container runtime")
	}
	if !d.containers.reconciling.Load() {
		t.Fatal("control: the pass is not running")
	}
	close(p.release)
	d.wg.Wait()
	if p.calls != 1 {
		t.Fatalf("%d passes ran at once, want 1", p.calls)
	}
	if d.containers.reconciling.Load() {
		t.Fatal("the finished pass is still marked running")
	}
}

// TestGoSafeRecoversAPanic: a panic on a background goroutine is logged
// with its stack and reported, and the daemon carries on. The control: fn
// really panicked (it never got past the nil map).
func TestGoSafeRecoversAPanic(t *testing.T) {
	var logs bytes.Buffer
	d := &daemon{log: slog.New(slog.NewTextHandler(&logs, nil)), reported: map[string]string{}}
	reached := false
	d.goSafe("github_projection", func() {
		var m map[string]int
		m["x"] = 1
		reached = true
	})
	d.wg.Wait()
	if reached {
		t.Fatal("control: fn did not panic")
	}
	if got := d.reportedCode("github_projection", ""); got != "panic" {
		t.Fatalf("reported %q", got)
	}
	if out := logs.String(); !strings.Contains(out, "recovered from a panic") || !strings.Contains(out, "assignment to entry in nil map") || !strings.Contains(out, "goroutine") {
		t.Fatalf("log lacks the panic and its stack:\n%s", out)
	}
}
