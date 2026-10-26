package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/open-conduit/open-conduit/extensions/posthog"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// PostHog telemetry (06-01). An enabled scope's `posthog` entry sends that
// scope's agent_turn events (one per finished execution, from the lifecycle
// pipeline) and the introspection extension's telemetry events through its
// connection. The exporter reads the current generation per batch, so an
// apply adds or removes entries without a restart; sinks are cached per
// connection config and resolve the project key lazily, so a rotated
// reference takes effect at once. Failures are counted by the pipeline and
// reported as a doctor code; they never touch the runtime.

type posthogState struct {
	mu    sync.Mutex
	sinks map[string]*posthog.Sink // by connection id + config
	// configs is published once configuration is open: the exporter runs
	// on the pipeline's goroutine from before that point.
	configs atomic.Pointer[management.ConfigService]
}

// posthogRegistry registers the connection and the (non-ingress) entry type.
func posthogRegistry(reg *config.Registry) {
	if reg.Connections == nil {
		reg.Connections = map[string]func(json.RawMessage) error{}
	}
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	reg.Connections[posthog.Type] = posthog.ValidateConnection
	reg.Extensions[posthog.Type] = config.ExtensionType{Validate: posthog.ValidateExtension}
}

type posthogEntry struct {
	scope, name, connection string
	settings                posthog.Settings
	sink                    *posthog.Sink
	err                     string
}

// posthogEntries lists the enabled scopes' posthog entries of the current
// generation with their sinks.
func (d *daemon) posthogEntries() []posthogEntry {
	configs := d.posthog.configs.Load()
	if configs == nil {
		return nil
	}
	snapshot, _ := configs.Current()
	if snapshot == nil {
		return nil
	}
	var out []posthogEntry
	for _, id := range snapshot.Order {
		scope := snapshot.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, x := range scope.Extensions {
			if x.Type != posthog.Type {
				continue
			}
			e := posthogEntry{scope: string(id), name: x.Name, connection: x.Connection}
			st, err := posthog.ParseExtension(x.Config)
			if err != nil {
				continue
			}
			e.settings = st
			var conn *config.Connection
			for i := range snapshot.Connections {
				if snapshot.Connections[i].ID == x.Connection && snapshot.Connections[i].Type == posthog.Type {
					conn = &snapshot.Connections[i]
				}
			}
			if conn == nil {
				e.err = "posthog_connection_type"
				out = append(out, e)
				continue
			}
			cfg, err := posthog.ParseConnection(conn.Config)
			if err != nil {
				e.err = "posthog_connection_invalid"
				out = append(out, e)
				continue
			}
			key := conn.ID + "\x00" + string(conn.Config)
			d.posthog.mu.Lock()
			if d.posthog.sinks == nil {
				d.posthog.sinks = map[string]*posthog.Sink{}
			}
			sink := d.posthog.sinks[key]
			if sink == nil {
				secrets := d.connectionSecrets(conn.ID)
				sink = posthog.NewSink(cfg, func(ctx context.Context) (string, error) { return secrets(ctx, posthog.SecretToken) }, nil)
				d.posthog.sinks[key] = sink
			}
			d.posthog.mu.Unlock()
			e.sink = sink
			out = append(out, e)
		}
	}
	return out
}

// posthogTargets are the entries the exporter and the introspection fan-out
// send to. A daemon dry run sends nothing.
func (d *daemon) posthogTargets() []posthog.Target {
	if d.o.DryRun {
		return nil
	}
	var out []posthog.Target
	for _, e := range d.posthogEntries() {
		if e.sink != nil {
			out = append(out, posthog.Target{Scope: e.scope, Settings: e.settings, Sink: e.sink})
		}
	}
	return out
}

func (d *daemon) posthogExporter() *posthog.Exporter {
	return &posthog.Exporter{Targets: d.posthogTargets, Diagnostic: func(code string) { d.report("posthog", "", code) }}
}

// posthogCheck reports the configured entries without service calls.
func (d *daemon) posthogCheck(context.Context) runtime.Check {
	check := runtime.Check{Name: "posthog", Status: runtime.CheckOK, Code: "ok"}
	entries := d.posthogEntries()
	if len(entries) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no PostHog entries are configured"
		return check
	}
	for _, e := range entries {
		item := e.scope + "/" + e.name + ": "
		if e.err != "" {
			check.Status, check.Code = runtime.CheckFail, e.err
			check.Items = append(check.Items, item+e.err)
			continue
		}
		var parts []string
		if e.settings.Turns {
			parts = append(parts, "agent_turn events")
		}
		if e.settings.Introspection {
			parts = append(parts, "introspection telemetry")
		}
		if d.o.DryRun {
			parts = append(parts, "dry run (nothing sent)")
		}
		check.Items = append(check.Items, item+joinOr(parts, "nothing selected"))
	}
	if code := d.reportedCode("posthog", ""); code != "" && check.Status == runtime.CheckOK {
		check.Status, check.Code = runtime.CheckWarn, "capture_failing"
		check.Items = append(check.Items, "last capture failed: "+code)
	}
	sort.Strings(check.Items)
	return check
}

// posthogAuthCheck (check_secrets only) resolves each entry's key privately
// and checks its form (PostHog cannot be asked about a key without sending
// an event, as in Open Supervisor).
func (d *daemon) posthogAuthCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "posthog_auth", Status: runtime.CheckOK, Code: "verified"}
	entries := d.posthogEntries()
	if len(entries) == 0 {
		check.Status, check.Code = runtime.CheckInfo, "not_configured"
		return check
	}
	for _, e := range entries {
		item := e.scope + "/" + e.name + ": "
		if e.sink == nil {
			check.Status, check.Code = runtime.CheckFail, e.err
			check.Items = append(check.Items, item+e.err)
			continue
		}
		where, err := e.sink.Check(ctx)
		if err != nil {
			code := "credential_unavailable"
			if errors.Is(err, posthog.ErrKey) {
				code = "invalid_key"
			}
			check.Status, check.Code = runtime.CheckFail, code
			check.Items = append(check.Items, item+code)
			continue
		}
		check.Items = append(check.Items, item+where)
	}
	sort.Strings(check.Items)
	return check
}

func joinOr(parts []string, empty string) string {
	if len(parts) == 0 {
		return empty
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}
