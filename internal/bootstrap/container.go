package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
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
	providers providerSet[extension.ContainerProvider]
	last      time.Time
	// reconciling is set while a reconcile pass runs off the loop.
	reconciling atomic.Bool
	// hosts are this installation's container labels (containerHosts).
	hosts atomic.Pointer[extension.ContainerHosts]
	// failed is what the last reconcile pass couldn't do (a container it
	// couldn't collect among it), for doctor; nil when it did it all.
	mu     sync.Mutex
	failed error
}

// reconcileFailed is what the last container reconcile pass couldn't do.
func (c *containerState) reconcileFailed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}

// openContainers loads this installation's container labels, making its
// ID on first start.
func (d *daemon) openContainers(ctx context.Context) error {
	hosts, err := d.loadContainerHosts(ctx)
	if err != nil {
		return fmt.Errorf("the container host label: %w", err)
	}
	d.containers.hosts.Store(&hosts)
	return nil
}

// containerHostNamespace and containerHostKey keep the installation's
// container labels in the coordination store, so a backup bundle carries
// them and a restored installation keeps its ID.
const (
	containerHostNamespace = "container"
	containerHostKey       = "host"
)

// containerHostRecord is the stored labels.
type containerHostRecord struct {
	// Host is the installation's ID, random and made once: every container
	// the daemon makes carries it (extension.ContainerLabelHost), so two
	// daemons sharing a container daemon, a remote or a cloud account
	// never touch each other's containers, wherever their state is.
	Host string `json:"host"`
	// Former are labels the daemon made containers under before it had
	// one: the hash of its state directory's path older versions used.
	// They stay recognised: nothing can prove no container carries one
	// (a provider may be down or ignore former labels), and a pinned run
	// under one would fail its recovery once it was dropped.
	Former []string `json:"former,omitempty"`
}

// adoptLegacyContainerDirs moves what a container provider kept in process
// under a directory of its own to the directory the host names its type
// (containerDir): the ssh provider's pinned host keys, once under
// <state>/remote. A file already in the new directory stays. The old
// directory becomes a link to the new one, because a run created before
// the upgrade pinned its ssh argv, known_hosts path included, in its run
// record, and its helper keeps using it to start, signal, inspect and copy
// back.
func adoptLegacyContainerDirs(state string) (int, error) {
	moved := 0
	for old, typ := range map[string]string{"remote": "ssh"} {
		from, to := filepath.Join(state, old), containerDir(state, typ)
		if fi, err := os.Lstat(from); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
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
		if err := os.Symlink(filepath.Join("containers", typ), from); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// legacyContainerHost is the label older versions gave every container: a
// hash of the state directory's path, which two installations at the same
// path on different machines share.
func legacyContainerHost(state string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(state)))
	return hex.EncodeToString(sum[:8])
}

// loadContainerHosts reads the installation's container labels, making
// them on first use: a random ID and, when the store already holds
// executions (an upgrade from a release that labelled by path), the path
// hash as a former label, since a daemon upgraded mid-run still has
// containers under it. They are kept in the store, so a restored backup
// keeps the ID: it is the same installation.
func (d *daemon) loadContainerHosts(ctx context.Context) (extension.ContainerHosts, error) {
	var hosts extension.ContainerHosts
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		raw, err := tx.Get(containerHostNamespace, containerHostKey)
		if err == nil {
			var r containerHostRecord
			if err := json.Unmarshal(raw, &r); err != nil || !validContainerHost(r.Host) {
				return fmt.Errorf("the stored container host label is unreadable")
			}
			hosts = extension.ContainerHosts{Host: r.Host, Former: r.Former}
			return nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return err
		}
		hosts = extension.ContainerHosts{Host: hex.EncodeToString(id)}
		xs, err := tx.ListExecutions("")
		if err != nil {
			return err
		}
		if len(xs) > 0 {
			hosts.Former = []string{legacyContainerHost(d.o.StateDir)}
		}
		return putContainerHosts(tx, hosts)
	})
	return hosts, err
}

