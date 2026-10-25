package cli

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/internal/cli/runview"
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
		f := newFlags("log", "range")
		pos, err := f.parse(args[1:])
		if err != nil || len(pos) != 1 {
			return usageError(o.errOut, "expected runs log ID")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		run, err := c.Run(cx, pos[0])
		if err != nil {
			return o.emit(nil, err)
		}
		a, ok := runview.LogArtifact(run)
		if !ok {
			fmt.Fprintln(o.errOut, "open-conduit: run has no artifacts")
			return 1
		}
		if err = c.ArtifactContent(cx, a.ID, f.get("range"), o.out); err != nil {
			return o.emit(nil, err)
		}
		return 0
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
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	d, err := c.Doctor(cx, *checkSecrets)
	if err != nil {
		return o.emit(nil, err)
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
