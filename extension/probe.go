package extension

import (
	"context"
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
type ProbeArg struct {
	Name, Summary string
	Required      bool
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
