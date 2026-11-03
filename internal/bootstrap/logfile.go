package bootstrap

import "github.com/open-conduit/open-conduit/protocol/sdk"

// The daemon's JSON-lines logs (lifecycle, telemetry events, an in-process
// extension's own) rotate by size, through the same writer an extension
// package's OpenLog gets (sdk.RotatingLog).
type rotatingLog = sdk.RotatingLog

func openLog(path string) (*rotatingLog, error) { return sdk.OpenLog(path) }
