// Package introspection is the first-party introspection extension: issue
// reporting (03-08), plus the daily sweep and triage, evidence aggregation,
// fix work, an optional host watch and telemetry events (05-03). Everything
// goes through public extension interfaces; nothing about it is core.
//
// A scope's `introspection` entry enables it. The extension registers its
// toolbelt operations and Event families, keeps its records in its own
// coordination KV namespaces and owns four Work kinds, each created by the
// `introspection` route on a binding the user chose:
//
//   - introspection.report: one per report. A report is a translated
//     toolbelt emission, an ordinary derived Event of the reporting
//     execution's trigger under the core chain-depth and per-chain limits.
//   - introspection.sweep and introspection.triage: each scope's standing
//     daily runs, started by the extension's own calendar timers (core timer
//     recurrences) and reading the agents' notes and reports as evidence.
//   - introspection.fix: one per fix a triage requests. Its Event cites the
//     report it fixes as its cause, so a fix traces to the report and the
//     report's origin through causation.
//
// Removing the entry removes the operations, routes and timers; see the
// removal policy in Removal.
//
// It depends only on public contracts.
package introspection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// Type is the registered extension type, toolbelt registration and route
// handler name.
const Type = "introspection"

// Work kinds the extension owns.
const (
	// WorkKind is the Work one report is processed as.
	WorkKind = "introspection.report"
	// WorkKindSweep and WorkKindTriage are each scope's standing daily runs.
	WorkKindSweep  = "introspection.sweep"
	WorkKindTriage = "introspection.triage"
	// WorkKindFix is the Work one requested fix is made as.
	WorkKindFix = "introspection.fix"
)

// Event families the extension declares.
const (
	// TypeReported is the durable report Event family.
	TypeReported = "introspection.issue.reported"
	// TypeSweepDue and TypeTriageDue are the daily runs' timer slots.
	TypeSweepDue  = "introspection.sweep.due"
	TypeTriageDue = "introspection.triage.due"
	// TypeFixRequested is a fix a triage asked for, derived from the report
	// it fixes.
	TypeFixRequested = "introspection.fix.requested"
)

// SchemaReport is the report Event payload and index record schema.
const SchemaReport = "introspection.report.v1"

// Namespace is the coordination KV namespace of the report index.
const Namespace = "introspection.reports"

// Kinds are the kinds of defect an agent can report: Open Supervisor's
// problem kinds plus runtime (Open Conduit itself misbehaving).
var Kinds = []string{"env", "docs", "tooling", "service", "runtime", "other"}

// SourceNamespace is the correlation source of a scope's report keys.
func SourceNamespace(scope domain.ID) string { return Type + "/" + string(scope) }

// ReportKey is a report's correlation key, owned by its processing Work.
func ReportKey(scope domain.ID, id string) domain.CorrelationKey {
	return domain.CorrelationKey{Source: SourceNamespace(scope), ExternalID: "report/" + id}
}

// WorkID is the processing Work of a report.
func WorkID(scope domain.ID, id string) domain.ID {
	return domain.ID(Type + "/" + string(scope) + "/report/" + id)
}

// RunKey is the key of a scope's standing sweep or triage Work for one
// schedule epoch.
func RunKey(scope domain.ID, run, epoch string) domain.CorrelationKey {
	return domain.CorrelationKey{Source: SourceNamespace(scope), ExternalID: run + "/" + epoch}
}

// FixKey is a requested fix's correlation key, owned by its fix Work.
func FixKey(scope domain.ID, id string) domain.CorrelationKey {
	return domain.CorrelationKey{Source: SourceNamespace(scope), ExternalID: "fix/" + id}
}

// ParseReportKey returns the report ID of a scope's report key.
func ParseReportKey(scope domain.ID, k domain.CorrelationKey) (string, bool) {
	if k.Source != SourceNamespace(scope) {
		return "", false
	}
	id, ok := strings.CutPrefix(k.ExternalID, "report/")
	return id, ok && id != ""
}

// Route config `work` values: what a route's binding processes. Each is an
// extension-owned Work kind on a binding the user chose.
const (
	WorkReport = "report"
	WorkSweep  = "sweep"
	WorkTriage = "triage"
	WorkFix    = "fix"
)

// RouteConfig is an `introspection` route's config.
type RouteConfig struct {
	// Work is what the route's binding processes: report (default), sweep,
	// triage or fix.
	Work string `json:"work,omitempty"`
}

// ParseRoute decodes a route's config.
func ParseRoute(config json.RawMessage) (RouteConfig, error) {
	var c RouteConfig
	if err := strict(config, &c); err != nil {
		return c, err
	}
	if c.Work == "" {
		c.Work = WorkReport
	}
	if _, ok := works[c.Work]; !ok {
		return c, errors.New("work must be report, sweep, triage or fix")
	}
	return c, nil
}

// ValidateRoute validates the `introspection` route's config. The binding
// is the route's own, chosen by the user.
func ValidateRoute(config json.RawMessage) error {
	_, err := ParseRoute(config)
	return err
}

// work describes one extension-owned Work kind: the Event family that
// creates it and how its key is read.
type work struct {
	kind, event string
	// id parses the key's external ID into the Work's ID suffix.
	id func(external string) (string, bool)
}

var works = map[string]work{
	WorkReport: {WorkKind, TypeReported, prefixed("report/")},
	WorkSweep:  {WorkKindSweep, TypeSweepDue, prefixed("sweep/")},
	WorkTriage: {WorkKindTriage, TypeTriageDue, prefixed("triage/")},
	WorkFix:    {WorkKindFix, TypeFixRequested, prefixed("fix/")},
}

func prefixed(p string) func(string) (string, bool) {
	return func(x string) (string, bool) {
		id, ok := strings.CutPrefix(x, p)
		return x, ok && id != "" && !strings.Contains(id, "/")
	}
}

// Kinds of Work the extension owns, for host policy (WorkKinds) and removal.
func WorkKinds() []string { return []string{WorkKind, WorkKindSweep, WorkKindTriage, WorkKindFix} }

// Route is the deterministic `introspection` route handler. An Event of the
// route's family addressed to a new key in the scope's namespace creates
// that key's Work (one per report or fix; one standing sweep and triage per
// scope); later Events on the key are its input. It matches nothing in a
// scope without an `introspection` entry, so removing the entry stops
// processing even for Events already in flight.
type Route struct{}

var _ extension.RouteHandler = Route{}

// Match selects the route's Events.
func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.CorrelationKey == nil || e.CorrelationKey.Source != SourceNamespace(in.Scope.ID) || !Enabled(in.Scope) {
		return extension.RouteDecision{}, nil
	}
	cfg, err := ParseRoute(in.Route.Config.Data)
	if err != nil {
		return extension.RouteDecision{}, nil
	}
	w := works[cfg.Work]
	if e.Type != w.event {
		return extension.RouteDecision{}, nil
	}
	suffix, ok := w.id(e.CorrelationKey.ExternalID)
	if !ok {
		return extension.RouteDecision{}, nil
	}
	if in.Work == nil {
		return extension.RouteDecision{Match: true, Kind: w.kind, CreateID: domain.ID(SourceNamespace(in.Scope.ID) + "/" + suffix)}, nil
	}
	if in.Work.Kind != w.kind {
		return extension.RouteDecision{}, nil
	}
	return extension.RouteDecision{Match: true}, nil
}

func strict(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
