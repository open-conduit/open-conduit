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
	work  client.Work
	waits []client.Wait
	calls []string
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
func (c *controlSource) Claim(_ context.Context, ref string) (string, error) {
	c.calls = append(c.calls, "claim "+ref)
	return "manual/claim", nil
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
		waits: []client.Wait{{ID: "wait-1", State: "active", Match: client.Match{Types: []string{"github.pull_request.merged"}}}}}
	var m tea.Model = New(src, time.Hour)
	m = drain(t, m, m.(Model).fetch())
	if !strings.Contains(m.View(), "c claim") {
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
	// c opens the claim prompt; enter submits the text.
	m, _ = press(m, "c")
	m, _ = press(m, "eng#12")
	if !strings.Contains(m.View(), "claim (scope#number): eng#12") {
		t.Fatalf("prompt:\n%s", m.View())
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(t, m, cmd)
	if src.calls[len(src.calls)-1] != "claim eng#12" {
		t.Fatalf("claim: %v", src.calls)
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
	if _, cmd = press(m, "a"); cmd == nil {
		t.Fatal("a must attach")
	}
}

func TestNoControlsWithoutASource(t *testing.T) {
	var m tea.Model = New(&fakeSource{runs: []client.Run{run("r1", "running", "job", 1, "1")}}, time.Hour)
	m = drain(t, m, m.(Model).fetch())
	if strings.Contains(m.View(), "c claim") {
		t.Fatal("controls offered by a read-only source")
	}
	if _, cmd := press(m, "x"); cmd != nil {
		t.Fatal("x acted without controls")
	}
}
