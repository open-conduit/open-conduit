package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/executors/claudecode"
)

// ClaudeCodeExecutor is the registration name bindings use for the Claude
// Code executor (Open Supervisor's agent adapter type).
const ClaudeCodeExecutor = "claude-code"

// claudeState is the host side of the Claude Code executor: its telemetry
// log and the background Maintain pass.
type claudeState struct {
	executor *claudecode.Executor
	mu       sync.Mutex
	running  bool
	last     time.Time
	log      *os.File
	logMu    sync.Mutex
}

// openClaudeCode registers the executor. Its private continuity lives in the
// coordination KV under its own namespace; per-turn telemetry goes to a local
// JSON-lines log outside core (an exporter extension may ship it later).
func (d *daemon) openClaudeCode() error {
	path := filepath.Join(d.o.StateDir, "logs", "claude-code.jsonl")
	f, err := openLog(path)
	if err != nil {
		return err
	}
	d.claude.log = f
	if err = d.preparePlaybooks(); err != nil {
		f.Close()
		return err
	}
	d.claude.executor, err = claudecode.New(claudecode.Options{
		StateDir: filepath.Join(d.o.StateDir, "executors", ClaudeCodeExecutor), Store: d.coord, Artifacts: d.artifacts,
		ToolbeltDir: d.o.ToolbeltDir, Report: d.claudeReport, Now: d.now,
		Render: d.renderEvents, PluginDir: d.pluginDir(), DryRun: d.o.DryRun,
	})
	if err != nil {
		f.Close()
	}
	return err
}

func (d *daemon) closeClaudeCode() {
	if d.claude.log != nil {
		d.claude.log.Close()
	}
}

// claudeReport appends one telemetry record to the executor's local log.
func (d *daemon) claudeReport(name string, attrs map[string]any) {
	line, err := json.Marshal(map[string]any{"time": d.now().UTC(), "name": name, "attrs": attrs})
	if err != nil {
		return
	}
	d.claude.logMu.Lock()
	defer d.claude.logMu.Unlock()
	_, _ = d.claude.log.Write(append(line, '\n'))
}

// maintainClaudeCode runs the executor's housekeeping (releasing the
// continuity of terminal Work) in the background, at most once per interval,
// so it never stalls the loop.
func (d *daemon) maintainClaudeCode(ctx context.Context) {
	c := &d.claude
	c.mu.Lock()
	if c.running || c.executor == nil || d.now().Sub(c.last) < d.o.ExecutorInterval {
		c.mu.Unlock()
		return
	}
	c.running, c.last = true, d.now()
	c.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			c.mu.Lock()
			c.running = false
			c.mu.Unlock()
		}()
		_, err := c.executor.Maintain(ctx)
		code := ""
		if err != nil && ctx.Err() == nil {
			code = errorCode(err)
		}
		d.report("claude_code_maintain", "", code)
	}()
}
