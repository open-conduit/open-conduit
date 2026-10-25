package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/alsiola/open-conduit/client"
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
			fmt.Fprintf(notice, "open-conduit: %s stays held; release it with `open-conduit work release %s --revision N`\n", id, id)
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
	runErr := run(spec.Argv, spec.Dir)
	return errors.Join(runErr, release())
}

func (o *operator) workControl(sub string, args []string) int {
	switch sub {
	case "hold", "release":
		return o.control(args, func(c *client.Client, id, rev, reason, key string) (any, error) {
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
		if err != nil || len(pos) != 1 || f.get("revision") == "" || f.get("binding") == "" {
			return usageError(o.errOut, "expected work rebind ID --revision N --binding B")
		}
		c, _, err := o.client()
		if err != nil {
			return o.emit(nil, err)
		}
		cx, cancel := ctx()
		defer cancel()
		return o.emit(c.RebindWork(cx, pos[0], f.get("revision"), f.get("binding"), f.get("reason"), f.key()))
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

// ClaimType is the GitHub extension's operator claim Event
// (github.claim.requested): routed by the `github` handler, it creates Work
// for the named issue whatever its labels.
const ClaimType = "github.claim.requested"

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

// githubNamespaces lists a scope's github extension entries (name →
// namespace) from its effective metadata.
func githubNamespaces(s client.Scope) map[string]string {
	var meta struct {
		Extensions []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Config struct {
				Namespace string `json:"namespace"`
				Scope     string `json:"scope"`
			} `json:"config"`
		} `json:"extensions"`
	}
	out := map[string]string{}
	if json.Unmarshal(s.Config, &meta) != nil {
		return out
	}
	for _, x := range meta.Extensions {
		if x.Type == "github" && x.Config.Scope == s.ID && x.Config.Namespace != "" {
			out[x.Name] = x.Config.Namespace
		}
	}
	return out
}

// Claim submits an operator claim for issue n in scope's github entry
// (entry may be empty when the scope has exactly one).
func Claim(cx context.Context, c *client.Client, scope, entry string, n int, key string) (client.Event, error) {
	s, err := c.Scope(cx, scope)
	if err != nil {
		return client.Event{}, err
	}
	entries := githubNamespaces(s)
	ns := entries[entry]
	if entry == "" && len(entries) == 1 {
		for _, v := range entries {
			ns = v
		}
	}
	if ns == "" {
		return client.Event{}, fmt.Errorf("scope %s has no single github entry%s; pass --entry", scope, map[bool]string{true: "", false: " named " + entry}[entry == ""])
	}
	data, _ := json.Marshal(map[string]int{"number": n})
	return c.SubmitEvent(cx, client.EventRequest{ScopeID: scope, Type: ClaimType, DeliveryID: fmt.Sprintf("claim/%s/%d/%s", scope, n, key),
		CorrelationKey: &client.Key{Source: ns, ExternalID: "issue/" + strconv.Itoa(n)}, Data: &client.Payload{Schema: "github.claim.v1", Data: data}}, key)
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
	return o.emit(Claim(cx, c, scope, f.get("entry"), n, f.key()))
}
