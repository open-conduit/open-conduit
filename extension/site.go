package extension

import (
	"context"
	"errors"
	"strings"
	"time"
)

// A site is a machine a container provider runs launches on, when that
// machine isn't the host. A container provider that chooses where a launch
// runs (one remote, a pool of them) implements ContainerPlacer: it places
// an execution before anything is acquired for it, and runs commands at
// the placement. The host hands the placement to the attempt's resource
// provider as a Site (ResourceAcquisition.Site), so a workspace can be made
// where the run executes rather than on the host's disk and copied there.
//
// The two providers never learn each other's business: the container
// provider knows nothing of what is run at its site (git, say), and the
// resource provider nothing of how it is reached (ssh, a pool). The host
// composes them.

// ErrSiteUnavailable means a command couldn't be run at a site: the site
// can't be reached, or its provider can't name it any more. A command that
// ran and failed is not an error but a SiteResult with its exit code.
var ErrSiteUnavailable = errors.New("site unavailable")

// SiteCommand is one command to run at a site: its argv, the directory it
// runs in ("" for the site's home), its environment (nothing of the site's
// own but what its provider always sets, such as PATH), its stdin and how
// long it may take (0 for the provider's default). Toolbelt, set only by
// the host for a command run on an execution's behalf, is that execution's
// toolbelt socket, which the provider makes reachable at the same path for
// the command, as it does for a launch, so git there signs in through the
// toolbelt's transport and no credential crosses to the site.
type SiteCommand struct {
	Argv     []string
	Dir      string
	Env      []string
	Stdin    []byte
	Timeout  time.Duration
	Toolbelt string
}

// SiteResult is a command's exit code and output. A provider may bound
// the output it returns; Truncated says it did.
type SiteResult struct {
	ExitCode       int
	Stdout, Stderr []byte
	Truncated      bool
}

// Site is a placement a resource provider may act at. ID names it stably,
// for the provider's own records (a lease kept at a site records it). Run
// runs one command there, within the same boundary and grant as a launch
// at that site.
type Site interface {
	ID() string
	Run(ctx context.Context, cmd SiteCommand) (SiteResult, error)
}

// Placement is where a container provider placed an execution: its ID,
// stable for as long as the machine is the same one, and the Daemon that
// a container created there reports (ContainerCreated.Daemon), so the host
// can prove the launch ran where the resource was made. Summary is a line
// for history.
type Placement struct {
	ID, Daemon, Summary string
}

// ContainerPlacer is an optional ContainerProvider capability: a provider
// that runs launches on a machine of its own choosing.
//
//   - Place chooses the machine for an execution under a binding's
//     container policy (ContainerPolicy.Paths is empty: nothing is acquired
//     yet). Prefer is the placement the Work last ran at, "" for none: a
//     provider places there when it still can, so the Work's workspace is
//     reused; when it can't (the machine is gone) it places elsewhere. A
//     provider that can't place now refuses with ErrSandboxUnavailable.
//   - SiteRun runs a command at a placement Place gave, for any binding
//     that uses the provider, after the execution too (a reconcile). A
//     placement the provider can't name or reach is ErrSiteUnavailable.
//
// Plan then plans on ContainerPolicy.Placement, and the container Create
// makes there reports the placement's Daemon.
type ContainerPlacer interface {
	Place(ctx context.Context, owner ResourceOwner, policy ContainerPolicy, prefer string) (Placement, error)
	SiteRun(ctx context.Context, placement string, cmd SiteCommand) (SiteResult, error)
}

// SiteID is the ID the host gives the site of a placement: the container
// provider's name and the placement's own ID, so two providers' placements
// never share one. SplitSiteID reverses it.
func SiteID(provider, placement string) string { return provider + "/" + placement }

// SplitSiteID returns the provider and placement a SiteID names.
func SplitSiteID(id string) (provider, placement string, ok bool) {
	provider, placement, ok = strings.Cut(id, "/")
	return provider, placement, ok && provider != "" && placement != ""
}

// PlacedSite is a placement bound as a Site. The host makes one; a
// resource provider only uses it.
type PlacedSite struct {
	Provider string
	Placer   ContainerPlacer
	At       Placement
	// Toolbelt, when set, is given to every command (SiteCommand.Toolbelt).
	Toolbelt string
}

func (s PlacedSite) ID() string { return SiteID(s.Provider, s.At.ID) }

func (s PlacedSite) Run(ctx context.Context, cmd SiteCommand) (SiteResult, error) {
	if len(cmd.Argv) == 0 {
		return SiteResult{}, errors.New("site command: empty argv")
	}
	cmd.Toolbelt = s.Toolbelt
	return s.Placer.SiteRun(ctx, s.At.ID, cmd)
}
