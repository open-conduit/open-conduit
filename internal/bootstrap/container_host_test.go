package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// sharedRuntime is one container runtime two daemons share (one remote
// Docker context, one ssh runner, one cloud account). Its listing is every
// container on it, whoever made it: what is the daemon's is for the
// daemon's labels to say.
type sharedRuntime struct {
	extension.ContainerProvider
	mu         sync.Mutex
	containers map[string]map[string]string
	removed    []string
}

func newSharedRuntime() *sharedRuntime {
	return &sharedRuntime{containers: map[string]map[string]string{}}
}

// launch makes a run's container as a daemon's binding does: labelled with
// the daemon's container host (bindContainer's Host).
func (r *sharedRuntime) launch(d *daemon, id, execution string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers[id] = map[string]string{extension.ContainerLabelHost: d.containerHost(), extension.ContainerLabelExecution: execution, extension.ContainerLabelSegment: "1"}
}

func (r *sharedRuntime) List(context.Context, extension.ResourceOwner) ([]extension.ContainerState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.containers))
	for id := range r.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []extension.ContainerState
	for _, id := range ids {
		out = append(out, extension.ContainerState{Ref: extension.ContainerRef{ID: id, Labels: r.containers[id]}, Running: true})
	}
	return out, nil
}

func (r *sharedRuntime) Remove(_ context.Context, ref extension.ContainerRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.containers, ref.ID)
	r.removed = append(r.removed, ref.ID)
	return nil
}

