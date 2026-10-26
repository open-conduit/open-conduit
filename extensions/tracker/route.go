package tracker

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// ManualSource is the core source of operator-submitted Events.
const ManualSource = "core.manual"

// WorkKind is the kind of Work claimed for an issue generation of a tracker.
func WorkKind(typ string) string { return typ + ".issue" }

// Route is a tracker's deterministic route handler (handler name = Type):
// the opt-in/skip claim policy, the route's optional attribute match and
// input selection. It reads only the detached Event, Scope, Work snapshot and
// route config. Labels and statuses inform the claim decision for an
// unowned issue and nothing else; Work lifecycle never depends on them.
type Route struct{ Type string }

var _ extension.RouteHandler = Route{}

// settingsFor finds the tracker entry whose namespace produced the event.
func (r Route) settingsFor(scope domain.Scope, source string) (Settings, bool) {
	var meta struct {
		Extensions []struct {
			Type   string          `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil {
		return Settings{}, false
	}
	for _, x := range meta.Extensions {
		if x.Type != r.Type {
			continue
		}
		s, err := ParseLenient(r.Type, x.Config)
		if err == nil && s.Namespace == source && s.Scope == string(scope.ID) {
			return s, true
		}
	}
	return Settings{}, false
}

func has(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// Admits is the claim admission rule (Open Supervisor parity): the issue is
// open and not waiting, carries no skip label, one of the required labels
// when any are configured, and not the claimed label unless this is a
// reopened generation (a claimed, unowned issue belongs to another owner).
func (l Labels) Admits(p IssuePayload) bool {
	if p.State != "open" || p.Waiting {
		return false
	}
	for _, s := range l.Skip {
		if has(p.Labels, s) {
			return false
		}
	}
	if has(p.Labels, l.Claimed) && p.Generation == "" {
		return false
	}
	if len(l.Require) == 0 {
		return true
	}
	for _, r := range l.Require {
		if has(p.Labels, r) {
			return true
		}
	}
	return false
}

// selfAuthored reports whether an agent comment was written for this Work
// (its own note), which is never handed back to it.
func selfAuthored(a *Agent, w *domain.Work) bool {
	if a == nil {
		return false
	}
	if a.Work != "" {
		return a.Work == string(w.ID)
	}
	for _, k := range w.CorrelationKeys {
		if key, _, ok := ParseIssueKey(k.ExternalID); ok && strings.EqualFold(a.From, key) {
			return true
		}
	}
	return a.From == ""
}

// Match selects input for existing Work and creates Work for admitted issues.
func (r Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	t := TypesOf(r.Type)
	if !strings.HasPrefix(e.Type, r.Type+".") || e.Type == t.Delivery {
		return extension.RouteDecision{}, nil
	}
	source := e.Source
	if e.Type == t.ClaimRequested {
		// Only an operator claims by hand; the key names the entry's issue.
		if e.Source != ManualSource || e.CorrelationKey == nil {
			return extension.RouteDecision{}, nil
		}
		if _, generation, ok := ParseIssueKey(e.CorrelationKey.ExternalID); !ok || generation != "" {
			return extension.RouteDecision{}, nil
		}
		source = e.CorrelationKey.Source
	}
	s, ok := r.settingsFor(in.Scope, source)
	if !ok || e.CorrelationKey == nil {
		return extension.RouteDecision{}, nil
	}
	if e.Type == t.ClaimRequested {
		// A manual claim overrides labels and match; it never feeds existing Work.
		return extension.RouteDecision{Match: in.Work == nil, Kind: WorkKind(r.Type)}, nil
	}
	if in.Work == nil {
		if e.Type != t.IssueOpen || e.Data.Schema != t.SchemaIssue {
			return extension.RouteDecision{}, nil
		}
		var p IssuePayload
		if json.Unmarshal(e.Data.Data, &p) != nil || !s.Labels.Admits(p) {
			return extension.RouteDecision{}, nil
		}
		if len(in.Route.Config.Data) > 0 {
			rc, err := ParseRoute(in.Route.Config.Data)
			if err != nil || !rc.Matches(p) {
				return extension.RouteDecision{}, nil
			}
		}
		return extension.RouteDecision{Match: true, Kind: WorkKind(r.Type)}, nil
	}
	switch e.Type {
	case t.IssueClosed:
		return extension.RouteDecision{Match: true}, nil
	case t.CommentHuman, t.CommentAgent:
		var p CommentPayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Edited {
			return extension.RouteDecision{}, nil
		}
		if e.Type == t.CommentAgent && selfAuthored(p.Agent, in.Work) {
			return extension.RouteDecision{}, nil
		}
		return extension.RouteDecision{Match: true}, nil
	}
	return extension.RouteDecision{}, nil
}
