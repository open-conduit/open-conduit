package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// containerStopGrace is how long a container is given to exit after the
// host's stop before it is killed.
const containerStopGrace = 10 * time.Second

func containerRegistry(r *config.Registry) {
	// No container provider ships yet: the Docker provider comes next.
	r.Containers = map[string]func(json.RawMessage) error{}
}

// containerState is the host side of the container providers.
type containerState struct {
	providers map[string]extension.ContainerProvider
	last      time.Time
}

func (d *daemon) openContainers() {
	d.containers.providers = map[string]extension.ContainerProvider{}
}

// containerHost is this daemon's ContainerLabelHost value: a hash of its
// state directory, so two daemons sharing a container daemon never touch
// each other's containers.
func (d *daemon) containerHost() string {
	sum := sha256.Sum256([]byte(filepath.Clean(d.o.StateDir)))
	return hex.EncodeToString(sum[:8])
}

// containerRecords is the host-owned directory an execution's container
// records (the signals the host sent) are kept in.
func (d *daemon) containerRecords(id domain.ID) string {
	return filepath.Join(d.endpoints.Dir(id), "container")
}

// bindContainer gives an attempt its binding's container, bound as
// bindSandbox binds a sandbox. A binding without one gets none.
func (d *daemon) bindContainer(_ context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
	desc, err := d.configs.Container(state)
	if err != nil {
		return runtime.ContainerBinding{}, err
	}
	if desc == nil {
		return runtime.ContainerBinding{}, nil
	}
	if !state.Capabilities.Container {
		return runtime.ContainerBinding{}, fmt.Errorf("%w: executor %s can't run in a container", extension.ErrSandboxUnavailable, state.Executor)
	}
	p := d.containers.providers[desc.Provider]
	if p == nil {
		return runtime.ContainerBinding{}, fmt.Errorf("%w: container provider %s", extension.ErrSandboxUnavailable, desc.Provider)
	}
	run := d.endpoints.Dir(state.ExecutionID)
	shared := []string{run, d.pluginDir()}
	socket := d.endpoints.Socket(state.ExecutionID)
	if socket != "" {
		shared = append(shared, filepath.Dir(socket))
	}
	var pinned *extension.ContainerPin
	if state.LaunchState == "recovering" {
		pinned = state.ContainerPin()
	}
	home, _ := os.UserHomeDir()
	b := extension.BindContainer(extension.ContainerBinding{
		Provider:  p,
		Owner:     extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID},
		Config:    domain.Payload{Schema: extension.ContainerConfigSchema, Data: append([]byte(nil), desc.Config...)},
		Host:      d.containerHost(),
		Segment:   state.SegmentNumber(),
		Protected: d.protectedPaths(),
		Shared:    shared,
		RunRoot:   filepath.Join(d.o.StateDir, "executors", state.Executor, "runs"),
		Toolbelt:  socket,
		Records:   d.containerRecords(state.ExecutionID),
		Home:      home,
		Pin: func(c extension.ContainerPin) error {
			c.Provider = desc.Provider
			return pin(c)
		},
		Pinned: pinned,
		Now:    d.now,
	})
	return runtime.ContainerBinding{Broker: b, Resources: b.Resources(resources), Settle: b.Settle}, nil
}

// containerSignalled records a signal the host is about to send an
// execution, for its latest container.
func (d *daemon) containerSignalled(ctx context.Context, id domain.ID, kind string) {
	c := d.current.Load()
	if c == nil {
		return
	}
	state, err := c.scheduler.InspectAttempt(ctx, id)
	if err != nil {
		return
	}
	if pin := state.ContainerPin(); pin != nil {
		_ = extension.RecordContainerSignal(d.containerRecords(id), pin.ID, kind)
	}
}

// stopContainer stops an attempt's pinned container once its executor has
// stopped, and returns nil only once the container is confirmed not
// running.
func (d *daemon) stopContainer(ctx context.Context, state runtime.AttemptState) error {
	pin := state.ContainerPin()
	if pin == nil {
		return nil
	}
	return extension.StopContainer(ctx, d.containers.providers[pin.Provider], pin, d.containerRecords(state.ExecutionID), containerStopGrace)
}

// reconcileContainers removes this host's containers whose execution is
// terminal or gone, with what each provider made for them, at most once
// per resource interval.
func (d *daemon) reconcileContainers(ctx context.Context) {
	if len(d.containers.providers) == 0 || d.now().Sub(d.containers.last) < d.o.ResourceInterval {
		return
	}
	d.containers.last = d.now()
	names := make([]string, 0, len(d.containers.providers))
	for name := range d.containers.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		errs = append(errs, extension.ReconcileContainers(ctx, d.containers.providers[name], d.containerHost(), func(ids []domain.ID) (map[domain.ID]bool, error) {
			live := map[domain.ID]bool{}
			err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
				for _, id := range ids {
					x, err := tx.Execution(id)
					if errors.Is(err, storage.ErrNotFound) {
						continue
					}
					if err != nil {
						return err
					}
					live[id] = !x.State.Terminal()
				}
				return nil
			})
			return live, err
		}))
	}
	if err := errors.Join(errs...); err != nil && ctx.Err() == nil {
		d.report("containers", "", "reconcile_failed")
		return
	}
	d.clear("containers", "")
}

// containerStatus adds the owner's containers to a resource owner's status:
// a finished execution's containers are removed before its workspace may
// be released (a failure keeps it), and a live one's mounts are reported in
// use. A listing that fails is an error, so nothing is released or swept
// on a guess.
func (d *daemon) containerStatus(ctx context.Context, o extension.ResourceOwner, st *extension.ResourceOwnerStatus) error {
	for _, p := range d.containers.providers {
		if st.ExecutionDone {
			if err := extension.RemoveContainers(ctx, p, o); err != nil {
				return err
			}
			continue
		}
		mounts, err := extension.ContainerMounts(ctx, p, o)
		if err != nil {
			return err
		}
		st.Mounts = append(st.Mounts, mounts...)
	}
	return nil
}
