package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	bitbucketext "github.com/open-conduit/open-conduit/extensions/bitbucket/pkg"
	codexext "github.com/open-conduit/open-conduit/extensions/codex/pkg"
	githubext "github.com/open-conduit/open-conduit/extensions/github/pkg"
	humanext "github.com/open-conduit/open-conduit/extensions/human/pkg"
	introspectionext "github.com/open-conduit/open-conduit/extensions/introspection/pkg"
	jiraext "github.com/open-conduit/open-conduit/extensions/jira/pkg"
	linearext "github.com/open-conduit/open-conduit/extensions/linear/pkg"
	observeext "github.com/open-conduit/open-conduit/extensions/observe/pkg"
	"github.com/open-conduit/open-conduit/extensions/pagerduty"
	posthogext "github.com/open-conduit/open-conduit/extensions/posthog/pkg"
	scheduleext "github.com/open-conduit/open-conduit/extensions/schedule/pkg"
	"github.com/open-conduit/open-conduit/extensions/sentry"
	shellext "github.com/open-conduit/open-conduit/extensions/shell/pkg"
	slackext "github.com/open-conduit/open-conduit/extensions/slack/pkg"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// runnable are the first-party extensions this binary serves as packages
// with `open-conduit extension-run <name>` (#428), by manifest name: one
// download and one build, each still its own supervised process. A
// package whose manifest command is protocol.HostRunCommand(name) runs
// here; which of them ship in the release archive is the bundling
// script's choice. Manifest is what `package-manifest` prints, which the
// daemon reads in process at discovery (hostManifest). Playbooks, when
// set, are what `package-playbooks DIR` writes beside the manifest.
var runnable = map[string]struct {
	Main      func(args []string) int
	Manifest  func() protocol.Manifest
	Playbooks func() map[string]string
}{
	"bitbucket":     {bitbucketext.Main, bitbucketext.Manifest, nil},
	"codex":         {codexext.Main, codexext.Manifest, nil},
	"github":        {githubext.Main, githubext.Manifest, nil},
	"human":         {humanext.Main, humanext.Manifest, nil},
	"introspection": {introspectionext.Main, introspectionext.Manifest, introspectionext.Playbooks},
	"jira":          {jiraext.Main, jiraext.Manifest, jiraext.Playbooks},
	"linear":        {linearext.Main, linearext.Manifest, linearext.Playbooks},
	"observe":       {observeext.Main, observeext.Manifest, nil},
	"pagerduty":     {pagerduty.Main, pagerduty.Manifest, nil},
	"posthog":       {posthogext.Main, posthogext.Manifest, nil},
	"schedule":      {scheduleext.Main, scheduleext.Manifest, nil},
	"sentry":        {sentry.Main, sentry.Manifest, nil},
	"shell":         {shellext.Main, shellext.Manifest, nil},
	"slack":         {slackext.Main, slackext.Manifest, slackext.Playbooks},
}

// hostManifest is extproc.HostManifest for this binary: discovery asks the
// host's own executable for a host-run package's manifest, and this binary
// is that executable, so it answers in process rather than running itself
// once per package at every start.
func hostManifest(name string) ([]byte, protocol.Manifest, error) {
	r, ok := runnable[name]
	if !ok {
		return nil, protocol.Manifest{}, fmt.Errorf("%w: %s", extproc.ErrNotServed, name)
	}
	m := r.Manifest()
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, protocol.Manifest{}, err
	}
	m, err = protocol.ParseManifest(raw)
	return raw, m, err
}

// extensionRun is `open-conduit extension-run <name> [args...]`: it
// serves the named first-party extension over protocol version 1 on
// stdin and stdout, or runs one of its subcommands (package-manifest,
// package-certification, package-playbooks DIR, a certification fixture). An extension that runs
// itself again (an executor's segments) does so through this same command
// (sdk.Self).
func extensionRun(args []string, stderr io.Writer) int {
	r, ok := runnable[firstArg(args)]
	if !ok {
		fmt.Fprintf(stderr, "usage: open-conduit %s NAME [ARGS...]\nNAME is one of: %s\n", protocol.HostRun, strings.Join(runnableNames(), ", "))
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "open-conduit %s: %v\n", protocol.HostRun, err)
		return 1
	}
	sdk.SetSelf(append([]string{self, protocol.HostRun}, args[0]))
	if len(args) == 3 && args[1] == "package-playbooks" {
		return writePlaybooks(r.Playbooks, args[2], stderr)
	}
	return r.Main(args[1:])
}

// writePlaybooks is `extension-run NAME package-playbooks DIR`: it writes
// the extension's playbooks into DIR as <name>.md, the package's playbooks
// directory (manifest provides.playbooks). It writes nothing for an
// extension with none.
func writePlaybooks(books func() map[string]string, dir string, stderr io.Writer) int {
	if books == nil {
		return 0
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "open-conduit %s: %v\n", protocol.HostRun, err)
		return 1
	}
	for name, text := range books() {
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(text), 0o644); err != nil {
			fmt.Fprintf(stderr, "open-conduit %s: %v\n", protocol.HostRun, err)
			return 1
		}
	}
	return 0
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func runnableNames() []string {
	out := make([]string, 0, len(runnable))
	for n := range runnable {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
