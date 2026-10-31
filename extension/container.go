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
// start and attach to it, and Adopt, in Recover. Stopping, reconciling and
// removing containers are the host's (StopContainer, ReconcileContainers,
// RemoveContainers): executors never see them, and there is no Signal call
// for executors.

// The labels every container the host creates carries.
const (
	// ContainerLabelHost names the Open Conduit host (a hash of its state
	// directory), so two daemons on one container daemon never touch each
	// other's containers.
	ContainerLabelHost = "dev.open-conduit.host"
	// ContainerLabelExecution is the execution the container belongs to.
	ContainerLabelExecution = "dev.open-conduit.execution"
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
)

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
type ContainerPolicy struct {
	Config domain.Payload
	Paths  SandboxPaths
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
//     with no ExecutionID lists every container of the host. A listing that
//     fails is an error, never an empty list.
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
	// segment's result. Anything else is ErrNotAdoptable.
	Adopt(ctx context.Context, alive bool) (ContainerExit, error)
}

// ContainerPin is what the host records with a segment for its container:
// the provider, the plan's digest and summary, the image digest, the
// container daemon's ID, the container's ID, the run directory and when it
// was pinned. The container must start after At.
type ContainerPin struct {
	Provider, Digest, Summary, Image, Daemon, ID, Run string
	At                                                time.Time
}

// ContainerConfigSchema labels a binding's container config.
const ContainerConfigSchema = "core.config.container.v1"

// ContainerConfig is a binding's container config: the grant. Image must be
// pinned by digest. Write and Read are host paths the container may write
// or read ({workspace}, {run} and ~ expand); Network.Allow the outbound
// hosts; Limits its resources. Provider holds the provider's own settings.
type ContainerConfig struct {
	Image   string   `json:"image"`
	Write   []string `json:"write,omitempty"`
	Read    []string `json:"read,omitempty"`
	Network struct {
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
// container_running, sandbox_denied or sandbox_unavailable.
func ContainerReason(err error) string {
	if errors.Is(err, ErrContainerRunning) {
		return "container_running"
	}
	return SandboxReason(err)
}
