package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// A container sandbox runs an executor's process in a container the host
// owns. It is a separate contract from the process sandbox (B09) and shares
// its rules: the binding's config is the grant, an executor's needs are only
// checked against it, the host's protected set always applies, and anything
// the host can't check or enforce refuses the launch.
//
// An executor sees a ContainerBroker with two calls: Launch, which creates
// the container (not started) and returns the argv its wrapper runs to
// start and attach to it, and Adopt, in Recover. Stopping, reconciling,
// collecting and removing containers are the host's (StopContainer,
// ReconcileContainers, CollectContainer, RemoveContainers): executors never
// see them, and there is no Signal call for executors.

// The labels every container the host creates carries.
const (
	// ContainerLabelHost names the Open Conduit installation (a random ID
	// made once and kept in its state, ContainerHosts), so two daemons on
	// one container daemon never touch each other's containers.
	ContainerLabelHost = "dev.open-conduit.host"
	// ContainerLabelExecution is the execution the container belongs to.
	ContainerLabelExecution = "dev.open-conduit.execution"
	// ContainerLabelWork is the Work its execution is for, so a launch
	// can find what another execution of the Work left uncollected.
	ContainerLabelWork = "dev.open-conduit.work"
	// ContainerLabelSegment is the attempt segment it was created for.
	ContainerLabelSegment = "dev.open-conduit.segment"
	// ContainerLabelDigest is the digest of the plan it was created from.
	ContainerLabelDigest = "dev.open-conduit.digest"
)

// ContainerSegmentAttach is the ContainerLabelSegment of an operator's
// attach container. Reconcile leaves a running one alone; a segment's
// launch removes it.
const ContainerSegmentAttach = "attach"

// ContainerTermMarker is the file the container's entry stub writes in the
// launch's run directory when the container is sent TERM. A TERM the host
// didn't send makes the segment's result interrupted, whatever its code.
const ContainerTermMarker = ".open-conduit-term"

var (
	// ErrContainerRunning means a container of the execution is still
	// running, so no other may launch beside it (container_running).
	ErrContainerRunning = errors.New("a container of the execution is running")
	// ErrContainerUnconfirmed means the host couldn't confirm a container's
	// state: its daemon is unreachable, or is not the daemon the container
	// was pinned on. Replacement stays blocked until it can.
	ErrContainerUnconfirmed = errors.New("container state unconfirmed")
	// ErrNoCapacity means a container provider has nowhere to put the launch
	// now (its pool is full) and nothing about the launch is wrong. Place,
	// Plan or Create may refuse with it, or with a NoCapacityError that says
	// when to ask again, as long as nothing has started. The host returns
	// the claim and holds the Work: no retry is spent.
	ErrNoCapacity = errors.New("no container capacity")
)

// NoCapacityError is ErrNoCapacity with the provider's account of it.
// Reason is a line for the Work's history, so it carries no credential.
// RetryAt, when set, is when to ask again; nil leaves that to the host's
// backoff. It is a hint: the host bounds it.
type NoCapacityError struct {
	Reason  string
	RetryAt *time.Time
}

func (e *NoCapacityError) Error() string {
	if e.Reason == "" {
		return ErrNoCapacity.Error()
	}
	return ErrNoCapacity.Error() + ": " + e.Reason
}

// Is makes a NoCapacityError match ErrNoCapacity.
func (e *NoCapacityError) Is(target error) bool { return target == ErrNoCapacity }

// ContainerRef identifies a container: the provider's ID and its labels.
type ContainerRef struct {
	ID     string
	Labels map[string]string
}

// ContainerState is a provider's view of one container. Daemon is the ID of
// the container daemon that answered. Gone means that daemon was reached
// and holds no such container; an error, not Gone, means it couldn't be
// asked. Mounts are the host paths the container mounts.
type ContainerState struct {
	Ref       ContainerRef
	Daemon    string
	Image     string
	Running   bool
	Gone      bool
	ExitCode  int
	StartedAt time.Time
	Mounts    []string
}

