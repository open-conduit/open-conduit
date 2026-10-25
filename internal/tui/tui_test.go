package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alsiola/open-conduit/client"
)

type fakeSource struct {
	runs    []client.Run
	queries []url.Values
	content string
	fail    error
}

func (f *fakeSource) Runs(_ context.Context, q url.Values) (client.Page[client.Run], error) {
	f.queries = append(f.queries, q)
	if f.fail != nil {
		return client.Page[client.Run]{}, f.fail
	}
	var out []client.Run
	for _, r := range f.runs {
		if (q.Get("state") == "" || r.State == q.Get("state")) && (q.Get("work_id") == "" || r.WorkID == q.Get("work_id")) {
			out = append(out, r)
		}
	}
	return client.Page[client.Run]{Items: out}, nil
}
func (f *fakeSource) Run(_ context.Context, id string) (client.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return client.Run{}, errors.New("not found")
}
func (f *fakeSource) ArtifactContent(_ context.Context, id, byteRange string, w io.Writer) error {
	_, err := io.WriteString(w, f.content)
	return err
}

func run(id, state, kind string, attempt uint32, created string) client.Run {
	return client.Run{Execution: client.Execution{ID: id, WorkID: "w-" + kind, State: state, Attempt: attempt, ExecutorBinding: "b", CreatedAt: created, Artifacts: []client.Artifact{{ID: "art-" + id, MediaType: "text/plain", Size: 10}}, Outcome: &client.Outcome{Kind: state}}, WorkKind: kind, Executor: "x", Trigger: &client.Trigger{Type: "t." + kind}, Retention: client.Retention{State: "retained"}}
}

func exec(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	m, _ = m.Update(cmd())
	return m
}

func TestModelFilterDetailLogAndErrors(t *testing.T) {
	src := &fakeSource{runs: []client.Run{run("r1", "failed", "job", 1, "1"), run("r2", "completed", "job", 2, "2"), run("r3", "running", "chat", 1, "3")}}
	for i := 0; i < 100; i++ {
		src.content += fmt.Sprintf("line %d\n", i)
	}
	var m tea.Model = New(src, time.Hour)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 20})
	m = exec(t, m, m.(Model).fetch())
	view := m.View()
	if !strings.Contains(view, "3 run(s)") || strings.Index(view, "t.chat") > strings.Index(view, "t.job") {
		t.Fatalf("newest first list:\n%s", view)
	}
	// f cycles to the running filter and refetches with it.
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m = exec(t, m, cmd)
	if q := src.queries[len(src.queries)-1]; q.Get("state") != "running" || !strings.Contains(m.View(), "1 run(s)") {
		t.Fatalf("filter query %v", q)
	}
	for range Filters[1:] {
		m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
		m = exec(t, m, cmd)
	}
	// Detail lists every attempt of the Work through the same row renderer.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = exec(t, m, cmd)
	if view = m.View(); !strings.Contains(view, "Run r2") || strings.Count(view, "t.job") < 3 {
		t.Fatalf("detail:\n%s", view)
	}
	// The log opens at its tail and scrolls.
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m = exec(t, m, cmd)
	if view = m.View(); !strings.Contains(view, "line 99") || strings.Contains(view, "line 10\n") {
		t.Fatalf("log tail:\n%s", view)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if !strings.Contains(m.View(), "line 70") {
		t.Fatalf("scroll:\n%s", m.View())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !strings.Contains(m.View(), "Run r2") {
		t.Fatal("esc from log returns to detail")
	}
	src.fail = errors.New("management unavailable")
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "error: management unavailable") {
		t.Fatalf("error not shown:\n%s", m.View())
	}
	if _, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("q must quit")
	}
}

func TestExpiredLogIsReportedNotFetched(t *testing.T) {
	r := run("r1", "failed", "job", 1, "1")
	r.Retention.State = "expired"
	src := &fakeSource{runs: []client.Run{r}, content: "must not be read"}
	var m tea.Model = New(src, time.Hour)
	m = exec(t, m, m.(Model).fetch())
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "expired under the retention policy") {
		t.Fatalf("expired log:\n%s", m.View())
	}
}
