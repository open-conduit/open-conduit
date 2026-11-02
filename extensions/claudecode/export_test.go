package claudecode

// HoldFollowers keeps every turn launched from now on from following its
// output until release is closed. The returned func restores the default.
func HoldFollowers(release <-chan struct{}) func() {
	testHookFollow = func() { <-release }
	return func() { testHookFollow = nil }
}
