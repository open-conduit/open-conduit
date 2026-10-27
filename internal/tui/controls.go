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

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// Controls are the operator actions the view offers when its source
// supports them. Every action is a management API call, so it commits an
// accepted core.manual.* Event with the operator's causal identity; the view
// has no private path to state. Extension context arrives as Work
// annotations and Wait records, and extension actions (claim, answer, …)
// as named actions the view lists and invokes, all without domain
// branches: the extension authorises an action and writes its Event.
type Controls interface {
	Work(ctx context.Context, id string) (client.Work, error)
	Waits(ctx context.Context, q url.Values) (client.Page[client.Wait], error)
	CancelExecution(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	CancelWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	RetryWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	HoldWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	ReleaseWork(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error)
	Scopes(ctx context.Context, q url.Values) (client.Page[client.Scope], error)
	// Actions lists what extensions offer on a scope or Work now, and
	// InvokeAction invokes one as the local operator.
	Actions(ctx context.Context, t client.ActionTarget) ([]client.Action, error)
	InvokeAction(ctx context.Context, t client.ActionTarget, provider, id, input, key string) (client.ActionResult, error)
	// AttachCommand holds, attaches to and then releases Work.
	AttachCommand(id string) tea.ExecCommand
}

// OperatorSource is the management client with attach.
type OperatorSource struct{ *client.Client }

// Operator wraps a management client for the interactive view.
func Operator(c *client.Client) OperatorSource { return OperatorSource{c} }

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
	work    client.Work
	waits   []client.Wait
	actions []client.Action
	err     error
}

// targeted is an action and what it applies to.
type targeted struct {
	target client.ActionTarget
	action client.Action
}

// actionMenu lists actions to pick from by number.
type actionMenu struct {
	title string
	items []targeted
}

type menuMsg struct {
	menu actionMenu
	err  error
}

// asking is an open one-line text prompt.
type asking struct {
	label, text string
	submit      func(string) tea.Cmd
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
		actions, err := ctl.Actions(ctx, client.ActionTarget{WorkID: id})
		return workMsg{work: w, waits: page.Items, actions: actions, err: err}
	}
}

// fetchScopeActions gathers every scope's actions into a menu.
func (m Model) fetchScopeActions() tea.Cmd {
	ctl := m.ctl
	return func() tea.Msg {
		ctx, cancel := call()
		defer cancel()
		scopes, err := ctl.Scopes(ctx, url.Values{"limit": {"200"}})
		if err != nil {
			return menuMsg{err: err}
		}
		menu := actionMenu{title: "scope actions"}
		for _, sc := range scopes.Items {
			t := client.ActionTarget{ScopeID: sc.ID}
			actions, err := ctl.Actions(ctx, t)
			if err != nil {
				return menuMsg{err: err}
			}
			for _, a := range actions {
				menu.items = append(menu.items, targeted{t, a})
			}
		}
		return menuMsg{menu: menu}
	}
}

// choose asks for an action's input (or a y/n when it takes none) and
// then invokes it.
func (m Model) choose(it targeted) Model {
	ctl := m.ctl
	invoke := func(input string) tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := call()
			defer cancel()
			res, err := ctl.InvokeAction(ctx, it.target, it.action.Provider, it.action.ID, input, key())
			if err != nil {
				return actionMsg{err: err}
			}
			text := it.action.Label + " accepted"
			if res.Message != "" {
				text = res.Message
			}
			return actionMsg{text: text + " (event " + res.Event.ID + ")"}
		}
	}
	where := it.target.ScopeID
	if it.target.WorkID != "" {
		where = it.target.WorkID
	}
	if it.action.Prompt == "" {
		m.confirm = &pending{prompt: it.action.Label + " (" + where + ")?", cmd: invoke("")}
		return m
	}
	m.ask = &asking{label: it.action.Label + " (" + where + ") " + it.action.Prompt, submit: invoke}
	return m
}

// actionLine renders numbered actions for picking.
func actionLine(items []targeted, scoped bool) string {
	var parts []string
	for i, it := range items {
		if i == 9 {
			break
		}
		label := it.action.Label
		if scoped {
			label = it.target.ScopeID + ": " + label
		}
		parts = append(parts, fmt.Sprintf("%d %s", i+1, label))
	}
	return strings.Join(parts, " · ")
}

// pick returns the action a digit key selects.
func pickAction(items []targeted, k string) (targeted, bool) {
	if len(k) != 1 || k[0] < '1' || k[0] > '9' {
		return targeted{}, false
	}
	i := int(k[0] - '1')
	if i >= len(items) {
		return targeted{}, false
	}
	return items[i], true
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
