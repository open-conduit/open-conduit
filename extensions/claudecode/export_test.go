package claudecode

import (
	"context"
	"net/http"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// HoldFollowers keeps every turn launched from now on from following its
// output until release is closed. The returned func restores the default.
func HoldFollowers(release <-chan struct{}) func() {
	testHookFollow = func() { <-release }
	return func() { testHookFollow = nil }
}

// StubChecks replaces the usage endpoint and `--version` the check calls.
func (x *Extension) StubChecks(usage func(ctx context.Context, configDir string) ([]UsageWindow, error), version func(ctx context.Context, bin string) (string, error)) {
	x.usage, x.versionOf = usage, version
}

// OnTaskObserved runs fn between the task sweep's liveness check and its
// read of a task's exit record. The returned func restores the default.
func OnTaskObserved(fn func(task string)) func() {
	testHookObserved.Store(fn)
	return func() { testHookObserved.Store(func(string) {}) }
}

// Process is a process the leak guard found: its ID, its group and its
// argument vector.
type Process = procInfo

// Processes lists the host's processes as the executor's sweep sees them.
func Processes() ([]Process, error) { return scanProcesses() }

// StubAuth points the credential check at a fake Anthropic API, its
// client, a fake Claude login and the host's binding secrets.
func (x *Extension) StubAuth(apiURL string, client *http.Client, login func(ctx context.Context, configDir string) (string, error), secrets func(ctx context.Context, scope domain.ID, binding string, names []string) (map[string]string, error)) {
	x.apiURL, x.authHTTP, x.loginToken, x.h.BindingSecrets = apiURL, client, login, secrets
}

// SetProbeBudget bounds each binding's credential probe.
func (x *Extension) SetProbeBudget(d time.Duration) { x.probeBudget = d }
