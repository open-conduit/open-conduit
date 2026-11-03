//go:build !race

package extproc

// raceEnv is empty outside race builds: see race_on.go.
const raceEnv = ""
