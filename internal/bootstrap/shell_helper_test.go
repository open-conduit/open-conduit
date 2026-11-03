package bootstrap

import (
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/shell"
)

// withShell is offlineExecutors with the shell executor, which runs as a
// bundled package (#428) and so isn't registered in this test process.
func withShell() map[string]extension.Executor {
	executors := offlineExecutors()
	executors[shell.Name] = shell.NewExtension().Executors()[shell.Name].Executor
	return executors
}
