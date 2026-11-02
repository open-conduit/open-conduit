package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/open-conduit/open-conduit/client"
)

// Operator controls added for engineering operation (03-05). Each is a thin
// wrapper over a management endpoint; none has a private path to state.

// toolbeltVars never reach an attached session: it is the operator's.
var toolbeltVars = []string{"OS_ENDPOINT=", "OS_TOKEN=", "OS_TOKEN_FILE="}

// RunAttached runs an attach command with the terminal, from the operator's
// own environment minus toolbelt credentials.
func RunAttached(argv []string, dir string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("empty attach command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		keep := true
		for _, p := range toolbeltVars {
			if strings.HasPrefix(kv, p) {
				keep = false
			}
		}
		if keep {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// NewKey is a fresh idempotency key for an operator mutation.
func NewKey() string { return randomKey() }

// revised runs a revisioned control against the Work's current revision,
// refetching when a concurrent change made it stale (the host loop moves
// Work on its own).
func revised(c *client.Client, id string, fn func(cx context.Context, w client.Work) error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		cx, cancel := ctx()
		var w client.Work
		if w, err = c.Work(cx, id); err == nil {
			err = fn(cx, w)
		}
		cancel()
		var api *client.Error
		if !errors.As(err, &api) || api.Code != "revision_conflict" {
			return err
		}
	}
	return err
}

// Attach holds the Work, fetches its attach command, runs it and releases
// the hold it placed when the session ends. Input that arrives meanwhile
// queues; the next attempt continues the attached session. keep leaves the
// hold in place.
func Attach(c *client.Client, id string, keep bool, run func(argv []string, dir string) error, notice io.Writer) error {
	// Only a hold this attach placed is released afterwards; an operator's
	// earlier hold stays.
	placed := false
	if err := revised(c, id, func(cx context.Context, w client.Work) error {
		if w.Hold != nil {
			return nil
		}
		_, err := c.HoldWork(cx, id, w.Revision, "operator attach", randomKey())
		placed = err == nil
		return err
	}); err != nil {
		return err
	}
	release := func() error {
		if !placed {
			return nil
		}
		if keep {
			fmt.Fprintf(notice, "open-conduit: %s stays held; release it with `open-conduit work release %s`\n", id, id)
			return nil
		}
		return revised(c, id, func(cx context.Context, w client.Work) error {
			if w.Hold == nil {
				return nil
			}
			_, err := c.ReleaseWork(cx, id, w.Revision, "operator detached", randomKey())
			return err
		})
	}
	cx, cancel := ctx()
	spec, err := c.AttachWork(cx, id, randomKey())
	cancel()
	if err != nil {
		return errors.Join(err, release())
	}
	fmt.Fprintf(notice, "open-conduit: attaching to %s (%s); new events queue until you exit\n", id, spec.Executor)
	// Work that ran in a container attaches in one: this client makes it
	// rather than another process finding the daemon again.
	if containerAttachCommand(spec.Argv) {
		runErr := ContainerAttach(c, id, func(argv []string) error { return run(argv, "") }, notice)
		return errors.Join(runErr, release())
	}
	runErr := run(spec.Argv, spec.Dir)
	return errors.Join(runErr, release())
}

// containerAttachCommand reports whether an attach command is `<binary>
// container attach <work>`.
func containerAttachCommand(argv []string) bool {
	return len(argv) == 4 && argv[1] == "container" && argv[2] == "attach"
}

// ContainerAttach is `open-conduit container attach ID`, for held Work
// whose session ran in a container: the daemon makes an attach container
// (under the binding's current policy, with the Work's home and
// workspace), run starts it attached on the terminal (the docker CLI, in
// the operator's own environment), and the container,
// its gateway and network are removed when it exits. Nothing runs on the
// host but the docker CLI.
func ContainerAttach(c *client.Client, id string, run func(argv []string) error, notice io.Writer) error {
	cx, cancel := ctx()
	a, err := c.ContainerAttach(cx, id, randomKey())
	cancel()
	if err != nil {
		return err
	}
	// An interrupt reaches the container through the terminal; this
	// process stays to remove it.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	runErr := run(a.Argv)
	cx, cancel = ctx()
	defer cancel()
	if err := c.ContainerDetach(cx, id, a.Ref, randomKey()); err != nil {
		fmt.Fprintf(notice, "open-conduit: the attach container %s was not removed: %v\n", a.Ref, err)
		return errors.Join(runErr, err)
	}
	return runErr
}

func (o *operator) workControl(sub string, args []string) int {
	switch sub {
	case "hold", "release":
		return o.control(args, workRevision, func(c *client.Client, id, rev, reason, key string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			if sub == "hold" {
				return c.HoldWork(cx, id, rev, reason, key)
			}
			return c.ReleaseWork(cx, id, rev, reason, key)
		})
	case "rebind":
		f := newFlags("rebind", "revision", "binding", "reason", "key")
		pos, err := f.parse(args)
		if err != nil || len(pos) != 1 || f.get("binding") == "" {
			return usageError(o.errOut, "expected work rebind ID --binding B [--revision N]")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		key := f.key()
		return o.emit(withRevision(c, pos[0], f.get("revision"), workRevision, func(rev string) (any, error) {
			cx, cancel := ctx()
			defer cancel()
			return c.RebindWork(cx, pos[0], rev, f.get("binding"), f.get("reason"), key)
		}))
	case "attach":
		f := newFlags("attach")
		keep := f.fs.Bool("keep-held", false, "")
		pos, err := f.parse(args)
		if err != nil || len(pos) != 1 {
			return usageError(o.errOut, "expected work attach ID [--keep-held]")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		run := o.env.Attach
		if run == nil {
			stdin := o.env.Stdin
			if stdin == nil {
				stdin = os.Stdin
			}
			run = func(argv []string, dir string) error { return RunAttached(argv, dir, stdin, o.out, o.errOut) }
		}
		if err := Attach(c, pos[0], *keep, run, o.errOut); err != nil {
			return o.emit(nil, err)
		}
		return 0
	}
	return -1
}

// container is `container attach ID` (the hidden `container run` is the
// binary's own, never reached here).
func (o *operator) container(args []string) int {
	if len(args) == 0 || args[0] != "attach" {
		return usageError(o.errOut, "expected container attach ID")
	}
	f := newFlags("container attach")
	pos, err := f.parse(args[1:])
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected container attach ID")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	run := o.env.Attach
	if run == nil {
		stdin := o.env.Stdin
		if stdin == nil {
			stdin = os.Stdin
		}
		run = func(argv []string, dir string) error { return RunAttached(argv, dir, stdin, o.out, o.errOut) }
	}
	if err := ContainerAttach(c, pos[0], func(argv []string) error { return run(argv, "") }, o.errOut); err != nil {
		return o.emit(nil, err)
	}
	return 0
}

// reconcileScope requests an immediate reconcile; without --revision the
// scope's current revision is used.
func (o *operator) reconcileScope(args []string) int {
	f := newFlags("reconcile", "revision", "key")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected scopes reconcile ID [--revision N]")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	rev := f.get("revision")
	if rev == "" {
		s, err := c.Scope(cx, pos[0])
		if err != nil {
			return o.emit(nil, err)
		}
		rev = s.Revision
	}
	return o.emit(c.ReconcileScope(cx, pos[0], rev, f.key()))
}

// claimRef is v1's claim syntax: [scope#]N, also #N.
var claimRef = regexp.MustCompile(`^(?:([a-z][a-z0-9_-]{0,62})?#)?([0-9]{1,10})$`)

// ParseClaim splits "scope#12", "#12" or "12".
func ParseClaim(s string) (scope string, number int, ok bool) {
	m := claimRef.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return m[1], n, true
}

// ClaimAction is the scope action extensions offer for claiming an item
// by hand.
const ClaimAction = "claim"

// InvokeNamed invokes action id on t through the provider named provider
// (its full name or its last path segment), or the only provider that
// offers the action when provider is empty. The extension authorises the
// call and writes the Event; this only addresses it.
func InvokeNamed(cx context.Context, c *client.Client, t client.ActionTarget, provider, id, input, key string) (client.ActionResult, error) {
	actions, err := c.Actions(cx, t)
	if err != nil {
		return client.ActionResult{}, err
	}
	var found []client.Action
	for _, a := range actions {
		if a.ID != id {
			continue
		}
		if provider == "" || a.Provider == provider || strings.HasSuffix(a.Provider, "/"+provider) {
			found = append(found, a)
		}
	}
	where := "scope " + t.ScopeID
	if t.WorkID != "" {
		where = "Work " + t.WorkID
	}
	switch len(found) {
	case 0:
		return client.ActionResult{}, fmt.Errorf("%s offers no %s action%s", where, id, map[bool]string{true: "", false: " from " + provider}[provider == ""])
	case 1:
		return c.InvokeAction(cx, t, found[0].Provider, id, input, key)
	}
	return client.ActionResult{}, fmt.Errorf("%s has several %s actions; pass --entry with one of their providers", where, id)
}

func (o *operator) claim(args []string) int {
	f := newFlags("claim", "scope", "entry", "key")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 {
		return usageError(o.errOut, "expected claim [SCOPE#]N [--scope S] [--entry NAME]")
	}
	scope, n, ok := ParseClaim(pos[0])
	if !ok {
		return usageError(o.errOut, "expected an issue number, #N or SCOPE#N")
	}
	if scope == "" {
		scope = f.get("scope")
	}
	if scope == "" {
		return usageError(o.errOut, "claim needs a scope: SCOPE#N or --scope")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	return o.emit(InvokeNamed(cx, c, client.ActionTarget{ScopeID: scope}, f.get("entry"), ClaimAction, strconv.Itoa(n), f.key()))
}

// actions lists or invokes the actions extensions offer on a scope or Work.
func (o *operator) actions(args []string) int {
	usage := "expected actions list (--scope S | --work W) | actions run PROVIDER ID (--scope S | --work W) [--input TEXT] [--key K]"
	if len(args) == 0 || args[0] != "list" && args[0] != "run" {
		return usageError(o.errOut, usage)
	}
	f := newFlags("actions", "scope", "work", "input", "key")
	asJSON := f.fs.Bool("json", false, "")
	pos, err := f.parse(args[1:])
	t := client.ActionTarget{ScopeID: f.get("scope"), WorkID: f.get("work")}
	if err != nil || (t.ScopeID == "") == (t.WorkID == "") || args[0] == "list" && len(pos) != 0 || args[0] == "run" && len(pos) != 2 {
		return usageError(o.errOut, usage)
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	if args[0] == "list" {
		actions, err := c.Actions(cx, t)
		if err != nil || *asJSON {
			return o.emit(actions, err)
		}
		return o.table(actions, actionColumns)
	}
	return o.emit(c.InvokeAction(cx, t, pos[0], pos[1], f.get("input"), f.key()))
}
