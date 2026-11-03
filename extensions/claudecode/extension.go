package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
)

// Name is the Claude Code executor's registration name.
const Name = "claude-code"

// maintainPace is how often the executor's housekeeping (releasing the
// continuity of terminal Work) runs.
const maintainPace = 30 * time.Second

// Extension registers the Claude Code executor through the in-process
// contract (#357): an agent turn, in no job pool. Before Start
// (configuration validation) the executor validates and describes config
// but runs nothing; Start opens it with the host's services. Per-turn
// telemetry goes to its own log, logs/claude-code.jsonl in the state
// directory the host gives it (its package's, <state>/extensions/claude-code).
type Extension struct {
	exec *Executor
	h    inproc.Host

	mu  sync.Mutex
	log io.WriteCloser

	// usage and versionOf stand in for the usage endpoint and
	// `--version` in tests.
	usage     usageRead
	versionOf func(ctx context.Context, bin string) (string, error)
}

// NewExtension returns the extension, its executor not yet opened.
func NewExtension() *Extension { return &Extension{exec: &Executor{}} }

var (
	_ inproc.Executing = (*Extension)(nil)
	_ inproc.Starter   = (*Extension)(nil)
	_ inproc.Stopper   = (*Extension)(nil)
	_ inproc.Tasker    = (*Extension)(nil)

	_ extension.SetupProbes = (*Extension)(nil)
)

func (*Extension) Name() string { return Name }

func (x *Extension) Executors() map[string]inproc.Executor {
	return map[string]inproc.Executor{Name: {Executor: x.exec, Fields: bindingFields()}}
}

// SetupProbes are the executor's probes for setup sessions
// (claude-code.status).
func (*Extension) SetupProbes() []extension.SetupProbe { return Probes{}.SetupProbes() }

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.h = h
	if h.OpenLog != nil {
		l, err := h.OpenLog(Name)
		if err != nil {
			return err
		}
		x.mu.Lock()
		x.log = l
		x.mu.Unlock()
	}
	render := RenderEvents
	if h.Render != nil {
		render = func(events []domain.Event) string { return renderVia(h.Render, events) }
	}
	return x.exec.Open(Options{
		StateDir: executorDir(h), Store: h.Store, Artifacts: h.Artifacts,
		ToolbeltDir: h.ToolbeltDir, Report: x.report, Now: h.Now,
		Render: render, PluginDir: h.PlaybookDir, DryRun: h.DryRun,
	})
}

// renderVia renders each Event through the extension that owns its family,
// and the rest with the generic renderer.
func renderVia(own func(context.Context, domain.Event) (string, bool), events []domain.Event) string {
	var parts []string
	for _, e := range events {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		text, ok := own(ctx, e)
		cancel()
		if !ok {
			text = RenderEvents([]domain.Event{e})
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

func (x *Extension) Stop() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.log != nil {
		_ = x.log.Close()
		x.log = nil
	}
}

// report appends one telemetry record to the executor's log.
func (x *Extension) report(name string, attrs map[string]any) {
	now := time.Now
	if x.h.Now != nil {
		now = x.h.Now
	}
	line, err := json.Marshal(map[string]any{"time": now().UTC(), "name": name, "attrs": attrs})
	if err != nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.log != nil {
		_, _ = x.log.Write(append(line, '\n'))
	}
}

// Tasks: the executor's housekeeping, releasing the continuity of terminal
// Work.
func (x *Extension) Tasks() []inproc.Task {
	return []inproc.Task{{Name: "maintain", Pace: maintainPace, Run: func(ctx context.Context) error {
		_, err := x.exec.Maintain(ctx)
		return err
	}}}
}

// executorDir is the executor's state directory: the one the host names
// (inproc.Host.ExecutorDir), else StateDir/executors/claude-code.
func executorDir(h inproc.Host) string {
	if h.ExecutorDir != nil {
		if d := h.ExecutorDir(Name); d != "" {
			return d
		}
	}
	return filepath.Join(h.StateDir, "executors", Name)
}
