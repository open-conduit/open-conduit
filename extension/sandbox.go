package extension

import (
	"context"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
)

// A process sandbox (B09) is a launch wrapper. An executor declares what its
// process needs and hands the argv it would run to its SandboxBroker; the
// broker merges those needs with the resource's and the host's protected
// set, has the binding's provider check them against the operator's policy,
// and returns the wrapped launch. Core never interprets a sandbox: the host
// binds the provider to the attempt, as it does a ResourceProvider.

// What a provider can promise Interrupt (SandboxPlan.Signals).
const (
	// SandboxTerm: a termination signal sent to the executor's process group
	// reaches the sandboxed command, which may handle it.
	SandboxTerm = "term"
	// SandboxKill: the sandboxed command runs outside the group (in a new
	// session, as bubblewrap starts it), so a stop always kills it.
	SandboxKill = "kill"
)

var (
	// ErrSandboxDenied means a need falls outside the operator's policy.
	// Nothing ran: a need is checked against the policy and never widens it.
	ErrSandboxDenied = errors.New("sandbox need outside the policy")
	// ErrSandboxUnavailable means the declared sandbox can't be enforced
	// here (no provider, a missing binary, an unsupported platform, a failed
	// preflight, a degraded mechanism or a policy that can't be expressed).
	// Nothing ran.
	ErrSandboxUnavailable = errors.New("sandbox unavailable")
)

// SandboxNeeds are paths, hosts and sockets a process needs. Paths are
// absolute. Where they overlap, the most specific path decides.
//
//   - Read must be readable and Write writable under the policy (a Write
//     need is readable too). Beneath a Deny they are the exceptions to it.
//   - ReadOnly is readable but never writable, even beneath a Write grant
//     (a workspace's own .git/config).
//   - Deny is neither readable nor writable, whatever the policy grants.
//   - Hosts are outbound hosts (host or host:port), Sockets unix sockets.
type SandboxNeeds struct {
	Read, Write, ReadOnly, Deny []string
	Hosts, Sockets              []string
}

// Merge returns n with o's needs added.
func (n SandboxNeeds) Merge(o SandboxNeeds) SandboxNeeds {
	cat := func(a, b []string) []string { return append(append([]string(nil), a...), b...) }
	return SandboxNeeds{Read: cat(n.Read, o.Read), Write: cat(n.Write, o.Write), ReadOnly: cat(n.ReadOnly, o.ReadOnly), Deny: cat(n.Deny, o.Deny), Hosts: cat(n.Hosts, o.Hosts), Sockets: cat(n.Sockets, o.Sockets)}
}

// SandboxLaunch is the launch an executor would make: the argv its /bin/sh
// wrapper runs as "$@", the working directory, the child's environment
// (KEY=VALUE), and RunDir, the segment's private directory. WrapperEnv
// overrides the sandbox tool's own environment and never reaches the child
// (a short TMPDIR for its sockets); a key there has its child value restored
// in the clear inside the sandbox, so it must never hold a secret.
//
// Secrets and TTY are for container launches only. Secrets (KEY=VALUE) are
// the child's credentials: a container provider keeps them out of the
// container's own environment (and so out of its inspect output and the
// plan) and has the container's entry read them from a 0600 file in RunDir.
// TTY asks for an interactive container on a terminal (an operator's
// attach), started attached by the argv returned.
type SandboxLaunch struct {
	Argv       []string
	Dir        string
	Env        []string
	WrapperEnv []string
	RunDir     string
	Secrets    []string
	TTY        bool
}

// SandboxPaths name this attempt's paths, for the policy's placeholders and
// the provider's own files. They are host-set, never taken from an executor:
// Workspace ({workspace}) is the launch's working directory, Run ({run}) the
// segment's run directory, Toolbelt ({toolbelt}) the toolbelt socket.
// Records is a host-owned directory, readable but never writable inside the
// sandbox, where the provider keeps the files its plan refers to. Home, for a
// container only, is the Work's own home (ExecutionContext.ContainerHome),
// mounted read-write at the same path.
type SandboxPaths struct {
	Workspace, Run, Toolbelt, Records, Home string
}

