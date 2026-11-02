package tui

import (
	"context"
	"errors"
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
	// EventsWithRoutes lists Events with how each was routed, for the
	// Events routing is stuck on or gave up on; SkipEvent and
	// RerouteEvent act on one.
	EventsWithRoutes(ctx context.Context, q url.Values) (client.Page[client.Event], error)
	SkipEvent(ctx context.Context, id, reason, key string) (client.ControlResult, error)
	RerouteEvent(ctx context.Context, id, reason, key string) (client.ControlResult, error)
	Scopes(ctx context.Context, q url.Values) (client.Page[client.Scope], error)
	// Actions lists what extensions offer on a scope or Work now, and
	// InvokeAction invokes one as the local operator.
	Actions(ctx context.Context, t client.ActionTarget) ([]client.Action, error)
	// Panels lists the panels extensions show on a target now: documents
	// the view renders as they are, whose buttons are listed actions.
	Panels(ctx context.Context, t client.ActionTarget) ([]client.Panel, error)
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
	stuck   []client.Event
	actions []client.Action
	panels  []client.Panel
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
		events, err := ctl.EventsWithRoutes(ctx, url.Values{"work_id": {id}, "limit": {"200"}})
		if err != nil {
			return workMsg{work: w, waits: page.Items, err: err}
		}
		stuck := stuckEvents(events.Items)
		t := client.ActionTarget{WorkID: id}
		panels, err := ctl.Panels(ctx, t)
		var e *client.Error
		if err != nil && !(errors.As(err, &e) && e.Status == 404 && e.Code == "not_found") {
			return workMsg{work: w, waits: page.Items, stuck: stuck, err: err}
		}
		actions, err := ctl.Actions(ctx, t)
		return workMsg{work: w, waits: page.Items, stuck: stuck, actions: actions, panels: panels, err: err}
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
	case "S", "E":
		return m.stuckKey(k, r.WorkID)
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

// stuckEvents are the Events routing has not routed or gave up on, oldest
// first.
func stuckEvents(events []client.Event) []client.Event {
	var out []client.Event
	for _, e := range events {
		if !e.Routed || e.Disposition == "failed" {
			out = append(out, e)
		}
	}
	return out
}

// StuckText renders the Events routing is stuck on or gave up on, with
// the keys that act on the first.
func StuckText(stuck []client.Event) string {
	if len(stuck) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n" + boldS.Render("Events routing is stuck on") + dimS.Render("  S skips the first unrouted · E routes the first again") + "\n")
	for _, e := range stuck {
		state := "unrouted"
		if e.Routed {
			state = e.Disposition
		}
		if r := e.Route; r != nil && r.Reason != "" {
			state += ": " + r.Reason
			if r.Attempts > 0 {
				state += fmt.Sprintf(" after %d attempt(s)", r.Attempts)
			}
		}
		fmt.Fprintf(&b, "  %-18s %s %s\n", e.Type, e.ID, state)
	}
	return b.String()
}

// stuckKey handles S and E on the Work's stuck Events.
func (m Model) stuckKey(k, workID string) (Model, tea.Cmd, bool) {
	if m.work == nil || m.work.ID != workID {
		return m, nil, false
	}
	var target *client.Event
	for i := range m.stuck {
		if k == "E" || !m.stuck[i].Routed {
			target = &m.stuck[i]
			break
		}
	}
	if target == nil {
		m.notice = "no event to " + map[string]string{"S": "skip", "E": "route again"}[k]
		return m, nil, true
	}
	id, ctl := target.ID, m.ctl
	if k == "S" {
		m.confirm = &pending{prompt: "skip event " + id + "? routing won't try it again", cmd: func() tea.Msg {
			ctx, cancel := call()
			defer cancel()
			res, err := ctl.SkipEvent(ctx, id, "operator (tui)", key())
			if err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: "event skip accepted (event " + res.EventID + ")"}
		}}
		return m, nil, true
	}
	return m, func() tea.Msg {
		ctx, cancel := call()
		defer cancel()
		res, err := ctl.RerouteEvent(ctx, id, "operator (tui)", key())
		if err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "event reroute accepted (routes " + res.RoutedEventID + ")"}
	}, true
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

// PanelText renders extensions' panels as text. A button shows as the
// number that picks it from actions (the view's numbered actions).
func PanelText(panels []client.Panel, actions []targeted) string {
	var b strings.Builder
	for _, p := range panels {
		b.WriteString("\n" + boldS.Render(p.Title) + dimS.Render("  "+p.Provider) + "\n")
		for _, sec := range p.Sections {
			if sec.Title != "" {
				b.WriteString("  " + boldS.Render(sec.Title) + "\n")
			}
			if sec.Text != "" {
				for _, l := range strings.Split(sec.Text, "\n") {
					b.WriteString("  " + l + "\n")
				}
			}
			for _, f := range sec.Fields {
				fmt.Fprintf(&b, "  %-18s %s\n", f.Name, f.Value)
			}
			if t := sec.Table; t != nil {
				b.WriteString(tableText(*t))
			}
			for _, m := range sec.Messages {
				who := m.Label
				if who == "" {
					who = map[string]string{"operator": "You", "agent": "Agent", "note": "Note"}[m.From]
				}
				b.WriteString("  " + boldS.Render(who) + "\n")
				for _, l := range strings.Split(m.Text, "\n") {
					b.WriteString("    " + l + "\n")
				}
				if m.Chosen != "" {
					b.WriteString("    " + dimS.Render("answered: "+m.Chosen) + "\n")
				}
			}
			for _, l := range sec.Links {
				b.WriteString("  " + l.Label + "  " + dimS.Render(l.URL) + "\n")
			}
			var keys []string
			for _, a := range sec.Actions {
				for i, it := range actions {
					if i < 9 && it.action.Provider == a.Provider && it.action.ID == a.ID {
						keys = append(keys, fmt.Sprintf("%d %s", i+1, a.Label))
					}
				}
			}
			if len(keys) > 0 {
				b.WriteString("  " + strings.Join(keys, " · ") + "\n")
			}
		}
	}
	return b.String()
}

// tableText lays a panel table out in padded columns.
func tableText(t client.PanelTable) string {
	width := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		width[i] = len([]rune(c))
	}
	for _, r := range t.Rows {
		for i, c := range r {
			if i < len(width) {
				width[i] = max(width[i], len([]rune(c)))
			}
		}
	}
	line := func(cells []string) string {
		parts := make([]string, len(cells))
		for i, c := range cells {
			if i == len(cells)-1 {
				parts[i] = c
				continue
			}
			parts[i] = c + strings.Repeat(" ", max(width[i]-len([]rune(c)), 0))
		}
		return "  " + strings.Join(parts, "  ") + "\n"
	}
	out := dimS.Render(strings.TrimRight(line(t.Columns), "\n")) + "\n"
	for _, r := range t.Rows {
		out += line(r)
	}
	return out
}

var _ Controls = OperatorSource{}
