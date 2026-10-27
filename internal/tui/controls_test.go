package tui

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/open-conduit/open-conduit/client"
)

// controlSource adds operator controls to the history fake and records
// every call with the revision it used.
type controlSource struct {
	fakeSource
	work         client.Work
	waits        []client.Wait
	calls        []string
	scopeActions map[string][]client.Action
	workActions  []client.Action
}

func (c *controlSource) Work(context.Context, string) (client.Work, error) { return c.work, nil }
func (c *controlSource) Waits(context.Context, url.Values) (client.Page[client.Wait], error) {
	return client.Page[client.Wait]{Items: c.waits}, nil
}
func (c *controlSource) rec(op, id, rev string) (client.ControlResult, error) {
	c.calls = append(c.calls, op+" "+id+" "+rev)
	return client.ControlResult{EventID: "manual/" + op}, nil
}
func (c *controlSource) CancelExecution(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return c.rec("cancel-execution", id, rev)
}
func (c *controlSource) CancelWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return c.rec("cancel-work", id, rev)
}
func (c *controlSource) RetryWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	return c.rec("retry", id, rev)
}
func (c *controlSource) HoldWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	c.work.Hold = &client.Hold{EventID: "h"}
	return c.rec("hold", id, rev)
}
func (c *controlSource) ReleaseWork(_ context.Context, id, rev, _, _ string) (client.ControlResult, error) {
	c.work.Hold = nil
	return c.rec("release", id, rev)
}
func (c *controlSource) Scopes(context.Context, url.Values) (client.Page[client.Scope], error) {
	var out client.Page[client.Scope]
	for id := range c.scopeActions {
		out.Items = append(out.Items, client.Scope{ID: id})
	}
	return out, nil
}
func (c *controlSource) Actions(_ context.Context, t client.ActionTarget) ([]client.Action, error) {
	if t.WorkID != "" {
		return c.workActions, nil
	}
	return c.scopeActions[t.ScopeID], nil
}

// InvokeAction records the call; input "refuse" is refused like a provider.
func (c *controlSource) InvokeAction(_ context.Context, t client.ActionTarget, provider, id, input, _ string) (client.ActionResult, error) {
	c.calls = append(c.calls, "action "+t.ScopeID+t.WorkID+" "+provider+" "+id+" "+input)
	if input == "refuse" {
		return client.ActionResult{}, &client.Error{Status: 422, Code: "not_allowed", Message: "you are not the one asked"}
	}
	return client.ActionResult{Event: client.Event{ID: "action/" + id}, Message: id + " done"}, nil
}
func (c *controlSource) AttachCommand(id string) tea.ExecCommand { return &fakeAttach{c: c, id: id} }

type fakeAttach struct {
	c  *controlSource
	id string
}

func (f *fakeAttach) Run() error          { f.c.calls = append(f.c.calls, "attach "+f.id); return nil }
func (f *fakeAttach) SetStdin(io.Reader)  {}
func (f *fakeAttach) SetStdout(io.Writer) {}
func (f *fakeAttach) SetStderr(io.Writer) {}

func press(m tea.Model, keys string) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{k}})
	}
	return m, cmd
}

// drain runs a command and its batches, feeding every message back.
func drain(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			m = drain(t, m, c)
		}
		return m
	}
	m, _ = m.Update(msg)
	return m
}

