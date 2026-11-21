package extension

import (
	"context"
	"encoding/json"
	"errors"
)

// SetupProbe is a named, read-only check a setup session's agent may run
// (issue #127): whether Claude Code is signed in, which GitHub accounts gh
// knows. A probe takes declared arguments, never a command line or a
// secret reference, runs without a shell, and returns safe output: no
// secret, no account detail it doesn't need. Each one lives with the code
// that knows its domain; its full name is its owner's name, a dot and Name
// ("claude-code.status").
type SetupProbe struct {
	Name    string
	Summary string
	Args    []ProbeArg
	Run     func(ctx context.Context, args map[string]string) (ProbeResult, error)
}

// ProbeArg is one argument a probe takes.
//
// Connection marks an argument whose value is a connection ID. The host then
// runs the probe only for a session in a scope whose authorization grants
// that connection, when the connection is of a type the probe's owner
// declares, and hands the probe the connection for that one run
// (ProbeConnectionFrom). The agent names the connection and never sees its
// credential. Only an applied connection qualifies: one still in a change
// set could point a stored credential at a URL the agent chose.
type ProbeArg struct {
	Name, Summary string
	Required      bool
	Connection    bool
}

// ProbeConnection is the connection a probe was run through: its nonsecret
// config, the names of its secrets, and a resolver that works only while
// this run lasts.
type ProbeConnection struct {
	ID, Type string
	Config   json.RawMessage
	Secrets  []string
	Secret   func(ctx context.Context, name string) (string, error)
}

type probeConnectionKey struct{}

// WithProbeConnection is ctx carrying the connection a probe runs through.
func WithProbeConnection(ctx context.Context, c ProbeConnection) context.Context {
	return context.WithValue(ctx, probeConnectionKey{}, c)
}

// ProbeConnectionFrom is the connection the host granted this probe run, if
// the run named one through a Connection argument.
func ProbeConnectionFrom(ctx context.Context) (ProbeConnection, bool) {
	c, ok := ctx.Value(probeConnectionKey{}).(ProbeConnection)
	return c, ok
}

// ProbeResult is a probe's output and the command it stands for, shown to
// the operator ("gh auth status").
type ProbeResult struct {
	Command string
	Output  any
}

// SetupProbes is implemented by an extension or executor that offers probes
// to setup sessions.
type SetupProbes interface {
	SetupProbes() []SetupProbe
}

// ErrProbeInput refuses a probe's arguments; its text says why and is shown
// to the agent, so it must be fixed and secret-free.
var ErrProbeInput = errors.New("invalid probe input")
