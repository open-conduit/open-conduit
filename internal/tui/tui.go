// Package tui is the operator terminal view over the unified run history.
// It uses only the public management client: every executor's attempts
// render through the same row, detail and log code, with no executor,
// work-kind or domain branches. With Controls it also offers manual claim,
// run/retry, hold/release, cancel and attach, and shows extension context
// (Work annotations and Waits) as data.
//
// The list shows one row per Work by default, named by the Work's label
// (its first annotation, else its first correlation key), with how many
// turns it has had; g switches to one row per turn.
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

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
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
	labels   map[string]string
	perTurn  bool
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
	// Operator controls (nil when the source offers none).
	ctl      Controls
	work     *client.Work
	waits    []client.Wait
	confirm  *pending
	input    *string
	notice   string
	artifact int
}

// pending is an action awaiting y/n confirmation.
type pending struct {
	prompt string
	cmd    tea.Cmd
}

func New(src Source, interval time.Duration) Model {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	m := Model{src: src, interval: interval, width: 120, height: 30, now: time.Now, labels: map[string]string{}}
	if ctl, ok := src.(Controls); ok {
		m.ctl = ctl
	}
	return m
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
type labelsMsg map[string]string

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
	src, index := m.src, m.artifact
	return func() tea.Msg {
		a, ok := runview.LogArtifact(r)
		if !ok {
			return logMsg{err: fmt.Errorf("run has no artifacts")}
		}
		if index > 0 && len(r.Artifacts) > 0 {
			// t cycles through every artifact (log, transcript, stderr).
			a = r.Artifacts[index%len(r.Artifacts)]
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

// fetchLabels names the listed Work not yet named. Labels don't change, so
// each Work is looked up once; without controls the view has no Work to read.
func (m Model) fetchLabels() tea.Cmd {
	if m.ctl == nil {
		return nil
	}
	var ids []string
	for _, r := range m.runs {
		if _, ok := m.labels[r.WorkID]; !ok {
			m.labels[r.WorkID] = ""
			ids = append(ids, r.WorkID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ctl := m.ctl
	return func() tea.Msg {
		out := labelsMsg{}
		for _, id := range ids {
			ctx, cancel := call()
			w, err := ctl.Work(ctx, id)
			cancel()
			if err == nil {
				out[id] = WorkLabel(w)
			}
		}
		return out
	}
}

// WorkLabel is the human name of Work: the first annotation an extension
// gives it, with an "owner/repo#12" reference shortened to "repo#12", else
// its first correlation key, else its ID.
func WorkLabel(w client.Work) string {
	for _, a := range w.Annotations {
		if a.Value == "" {
			continue
		}
		if repo, ref, ok := strings.Cut(a.Value, "#"); ok && !strings.ContainsAny(a.Value, " :") {
			return repo[strings.LastIndex(repo, "/")+1:] + "#" + ref
		}
		return a.Value
	}
	for _, k := range w.CorrelationKeys {
		return k.Source + " " + k.ExternalID
	}
	return w.ID
}

func (m Model) label(workID string) string {
	if l := m.labels[workID]; l != "" {
		return l
	}
	return workID
}

// item is one list row: a run and its place among its Work's turns.
type item struct {
	run   client.Run
	turns string
}

// items are the list rows: the latest turn of each Work with its turn count,
// or with perTurn every turn as "k/n" of its Work (counting the runs listed).
func (m Model) items() []item {
	total := map[string]int{}
	for _, r := range m.runs {
		total[r.WorkID]++
	}
	seen := map[string]int{}
	var out []item
	for _, r := range m.runs {
		seen[r.WorkID]++
		n := total[r.WorkID]
		switch {
		case m.perTurn:
			out = append(out, item{r, fmt.Sprintf("%d/%d", n-seen[r.WorkID]+1, n)})
		case seen[r.WorkID] == 1:
			out = append(out, item{r, strconv.Itoa(n)})
		}
	}
	return out
}

// focus moves the cursor to the row of run id, else of its Work.
func (m *Model) focus(id, work string) {
	m.cursor = 0
	items := m.items()
	for i, it := range items {
		if it.run.WorkID == work {
			m.cursor = i
			break
		}
	}
	for i, it := range items {
		if it.run.ID == id {
			m.cursor = i
		}
	}
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.fetch(), m.tick()) }

func (m Model) selected() (client.Run, bool) {
	items := m.items()
	if m.cursor < 0 || m.cursor >= len(items) {
		return client.Run{}, false
	}
	return items[m.cursor].run, true
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
		current, _ := m.selected()
		m.runs = msg.runs
		m.focus(current.ID, current.WorkID)
		return m, m.fetchLabels()
	case labelsMsg:
		for id, l := range msg {
			m.labels[id] = l
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
	case workMsg:
		if msg.err != nil {
			m.err = errText(msg.err)
		}
		if msg.work.ID != "" {
			w := msg.work
			m.work, m.waits = &w, msg.waits
		}
	case actionMsg:
		if msg.err != nil {
			m.err, m.notice = errText(msg.err), ""
		} else {
			m.err, m.notice = "", msg.text
		}
		if m.mode == detailMode && m.detail != nil {
			return m, tea.Batch(m.fetchDetail(m.detail.ID, m.detail.WorkID), m.fetchWork(m.detail.WorkID))
		}
		return m, m.fetch()
	case tea.KeyMsg:
		return m.key(msg.String())
	}
	return m, nil
}

// prompted handles keys while a confirmation or text prompt is open.
func (m Model) prompted(k string) (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		p := m.confirm
		m.confirm = nil
		if k == "y" || k == "Y" {
			return m, p.cmd
		}
		m.notice = "cancelled"
		return m, nil
	}
	switch k {
	case "esc", "ctrl+c":
		m.input = nil
	case "enter":
		ref := strings.TrimSpace(*m.input)
		m.input = nil
		ctl := m.ctl
		return m, func() tea.Msg {
			ctx, cancel := call()
			defer cancel()
			id, err := ctl.Claim(ctx, ref)
			if err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{text: "claim " + ref + " accepted (event " + id + ")"}
		}
	case "backspace":
		if r := []rune(*m.input); len(r) > 0 {
			v := string(r[:len(r)-1])
			m.input = &v
		}
	default:
		if len([]rune(k)) == 1 {
			v := *m.input + k
			m.input = &v
		}
	}
	return m, nil
}

func (m Model) page() int { return max(m.height-4, 1) }

func (m Model) key(k string) (tea.Model, tea.Cmd) {
	if m.confirm != nil || m.input != nil {
		return m.prompted(k)
	}
	switch k {
	case "ctrl+c", "q":
		return m, tea.Quit
	}
	m.notice = ""
	switch m.mode {
	case listMode:
		if r, ok := m.selected(); ok {
			if next, cmd, handled := m.controlKey(k, r); handled {
				return next, cmd
			}
		}
		switch k {
		case "c":
			if m.ctl != nil {
				empty := ""
				m.input = &empty
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items())-1 {
				m.cursor++
			}
		case "g":
			current, _ := m.selected()
			m.perTurn = !m.perTurn
			m.focus(current.ID, current.WorkID)
		case "r":
			return m, m.fetch()
		case "f":
			m.filter = (m.filter + 1) % len(Filters)
			return m, m.fetch()
		case "enter":
			if r, ok := m.selected(); ok {
				m.work, m.waits = nil, nil
				return m, tea.Batch(m.fetchDetail(r.ID, r.WorkID), m.fetchWork(r.WorkID))
			}
		case "l":
			if r, ok := m.selected(); ok {
				m.artifact = 0
				return m, m.fetchLog(r)
			}
		}
	case detailMode:
		if m.detail != nil {
			if next, cmd, handled := m.controlKey(k, *m.detail); handled {
				return next, cmd
			}
		}
		switch k {
		case "esc", "backspace", "h", "left":
			m.mode, m.err = listMode, ""
			return m, m.fetch()
		case "l":
			if m.detail != nil {
				m.artifact = 0
				return m, m.fetchLog(*m.detail)
			}
		case "r":
			if m.detail != nil {
				return m, tea.Batch(m.fetchDetail(m.detail.ID, m.detail.WorkID), m.fetchWork(m.detail.WorkID))
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
		case "t":
			// Next artifact of the run (log, transcript, stderr).
			if r, ok := m.logRun(); ok {
				m.artifact++
				return m, m.fetchLog(r)
			}
		}
	}
	return m, nil
}

// logRun is the run whose log is shown.
func (m Model) logRun() (client.Run, bool) {
	if m.detail != nil {
		return *m.detail, true
	}
	return m.selected()
}

// listHeader prefixes the shared run columns with the Work and its turns.
const listHeader = "WORK                     TURNS " + runview.Header

// fitTail pads to n runes, or keeps the last n so an issue's number shows.
func fitTail(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return "…" + string(r[len(r)-n+1:])
	}
	return runview.Fit(s, n)
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
		items := m.items()
		view, keys := "by work", "enter detail · g every turn · l log · f filter · r refresh · q quit"
		if m.perTurn {
			view, keys = "every turn", "enter detail · g by work · l log · f filter · r refresh · q quit"
		}
		if m.ctl != nil {
			keys += " · c claim · R run/retry · H hold · a attach · x/X cancel"
		}
		works := map[string]bool{}
		for _, r := range m.runs {
			works[r.WorkID] = true
		}
		b.WriteString(boldS.Render("Open Conduit runs") + dimS.Render(fmt.Sprintf("  filter: %s  %s  %d work · %d run(s)   %s", filter, view, len(works), len(m.runs), keys)) + "\n")
		b.WriteString(dimS.Render(listHeader) + "\n")
		visible := max(m.height-4, 1)
		start := 0
		if m.cursor >= visible {
			start = m.cursor - visible + 1
		}
		for i := start; i < len(items) && i < start+visible; i++ {
			it := items[i]
			row := runview.Fit(fitTail(m.label(it.run.WorkID), 24)+" "+runview.Fit(it.turns, 5)+" "+runview.Row(it.run, now), max(m.width, 20))
			if i == m.cursor {
				row = selS.Render(row)
			}
			b.WriteString(row + "\n")
		}
		if len(items) == 0 {
			b.WriteString(dimS.Render("no runs") + "\n")
		}
	case detailMode:
		if m.detail != nil {
			b.WriteString(runview.Detail(*m.detail, m.attempts, now))
		}
		b.WriteString(WorkContext(m.work, m.waits))
		keys := "l log · r refresh · esc back · q quit"
		if m.ctl != nil {
			keys += " · R run/retry · H hold/release · a attach · x cancel run · X cancel work"
		}
		b.WriteString("\n" + dimS.Render(keys) + "\n")
	case logMode:
		b.WriteString(boldS.Render("Log "+m.logTitle) + dimS.Render("  ↑/↓ pgup/pgdn scroll · t next artifact · esc back · q quit") + "\n")
		end := min(m.offset+m.page(), len(m.log))
		for _, l := range m.log[m.offset:end] {
			b.WriteString(l + "\n")
		}
	}
	switch {
	case m.confirm != nil:
		b.WriteString(boldS.Render(m.confirm.prompt+" y/n") + "\n")
	case m.input != nil:
		b.WriteString(boldS.Render("claim (scope#number): "+*m.input+"▏") + dimS.Render("  enter submit · esc cancel") + "\n")
	case m.notice != "":
		b.WriteString(dimS.Render(m.notice) + "\n")
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