// containerDaemon is a daemon with its own store, at state (a path, which
// another daemon on another machine may share), using runtime. earlier are
// executions its store held before it first started with a container
// host ID (an upgrade); nil is a new installation.
func containerDaemon(t *testing.T, state string, providers map[string]extension.ContainerProvider, earlier map[domain.ID]domain.ExecutionState) *daemon {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d := &daemon{o: Options{StateDir: state}, coord: store, now: func() time.Time { return time.Unix(1700000000, 0) },
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	d.containers.providers.set(providers)
	if earlier != nil {
		d.record(t, earlier)
	}
	if err := d.openContainers(ctx); err != nil {
		t.Fatal(err)
	}
	return d
}

// record puts executions in the daemon's store, each of its own Work:
// pending (live) or cancelled (over).
func (d *daemon) record(t *testing.T, executions map[domain.ID]domain.ExecutionState) {
	t.Helper()
	at := d.now()
	err := d.coord.Transact(context.Background(), storage.Guard{Now: at}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0); err != nil && !errors.Is(err, storage.ErrConflict) {
			return err
		}
		for id, state := range executions {
			event := domain.Event{ID: "e-" + id, Type: "fixture.changed", Source: "fixture", DeliveryID: string(id), OccurredAt: at, ReceivedAt: at}
			if _, _, err := tx.AcceptEvent(event); err != nil {
				return err
			}
			work := "w-" + id
			if err := tx.PutWork(domain.Work{ID: work, ScopeID: "eng", Kind: "fixture.work", State: domain.WorkReady}, 0); err != nil {
				return err
			}
			x := domain.Execution{ID: id, WorkID: work, ExecutorBinding: "run", Executor: "fixture", Attempt: 1, State: domain.ExecutionPending, TriggerID: event.ID}
			if err := tx.PutExecution(x, 0); err != nil {
				return err
			}
			if state != domain.ExecutionPending {
				if err := endExecution(tx, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// endExecution cancels a pending execution.
func endExecution(tx storage.Tx, id domain.ID) error {
	x, err := tx.Execution(id)
	if err != nil {
		return err
	}
	rev := x.Revision
	x.State = domain.ExecutionCancelled
	return tx.PutExecution(x, rev)
}

// samePathRuns is #724's scenario: two daemons whose state directories
// have the same path, on different machines, share one container runtime.
// Each launches a run and has one that ended; then each reconciles. It
// returns what reconcile removed. relabel, when set, gives each daemon its
// labels before anything launches (the negative control's path hash).
func samePathRuns(t *testing.T, relabel func(*daemon)) []string {
	const state = "/var/lib/open-conduit"
	rt := newSharedRuntime()
	shared := map[string]extension.ContainerProvider{"shared": rt}
	a, b := containerDaemon(t, state, shared, nil), containerDaemon(t, state, shared, nil)
	if relabel != nil {
		relabel(a)
		relabel(b)
	}
	a.record(t, map[domain.ID]domain.ExecutionState{"xa": domain.ExecutionPending, "xa-done": domain.ExecutionCancelled})
	b.record(t, map[domain.ID]domain.ExecutionState{"xb": domain.ExecutionPending, "xb-done": domain.ExecutionCancelled})
	rt.launch(a, "a-run", "xa")
	rt.launch(a, "a-ended", "xa-done")
	rt.launch(b, "b-run", "xb")
	rt.launch(b, "b-ended", "xb-done")
	a.reconcileContainersPass(context.Background())
	b.reconcileContainersPass(context.Background())
	sort.Strings(rt.removed)
	return rt.removed
}

// TestSamePathDaemonsKeepTheirOwnContainers (#724): two daemons at the same
// state directory path, sharing a container runtime, label their
// containers with their installations' IDs, so each reconcile removes its
// own ended run's container and never the other's live one. The negative
// control: with the path hash for a label, each removes the other's live
// run as an execution it doesn't know.
func TestSamePathDaemonsKeepTheirOwnContainers(t *testing.T) {
	if removed := samePathRuns(t, nil); len(removed) != 2 || removed[0] != "a-ended" || removed[1] != "b-ended" {
		t.Fatalf("reconcile removed %v, want only each daemon's own ended run's", removed)
	}
	pathHash := func(d *daemon) {
		d.containers.hosts.Store(&extension.ContainerHosts{Host: legacyContainerHost(d.o.StateDir)})
	}
	removed := samePathRuns(t, pathHash)
	if len(removed) != 4 {
		t.Fatalf("control: with the path hash reconcile removed %v, want the other daemon's live runs too", removed)
	}
}

// TestUpgradedDaemonKeepsItsPathHashContainers (#724): a daemon upgraded
// mid-run gets an installation ID and still recognises the containers it
// made under its path hash: reconcile keeps its live run's container,
// removes it once the run is over, and leaves one of an execution it
// doesn't know (another installation at the same path made it). The former
// label is never dropped, so it survives passes with nothing under it and
// a restart, and the ID is kept. The control: a new installation, its
// store empty, has no former label.
func TestUpgradedDaemonKeepsItsPathHashContainers(t *testing.T) {
	const state = "/home/conduit/.local/state/open-conduit"
	legacy := legacyContainerHost(state)
	rt := newSharedRuntime()
	rt.containers["old-run"] = map[string]string{extension.ContainerLabelHost: legacy, extension.ContainerLabelExecution: "x-old", extension.ContainerLabelSegment: "1"}
	rt.containers["theirs"] = map[string]string{extension.ContainerLabelHost: legacy, extension.ContainerLabelExecution: "x-theirs", extension.ContainerLabelSegment: "1"}
	d := containerDaemon(t, state, map[string]extension.ContainerProvider{"shared": rt}, map[domain.ID]domain.ExecutionState{"x-old": domain.ExecutionPending})
	hosts := d.containerHosts()
	if hosts.Host == legacy || len(hosts.Former) != 1 || hosts.Former[0] != legacy {
		t.Fatalf("upgraded labels %+v, want a new ID with the path hash as a former label", hosts)
	}
	d.reconcileContainersPass(context.Background())
	if len(rt.removed) != 0 {
		t.Fatalf("reconcile removed %v while the run is live", rt.removed)
	}
	// The run ends: its container goes, and another installation's stays.
	if err := d.coord.Transact(context.Background(), storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		return endExecution(tx, "x-old")
	}); err != nil {
		t.Fatal(err)
	}
	d.reconcileContainersPass(context.Background())
	if len(rt.removed) != 1 || rt.removed[0] != "old-run" {
		t.Fatalf("reconcile removed %v, want only the ended run's", rt.removed)
	}
	delete(rt.containers, "theirs")
	d.reconcileContainersPass(context.Background())
	if err := d.openContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := d.containerHosts(); got.Host != hosts.Host || len(got.Former) != 1 || got.Former[0] != legacy {
		t.Fatalf("labels %+v after passes with nothing under the path hash and a restart, want %+v kept", got, hosts)
	}
	if fresh := containerDaemon(t, state, nil, nil).containerHosts(); fresh.Host == "" || fresh.Host == hosts.Host || len(fresh.Former) != 0 {
		t.Fatalf("control: a new installation's labels %+v, want a new ID alone", fresh)
	}
}

// TestFormerLabelSurvivesAMissingProvider (#724 review): the first pass
// after an upgrade runs while one of two container packages isn't running.
// The one that is lists nothing under the path hash; the former label
// stays, so the missing provider's pinned run is still the daemon's when
// it comes back, and reconcile keeps its live container then.
func TestFormerLabelSurvivesAMissingProvider(t *testing.T) {
	const state = "/var/lib/open-conduit"
	legacy := legacyContainerHost(state)
	docker, ssh := newSharedRuntime(), newSharedRuntime()
	ssh.containers["remote-run"] = map[string]string{extension.ContainerLabelHost: legacy, extension.ContainerLabelExecution: "x-remote", extension.ContainerLabelSegment: "1"}
	d := containerDaemon(t, state, map[string]extension.ContainerProvider{"docker": docker}, map[domain.ID]domain.ExecutionState{"x-remote": domain.ExecutionPending})
	d.reconcileContainersPass(context.Background())
	if got := d.containerHosts(); len(got.Former) != 1 || got.Former[0] != legacy {
		t.Fatalf("labels %+v after a pass without the ssh provider, want the path hash kept", got)
	}
	if err := d.openContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.containers.providers.set(map[string]extension.ContainerProvider{"docker": docker, "ssh": ssh})
	d.reconcileContainersPass(context.Background())
	if len(ssh.removed) != 0 || !d.containerHosts().Owns(legacy) {
		t.Fatalf("once the ssh provider is back: removed %v, labels %+v; want its live run kept as the daemon's", ssh.removed, d.containerHosts())
	}
}