func TestOperatorControls(t *testing.T) {
	live := run("r1", "running", "github.issue", 1, "1")
	live.Revision = "4"
	src := &controlSource{fakeSource: fakeSource{runs: []client.Run{live}}, work: client.Work{ID: "w-github.issue", State: "running", Revision: "9",
		CorrelationKeys: []client.Key{{Source: "github/github.com/o/r", ExternalID: "issue/12"}},
		Annotations:     []client.Annotation{{Source: "github/eng/r", Name: "url", Value: "https://github.com/o/r/issues/12"}}},
		waits:        []client.Wait{{ID: "wait-1", State: "active", Match: client.Match{Types: []string{"github.pull_request.merged"}}}},
		scopeActions: map[string][]client.Action{"eng": {{Provider: "github/eng/r", ID: "claim", Label: "Claim an issue by hand", Prompt: "issue number"}}},
		workActions:  []client.Action{{Provider: "human", ID: "approve", Label: "Approve"}, {Provider: "human", ID: "answer", Label: "Answer the question", Prompt: "answer"}}}
	var m tea.Model = New(src, time.Hour)
	m = drain(t, m, m.(Model).fetch())
	if !strings.Contains(m.View(), "c scope actions") {
		t.Fatalf("control keys not offered:\n%s", m.View())
	}
	// x asks before cancelling the live attempt at its revision.
	m, _ = press(m, "x")
	if !strings.Contains(m.View(), "cancel execution r1? y/n") {
		t.Fatalf("no confirmation:\n%s", m.View())
	}
	m, cmd := press(m, "n")
	if cmd != nil || len(src.calls) != 0 {
		t.Fatal("declined cancel ran")
	}
	m, _ = press(m, "x")
	m, cmd = press(m, "y")
	m = drain(t, m, cmd)
	if len(src.calls) != 1 || src.calls[0] != "cancel-execution r1 4" || !strings.Contains(m.View(), "accepted (event manual/cancel-execution)") {
		t.Fatalf("cancel: %v\n%s", src.calls, m.View())
	}
	// R retries and H holds then releases, each at the Work's revision.
	m, cmd = press(m, "R")
	m = drain(t, m, cmd)
	m, cmd = press(m, "H")
	m = drain(t, m, cmd)
	m, cmd = press(m, "H")
	m = drain(t, m, cmd)
	if strings.Join(src.calls[1:], ",") != "retry w-github.issue 9,hold w-github.issue 9,release w-github.issue 9" {
		t.Fatalf("calls: %v", src.calls)
	}
	// c lists the scopes' actions; a number picks one, its prompt takes
	// the input, and enter invokes it by name.
	m, cmd = press(m, "c")
	m = drain(t, m, cmd)
	if !strings.Contains(m.View(), "1 eng: Claim an issue by hand") {
		t.Fatalf("menu:\n%s", m.View())
	}
	m, _ = press(m, "1")
	m, _ = press(m, "12")
	if !strings.Contains(m.View(), "Claim an issue by hand (eng) issue number: 12") {
		t.Fatalf("prompt:\n%s", m.View())
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(t, m, cmd)
	if src.calls[len(src.calls)-1] != "action eng github/eng/r claim 12" || !strings.Contains(m.View(), "claim done (event action/claim)") {
		t.Fatalf("claim: %v\n%s", src.calls, m.View())
	}
	// Detail shows extension context as data: keys, annotations, waits.
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(t, m, cmd)
	view := m.View()
	for _, want := range []string{"Run r1", "issue/12", "https://github.com/o/r/issues/12", "wait wait-1", "github.pull_request.merged"} {
		if !strings.Contains(view, want) {
			t.Fatalf("detail lacks %q:\n%s", want, view)
		}
	}
	// Detail lists the Work's actions; one without a prompt asks y/n, and
	// a refusal shows the provider's code and message.
	if !strings.Contains(view, "1 Approve · 2 Answer the question") {
		t.Fatalf("detail lacks actions:\n%s", view)
	}
	m, _ = press(m, "1")
	m, cmd = press(m, "y")
	m = drain(t, m, cmd)
	if src.calls[len(src.calls)-1] != "action w-github.issue human approve " {
		t.Fatalf("approve: %v", src.calls)
	}
	m, _ = press(m, "2")
	m, _ = press(m, "refuse")
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(t, m, cmd)
	if !strings.Contains(m.View(), "not_allowed: you are not the one asked") {
		t.Fatalf("refusal not shown:\n%s", m.View())
	}
	if _, cmd = press(m, "a"); cmd == nil {
		t.Fatal("a must attach")
	}
}

func TestNoControlsWithoutASource(t *testing.T) {
	var m tea.Model = New(&fakeSource{runs: []client.Run{run("r1", "running", "job", 1, "1")}}, time.Hour)
	m = drain(t, m, m.(Model).fetch())
	if strings.Contains(m.View(), "c scope actions") {
		t.Fatal("controls offered by a read-only source")
	}
	if _, cmd := press(m, "x"); cmd != nil {
		t.Fatal("x acted without controls")
	}
}

// labelSource answers each Work by its ID.
type labelSource struct {
	controlSource
	works map[string]client.Work
	looks int
}

func (l *labelSource) Work(_ context.Context, id string) (client.Work, error) {
	l.looks++
	return l.works[id], nil
}

// TestListGroupsTurnsByWork: a later turn of an issue is not a new row; the
// row names the issue and counts its turns, and g lists every turn.
func TestListGroupsTurnsByWork(t *testing.T) {
	first := run("r1", "completed", "github.issue", 1, "1")
	first.WorkID = "w-issue"
	second := run("r2", "running", "github.issue", 2, "3")
	second.WorkID = "w-issue"
	second.Trigger = &client.Trigger{Type: "github.checks.failed"}
	thread := run("r3", "completed", "slack.thread", 1, "2")
	src := &labelSource{controlSource: controlSource{fakeSource: fakeSource{runs: []client.Run{first, second, thread}}}, works: map[string]client.Work{
		"w-issue":        {ID: "w-issue", Annotations: []client.Annotation{{Source: "runtime", Name: "waiting", Value: "limit reached: jobs (settings.job_concurrency) 1/1 running"}, {Name: "issue", Value: "open-conduit/open-conduit#1"}, {Name: "url", Value: "https://github.com/x"}}},
		"w-slack.thread": {ID: "w-slack.thread", CorrelationKeys: []client.Key{{Source: "slack", ExternalID: "thread/C1/17"}}},
	}}
	var m tea.Model = New(src, time.Hour)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	for range 2 {
		var cmd tea.Cmd
		m, cmd = m.Update(m.(Model).fetch()())
		m = drain(t, m, cmd)
	}
	view := m.View()
	if src.looks != 2 {
		t.Fatalf("each Work is looked up once, got %d", src.looks)
	}
	if !strings.Contains(view, "2 work · 3 run(s)") || strings.Count(view, "open-conduit#1") != 1 || strings.Contains(view, "open-conduit/open-conduit") || !strings.Contains(view, "slack thread/C1/17") {
		t.Fatalf("by work:\n%s", view)
	}
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "open-conduit#1") && (!strings.Contains(l, "running") || !strings.Contains(l, " 2 ")) {
			t.Fatalf("issue row shows its latest turn and count: %q", l)
		}
	}
	// g lists every turn, each placed among its Work's turns, and keeps the
	// selection on the same Work.
	m, _ = press(m, "g")
	view = m.View()
	if strings.Count(view, "open-conduit#1") != 2 || !strings.Contains(view, "1/2") || !strings.Contains(view, "2/2") || !strings.Contains(view, "every turn") {
		t.Fatalf("every turn:\n%s", view)
	}
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(t, m, cmd)
	if !strings.Contains(m.View(), "Run r2") {
		t.Fatalf("selection moved:\n%s", m.View())
	}
}
