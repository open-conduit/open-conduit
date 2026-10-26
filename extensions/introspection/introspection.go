// Package introspection is the first-party issue-reporting bootstrap
// (03-08): agents report defects they hit in early daily use through public
// extension interfaces only. A scope's `introspection` entry enables it; the
// extension registers its toolbelt operations, declares the
// `introspection.issue.reported` family and keeps a report index in its own
// coordination KV namespace. A report is a translated toolbelt emission, so
// it is an ordinary derived Event of the reporting execution's trigger, under
// the core chain-depth and per-chain rate limits like any other. Processing a
// report is ordinary Work: the `introspection` route (bound explicitly to a
// user-chosen binding in configuration) gives each report Work of its own,
// and the triage playbook runs as manual Work. There is no executor, timer or
// privileged path here; stage 05 (05-03) completes sweep, schedules and the
// report lifecycle on top of it.
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

// WorkKind is the Work one report is processed as.
const WorkKind = "introspection.report"

// TypeReported is the durable report Event family.
const TypeReported = "introspection.issue.reported"

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

// ParseReportKey returns the report ID of a scope's report key.
func ParseReportKey(scope domain.ID, k domain.CorrelationKey) (string, bool) {
	if k.Source != SourceNamespace(scope) {
		return "", false
	}
	id, ok := strings.CutPrefix(k.ExternalID, "report/")
	return id, ok && id != ""
}

// Config is an entry's opaque config. It has no fields yet: the entry's
// presence enables reporting for its scope (and scopes inheriting it).
type Config struct{}

// ValidateExtension validates an `introspection` entry: an empty object and
// no secrets.
func ValidateExtension(config json.RawMessage, secretNames []string) error {
	if len(secretNames) > 0 {
		return errors.New("introspection takes no secrets")
	}
	var c Config
	return strict(config, &c)
}

// ValidateRoute validates the `introspection` route's config: an empty
// object. The binding is the route's own, chosen by the user.
func ValidateRoute(config json.RawMessage) error {
	var c struct{}
	return strict(config, &c)
}

// Enabled reports whether the effective scope (core.config.scope.v1
// metadata) is enabled and has an `introspection` entry.
func Enabled(scope domain.Scope) bool {
	var meta struct {
		Enabled    bool `json:"enabled"`
		Extensions []struct {
			Type string `json:"type"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil || !meta.Enabled {
		return false
	}
	for _, x := range meta.Extensions {
		if x.Type == Type {
			return true
		}
	}
	return false
}

// Route is the deterministic `introspection` route handler. A report Event
// addressed to a new report key creates that report's `introspection.report`
// Work; later Events on the key are its input. It matches nothing in a scope
// without an `introspection` entry, so removing the entry stops processing
// even for reports already in flight.
type Route struct{}

var _ extension.RouteHandler = Route{}

// Match selects report Events.
func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Type != TypeReported || e.CorrelationKey == nil || !Enabled(in.Scope) {
		return extension.RouteDecision{}, nil
	}
	id, ok := ParseReportKey(in.Scope.ID, *e.CorrelationKey)
	if !ok {
		return extension.RouteDecision{}, nil
	}
	if in.Work == nil {
		return extension.RouteDecision{Match: true, Kind: WorkKind, CreateID: WorkID(in.Scope.ID, id)}, nil
	}
	if in.Work.Kind != WorkKind {
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
