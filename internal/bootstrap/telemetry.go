package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The host's telemetry events log (logs/introspection-telemetry.jsonl): one
// JSON line per named telemetry event an extension captures through
// inproc.Host.Capture, which every inproc.Sink (PostHog entries) receives
// too. Whether an extension captures anything is its own config's choice.

type telemetryLog struct {
	mu   sync.Mutex
	file *os.File
}

// Capture writes the event, with its scope (when it has one) among the
// properties.
func (l *telemetryLog) Capture(_ context.Context, scope, name string, at time.Time, props map[string]any) error {
	if scope != "" {
		all := map[string]any{"scope": scope}
		for k, v := range props {
			all[k] = v
		}
		props = all
	}
	line, err := json.Marshal(map[string]any{"event": name, "at": at.UTC(), "properties": props})
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.file.Write(append(line, '\n'))
	return err
}

// openTelemetry opens the telemetry events log behind the fan-out to the
// extensions' sinks.
func (d *daemon) openTelemetry() error {
	f, err := openLog(filepath.Join(filepath.Dir(d.o.LifecycleLog), "introspection-telemetry.jsonl"))
	if err != nil {
		return err
	}
	d.telemetryFile = f
	d.telemetry = telemetryFanout{primary: &telemetryLog{file: f}, sinks: d.sinks}
	return nil
}

func (d *daemon) closeTelemetry() {
	if d.telemetryFile != nil {
		_ = d.telemetryFile.Close()
	}
}

// registeredOnly narrows a binding's granted operations to those registered
// on the generation's toolbelt, so a binding that still lists the operations
// of a disabled extension keeps running without them (its calls are
// undeclared) instead of failing to obtain a token.
func registeredOnly(granted, registered []string) []string {
	known := map[string]bool{}
	for _, op := range registered {
		known[op] = true
	}
	out := make([]string, 0, len(granted))
	for _, op := range granted {
		if known[op] {
			out = append(out, op)
		}
	}
	return out
}
