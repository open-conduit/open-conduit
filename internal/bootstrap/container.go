package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// containerStopGrace is how long a container is given to exit after the
// host's stop before it is killed.
const containerStopGrace = 10 * time.Second

// containerState is the host side of the container providers: the
// packages' declared ones (manifest containers), by type.
type containerState struct {
	providers map[string]extension.ContainerProvider
	last      time.Time
	// reconciling is set while a reconcile pass runs off the loop.
	reconciling atomic.Bool
}

func (d *daemon) openContainers() {
	d.containers.providers = map[string]extension.ContainerProvider{}
}

// adoptLegacyContainerDirs moves what a container provider kept in process
// under a directory of its own to the directory the host names its type
// (containerDir): the ssh provider's pinned host keys, once under
// <state>/remote. A file already in the new directory stays.
func adoptLegacyContainerDirs(state string) (int, error) {
	moved := 0
	for old, typ := range map[string]string{"remote": "ssh"} {
		from, to := filepath.Join(state, old), containerDir(state, typ)
		entries, err := os.ReadDir(from)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return moved, err
		}
		if err := os.MkdirAll(to, 0o700); err != nil {
			return moved, err
		}
		for _, e := range entries {
			dst := filepath.Join(to, e.Name())
			if _, err := os.Lstat(dst); err == nil {
				continue
			}
			if err := os.Rename(filepath.Join(from, e.Name()), dst); err != nil {
				return moved, err
			}
			moved++
		}
		if err := os.RemoveAll(from); err != nil {
			return moved, err
		}
	}
	return moved, nil
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
	// A container that can't reach a host socket (Docker on macOS) reaches
	// the toolbelt on an address the host serves for the execution, which
	// the provider names, on each launch and recovery (a restarted daemon
	// serves a new one).
	if f, ok := p.(extension.ContainerForwarder); ok && socket != "" {
		host, err := f.Forward(ctx, domain.Payload{Schema: extension.ContainerConfigSchema, Data: append([]byte(nil), desc.Config...)})
		if err == nil && host != "" {
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
		Phase:  runtime.LaunchPhases(ctx),
	})
	// The segment's end is the end of what extensions issued for it.
	settle := func(r extension.Result) extension.Result {
		d.segmentEnded(state.ExecutionID)
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
	if !d.containers.reconciling.CompareAndSwap(false, true) {
		return
	}
	d.containers.last = d.now()
	// It runs off the loop, like the projections: a wedged container
	// runtime must not hold routing, timers and starts.
	d.goSafe("containers", func() {
		defer d.containers.reconciling.Store(false)
		d.reconcileContainersPass(ctx)
	})
}

func (d *daemon) reconcileContainersPass(ctx context.Context) {
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

// segmentEnded tells every extension that issued something for an
// execution's container segment (git tokens) that the segment has ended.
func (d *daemon) segmentEnded(id domain.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, x := range d.inproc {
		if e, ok := x.(inproc.SegmentEnder); ok {
			e.SegmentEnded(ctx, id)
		}
	}
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
	if errors.Is(err, extension.ErrUnsupported) {
		return management.ContainerAttachment{}, management.ErrAttachUnsupported
	}
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
