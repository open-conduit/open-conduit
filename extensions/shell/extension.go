package shell

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/open-conduit/open-conduit/extension/inproc"
)

// Name is the shell executor's registration name.
const Name = "shell"

// Extension registers the shell executor through the in-process contract
// (#357): a job, in the jobs capacity pool. Before Start (configuration
// validation) the executor validates and describes config but runs
// nothing; Start opens it with the host's state directory, artifacts and
// toolbelt.
type Extension struct{ exec *Executor }

// NewExtension returns the extension, its executor not yet opened.
func NewExtension() *Extension { return &Extension{exec: &Executor{}} }

var (
	_ inproc.Executing = (*Extension)(nil)
	_ inproc.Starter   = (*Extension)(nil)
)

func (*Extension) Name() string { return Name }

func (x *Extension) Executors() map[string]inproc.Executor {
	return map[string]inproc.Executor{Name: {Executor: x.exec, Pool: inproc.PoolJobs}}
}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	var dir string
	if h.ExecutorDir != nil {
		dir = h.ExecutorDir(Name)
	}
	if dir == "" && h.StateDir != "" {
		dir = filepath.Join(h.StateDir, "executors", Name)
	}
	if dir == "" {
		return errors.New("shell: the host named no state directory")
	}
	return x.exec.Open(Options{StateDir: dir, Artifacts: h.Artifacts, ToolbeltDir: h.ToolbeltDir, Now: h.Now})
}
