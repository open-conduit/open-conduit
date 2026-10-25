package extension

import (
	"context"
	"encoding/json"
	"errors"
)

// Setup is optional for an extension: it adds what the extension needs to a
// local config (`open-conduit init <type>`). Core only hosts it: it parses
// its own flags (--config, --force, --dry-run), passes every other argument
// through, merges the returned connections and scope into the config file,
// validates the result offline against the daemon's registrations and saves
// it. Everything service-specific (which questions to ask, which URLs to
// open, how to verify credentials and what the scope looks like) belongs to
// the extension. A Setup never writes the config itself and never sees a
// stored secret again after handing it to the host.
type Setup interface {
	// SetupInfo names the setup (`init <Type>`) and summarizes it for the
	// `init` listing.
	SetupInfo() SetupInfo
	// Setup runs the interactive or flag-driven flow. args are the
	// arguments after the type, minus the host's own flags.
	Setup(ctx context.Context, host SetupHost, args []string) (SetupResult, error)
}

type SetupInfo struct {
	Type    string
	Summary string
	// Usage is a one-line flag synopsis for help output.
	Usage string
}

// SetupHost is what core supplies to a Setup. Implementations are the CLI
// (a terminal) and test doubles.
type SetupHost interface {
	// Printf writes guidance to the operator.
	Printf(format string, args ...any)
	// Prompt asks one question. A secret prompt does not echo. An empty
	// answer returns def. ErrSetupNoInput means no interactive input exists.
	Prompt(question string, p Prompt) (string, error)
	// Choose asks the operator to pick one of options and returns its index.
	Choose(question string, options []string) (int, error)
	// OpenURL opens a browser at url (or prints it when none is available).
	OpenURL(url string) error
	// StoreSecret stores a value through the host's secret store (keyring,
	// else the owner-only secrets file) and returns its reference, for
	// example "keyring:slack-bot-token". In a dry run it stores nothing and
	// returns the reference it would use.
	StoreSecret(name, value string) (string, error)
	// LookPath reports whether an executable is on PATH.
	LookPath(name string) (string, bool)
	// Run runs a command attached to the operator's terminal in dir.
	Run(ctx context.Context, dir, name string, args ...string) error
	// Config is the existing config document (nil when there is none), so
	// a setup can reuse connections or pick a free scope ID.
	Config() []byte
	// Cwd is the operator's working directory; StateDir is Open Conduit's
	// private state directory (for files a setup must prepare).
	Cwd() string
	StateDir() string
	// DryRun reports `--dry-run`: no secrets are stored and nothing is saved.
	DryRun() bool
}

type Prompt struct {
	Default string
	Secret  bool
}

// SetupResult is merged by the host. Connections are added when no
// connection with the same id exists (an existing one is kept); the scope
// replaces an existing scope with the same id only with --force.
type SetupResult struct {
	Connections []json.RawMessage
	Scope       json.RawMessage
	// Summary lines are printed after the config is saved; Next lists
	// follow-up commands.
	Summary []string
	Next    []string
}

var (
	ErrSetupNoInput = errors.New("no interactive input")
	// ErrSetupUsage wraps an invalid-argument refusal; the host reports it
	// as a usage error (exit status 2) with the wrapped message.
	ErrSetupUsage = errors.New("setup usage")
)