// validContainerHost is whether a label is one the host makes: hex, so it
// fits a label filter and a name.
func validContainerHost(s string) bool {
	if len(s) < 16 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func putContainerHosts(tx storage.Tx, hosts extension.ContainerHosts) error {
	raw, err := json.Marshal(containerHostRecord{Host: hosts.Host, Former: hosts.Former})
	if err != nil {
		return err
	}
	return tx.Put(containerHostNamespace, containerHostKey, raw)
}

// containerHosts are this daemon's container labels: its installation's
// ID, which labels every container it makes, and any former label it still
// recognises (loadContainerHosts).
func (d *daemon) containerHosts() extension.ContainerHosts {
	if h := d.containers.hosts.Load(); h != nil {
		return *h
	}
	return extension.ContainerHosts{}
}

// containerHost is this daemon's ContainerLabelHost value.
func (d *daemon) containerHost() string { return d.containerHosts().Host }

// containerRecords is the host-owned directory an execution's container
// records (the signals the host sent) are kept in.
func (d *daemon) containerRecords(id domain.ID) string {
	return filepath.Join(d.endpoints.Dir(id), "container")
}

// bindContainer gives an attempt its binding's container, bound as
// bindSandbox binds a sandbox. A binding without one gets none.
func (d *daemon) bindContainer(ctx context.Context, state runtime.AttemptState, resources extension.ResourceBroker, env map[string]string, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
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
	p := d.containers.providers.all()[desc.Provider]
	if p == nil {
		return runtime.ContainerBinding{}, fmt.Errorf("%w: container provider %s", extension.ErrSandboxUnavailable, desc.Provider)
	}
	withheld, err := d.containerCredentials(ctx, state.ScopeID, desc.Provider, desc.Config)
	if err != nil {
		return runtime.ContainerBinding{}, err
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
			if err = forwardAddress(host); err == nil {
				_, err = d.endpoints.ServeTCP(state.ExecutionID, host)
			}
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
	hosts := d.containerHosts()
	b := extension.BindContainer(extension.ContainerBinding{
		Provider:    p,
		Owner:       extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID},
		Config:      domain.Payload{Schema: extension.ContainerConfigSchema, Data: append([]byte(nil), desc.Config...)},
		Host:        hosts.Host,
		FormerHosts: hosts.Former,
		Segment:     state.SegmentNumber(),
		Protected:   d.protectedPaths(),
		Shared:      shared,
		RunRoot:     filepath.Join(d.o.StateDir, "executors", state.Executor, "runs"),
		Toolbelt:    socket,
		TokenFile:   tokenFile(socket, d.endpoints.TokenFile(state.ExecutionID)),
		Env:         env,
		Records:     d.containerRecords(state.ExecutionID),
		Home:        home,
		WorkHome:    workHome,
		Pin: func(c extension.ContainerPin) error {
			c.Provider = desc.Provider
			return pin(c)
		},
		Pinned: pinned,
		Now:    d.now,
		Phase:  runtime.LaunchPhases(ctx),
		// A provider that places launches goes back to where the Work
		// last ran, so its workspace there is reused.
		Name:     desc.Provider,
		Prefer:   workPlacement(workHome, desc.Provider),
		Placed:   func(at extension.Placement) { placeWork(workHome, desc.Provider, at) },
		Withheld: func() []string { return withheld },
		// Bound again with the site, so the resource can be made there;
		// a binding that can't be bound again keeps the host's.
		AtSite: func(site extension.Site) extension.ResourceBroker {
			if r, err := d.bindResourcesAt(ctx, state, site); err == nil && r != nil {
				return r
			}
			return resources
		},
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

// workPlacement is the placement the Work last ran at under provider,
// kept in <key>.site beside its home (never mounted), "" for none.
func workPlacement(home, provider string) string {
	raw, err := os.ReadFile(home + ".site")
	if err != nil {
		return ""
	}
	p, at, ok := extension.SplitSiteID(strings.TrimSpace(string(raw)))
	if !ok || p != provider {
		return ""
	}
	return at
}

// placeWork records where the Work was placed, for its next launch. A
// failure to record only loses the preference.
func placeWork(home, provider string, at extension.Placement) {
	tmp := home + ".site.tmp"
	if os.WriteFile(tmp, []byte(extension.SiteID(provider, at.ID)+"\n"), 0o600) == nil {
		_ = os.Rename(tmp, home+".site")
	}
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
	var done map[domain.ID]bool
	err = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		done = map[domain.ID]bool{}
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
			errs = append(errs, os.RemoveAll(strings.TrimSuffix(name, ".work")), os.Remove(name), ignoreMissing(os.Remove(strings.TrimSuffix(name, ".work")+".site")))
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
	return extension.StopContainer(ctx, d.containers.providers.all()[pin.Provider], pin, d.containerRecords(state.ExecutionID), containerStopGrace)
}

// reconcileContainers removes this host's containers whose execution runs
// no segment (parked, terminal or gone), with what each provider made for
// them, and the homes of finished Work, at most once per resource
// interval.
func (d *daemon) reconcileContainers(ctx context.Context) {
	if len(d.containers.providers.all()) == 0 || d.now().Sub(d.containers.last) < d.o.ResourceInterval {
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
	providers := d.containers.providers.all()
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	hosts := d.containerHosts()
	for _, name := range names {
		removed, err := extension.ReconcileHostContainers(ctx, providers[name], hosts, func(ids []domain.ID) (map[domain.ID]extension.ContainerKeep, error) {
			var keep map[domain.ID]extension.ContainerKeep
			err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
				keep = map[domain.ID]extension.ContainerKeep{}
				for _, id := range ids {
					x, err := tx.Execution(id)
					if errors.Is(err, storage.ErrNotFound) {
						continue
					}
					if err != nil {
						return err
					}
					keep[id] = containersKept(x.State)
				}
				return nil
			})
			return keep, err
		})
		if removed > 0 {
			d.log.Info("removed what ended or parked runs left in containers", "provider", name, "removed", removed)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	containersErr := errors.Join(errs...)
	if ctx.Err() == nil {
		d.containers.mu.Lock()
		d.containers.failed = containersErr
		d.containers.mu.Unlock()
	}
	if err := errors.Join(containersErr, d.sweepHomes(ctx)); err != nil && ctx.Err() == nil {
		code := "reconcile_failed"
		if errors.Is(err, extension.ErrContainerUncollected) {
			code = "container_uncollected"
		}
		d.report("containers", "", code, err.Error())
		return
	}
	d.clear("containers", "")
}

// containersKept is how much of an execution's containers reconcile
// keeps. A pending or running execution keeps them all. A parked
// execution's segment is over and its result recorded, and its wake
// launches a new container; what it has is a stopped one and what the
// provider made beside it, which can hold resources with a small fixed
// limit (Docker's networks), so reconcile removes them as it does a
// finished execution's (#669). It keeps a parked execution's attach,
// which an operator may be launching (nothing of it runs until their CLI
// starts it); their detach or the wake's launch removes it.
func containersKept(state domain.ExecutionState) extension.ContainerKeep {
	switch state {
	case domain.ExecutionPending, domain.ExecutionRunning:
		return extension.ContainerKeepAll
	case domain.ExecutionParked:
		return extension.ContainerKeepAttach
	}
	return extension.ContainerKeepNone
}

// segmentEnded tells every extension that issued something for an
// execution's container segment (git tokens) that the segment has ended.
func (d *daemon) segmentEnded(id domain.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, x := range d.extensionPackages() {
		x.SegmentEnded(ctx, id)
	}
}

// containerStatus adds the owner's containers to a resource owner's status:
// a finished execution's containers are removed before its workspace may
// be released (a failure keeps it), and a live one's mounts are reported in
// use. A listing that fails is an error, so nothing is released or swept
// on a guess.
func (d *daemon) containerStatus(ctx context.Context, o extension.ResourceOwner, st *extension.ResourceOwnerStatus) error {
	for _, p := range d.containers.providers.all() {
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
	a, ok := d.executorMap()[executor].(extension.ContainerAttachExecutor)
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
	p := d.containers.providers.all()[desc.Provider]
	if p == nil {
		return management.ContainerAttachment{}, fmt.Errorf("%w: container provider %s", extension.ErrSandboxUnavailable, desc.Provider)
	}
	withheld, err := d.containerCredentials(ctx, state.ScopeID, desc.Provider, desc.Config)
	if err != nil {
		return management.ContainerAttachment{}, err
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
	// A workspace the last segment had at its site is where the attach
	// goes, and what it is launched in.
	prefer, workspace, err := extension.AttachWorkspace(state.ContainerPin(), desc.Provider, workPlacement(workHome, desc.Provider))
	if err != nil {
		return management.ContainerAttachment{}, err
	}
	home, _ := os.UserHomeDir()
	var pinned extension.ContainerPin
	b := extension.BindContainer(extension.ContainerBinding{
		Provider: p,
		Owner:    extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: execution},
		Config:   domain.Payload{Schema: extension.ContainerConfigSchema, Data: append([]byte(nil), desc.Config...)},
		Host:     d.containerHost(),
		Attach:   true,
		Withheld: func() []string { return withheld },
		// An attach goes where the Work last ran, and never moves it.
		Name:      desc.Provider,
		Prefer:    prefer,
		Workspace: workspace,
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
	var execution domain.ID
	if err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		pin = nil
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
					pin, execution = &a, x.ID
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
	p := d.containers.providers.all()[pin.Provider]
	if p == nil {
		return fmt.Errorf("%w: container provider %s", extension.ErrSandboxUnavailable, pin.Provider)
	}
	// A terminal whose client went leaves its container running: the attach
	// process was only attached to it. A container that has exited is left
	// to Remove.
	if err := extension.StopContainer(ctx, p, pin, d.containerRecords(execution), containerStopGrace); err != nil {
		return err
	}
	return extension.RemoveContainer(ctx, p, extension.ContainerRef{ID: pin.ID})
}

// forwardAddress checks an address a provider names for an execution's
// toolbelt forward before the host serves on it: a literal loopback or
// private IP (macOS's loopback, a Linux bridge's gateway), never an
// unspecified one or a name, so no package can have the toolbelt served
// on every interface or a public one.
func forwardAddress(host string) error {
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || !(ip.IsLoopback() || ip.IsPrivate()) {
		return fmt.Errorf("the provider named %q, not a loopback or private IP address", host)
	}
	return nil
}

// ignoreMissing is err, unless it says the file wasn't there.
func ignoreMissing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// tokenFile is the execution's toolbelt token file, "" when it has no
// toolbelt socket.
func tokenFile(socket, file string) string {
	if socket == "" {
		return ""
	}
	return file
}

// resolveSite is the host's SiteResolver for a resource type: the site id
// names, as a Site with no toolbelt, only when the host gave that type the
// site in an acquisition (siteLedger), a binding of an enabled scope in
// the current config still pairs the type with the site's container
// provider, and that provider places launches. Anything else is
// unavailable, so a resource provider reaches only the machines the host
// placed its own acquisitions on.
func (d *daemon) resolveSite(_ context.Context, resourceType, id string) (extension.Site, error) {
	current, _ := d.configs.Current()
	return siteFor(current, d.containers.providers.all(), d.sites.given, resourceType, id)
}

func siteFor(current *config.Snapshot, providers map[string]extension.ContainerProvider, given func(resourceType, site string) bool, resourceType, id string) (extension.Site, error) {
	provider, placement, ok := extension.SplitSiteID(id)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a site", extension.ErrSiteUnavailable, id)
	}
	paired := false
	if current != nil {
		for _, s := range current.Scopes {
			if !s.Enabled {
				continue
			}
			for _, b := range s.Bindings {
				if b.Resource != nil && b.Container != nil && b.Resource.Provider == resourceType && b.Container.Provider == provider {
					paired = true
				}
			}
		}
	}
	if !paired {
		return nil, fmt.Errorf("%w: no binding pairs resource provider %s with container provider %s", extension.ErrSiteUnavailable, resourceType, provider)
	}
	if !given(resourceType, id) {
		return nil, fmt.Errorf("%w: the host never gave resource provider %s an acquisition at %s", extension.ErrSiteUnavailable, resourceType, id)
	}
	placer, ok := providers[provider].(extension.ContainerPlacer)
	if !ok {
		return nil, fmt.Errorf("%w: container provider %s doesn't place launches", extension.ErrSiteUnavailable, provider)
	}
	return extension.PlacedSite{Provider: provider, Placer: placer, At: extension.Placement{ID: placement}}, nil
}

// containerCredentials are the credentials a launch's container must not
// carry (extension.ContainerBinding.Withheld): the current values of the
// connection its config names (extension.ContainerConfig.Connection),
// resolved for the package providing the container, as its secret.resolve
// would give them. None when the config names no connection. They are
// resolved for each launch rather than remembered from what the package
// asked for, so the check holds after a restart and for a credential the
// provider keeps itself. One that can't be resolved refuses the launch:
// the host can't check what it can't see.
func (d *daemon) containerCredentials(ctx context.Context, scope domain.ID, provider string, raw json.RawMessage) ([]string, error) {
	var cfg struct {
		Connection string `json:"connection"`
	}
	if json.Unmarshal(raw, &cfg) != nil || cfg.Connection == "" {
		return nil, nil
	}
	refuse := func(why string) error {
		return fmt.Errorf("%w: the container's connection %s %s", extension.ErrSandboxUnavailable, cfg.Connection, why)
	}
	s, _ := d.extensionSnapshot()
	if s == nil || s.Scopes[scope] == nil {
		return nil, refuse("is not in the current config")
	}
	var conn *config.Connection
	for i := range s.Connections {
		if s.Connections[i].ID == cfg.Connection {
			conn = &s.Connections[i]
		}
	}
	if conn == nil {
		return nil, refuse("is not in the current config")
	}
	pkg := d.containerOwner(provider)
	if pkg == "" {
		return nil, refuse("has no provider package running")
	}
	resolver := d.resolver.For(pkg)
	names := make([]string, 0, len(conn.SecretRefs))
	for n := range conn.SecretRefs {
		names = append(names, n)
	}
	slices.Sort(names)
	var out []string
	for _, n := range names {
		v, err := resolver.ResolveNamed(ctx, n, conn.SecretRefs[n])
		if err != nil {
			return nil, refuse("could not be resolved to check the launch against")
		}
		out = append(out, v)
	}
	return out, nil
}

// containerOwner is the name of the running package that provides
// container type typ, "" when none does.
func (d *daemon) containerOwner(typ string) string {
	d.external.mu.Lock()
	defer d.external.mu.Unlock()
	for name, x := range d.external.byName {
		for _, c := range x.m.Provides.Containers {
			if c.Type == typ {
				return name
			}
		}
	}
	return ""
}
