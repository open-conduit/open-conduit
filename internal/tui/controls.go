package tui

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/internal/cli"
)

// Controls are the operator actions the view offers when its source
// supports them. Every action is a management API call, so it commits an
// accepted core.manual.* Event with the operator's causal identity; the view
// has no private path to state. Extension context arrives as Work
// annotations and Wait records, rendered without domain branches.
type Controls interface {
	Work(ctx context.Context, id string) (client.Work, error)
	Waits(ctx context.Context, q url.Values) (client.Page[client.Wait], error)
	CancelExecution(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	CancelWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	RetryWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	HoldWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	ReleaseWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	// Claim claims an item by hand from "scope#ref" text.
	Claim(ctx context.Context, ref string) (string, error)
	// AttachCommand holds, attaches to and then releases Work.
	AttachCommand(id string) tea.ExecCommand
}

// OperatorSource is the management client with claim and attach.
type OperatorSource struct{ *client.Client }

// Operator wraps a management client for the interactive view.
func Operator(c *client.Client) OperatorSource { return OperatorSource{c} }

// Claim submits a GitHub manual claim ("scope#12").
func (o OperatorSource) Claim(ctx context.Context, ref string) (string, error) {
	scope, n, ok := cli.ParseClaim(ref)
	if !ok || scope == "" {
		return "", fmt.Errorf("claim takes scope#number")
	}
	e, err := cli.Claim(ctx, o.Client, scope, "", n, "tui-claim-"+fmt.Sprint(n)+"-"+scope)
	if err != nil {
		return "", err
	}
	return e.ID, nil
}

// AttachCommand runs the attach flow with the terminal.
func (o OperatorSource) AttachCommand(id string) tea.ExecCommand {
	return &attachCmd{c: o.Client, id: id, in: os.Stdin, out: os.Stdout, err: os.Stderr}
}

type attachCmd struct {
	c        *client.Client
	id       string
	in       io.Reader
	out, err io.Writer
}

func (a *attachCmd) SetStdin(r io.Reader)  { a.in = r }
func (a *attachCmd) SetStdout(w io.Writer) { a.out = w }
func (a *attachCmd) SetStderr(w io.Writer) { a.err = w }
func (a *attachCmd) Run() error {
	return cli.Attach(a.c, a.id, false, func(argv []string, dir string) error {
		return cli.RunAttached(argv, dir, a.in, a.out, a.err)
	}, a.err)
}

type workMsg struct {
	work  client.Work
	waits []client.Wait
	err   error
}

type actionMsg struct {
	text string
	err  error
}

func (m Model) fetchWork(id string) tea.Cmd {
	ctl := m.ctl
	if ctl == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := call()
		defer cancel()
		w, err := ctl.Work(ctx, id)
		if err != nil {
			return workMsg{err: err}
		}
		page, err := ctl.Waits(ctx, url.Values{"work_id": {id}, "limit": {"200"}})
		if err != nil {
			return workMsg{work: w, err: err}
		}
		return workMsg{work: w, waits: page.Items}
	}
}

// action runs one control against the current Work revision.
func (m Model) action(label string, fn func(ctx context.Context, ctl Controls, w client.Work) (client.ControlResult, error), workID string) tea.Cmd {
	ctl := m.ctl
	return func() tea.Msg {
		ctx, cancel := call()
		defer cancel()
		w, err := ctl.Work(ctx, workID)
		if err != nil {
			return actionMsg{err: err}
		}
		res, err := fn(ctx, ctl, w)
		if err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: label + " accepted (event " + res.EventID + ")"}
	}
}

func key() string { return cli.NewKey() }

// controlKey handles an action key in list or detail mode.
func (m Model) controlKey(k string, r client.Run) (Model, tea.Cmd, bool) {
	if m.ctl == nil {
		return m, nil, false
	}
	switch k {
	case "x":
		if r.State != "running" && r.State != "pending" && r.State != "parked" {
			m.notice = "the run is not live; X cancels the Work"
			return m, nil, true
		}
		m.confirm = &pending{prompt: "cancel execution " + r.ID + "?", cmd: func() tea.Msg {
			ctx, cancel := call()
			defer cancel()
			res, err := m.ctl.CancelExecution(ctx, r.ID, r.Revision, "operator (tui)", key())
			if err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: "execution cancel accepted (event " + res.EventID + ")"}
		}}
		return m, nil, true
	case "X":
		m.confirm = &pending{prompt: "cancel work " + r.WorkID + " (terminal)?", cmd: m.action("work cancel", func(ctx context.Context, ctl Controls, w client.Work) (client.ControlResult, error) {
			return ctl.CancelWork(ctx, w.ID, w.Revision, "operator (tui)", key())
		}, r.WorkID)}
		return m, nil, true
	case "R":
		return m, m.action("retry", func(ctx context.Context, ctl Controls, w client.Work) (client.ControlResult, error) {
			return ctl.RetryWork(ctx, w.ID, w.Revision, "operator (tui)", key())
		}, r.WorkID), true
	case "H":
		return m, m.action("hold/release", func(ctx context.Context, ctl Controls, w client.Work) (client.ControlResult, error) {
			if w.Hold != nil {
				return ctl.ReleaseWork(ctx, w.ID, w.Revision, "operator (tui)", key())
			}
			return ctl.HoldWork(ctx, w.ID, w.Revision, "operator (tui)", key())
		}, r.WorkID), true
	case "a":
		return m, tea.Exec(m.ctl.AttachCommand(r.WorkID), func(err error) tea.Msg {
			if err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: "detached from " + r.WorkID}
		}), true
	}
	return m, nil, false
}

// WorkContext renders the Work's hold, extension annotations and waits.
func WorkContext(w *client.Work, waits []client.Wait) string {
	if w == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nWork " + w.ID + "  " + w.State + "  revision " + w.Revision + "\n")
	for _, k := range w.CorrelationKeys {
		fmt.Fprintf(&b, "  %-18s %s\n", "key", k.Source+" "+k.ExternalID)
	}
	if w.Hold != nil {
		fmt.Fprintf(&b, "  %-18s %s\n", "held", strings.TrimSpace(w.Hold.Reason+" since "+w.Hold.At))
	}
	for _, a := range w.Annotations {
		fmt.Fprintf(&b, "  %-18s %s\n", a.Name, a.Value)
	}
	sort.SliceStable(waits, func(i, j int) bool { return waits[i].CreatedAt < waits[j].CreatedAt })
	for _, wt := range waits {
		if wt.State != "active" && wt.WakeEventID == "" {
			continue
		}
		v := wt.State
		if len(wt.Match.Types) > 0 {
			v += " for " + strings.Join(wt.Match.Types, ", ")
		}
		if wt.WakeEventID != "" {
			v += " (woken by " + wt.WakeEventID + ")"
		}
		fmt.Fprintf(&b, "  %-18s %s\n", "wait "+wt.ID, v)
	}
	return b.String()
}

var _ Controls = OperatorSource{}
