package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
)

const upgradeHelp = `Usage: open-conduit upgrade [--check] [--version VERSION] [--no-restart] [--json]

Installs the latest release in place of this open-conduit and the os beside
it, after checking the archive against the release's checksums.txt, then
restarts the background service so the daemon runs the new version.

  --check       only say whether a newer release is out
  --version V   install release V (for example 1.9.0) instead of the latest
  --no-restart  leave the background service running the old version
  --json        print the result as JSON

A Homebrew install is upgraded with brew: ` + upgrade.BrewCommand + `.
`

// upgradeResult is what upgrade --json prints.
type upgradeResult struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	URL       string `json:"url,omitempty"`
	Newer     bool   `json:"newer"`
	Homebrew  bool   `json:"homebrew,omitempty"`
	Installed string `json:"installed,omitempty"`
	Restarted bool   `json:"restarted,omitempty"`
}

func (o *operator) selfUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(o.errOut)
	check := fs.Bool("check", false, "")
	want := fs.String("version", "", "")
	noRestart := fs.Bool("no-restart", false, "")
	asJSON := fs.Bool("json", false, "")
	fs.Usage = func() { fmt.Fprint(o.out, upgradeHelp) }
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil || fs.NArg() != 0 {
		return usageError(o.errOut, "expected upgrade [--check] [--version VERSION] [--no-restart] [--json]")
	}
	u := o.env.Updater
	if u == nil {
		fmt.Fprintln(o.errOut, "open-conduit: upgrade is not available in this binary")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res := upgradeResult{Current: o.version, Homebrew: u.Homebrew()}
	rel, err := u.Latest(ctx)
	if err != nil && *want == "" {
		fmt.Fprintf(o.errOut, "open-conduit: upgrade: %s\n", err)
		return 1
	}
	res.Latest, res.URL = rel.Version, rel.URL
	res.Newer = upgrade.Newer(o.version, rel.Version)
	print := func() int {
		if *asJSON {
			enc := json.NewEncoder(o.out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(res)
		}
		return 0
	}
	if *check {
		if !*asJSON {
			switch {
			case res.Newer && res.Homebrew:
				fmt.Fprintf(o.out, "open-conduit %s; %s is out (%s)\nrun %s\n", o.version, rel.Version, rel.URL, upgrade.BrewCommand)
			case res.Newer:
				fmt.Fprintf(o.out, "open-conduit %s; %s is out (%s)\nrun open-conduit upgrade\n", o.version, rel.Version, rel.URL)
			default:
				fmt.Fprintf(o.out, "open-conduit %s is up to date (latest release %s)\n", o.version, rel.Version)
			}
		}
		return print()
	}
	target := *want
	if target == "" {
		if !res.Newer {
			if !*asJSON {
				fmt.Fprintf(o.out, "open-conduit %s is up to date (latest release %s)\n", o.version, rel.Version)
			}
			return print()
		}
		target = rel.Version
	}
	if err := u.Install(ctx, target); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: upgrade: %s\n", err)
		return 1
	}
	res.Installed = target
	if !*asJSON {
		fmt.Fprintf(o.out, "installed open-conduit %s in %s\n", target, filepath.Dir(u.Executable))
	}
	if *noRestart {
		if !*asJSON {
			fmt.Fprintln(o.out, "a running daemon keeps the old version until it restarts")
		}
		return print()
	}
	restarted, err := o.restartIfRunning()
	if err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: upgrade: restarting the service: %s\n", err)
		return 1
	}
	res.Restarted = restarted
	if !*asJSON {
		if restarted {
			fmt.Fprintf(o.out, "restarted the background service on %s\n", target)
		} else {
			fmt.Fprintln(o.out, "no background service is running: restart open-conduit serve, if it runs, to use the new version")
		}
	}
	return print()
}

// restartIfRunning restarts the background service when it is installed
// and running, and says whether it did.
func (o *operator) restartIfRunning() (bool, error) {
	h := o.env.Service
	if h == nil || (h.GOOS != "darwin" && h.GOOS != "linux") {
		return false, nil
	}
	s, err := o.serviceQuery(h)
	if err != nil || !s.Installed || !s.Running {
		return false, err
	}
	return true, o.serviceRestart(h)
}

// ServiceControl lets the web view restart the background service once an
// update is installed.
type ServiceControl struct{ o *operator }

// NewServiceControl is a ServiceControl for host, reading HOME and the
// other locations from getenv.
func NewServiceControl(host *ServiceHost, getenv func(string) string) *ServiceControl {
	return &ServiceControl{o: &operator{out: io.Discard, errOut: io.Discard, env: Env{Getenv: getenv, Service: host}}}
}

// Running says whether the background service is installed and running.
func (c *ServiceControl) Running() bool {
	h := c.o.env.Service
	if h == nil || (h.GOOS != "darwin" && h.GOOS != "linux") {
		return false
	}
	s, err := c.o.serviceQuery(h)
	return err == nil && s.Installed && s.Running
}

// Restart restarts the background service, which may be the process
// calling it.
func (c *ServiceControl) Restart() error {
	_, err := c.o.restartIfRunning()
	return err
}
