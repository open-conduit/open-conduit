package integration_test

import (
	"strconv"
	"strings"
)

// raceExitMillis is how long a race-built process waits at exit for late
// race reports (GORACE atexit_sleep_ms). The race runtime's default is a
// second, which every daemon, package process and certification session
// these tests start paid in turn as it exited: most of a package's
// certification, and enough over the package to push a plain go test
// -race ./... past go test's default ten-minute timeout (#491). A report is
// printed by the goroutine that finds the race, so a short wait still lets
// one finish.
const raceExitMillis = 50

// shortRaceExit adds raceExitMillis to the GORACE options gorace unless
// they set atexit_sleep_ms themselves.
func shortRaceExit(gorace string) string {
	if strings.Contains(gorace, "atexit_sleep_ms=") {
		return gorace
	}
	return strings.TrimSpace(gorace + " atexit_sleep_ms=" + strconv.Itoa(raceExitMillis))
}
