package schedule

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// ManualSource is the source of operator-submitted Events (runtime's
// core.manual), which the route accepts as manual job triggers.
const ManualSource = "core.manual"

// Route is the deterministic `schedule` route handler. A trigger correlates
// to its job's key in the entry's namespace; the route creates the job's one
// standing `schedule.job` Work for the first trigger and makes every later
// trigger that Work's input. It reads only the detached Event, Scope, Work
// and its own route config. It matches:
//
//   - timed slots, webhook POSTs and code-host deliveries from the entry's
//     own namespace, and
//   - operator Events (source core.manual, never core.* types) addressed to
//     the job's key: a manual run ("by hand").
//
// A trigger for a job that is no longer declared, or that this route's
// config does not select, does not match, so removing a job stops it even
// for triggers already in flight.
type Route struct{}

var _ extension.RouteHandler = Route{}

// Entries decodes the effective scope's schedule entries (core.config.scope.v1
// metadata), ignoring invalid ones.
func Entries(scope domain.Scope) []Settings {
	var meta struct {
		Extensions []struct {
			Type        string          `json:"type"`
			Config      json.RawMessage `json:"config"`
			SecretNames []string        `json:"secret_names"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil {
		return nil
	}
	var out []Settings
	for _, x := range meta.Extensions {
		if x.Type != Type {
			continue
		}
		if s, err := ParseExtension(x.Config, x.SecretNames); err == nil && s.Scope == string(scope.ID) {
			out = append(out, s)
		}
	}
	return out
}

// Match selects a job's triggers.
func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	key := e.CorrelationKey
	if key == nil || strings.HasPrefix(e.Type, "core.") {
		return extension.RouteDecision{}, nil
	}
	name, ok := ParseJobKey(key.ExternalID)
	if !ok {
		return extension.RouteDecision{}, nil
	}
	kind := ""
	switch {
	case e.Source == key.Source && e.Type == TypeSlot:
		kind = KindTimer
	case e.Source == key.Source && e.Type == TypeWebhook:
		kind = KindWebhook
	case e.Source == key.Source && e.Type == TypeEvent:
		kind = KindEvent
	case e.Source == ManualSource:
		kind = KindManual
	default:
		return extension.RouteDecision{}, nil
	}
	declared := false
	for _, s := range Entries(in.Scope) {
		if s.Namespace != key.Source {
			continue
		}
		_, declared = s.Job(name)
	}
	if !declared {
		return extension.RouteDecision{}, nil
	}
	if len(in.Route.Config.Data) > 0 {
		rc, err := ParseRoute(in.Route.Config.Data)
		if err != nil {
			return extension.RouteDecision{}, nil
		}
		if len(rc.Jobs) > 0 && !slices.Contains(rc.Jobs, name) {
			return extension.RouteDecision{}, nil
		}
		if len(rc.Triggers) > 0 && !slices.Contains(rc.Triggers, kind) {
			return extension.RouteDecision{}, nil
		}
	}
	if in.Work == nil {
		return extension.RouteDecision{Match: true, Kind: WorkKind, CreateID: domain.ID(WorkID(key.Source, name))}, nil
	}
	if in.Work.Kind != WorkKind {
		return extension.RouteDecision{}, nil
	}
	return extension.RouteDecision{Match: true}, nil
}

func delivery(e domain.Event) bool { return e.Type == TypeWebhook || e.Type == TypeEvent }

// SelectTrigger is the job Work kind's trigger policy (05-01, B09): the
// one-pending-run rule. Every trigger accepted while a run goes (or before
// the next run starts) waits as ordered input, and the next attempt takes the
// whole batch, so at most one run is pending however many triggers arrive.
// Which trigger that run is for follows v1: the newest delivery (webhook or
// code-host event) takes the place of older deliveries and of any timer or
// manual request; without a delivery the earliest request stands, so a later
// timer slot never displaces a pending one. Nothing is dropped from history:
// the attempt still receives every input.
func SelectTrigger(inputs []domain.Event) int {
	for i := len(inputs) - 1; i >= 0; i-- {
		if delivery(inputs[i]) {
			return i
		}
	}
	return 0
}
