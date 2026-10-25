// Package tui is the minimal operator terminal view over the unified run
// history. It uses only the public management client: every executor's
// attempts render through the same row, detail and log code, with no
// executor, work-kind or domain branches.
package tui

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/internal/cli/runview"
)

// Source is the subset of the management client the view needs.
type Source interface {
	Runs(context.Context, url.Values) (client.Page[client.Run], error)
	Run(context.Context, string) (client.Run, error)
	ArtifactContent(ctx context.Context, id, byteRange string, w io.Writer) error
}

// Filters cycles through state filters with the f key.
var Filters = []string{"", "running", "parked", "failed", "completed", "waiting", "handoff", "cancelled"}

// LogTail bounds how much of a log artifact the view fetches.
const LogTail = 64 << 10

type mode int

const (
	listMode mode = iota
	detailMode
	logMode
)

var (
	boldS = lipgloss.NewStyle().Bold(true)
	dimS  = lipgloss.NewStyle().Faint(true)
	selS  = lipgloss.NewStyle().Reverse(true)
	errS  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// Model is the bubbletea model. It is exported so tests can drive Update and
// View without a terminal.
type Model struct {
	src      Source
	interval time.Duration
	runs     []client.Run
	cursor   int
	filter   int
	mode     mode
	detail   *client.Run
	attempts []client.Run
	log      []string
	logTitle string
	offset   int
	width    int
	height   int
	err      string
	now      func() time.Time
}

func New(src Source, interval time.Duration) Model {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return Model{src: src, interval: interval, width: 120, height: 30, now: time.Now}
}

type runsMsg struct {
	runs []client.Run
	err  error
}
type detailMsg struct {
	run      client.Run
	attempts []client.Run
	err      error
}
type logMsg struct {
	title string
	text  string
	err   error
}
type tickMsg struct{}

func call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// FetchRuns loads the newest page of history for a state filter.
func FetchRuns(src Source, state string) ([]client.Run, error) {
	ctx, cancel := call()
	defer cancel()
	q := url.Values{"limit": {"200"}}
	if state != "" {
		q.Set("state", state)
	}
	page, err := src.Runs(ctx, q)
	if err != nil {
		return nil, err
	}
	runs := page.Items
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt > runs[j].CreatedAt })
	return runs, nil
}

func (m Model) fetch() tea.Cmd {
	src, state := m.src, Filters[m.filter]
	return func() tea.Msg {
		runs, err := FetchRuns(src, state)
		return runsMsg{runs, err}
	}
}
func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}
func (m Model) fetchDetail(id, work string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := call()
		defer cancel()
		run, err := src.Run(ctx, id)
		if err != nil {
			return detailMsg{err: err}
		}
		page, err := src.Runs(ctx, url.Values{"work_id": {work}, "limit": {"200"}})
		if err != nil {
			return detailMsg{run: run, err: err}
		}
		attempts := page.Items
		sort.SliceStable(attempts, func(i, j int) bool { return attempts[i].Attempt < attempts[j].Attempt })
		return detailMsg{run: run, attempts: attempts}
	}
}

func (m Model) fetchLog(r client.Run) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		a, ok := runview.LogArtifact(r)
		if !ok {
			return logMsg{err: fmt.Errorf("run has no artifacts")}
		}
		if r.Retention.State == "expired" {
			return logMsg{err: fmt.Errorf("artifact content expired under the retention policy")}
		}
		ctx, cancel := call()
		defer cancel()
		var b strings.Builder
		byteRange := ""
		if a.Size > LogTail {
			byteRange = "bytes=-" + strconv.Itoa(LogTail)
		}
		if a.Size == 0 {
			return logMsg{title: a.ID}
		}
		if err := src.ArtifactContent(ctx, a.ID, byteRange, &b); err != nil {
			return logMsg{err: err}
		}
		return logMsg{title: a.ID, text: b.String()}
	}
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.fetch(), m.tick()) }