// ContainerPolicy is the operator's policy for one launch: the binding's
// container config (ContainerConfigSchema, the grant) and this attempt's
// paths.
//
// Placement is the placement the provider gave the execution
// (ContainerPlacer.Place), "" when it places nothing: the provider plans
// and creates there.
type ContainerPolicy struct {
	Config    domain.Payload
	Paths     SandboxPaths
	Placement string
}

// ContainerPlan is what a provider will create: the image pinned by digest,
// a one-line Summary and Data, the effective policy (mounts, network,
// limits), for history. Digest pins the plan: the host computes it from
// Image and Data.
type ContainerPlan struct {
	Image, Digest, Summary string
	Data                   domain.Payload
}

// ContainerCreated is a container created and not started: its reference,
// the daemon it was created on, and the argv and environment the
// executor's wrapper runs to start it, attached, and wait for its exit.
//
// Argv and Env reach the run: the wrapper runs with them, beside the
// executor. A credential the provider resolves for its connection
// (ContainerConfig.Connection) never goes in them, nor in the plan, the
// labels or the reference: a helper that must reach the provider's API
// from there reads a short-lived, narrow credential the provider writes to
// its own owner-only records. The host refuses a launch that carries one
// (ContainerBinding.Withheld).
type ContainerCreated struct {
	Ref       ContainerRef
	Daemon    string
	Argv, Env []string
}

// ContainerProvider is an optional extension that runs launches in
// containers. Core never calls it; the host binds it to an attempt.
//
//   - Plan applies the policy and checks every need against it, as a
//     SandboxProvider does. It never widens the policy.
//   - Create creates the plan's container with labels and never starts it.
//   - Signal sends a signal to a running container; Stop stops it within
//     grace, then kills it.
//   - Inspect reports one container (Gone when its daemon holds none).
//   - List reports every container carrying the owner's labels; an owner
//     with no ExecutionID lists every container of the host and then, as
//     stopped entries carrying the execution's labels, anything else the
//     provider made for an execution that would outlive it (a network, a
//     volume), so reconcile removes what a launch or removal cut short
//     left. A listing that fails is an error, never an empty list.
//   - Remove removes the container and what the provider made for it (a
//     gateway, a network). Removing one that is gone is not an error.
type ContainerProvider interface {
	Plan(ctx context.Context, owner ResourceOwner, policy ContainerPolicy, needs SandboxNeeds, launch SandboxLaunch) (ContainerPlan, error)
	Create(ctx context.Context, plan ContainerPlan, labels map[string]string) (ContainerCreated, error)
	Signal(ctx context.Context, ref ContainerRef, signal string) error
	Stop(ctx context.Context, ref ContainerRef, grace time.Duration) error
	Inspect(ctx context.Context, ref ContainerRef) (ContainerState, error)
	List(ctx context.Context, owner ResourceOwner) ([]ContainerState, error)
	Remove(ctx context.Context, ref ContainerRef) error
}

// ContainerForwarder is an optional ContainerProvider capability: a
// provider whose containers can't reach a host socket (Docker on macOS,
// where the socket can't cross the VM) reach the execution's toolbelt
// through a TCP address the host serves for that execution instead.
// Forward names the address to listen on for a binding's container config,
// or "" when its containers reach the socket itself. The host asks before
// each launch and recovery and serves the toolbelt there itself.
type ContainerForwarder interface {
	Forward(ctx context.Context, config domain.Payload) (string, error)
}