// SandboxPolicy is the operator's policy for one launch: the binding's
// opaque sandbox config and this attempt's paths.
type SandboxPolicy struct {
	Config domain.Payload
	Paths  SandboxPaths
}

// SandboxPlan is a wrapped launch: the argv and environment to run in place
// of the executor's, what Interrupt can promise (SandboxTerm or SandboxKill),
// a one-line Summary and Data, the effective policy, for history. Digest pins
// the plan: the host computes it from Argv and Data and records it with the
// segment before the plan reaches the executor.
type SandboxPlan struct {
	Argv, Env                []string
	Digest, Signals, Summary string
	Data                     domain.Payload
}

// SandboxProvider is an optional extension that wraps launches in a process
// sandbox. Core never calls it.
//
// Plan applies the operator's policy and checks every need against it. It
// never widens the policy: a need outside it is ErrSandboxDenied, and
// anything it can't enforce is ErrSandboxUnavailable. It never changes the
// policy of a process already running.
//
// Adopt proves, when a restarted host recovers a segment, that the process
// tree running the plan is still inside the boundary the plan launched:
// root is the sandbox root's pid, recorded as it started, and parent the
// executor's wrapper that runs it. root is 0 when the root never started or
// has exited. Anything it can't prove is an error, and the segment is not
// adopted.
type SandboxProvider interface {
	Plan(ctx context.Context, owner ResourceOwner, policy SandboxPolicy, needs SandboxNeeds, launch SandboxLaunch) (SandboxPlan, error)
	Adopt(ctx context.Context, plan SandboxPlan, parent, root int) error
}

// SandboxBroker is what an executor sees in its ExecutionContext: the
// binding's provider bound to the attempt by the host, with the resource's
// needs and the host's protected set merged in. It is nil when the binding
// declares no sandbox.
type SandboxBroker interface {
	// Plan wraps one launch. Its needs are the executor's own.
	Plan(ctx context.Context, needs SandboxNeeds, launch SandboxLaunch) (SandboxPlan, error)
	// Adopt proves a recovered segment's process, the wrapper with pid
	// parent, still runs inside the sandbox it was launched in.
	Adopt(ctx context.Context, parent int) error
}

// WrapLaunch is the one call an executor makes where it builds the argv for
// its wrapper. With no broker it returns the launch unchanged. Otherwise it
// fails closed: any error means nothing may run.
func WrapLaunch(ctx context.Context, b SandboxBroker, needs SandboxNeeds, l SandboxLaunch) (SandboxPlan, error) {
	if b == nil {
		return SandboxPlan{Argv: l.Argv, Env: l.Env}, nil
	}
	p, err := b.Plan(ctx, needs, l)
	if err != nil {
		return SandboxPlan{}, err
	}
	if len(p.Argv) == 0 || p.Digest == "" {
		return SandboxPlan{}, ErrSandboxUnavailable
	}
	return p, nil
}

// AdoptSandboxed is the call an executor's Recover makes before adopting a
// live segment: with a broker, the wrapper with pid parent must still run
// inside the segment's sandbox, or the segment is ErrNotAdoptable.
func AdoptSandboxed(ctx context.Context, b SandboxBroker, parent int) error {
	if b == nil {
		return nil
	}
	if err := b.Adopt(ctx, parent); err != nil {
		return errors.Join(ErrNotAdoptable, err)
	}
	return nil
}

// SandboxReason is the structured failure reason for a WrapLaunch error:
// sandbox_denied or sandbox_unavailable.
func SandboxReason(err error) string {
	if errors.Is(err, ErrSandboxDenied) {
		return "sandbox_denied"
	}
	return "sandbox_unavailable"
}
