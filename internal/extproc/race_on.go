//go:build race

package extproc

// raceEnv is kept in a race-built host's package environment: the race
// detector's settings (GORACE) of a test host reach the race-built packages
// it starts, so a test can shorten their exit (atexit_sleep_ms) or direct
// their reports the same way for the whole process tree. A release host is
// never race-built and keeps the B05 baseline alone.
const raceEnv = "GORACE"
