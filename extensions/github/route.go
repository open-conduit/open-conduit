package github

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// WorkKind is the kind of Work claimed for an issue generation.
const WorkKind = "github.issue"

// TypeClaimRequested is an operator's claim by hand: a core.manual Event
// whose correlation key is the entry's `issue/N`. It creates Work for the
// issue whatever its labels (v1 manual claim overriding routes).
const TypeClaimRequested = "github.claim.requested"

// ManualSource is the core source of operator-submitted Events.
const ManualSource = "core.manual"

// Route is the deterministic `github` route handler: the adapter's opt-in/skip
// claim policy and input selection. It reads only the detached Event, Scope
// and Work snapshot. Labels inform the claim decision for an unowned issue and
// nothing else; Work lifecycle never depends on them.
type Route struct{}

var _ extension.RouteHandler = Route{}

// scopeExtensions decodes the extension list from the effective scope
// metadata (core.config.scope.v1), ignoring everything else in it.
func scopeExtensions(scope domain.Scope) []struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
} {
	var meta struct {
		Extensions []struct {
			Type   string          `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil {
		return nil
	}
	return meta.Extensions
}

// settingsFor finds the github entry whose namespace produced the event.
func settingsFor(scope domain.Scope, source string) (Settings, bool) {
	for _, x := range scopeExtensions(scope) {
		if x.Type != Type {
			continue
		}
		s, err := parseSettings(x.Config, nil, false)
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

// Admits is the claim admission rule (Open Supervisor parity): no skip label,
// one of the required labels when any are configured, and not carrying the
// claimed label unless this is a reopened generation (a claimed, unowned issue
// belongs to another owner). Scheduled issues are claimed by HoldRoute.
func (l Labels) Admits(p IssuePayload) bool {
	if p.State != "open" || has(p.Labels, l.Scheduled) {
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
		if n, _, ok := ParseIssueKey(k.ExternalID); ok && strings.TrimPrefix(a.From, "#") == strconv.Itoa(n) {
			return true
		}
	}
	return a.From == ""
}

// Match selects input for existing Work and creates Work for admitted issues.
func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if !strings.HasPrefix(e.Type, "github.") || e.Type == TypeDelivery {
		return extension.RouteDecision{}, nil
	}
	source := e.Source
	if e.Type == TypeHoldReleased && e.CorrelationKey != nil {
		// Emitted by the hold executor: its source is the execution.
		source = e.CorrelationKey.Source
	}
	if e.Type == TypeClaimRequested {
		// Only an operator claims by hand; the key names the entry's issue.
		if e.Source != ManualSource || e.CorrelationKey == nil {
			return extension.RouteDecision{}, nil
		}
		if _, generation, ok := ParseIssueKey(e.CorrelationKey.ExternalID); !ok || generation != "" {
			return extension.RouteDecision{}, nil
		}
		source = e.CorrelationKey.Source
	}
	s, ok := settingsFor(in.Scope, source)
	if !ok || e.CorrelationKey == nil {
		return extension.RouteDecision{}, nil
	}
	if e.Type == TypeClaimRequested {
		// A manual claim overrides the admission labels (v1 claim by hand).
		// It never feeds existing Work, which already owns the issue.
		return extension.RouteDecision{Match: in.Work == nil, Kind: WorkKind}, nil
	}
	if in.Work == nil {
		if e.Type != TypeIssueOpen || e.Data.Schema != SchemaIssue {
			return extension.RouteDecision{}, nil
		}
		var p IssuePayload
		if json.Unmarshal(e.Data.Data, &p) != nil || !s.Labels.Admits(p) {
			return extension.RouteDecision{}, nil
		}
		return extension.RouteDecision{Match: true, Kind: WorkKind}, nil
	}
	switch e.Type {
	case TypeIssueClosed, TypePullMerged, TypePullClosed, TypeChecksPassed, TypeChecksFailed, TypeChecksNone, TypeChecksMissing:
		return extension.RouteDecision{Match: true}, nil
	case TypeHoldReleased:
		return extension.RouteDecision{Match: in.Event.Event.WorkID == in.Work.ID}, nil
	case TypeCommentHuman, TypeCommentAgent:
		var p CommentPayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Edited {
			return extension.RouteDecision{}, nil
		}
		if p.Kind == KindReview && strings.TrimSpace(p.Body) == "" {
			return extension.RouteDecision{}, nil
		}
		if e.Type == TypeCommentAgent && selfAuthored(p.Agent, in.Work) {
			return extension.RouteDecision{}, nil
		}
		return extension.RouteDecision{Match: true}, nil
	}
	return extension.RouteDecision{}, nil
}