func (m Model) selected() (client.Run, bool) {
	if m.cursor < 0 || m.cursor >= len(m.runs) {
		return client.Run{}, false
	}
	return m.runs[m.cursor], true
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		if m.mode == listMode {
			return m, tea.Batch(m.fetch(), m.tick())
		}
		return m, m.tick()
	case runsMsg:
		if msg.err != nil {
			m.err = errText(msg.err)
			return m, nil
		}
		m.err = ""
		var current string
		if r, ok := m.selected(); ok {
			current = r.ID
		}
		m.runs = msg.runs
		m.cursor = 0
		for i, r := range m.runs {
			if r.ID == current {
				m.cursor = i
			}
		}
	case detailMsg:
		if msg.err != nil {
			m.err = errText(msg.err)
		} else {
			m.err = ""
		}
		if msg.run.ID != "" {
			run := msg.run
			m.detail, m.attempts, m.mode = &run, msg.attempts, detailMode
		}
	case logMsg:
		if msg.err != nil {
			m.err = errText(msg.err)
			return m, nil
		}
		m.err = ""
		m.log, m.logTitle, m.offset, m.mode = strings.Split(strings.TrimRight(msg.text, "\n"), "\n"), msg.title, 0, logMode
		if len(m.log) > m.page() {
			m.offset = len(m.log) - m.page()
		}
	case tea.KeyMsg:
		return m.key(msg.String())
	}
	return m, nil
}

func (m Model) page() int { return max(m.height-4, 1) }

func (m Model) key(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "ctrl+c", "q":
		return m, tea.Quit
	}
	switch m.mode {
	case listMode:
		switch k {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.runs)-1 {
				m.cursor++
			}
		case "r":
			return m, m.fetch()
		case "f":
			m.filter = (m.filter + 1) % len(Filters)
			return m, m.fetch()
		case "enter":
			if r, ok := m.selected(); ok {
				return m, m.fetchDetail(r.ID, r.WorkID)
			}
		case "l":
			if r, ok := m.selected(); ok {
				return m, m.fetchLog(r)
			}
		}
	case detailMode:
		switch k {
		case "esc", "backspace", "h", "left":
			m.mode, m.err = listMode, ""
			return m, m.fetch()
		case "l":
			if m.detail != nil {
				return m, m.fetchLog(*m.detail)
			}
		case "r":
			if m.detail != nil {
				return m, m.fetchDetail(m.detail.ID, m.detail.WorkID)
			}
		}
	case logMode:
		switch k {
		case "esc", "backspace", "h", "left":
			m.err = ""
			if m.detail != nil {
				m.mode = detailMode
			} else {
				m.mode = listMode
			}
		case "up", "k":
			m.offset = max(m.offset-1, 0)
		case "down", "j":
			m.offset = min(m.offset+1, max(len(m.log)-m.page(), 0))
		case "pgup":
			m.offset = max(m.offset-m.page(), 0)
		case "pgdown", " ":
			m.offset = min(m.offset+m.page(), max(len(m.log)-m.page(), 0))
		}
	}
	return m, nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (m Model) View() string {
	now := m.now()
	var b strings.Builder
	switch m.mode {
	case listMode:
		filter := Filters[m.filter]
		if filter == "" {
			filter = "all"
		}
		b.WriteString(boldS.Render("Open Conduit runs") + dimS.Render(fmt.Sprintf("  filter: %s  %d run(s)   enter detail · l log · f filter · r refresh · q quit", filter, len(m.runs))) + "\n")
		b.WriteString(dimS.Render(runview.Header) + "\n")
		visible := max(m.height-4, 1)
		start := 0
		if m.cursor >= visible {
			start = m.cursor - visible + 1
		}
		for i := start; i < len(m.runs) && i < start+visible; i++ {
			row := runview.Fit(runview.Row(m.runs[i], now), max(m.width, 20))
			if i == m.cursor {
				row = selS.Render(row)
			}
			b.WriteString(row + "\n")
		}
		if len(m.runs) == 0 {
			b.WriteString(dimS.Render("no runs") + "\n")
		}
	case detailMode:
		if m.detail != nil {
			b.WriteString(runview.Detail(*m.detail, m.attempts, now))
		}
		b.WriteString("\n" + dimS.Render("l log · r refresh · esc back · q quit") + "\n")
	case logMode:
		b.WriteString(boldS.Render("Log "+m.logTitle) + dimS.Render("  ↑/↓ pgup/pgdn scroll · esc back · q quit") + "\n")
		end := min(m.offset+m.page(), len(m.log))
		for _, l := range m.log[m.offset:end] {
			b.WriteString(l + "\n")
		}
	}
	if m.err != "" {
		b.WriteString(errS.Render("error: "+m.err) + "\n")
	}
	return b.String()
}

// Run starts the interactive view on the terminal.
func Run(src Source, in io.Reader, out io.Writer) error {
	p := tea.NewProgram(New(src, 2*time.Second), tea.WithInput(in), tea.WithOutput(out), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
