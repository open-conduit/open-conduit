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
	"github.com/open-conduit/open-conduit/internal/management"
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
	// tokens is when the git tokens of ended executions were last swept.
	tokens time.Time
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
	workHome, err := d.workHome(state.WorkID)
	if err != nil {
		return runtime.ContainerBinding{}, fmt.Errorf("%w: the Work's home: %v", extension.ErrSandboxUnavailable, err)
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
		WorkHome:  workHome,
		Pin: func(c extension.ContainerPin) error {
			c.Provider = desc.Provider
			return pin(c)
		},
		Pinned: pinned,
		Now:    d.now,
	})
	// The segment's end is the end of the git tokens minted for it.
	settle := func(r extension.Result) extension.Result {
		d.revokeGitTokens(state.ExecutionID)
		return b.Settle(r)
	}
	return runtime.ContainerBinding{Broker: b, Resources: b.Resources(resources), Settle: settle, Home: workHome}, nil
}

// homesDir holds each Work's container home, <key>/, beside <key>.work,
// which names the Work and is never mounted.
func (d *daemon) homesDir() string { return filepath.Join(d.o.StateDir, "homes") }

// workHome is the Work's container home, made (empty, owner-only) on first
// use. It is per Work, not per execution, so a session resumes across
// segments, and removed once the Work is finished (sweepHomes).
func (d *daemon) workHome(work domain.ID) (string, error) {
	sum := sha256.Sum256([]byte(work))
	key := hex.EncodeToString(sum[:16])
	dir := filepath.Join(d.homesDir(), key)
	if err := os.MkdirAll(d.homesDir(), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(dir+".work", []byte(string(work)+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return dir, nil
}

// sweepHomes removes the container home of each Work that is finished or
// gone, as the claude-code executor forgets its continuity.
func (d *daemon) sweepHomes(ctx context.Context) error {
	names, err := filepath.Glob(filepath.Join(d.homesDir(), "*.work"))
	if err != nil || len(names) == 0 {
		return err
	}
	works := map[string]domain.ID{}
	for _, name := range names {
		raw, err := os.ReadFile(name)
		if err == nil {
			works[name] = domain.ID(strings.TrimSpace(string(raw)))
		}
	}
	done := map[domain.ID]bool{}
	err = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		for _, id := range works {
			w, err := tx.Work(id)
			switch {
			case errors.Is(err, storage.ErrNotFound):
				done[id] = true
			case err != nil:
				return err
			default:
				done[id] = w.State.Terminal()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var errs []error
	for name, id := range works {
		if done[id] {
			errs = append(errs, os.RemoveAll(strings.TrimSuffix(name, ".work")), os.Remove(name))
		}
	}
	return errors.Join(errs...)
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
// terminal or gone, with what each provider made for them, and the homes
// of finished Work, at most once per resource interval.
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
	errs = append(errs, d.sweepHomes(ctx))
	if err := errors.Join(errs...); err != nil && ctx.Err() == nil {
		d.report("containers", "", "reconcile_failed")
		return
	}
	d.clear("containers", "")
}

// revokeGitTokens revokes the git tokens the GitHub tools minted for an
// execution (github.git.credential, behind `os git-credential`) once its
// segment has ended. One GitHub couldn't be told about is kept for
// sweepGitTokens.
func (d *daemon) revokeGitTokens(id domain.ID) {
	if d.tools == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.tools.RevokeGitTokens(ctx, id); err != nil {
		d.report("git_credentials", "", "revoke_failed")
	}
}

// sweepGitTokens revokes the git tokens of every execution that isn't
// running, at most once per resource interval: those whose segment ended
// without the host's Settle (a stop, a lost lease) or whose revocation
// failed.
func (d *daemon) sweepGitTokens(ctx context.Context) {
	if d.tools == nil || d.now().Sub(d.containers.tokens) < d.o.ResourceInterval {
		return
	}
	d.containers.tokens = d.now()
	err := d.tools.SweepGitTokens(ctx, func(ids []domain.ID) (map[domain.ID]bool, error) {
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
				live[id] = x.State == domain.ExecutionRunning
			}
			return nil
		})
		return live, err
	})
	if err != nil && ctx.Err() == nil {
		d.report("git_credentials", "", "revoke_failed")
		return
	}
	d.clear("git_credentials", "")
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

// containerAttach makes the attach container for an operator's `open-conduit
// container attach`: under the binding's current container policy, with
// the Work's home and workspace, a gateway and an internal network, and no
// toolbelt. Its plan is pinned and recorded with the attempt it attaches
// to before it is returned. The executor says what runs in it.
func (d *daemon) containerAttach(ctx context.Context, executor string, w domain.Work, execution domain.ID) (management.ContainerAttachment, error) {
	a, ok := d.executors[executor].(extension.ContainerAttachExecutor)
	if !ok {
		return management.ContainerAttachment{}, management.ErrAttachUnsupported
	}
	var state runtime.AttemptState
	if err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		var err error
		state, err = runtime.AttemptStateInTx(tx, execution)
		return err
	}); err != nil {
		return management.ContainerAttachment{}, err
	}
	desc, err := d.configs.Container(state)
	if err != nil {
		return management.ContainerAttachment{}, err
	}
	if desc == nil {
		return management.ContainerAttachment{}, fmt.Errorf("%w: the binding no longer declares a container", extension.ErrSandboxUnavailable)
	}
	p := d.containers.providers[desc.Provider]
	if p == nil {
		return management.ContainerAttachment{}, fmt.Errorf("%w: container provider %s", extension.ErrSandboxUnavailable, desc.Provider)
	}
	secrets, err := d.configs.Secrets(d.resolver)(ctx, state)
	if err != nil {
		return management.ContainerAttachment{}, err
	}
	workHome, err := d.workHome(w.ID)
	if err != nil {
		return management.ContainerAttachment{}, fmt.Errorf("%w: the Work's home: %v", extension.ErrSandboxUnavailable, err)
	}
	spec, err := a.ContainerAttach(ctx, extension.ContainerAttachRequest{Work: w, Config: state.Binding.Config, Secrets: secrets, Home: workHome})
	if err != nil {
		return management.ContainerAttachment{}, err
	}
	home, _ := os.UserHomeDir()
	var pinned extension.ContainerPin
	b := extension.BindContainer(extension.ContainerBinding{
		Provider:  p,
		Owner:     extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: execution},
		Config:    domain.Payload{Schema: extension.ContainerConfigSchema, Data: append([]byte(nil), desc.Config...)},
		Host:      d.containerHost(),
		Attach:    true,
		Protected: d.protectedPaths(),
		Shared:    []string{d.pluginDir()},
		RunRoot:   filepath.Join(d.o.StateDir, "executors", executor, "runs"),
		Records:   d.containerRecords(execution),
		Home:      home,
		WorkHome:  workHome,
		Pin: func(c extension.ContainerPin) error {
			c.Provider = desc.Provider
			pinned = c
			return d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
				return runtime.RecordAttachInTx(tx, execution, c)
			})
		},
		Now: d.now,
	})
	plan, err := b.Launch(ctx, spec.Needs, spec.Launch)
	if err != nil {
		return management.ContainerAttachment{}, err
	}
	return management.ContainerAttachment{Ref: pinned.ID, Argv: plan.Argv, Env: plan.Env}, nil
}

// containerDetach removes an attach container of the Work, with its
// gateway and network: one recorded with one of the Work's attempts.
func (d *daemon) containerDetach(ctx context.Context, w domain.Work, ref string) error {
	var pin *extension.ContainerPin
	if err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(w.ID)
		if err != nil {
			return err
		}
		for _, x := range xs {
			st, err := runtime.AttemptStateInTx(tx, x.ID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			} else if err != nil {
				return err
			}
			for _, a := range st.Attaches {
				if a.ID == ref {
					pin = &a
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if pin == nil {
		return storage.ErrNotFound
	}
	p := d.containers.providers[pin.Provider]
	if p == nil {
		return fmt.Errorf("%w: container provider %s", extension.ErrSandboxUnavailable, pin.Provider)
	}
	return p.Remove(ctx, extension.ContainerRef{ID: pin.ID})
}
