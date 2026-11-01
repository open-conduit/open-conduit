package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// DevRequest is `open-conduit dev`: run the package in Dir against a
// throwaway daemon whose config and state live in a temporary directory,
// never the operator's.
type DevRequest struct {
	Dir string
	// Secrets are the generated scope's secrets: each a value, or a
	// reference (env:VAR) resolved from dev's environment.
	Secrets []DevSecret
	// Entries are the configs of the generated entries, by extension type.
	Entries map[string]json.RawMessage
	// Agent is the claude-code binary the scope's binding runs (claude on
	// PATH when empty).
	Agent string
	// WebAddr is where the web view listens (a free local port when empty).
	WebAddr string
	// Watch is how often Dir is checked for changes (500ms when zero).
	Watch time.Duration
	// Out receives what dev reports: the URLs, each rebuild, the stop.
	Out io.Writer
}

// DevSecret is one --secret: Value, or Ref when given as env:VAR.
type DevSecret struct{ Name, Value, Ref string }

// devHelp is dev's usage.
const devHelp = `Usage: open-conduit dev [DIR] [--secret NAME=VALUE|NAME=env:VAR]... [--entry TYPE=JSON]... [--agent PATH] [--web-addr 127.0.0.1:PORT]
Builds the extension package in DIR (default .), starts a daemon on a config and state directory of
its own in a temporary directory, with the scope extensiontest generates from the package's manifest
(an entry per extension type, a claude-code binding granted its operations, its routes and a manual
route), serves the web view and prints its URL. When the package's files change it rebuilds the
package and restarts the daemon on the same state; Ctrl-C stops everything and removes the directory.
  --secret NAME=VALUE    a secret every generated entry selects (NAME=env:VAR reads it from the environment)
  --entry TYPE=JSON      the config of the generated entry for TYPE ({} when not given)
  --agent PATH           the Claude Code CLI the binding runs (default: claude on PATH)
  --web-addr ADDR        where the web view listens (default: a free port on 127.0.0.1)
`

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func (o *operator) dev(args []string) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(o.out, devHelp)
		return 0
	}
	f := newFlags("dev", "agent", "web-addr")
	var secrets, entries repeated
	f.fs.Var(&secrets, "secret", "")
	f.fs.Var(&entries, "entry", "")
	watch := f.fs.Duration("watch-interval", 500*time.Millisecond, "")
	pos, err := f.parse(args)
	if err != nil || len(pos) > 1 || *watch <= 0 {
		return usageError(o.errOut, "expected dev [DIR] [--secret NAME=VALUE]... [--entry TYPE=JSON]... [--agent PATH] [--web-addr ADDR]")
	}
	r := DevRequest{Dir: o.abs("."), Agent: f.get("agent"), WebAddr: f.get("web-addr"), Watch: *watch, Out: o.out}
	if len(pos) == 1 {
		r.Dir = o.abs(pos[0])
	}
	if r.Agent != "" {
		r.Agent = o.abs(r.Agent)
	}
	for _, s := range secrets {
		name, value, ok := strings.Cut(s, "=")
		if !ok || name == "" {
			return usageError(o.errOut, "--secret takes NAME=VALUE or NAME=env:VAR")
		}
		sec := DevSecret{Name: name, Value: value}
		if ref, isRef := strings.CutPrefix(value, "env:"); isRef {
			if ref == "" {
				return usageError(o.errOut, "--secret "+name+"=env: names no variable")
			}
			sec = DevSecret{Name: name, Ref: value}
		}
		r.Secrets = append(r.Secrets, sec)
	}
	for _, e := range entries {
		typ, raw, ok := strings.Cut(e, "=")
		if !ok || typ == "" || !json.Valid([]byte(raw)) {
			return usageError(o.errOut, "--entry takes TYPE=JSON, the entry's config as a JSON object")
		}
		if r.Entries == nil {
			r.Entries = map[string]json.RawMessage{}
		}
		r.Entries[typ] = json.RawMessage(raw)
	}
	if o.env.Extensions == nil || o.env.Extensions.Dev == nil {
		fmt.Fprintln(o.errOut, "open-conduit: dev is not available in this binary")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := o.env.Extensions.Dev(ctx, r); err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: dev: %s\n", err)
		return 1
	}
	return 0
}
