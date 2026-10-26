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

	"github.com/open-conduit/open-conduit/client"
)

type fakeSource struct {
	runs    []client.Run
	queries []url.Values
	content string
	// contents, when set, is each artifact's content by ID.
	contents map[string]string
	// live is a running run's output; nil answers not_live.
	live *client.LiveRun
	fail error
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
	content := f.content
	if c, ok := f.contents[id]; ok {
		content = c
	}
	_, err := io.WriteString(w, content)
	return err
}
func (f *fakeSource) RunLive(_ context.Context, id string, tail int64) (client.LiveRun, error) {
	if f.live == nil {
		return client.LiveRun{}, &client.Error{Status: 409, Code: "not_live"}
	}
	return *f.live, nil
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

// TestJobAndTurnShareTheView ports v1 tui/jobs_test.go TestRunDetail onto
// the one run view (05-02): a shell job run and an agent turn are listed by
// the same rows, and a job's detail shows its trigger, outcome, workspace and
// log through the same renderer as the turn's usage.
func TestJobAndTurnShareTheView(t *testing.T) {
	job := run("job-1", "failed", "schedule.job", 1, "1")
	job.Executor, job.ExecutorBinding = "shell", "post-reels"
	job.Trigger = &client.Trigger{ID: "t1", Type: "schedule.event", Source: "schedule/demo"}
	job.Outcome = &client.Outcome{Kind: "failed", Summary: "exit status 1"}
	job.Workspace = &client.RunWorkspace{Kind: "workspace", Ref: "fresh-1", Dir: "/tmp/ws/fresh-1", Summary: "fresh worktree at origin/main 1a2b3c4"}
	turn := run("turn-1", "completed", "github.issue", 1, "2")
	turn.Executor, turn.ExecutorBinding = "claude-code", "agent"
	cost, in := 0.0123, uint64(42)
	turn.Usage = &client.Usage{Cost: &cost, Currency: "USD", InputTokens: &in}
	src := &fakeSource{runs: []client.Run{job, turn}, content: "posting 2 reels\nfailed: 401\n"}
	var m tea.Model = New(src, time.Hour)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	m = exec(t, m, m.(Model).fetch())
	view := m.View()
	for _, want := range []string{"shell", "claude-code", "schedule.job", "github.issue", "schedule.event"} {
		if !strings.Contains(view, want) {
			t.Fatalf("list lacks %q:\n%s", want, view)
		}
	}
	// The newest (the turn) is first; the job's detail is one row down.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = exec(t, m, cmd)
	view = m.View()
	for _, want := range []string{"Run job-1", "schedule.event from schedule/demo", "failed: exit status 1", "workspace", "/tmp/ws/fresh-1 (fresh worktree at origin/main 1a2b3c4)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("job detail lacks %q:\n%s", want, view)
		}
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "failed: 401") {
		t.Fatalf("job log:\n%s", m.View())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = exec(t, m, cmd)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = exec(t, m, cmd)
	if view = m.View(); !strings.Contains(view, "Run turn-1") || !strings.Contains(view, "cost 0.0123 USD") || strings.Contains(view, "workspace") {
		t.Fatalf("turn detail:\n%s", view)
	}
}

// A running turn's log follows its live output, t steps to its transcript,
// and once it ends the view settles on its artifacts.
func TestRunningRunLogFollows(t *testing.T) {
	r := run("r1", "running", "github.issue", 1, "1")
	r.Artifacts = nil
	src := &fakeSource{runs: []client.Run{r}}
	setLive := func(activity string) {
		src.live = &client.LiveRun{RunID: "r1", Outputs: []client.LiveOutput{
			{Name: "activity", MediaType: "text/plain; charset=utf-8", Content: activity},
			{Name: "transcript", MediaType: "application/x-ndjson", Content: `{"type":"system"}`},
			{Name: "stderr", MediaType: "text/plain; charset=utf-8"},
		}}
	}
	setLive("session started\nassistant: reading the issue\n")
	var m tea.Model = New(src, time.Hour)
	m = exec(t, m, m.(Model).fetch())
	if !strings.Contains(m.View(), "t transcript") {
		t.Fatalf("list footer lacks t:\n%s", m.View())
	}
	m, cmd := press(m, "l")
	m = exec(t, m, cmd)
	if v := m.View(); !strings.Contains(v, "assistant: reading the issue") || !strings.Contains(v, "following") {
		t.Fatalf("live log:\n%s", v)
	}
	// A tick refetches while the view follows.
	if _, cmd = m.Update(tickMsg{}); cmd == nil {
		t.Fatal("a tick must refetch a followed log")
	}
	setLive("session started\nassistant: reading the issue\n  tool Bash ls\n")
	m = exec(t, m, m.(Model).refreshLog())
	if !strings.Contains(m.View(), "tool Bash ls") {
		t.Fatalf("followed log:\n%s", m.View())
	}
	m, cmd = press(m, "t")
	m = exec(t, m, cmd)
	if v := m.View(); !strings.Contains(v, `{"type":"system"}`) || !strings.Contains(v, "r1 transcript") {
		t.Fatalf("live transcript:\n%s", v)
	}
	// The turn ends: the view settles on the published transcript.
	src.live = nil
	src.runs[0].State = "completed"
	src.runs[0].Artifacts = []client.Artifact{{ID: "act", MediaType: "text/plain; charset=utf-8", Size: 5}, {ID: "tr", MediaType: "application/x-ndjson", Size: 5}}
	src.contents = map[string]string{"act": "final activity", "tr": "final transcript"}
	m = exec(t, m, m.(Model).refreshLog())
	if v := m.View(); !strings.Contains(v, "final transcript") || strings.Contains(v, "following") {
		t.Fatalf("ended log:\n%s", v)
	}
	if _, cmd = m.Update(tickMsg{}); cmd == nil {
		t.Fatal("tick keeps ticking")
	}
	if m.(Model).live {
		t.Fatal("an ended run is not followed")
	}
}

// t from a run's detail opens its transcript; a run without one says so.
func TestTranscriptKey(t *testing.T) {
	turn := run("turn-1", "completed", "github.issue", 1, "2")
	turn.Artifacts = []client.Artifact{{ID: "act", MediaType: "text/plain; charset=utf-8", Size: 5}, {ID: "tr", MediaType: "application/x-ndjson", Size: 5}, {ID: "err", MediaType: "text/plain; charset=utf-8", Size: 5}}
	job := run("job-1", "failed", "schedule.job", 1, "1")
	src := &fakeSource{runs: []client.Run{turn, job}, contents: map[string]string{"act": "activity", "tr": "transcript lines", "err": "stderr"}}
	var m tea.Model = New(src, time.Hour)
	m = exec(t, m, m.(Model).fetch())
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "t transcript") {
		t.Fatalf("detail footer lacks t:\n%s", m.View())
	}
	m, cmd = press(m, "t")
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "transcript lines") {
		t.Fatalf("transcript:\n%s", m.View())
	}
	// t in the log view steps on to stderr, then wraps to the activity.
	m, cmd = press(m, "t")
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "stderr") {
		t.Fatalf("next output:\n%s", m.View())
	}
	m, cmd = press(m, "t")
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "activity") {
		t.Fatalf("wrapped output:\n%s", m.View())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = exec(t, m, cmd)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd = press(m, "t")
	m = exec(t, m, cmd)
	if !strings.Contains(m.View(), "run has no transcript") {
		t.Fatalf("job transcript:\n%s", m.View())
	}
}
