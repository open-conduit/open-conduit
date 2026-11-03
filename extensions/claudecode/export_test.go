package claudecode

import "context"

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
