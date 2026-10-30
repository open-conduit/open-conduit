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
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/docker"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// containerStopGrace is how long a container is given to exit after the
// host's stop before it is killed.
const containerStopGrace = 10 * time.Second

func containerRegistry(r *config.Registry) {
	r.Containers = map[string]func(json.RawMessage) error{docker.Type: docker.ValidateConfig}
}

// containerState is the host side of the container providers.
type containerState struct {
	providers map[string]extension.ContainerProvider
	docker    *docker.Provider
	last      time.Time
}

// openContainers makes the providers. The gateway image is the one this
// build pins (docker.GatewayImage) unless OPEN_CONDUIT_GATEWAY_IMAGE names
// another, and OPEN_CONDUIT_CONTAINER_FORWARD=tcp forces the toolbelt
// forward macOS uses, for its tests on Linux.
func (d *daemon) openContainers() {
	self, _ := os.Executable()
	gateway := docker.GatewayImage
	if v := os.Getenv("OPEN_CONDUIT_GATEWAY_IMAGE"); v != "" {
		gateway = v
	}
	d.containers.docker = docker.New(docker.Options{Helper: self, Host: d.containerHost(), Gateway: gateway, Forward: os.Getenv("OPEN_CONDUIT_CONTAINER_FORWARD"), Contexts: func() []string {
		var out []string
		for _, b := range d.dockerBindings() {
			out = append(out, b.context)
		}
		return out
	}})
	d.containers.providers = map[string]extension.ContainerProvider{docker.Type: d.containers.docker}
}

// dockerBinding is a binding that runs in a Docker container: its name,
// context and image.
type dockerBinding struct{ name, context, image string }

// dockerBindings are the enabled scopes' bindings that declare a docker
// container, in config order.
func (d *daemon) dockerBindings() []dockerBinding {
	snapshot, _ := d.configs.Current()
	if snapshot == nil {
		return nil
	}
	var out []dockerBinding
	for _, id := range snapshot.Order {
		scope := snapshot.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, b := range scope.Bindings {
			if b.Container == nil || b.Container.Provider != docker.Type {
				continue
			}
			if cfg, s, err := docker.ParseConfig(b.Container.Config); err == nil {
				out = append(out, dockerBinding{string(id) + "/" + b.Name, s.Context, cfg.Image})
			}
		}
	}
	return out
}

// containerCheck is doctor's view of the Docker provider for each context
// and image the config uses: the daemon reachable, and its ID; Engine 28
// or later; a file written under --user into the workspace root owned by
// the operator on the host; the gateway image there by digest; and the
// internal-network preflight. Any failure blocks the provider.
func (d *daemon) containerCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "container"}
	bindings := d.dockerBindings()
	if len(bindings) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no binding declares a container"
		return check
	}
	check.Status, check.Code = runtime.CheckOK, "ok"
	root := filepath.Join(d.o.StateDir, "resources", "git", "workspaces")
	seen := map[string]bool{}
	var details, names []string
	for _, b := range bindings {
		names = append(names, b.name)
		if seen[b.context+"\x00"+b.image] {
			continue
		}
		seen[b.context+"\x00"+b.image] = true
		h := d.containers.docker.Health(ctx, b.context, b.image, root, false)
		check.Items = append(check.Items, h.Items...)
		if h.Status != "ok" && check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckFail, h.Code
		}
		details = append(details, h.Detail)
	}
	check.Detail = strings.Join(details, "; ")
	if check.Status == runtime.CheckFail {
		check.Detail += "; container bindings run nothing until this is fixed"
	}
	check.Items = append(check.Items, "in containers: "+strings.Join(names, ", "))
	return check
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
func (d *daemon) bindContainer(ctx context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
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
	// A gateway that can't reach a host socket (macOS) reaches the
	// toolbelt on a loopback port the host serves for the execution, on
	// each launch and recovery (a restarted daemon serves a new one).
	if socket != "" && desc.Provider == docker.Type && d.containers.docker.Forward() == docker.ForwardTCP {
		_, s, err := docker.ParseConfig(desc.Config)
		if err != nil {
			return runtime.ContainerBinding{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
		}
		host, err := d.containers.docker.ForwardListen(ctx, s.Context)
		if err == nil {
			_, err = d.endpoints.ServeTCP(state.ExecutionID, host)
		}
		if err != nil {
			return runtime.ContainerBinding{}, fmt.Errorf("%w: the toolbelt forward: %v", extension.ErrSandboxUnavailable, err)
		}
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
