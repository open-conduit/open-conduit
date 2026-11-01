//go:build race

package extensiontest

// raceEnabled builds the daemon with the race detector too, so a race
// test of a package also race-checks the daemon it drives.
const raceEnabled = true