// ContainerCollector is an optional ContainerProvider capability: a
// provider whose containers write somewhere other than the host's own
// paths (another machine) brings back what an exited container wrote, as
// the container's wrapper does when the run ends. The host collects an
// exited container before Adopt returns its exit and before it removes it
// (a launch removing what an earlier one left, RemoveContainers,
// reconcile, an attach's detach), so a run whose wrapper died, with the
// daemon that ran it, still brings its work back.
//
// Collect brings back only what the run was granted to write, with what a
// normal end leaves out left out (a read-only need such as .git/config,
// what the host protects). Collecting a container already collected, by
// its wrapper or by Collect, changes nothing, so a change the host made
// since is never undone; one with nothing to collect (gone, never
// started, something the provider made beside a container) is nil. A
// running container is an error. Any error keeps the container: it is
// neither adopted nor removed, and the host collects it again next time.
type ContainerCollector interface {
	Collect(ctx context.Context, ref ContainerRef) error
}

// ContainerCollecting is optional beside ContainerCollector, for an
// adapter that implements Collect whether or not its provider collects (a
// package's, whose manifest says): Collecting false means it collects
// nothing, and the host treats it as a provider that doesn't collect.
type ContainerCollecting interface {
	Collecting() bool
}

// Collects reports whether p collects its containers' output.
func Collects(p ContainerProvider) bool {
	if _, ok := p.(ContainerCollector); !ok {
		return false
	}
	if c, ok := p.(ContainerCollecting); ok {
		return c.Collecting()
	}
	return true
}

// ErrContainerUncollected means an exited container's output could not be
// collected (ContainerCollector), so the host keeps the container.
var ErrContainerUncollected = errors.New("container output not collected")

// CollectContainer collects ref's output when p is a ContainerCollector,
// and is nil for any other provider. Its error is ErrContainerUncollected.
func CollectContainer(ctx context.Context, p ContainerProvider, ref ContainerRef) error {
	if !Collects(p) {
		return nil
	}
	if err := p.(ContainerCollector).Collect(ctx, ref); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrContainerUncollected, ref.ID, err)
	}
	return nil
}

// RemoveContainer is how the host removes a container: collected first
// (CollectContainer), and kept when that fails.
func RemoveContainer(ctx context.Context, p ContainerProvider, ref ContainerRef) error {
	if err := CollectContainer(ctx, p, ref); err != nil {
		return err
	}
	return p.Remove(ctx, ref)
}

// ContainerExit is what Adopt found when a recovered segment's wrapper was
// dead and its container had exited: the container's exit code, and
// whether it was stopped by a TERM the host didn't send.
type ContainerExit struct {
	Exited      bool
	ExitCode    int
	Interrupted bool
}

// ContainerBroker is what an executor sees in ExecutionContext.Container: a
// provider bound to the attempt by the host. It is nil unless the binding
// declares a container.
type ContainerBroker interface {
	// Launch creates the launch's container and returns the argv and
	// environment its wrapper runs in place of the executor's, with
	// Signals SandboxTerm. Any error means nothing may run.
	Launch(ctx context.Context, needs SandboxNeeds, launch SandboxLaunch) (SandboxPlan, error)
	// Adopt proves a recovered segment's container is the one pinned. With
	// its wrapper alive (alive), the segment is adopted as it runs. With
	// the wrapper dead, an exited container's exit is returned as the
	// segment's result, once its output is collected (ContainerCollector).
	// Anything else, a collect that fails among it, is ErrNotAdoptable.
	Adopt(ctx context.Context, alive bool) (ContainerExit, error)
}

// ContainerPin is what the host records with a segment for its container:
// the provider, the plan's digest and summary, the image digest, the
// container daemon's ID, the container's ID, the run directory and when it
// was pinned. The container must start after At. A segment whose workspace
// was at its site (Resource.Site) also records the workspace's path there
// and the site, so an attach to the Work's session goes back to it.
type ContainerPin struct {
	Provider, Digest, Summary, Image, Daemon, ID, Run string
	At                                                time.Time
	Workspace, Site                                   string `json:",omitempty"`
}

// ContainerEgress is how a provider holds a binding's network.allow. Every
// provider says which in the same words (Describe), in its plan's summary
// and in doctor, so an operator reads the same guarantee whichever
// provider runs the launch.
type ContainerEgress string

