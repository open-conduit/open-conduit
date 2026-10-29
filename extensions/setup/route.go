package setup

import (
	"context"
	"encoding/json"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Route is the `setup` route handler. It takes only the Events the
// extension's own actions produce (source core.action) on a session key of
// the route's scope: setup.start gives an unowned key its session Work,
// with the chosen model as the Work's context, and setup.message and
// setup.answer are more input for the session that owns the key. Nothing
// the agent emits is taken, so its own words never wake it.
type Route struct{}

var (
	_ extension.RouteHandler = Route{}
	_ extension.RouteSampler = Route{}
)

func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	applied := e.Source == Type && e.Type == TypeApplied
	if e.Source != extension.ActionSource && !applied {
		return extension.Skip("only the operator's setup actions, and the generation that applies a session's changes, reach a session"), nil
	}
	if e.CorrelationKey == nil {
		return extension.Skip("the event names no setup session"), nil
	}
	id, ok := sessionID(in.Scope.ID, *e.CorrelationKey)
	if !ok {
		return extension.Skip("not a setup session of this scope"), nil
	}
	if in.Work == nil {
		if e.Type != TypeStart {
			return extension.Skip("no setup session owns this key"), nil
		}
		var s Start
		if e.Data.Schema != SchemaStart || json.Unmarshal(e.Data.Data, &s) != nil {
			return extension.Skip("the start is unreadable"), nil
		}
		return extension.RouteDecision{Match: true, Kind: WorkKind, CreateID: WorkID(in.Scope.ID, id),
			Context: payload(SchemaContext, s), Grants: s.Grants, Display: &domain.WorkDisplay{Title: "Setup session"},
			Reason: "the operator started a setup session"}, nil
	}
	if in.Work.Kind != WorkKind {
		return extension.Skip("the key is owned by other Work"), nil
	}
	switch e.Type {
	case TypeMessage:
		return extension.RouteDecision{Match: true, Reason: "the operator's message to the session"}, nil
	case TypeAnswer:
		return extension.RouteDecision{Match: true, Reason: "the operator's answer to the session"}, nil
	case TypeDecision:
		return extension.RouteDecision{Match: true, Reason: "the operator's decision on the session's changes"}, nil
	case TypeChecks:
		return extension.RouteDecision{Match: true, Reason: "the operator's validation of the session's changes"}, nil
	case TypeApplied:
		return extension.RouteDecision{Match: true, Reason: "the operator applied the session's changes"}, nil
	}
	return extension.Skip("the session has already started"), nil
}

// Samples offers starting a session.
func (Route) Samples(_ context.Context, in extension.SampleInput) []extension.RouteSample {
	key := SessionKey(in.Scope.ID, "example")
	return []extension.RouteSample{{ID: "session-started", Title: "The operator starts a setup session",
		Event: domain.Event{Type: TypeStart, Source: extension.ActionSource, CorrelationKey: &key, Data: payload(SchemaStart, Start{})}}}
}
