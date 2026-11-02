package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	codexext "github.com/open-conduit/open-conduit/extensions/codex/pkg"
	githubext "github.com/open-conduit/open-conduit/extensions/github/pkg"
	"github.com/open-conduit/open-conduit/extensions/pagerduty"
	posthogext "github.com/open-conduit/open-conduit/extensions/posthog/pkg"
	"github.com/open-conduit/open-conduit/extensions/sentry"
	shellext "github.com/open-conduit/open-conduit/extensions/shell/pkg"
	slackext "github.com/open-conduit/open-conduit/extensions/slack/pkg"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// runnable are the first-party extensions this binary serves as packages
// with `open-conduit extension-run <name>` (#428), by manifest name: one
// download and one build, each still its own supervised process. A
// package whose manifest command is protocol.HostRunCommand(name) runs
// here; which of them ship in the release archive is the bundling
// script's choice.
var runnable = map[string]func(args []string) int{
	"codex":     codexext.Main,
	"github":    githubext.Main,
	"pagerduty": pagerduty.Main,
	"posthog":   posthogext.Main,
	"sentry":    sentry.Main,
	"shell":     shellext.Main,
	"slack":     slackext.Main,
}

// extensionRun is `open-conduit extension-run <name> [args...]`: it
// serves the named first-party extension over protocol version 1 on
// stdin and stdout, or runs one of its subcommands (package-manifest,
// package-certification, a certification fixture). An extension that runs
// itself again (an executor's segments) does so through this same command
// (sdk.Self).
func extensionRun(args []string, stderr io.Writer) int {
	if len(args) == 0 || runnable[args[0]] == nil {
		fmt.Fprintf(stderr, "usage: open-conduit %s NAME [ARGS...]\nNAME is one of: %s\n", protocol.HostRun, strings.Join(runnableNames(), ", "))
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "open-conduit %s: %v\n", protocol.HostRun, err)
		return 1
	}
	sdk.SetSelf(append([]string{self, protocol.HostRun}, args[0]))
	return runnable[args[0]](args[1:])
}

func runnableNames() []string {
	out := make([]string, 0, len(runnable))
	for n := range runnable {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