const (
	// EgressEnforced: the run's only way out is the egress gateway (the
	// gateway package or its released image), which reaches only the hosts
	// network.allow lists and refuses a name that resolves inward.
	EgressEnforced ContainerEgress = "enforced"
	// EgressAdvisory: network.allow is checked against the hosts the
	// executor declares (ContainerBinding), but the run reaches whatever
	// its own network lets it.
	EgressAdvisory ContainerEgress = "advisory"
)

// Describe is the egress as an operator reads it: "network.allow
// enforced: <how>" or "network.allow advisory: <how>", how being the
// provider's own account of the run's way out.
func (e ContainerEgress) Describe(how string) string {
	return "network.allow " + string(e) + ": " + how
}

// ContainerConfigSchema labels a binding's container config.
const ContainerConfigSchema = "core.config.container.v1"

// ContainerConfig is a binding's container config: the grant. Image must be
// pinned by digest. Write and Read are host paths the container may write
// or read ({workspace}, {run} and ~ expand); Network.Allow the outbound
// hosts; Limits its resources. Provider holds the provider's own settings.
//
// Connection names the connection the provider reaches its API with (a
// cloud account, a registry login, a cluster): one of a type the
// provider's package declares, granted by the binding's scope. The
// provider resolves its secrets with secret.resolve, in the owner's scope,
// when it needs them; the host gives them to no one else, and never to the
// run (ContainerCreated).
type ContainerConfig struct {
	Image      string   `json:"image"`
	Connection string   `json:"connection,omitempty"`
	Write      []string `json:"write,omitempty"`
	Read       []string `json:"read,omitempty"`
	Network    struct {
		Allow []string `json:"allow,omitempty"`
	} `json:"network,omitzero"`
	Limits   ContainerLimits `json:"limits,omitzero"`
	Provider json.RawMessage `json:"provider,omitempty"`
}

// ContainerLimits are a container's resource limits; zero is the
// provider's default.
type ContainerLimits struct {
	CPUs   float64 `json:"cpus,omitempty"`
	Memory string  `json:"memory,omitempty"`
	PIDs   int     `json:"pids,omitempty"`
}

var imageDigest = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// connectionID is the shape of a connection's id in config.
var connectionID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// ParseContainerConfig reads a binding's container config strictly. An
// image not pinned by digest is refused, as is a path that isn't absolute
// or a placeholder, or a negative limit.
func ParseContainerConfig(raw []byte) (ContainerConfig, error) {
	var c ContainerConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return ContainerConfig{}, fmt.Errorf("container config: %v", err)
	}
	if !imageDigest.MatchString(c.Image) {
		return ContainerConfig{}, errors.New("container image must be pinned by digest (name@sha256:<64 hex digits>)")
	}
	for _, p := range append(append([]string{}, c.Write...), c.Read...) {
		if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~/") && p != "~" && !strings.HasPrefix(p, "{workspace}") && !strings.HasPrefix(p, "{run}") {
			return ContainerConfig{}, fmt.Errorf("container path %q must be absolute, under ~, or start with {workspace} or {run}", p)
		}
	}
	if c.Connection != "" && !connectionID.MatchString(c.Connection) {
		return ContainerConfig{}, fmt.Errorf("container connection %q must be a connection id", c.Connection)
	}
	for _, h := range c.Network.Allow {
		if h == "" || strings.ContainsAny(h, "/ ") {
			return ContainerConfig{}, fmt.Errorf("network.allow entry %q must be a host or host:port", h)
		}
	}
	if c.Limits.CPUs < 0 || c.Limits.PIDs < 0 {
		return ContainerConfig{}, errors.New("container limits can't be negative")
	}
	return c, nil
}

// ContainerReason is the structured failure reason for a Launch error:
// container_running, no_capacity, sandbox_denied or sandbox_unavailable.
func ContainerReason(err error) string {
	if errors.Is(err, ErrContainerRunning) {
		return "container_running"
	}
	if errors.Is(err, ErrNoCapacity) {
		return "no_capacity"
	}
	return SandboxReason(err)
}
