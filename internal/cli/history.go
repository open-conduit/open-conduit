package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

var runFilters = []string{"scope-id", "work-id", "state", "executor-binding", "executor", "work-kind", "outcome"}

// runs implements the unified history: list/get JSON records, a table view
// and log access, identical for every executor.
func (o *operator) runs(args []string) int {
	if len(args) == 0 {
		return usageError(o.errOut, "missing runs subcommand")
	}
	switch args[0] {
	case "list", "get":
		return o.listOrGet(args, runFilters, func(c *client.Client, q url.Values) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.Runs(cx, q)
		}, func(c *client.Client, id string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.Run(cx, id)
		})
	case "table":
		f := newFlags("table", append(runFilters, "limit", "cursor")...)
		if pos, err := f.parse(args[1:]); err != nil || len(pos) != 0 {
			return usageError(o.errOut, "invalid runs table flags")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		page, err := c.Runs(cx, f.query(runFilters...))
		if err != nil {
			return o.emit(nil, err)
		}
		now := time.Now()
		fmt.Fprintln(o.out, runview.Header)
		for _, r := range page.Items {
			fmt.Fprintln(o.out, runview.Row(r, now))
		}
		if page.NextCursor != "" {
			fmt.Fprintf(o.out, "next cursor: %s\n", page.NextCursor)
		}
		return 0
	case "log":
		return o.runLog(args[1:])
	}
	return usageError(o.errOut, "expected runs list|get|table|log")
}

// doctor prints health checks and exits 1 when any check fails.
func (o *operator) doctor(args []string) int {
	fs := newFlags("doctor")
	checkSecrets := fs.fs.Bool("check-secrets", false, "")
	asJSON := fs.fs.Bool("json", false, "")
	if pos, err := fs.parse(args); err != nil || len(pos) != 0 {
		return usageError(o.errOut, "invalid doctor flags")
	}
	cx, cancel := ctx()
	defer cancel()
	c, _, err := o.client()
	var d client.Doctor
	if err == nil {
		d, err = c.Doctor(cx, *checkSecrets)
	}
	if nr, ok := daemonDown(err); ok {
		d, err = o.offlineDoctor(cx, nr), nil
	}
	if err != nil {
		return o.emit(nil, err)
	}
	if svc, ok := o.serviceCheck(); ok {
		// Offline, it follows the daemon's own check.
		at := len(d.Checks)
		if len(d.Checks) > 0 && d.Checks[0].Name == "daemon" {
			at = 1
		}
		d.Checks = append(d.Checks[:at], append([]client.Check{svc}, d.Checks[at:]...)...)
		if svc.Status == "warn" && d.State == "ok" {
			d.State = "warn"
		}
	}
	if *asJSON {
		if code := o.emit(d, nil); code != 0 {
			return code
		}
	} else {
		w := tabwriter.NewWriter(o.out, 0, 2, 2, ' ', 0)
		for _, ch := range d.Checks {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", strings.ToUpper(ch.Status), ch.Name, ch.Code, ch.Detail)
			for _, item := range ch.Items {
				fmt.Fprintf(w, "\t\t\t  %s\n", item)
			}
		}
		_ = w.Flush()
		fmt.Fprintf(o.out, "overall: %s\n", d.State)
	}
	if d.State == "fail" {
		return 1
	}
	return 0
}

// offlineDoctor runs the checks that need no daemon (the config file, its
// bindings, the agent CLIs) and reports the daemon's own as skipped.
func (o *operator) offlineDoctor(cx context.Context, nr *notRunningError) client.Doctor {
	d := client.Doctor{Checks: []client.Check{{Name: "daemon", Status: "warn", Code: "not_running", Detail: nr.Error()}}}
	path := o.configPath("")
	if data, err := os.ReadFile(path); err != nil {
		d.Checks = append(d.Checks, client.Check{Name: "config", Status: "fail", Code: "missing", Detail: fmt.Sprintf("no config at %s; create one with open-conduit init", path)})
	} else if o.env.OfflineChecks != nil {
		// The entrypoint's checks end with the daemon's own, as skipped.
		d.Checks = append(d.Checks, o.env.OfflineChecks(cx, path, data)...)
	} else if !json.Valid(data) {
		d.Checks = append(d.Checks, client.Check{Name: "config", Status: "fail", Code: "invalid", Detail: path + " is not valid JSON"})
	}
	if last := d.Checks[len(d.Checks)-1]; last.Name != "skipped" {
		d.Checks = append(d.Checks, client.Check{Name: "skipped", Status: "info", Code: "daemon_not_running", Detail: "the checks that need a running daemon"})
	}
	d.State = "warn"
	for _, ch := range d.Checks {
		if ch.Status == "fail" {
			d.State = "fail"
		}
	}
	return d
}

// tui opens the interactive run history view on the controlling terminal.
func (o *operator) tui(args []string) int {
	if len(args) != 0 {
		return usageError(o.errOut, "tui takes no arguments")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	if o.env.TUI == nil {
		fmt.Fprintln(o.errOut, "open-conduit: tui is not available in this binary")
		return 1
	}
	if err = o.env.TUI(c); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: tui: %s\n", err)
		return 1
	}
	return 0
}

// WebRequest is what open-conduit web serves: the view over Client on the
// loopback Addr, named Host, with its sign-in URLs written to Out. SignIn
// is the file that keeps the sign-in across restarts. With URLOnly it only
// writes the sign-in URLs, and Client is nil.
type WebRequest struct {
	Client  *client.Client
	Addr    string
	Host    string
	SignIn  string
	URLOnly bool
	Out     io.Writer
}

// WebSignInPath is where the web view keeps its sign-in, beside the
// operator token.
func WebSignInPath(stateDir string) string {
	return filepath.Join(stateDir, "private", "web.signin")
}

// web serves the browser view until interrupted; web url prints its
// sign-in URLs.
func (o *operator) web(args []string) int {
	f := newFlags("web", "addr", "host")
	rest, err := f.parse(args)
	urlOnly := len(rest) == 1 && rest[0] == "url"
	if err != nil || (len(rest) != 0 && !urlOnly) {
		return usageError(o.errOut, "web takes only url, --addr HOST:PORT and --host NAME")
	}
	addr, host := f.get("addr"), f.get("host")
	if addr == "" {
		addr = o.env.Getenv("OPEN_CONDUIT_WEB_ADDR")
	}
	if host == "" {
		host = o.env.Getenv("OPEN_CONDUIT_WEB_HOST")
	}
	if o.env.Web == nil {
		fmt.Fprintln(o.errOut, "open-conduit: web is not available in this binary")
		return 1
	}
	r := WebRequest{Addr: addr, Host: host, SignIn: WebSignInPath(o.stateDir()), URLOnly: urlOnly, Out: o.out}
	if !urlOnly {
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		r.Client = c
	}
	if err = o.env.Web(r); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: web: %s\n", err)
		return 1
	}
	return 0
}
