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

// TypeClaimRequested is an operator's claim by hand: the Event of the
// entry's "claim" action (Actions), accepted under extension.ActionSource,
// whose correlation key is the entry's `issue/N`. It creates Work for the
// issue whatever its labels (v1 manual claim overriding routes). The same
// Event posted any other way claims nothing.
const TypeClaimRequested = "github.claim.requested"

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

// refusal says why Admits refuses p ("" when it admits it).
func (l Labels) refusal(p IssuePayload) string {
	switch {
	case p.State != "open":
		return "the issue is " + p.State
	case has(p.Labels, l.Scheduled):
		return "the issue carries " + l.Scheduled + "; a hold route claims it"
	}
	for _, s := range l.Skip {
		if has(p.Labels, s) {
			return "the issue carries the skip label " + s
		}
	}
	if has(p.Labels, l.Claimed) && p.Generation == "" {
		return "the issue already carries " + l.Claimed
	}
	if !l.Admits(p) {
		return "the issue carries none of " + strings.Join(l.Require, ", ")
	}
	return ""
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

// Match selects input for existing Work and creates Work for admitted
// issues, saying what the Work is when the input tells (display).
func (r Route) Match(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	d, err := r.match(ctx, in)
	if err == nil && d.Match {
		d.Display = display(in.Work, in.Event.Event)
	}
	return d, err
}

func (Route) match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if !strings.HasPrefix(e.Type, "github.") || e.Type == TypeDelivery {
		return extension.Skip("not a GitHub event"), nil
	}
	source := e.Source
	if e.Type == TypeHoldReleased && e.CorrelationKey != nil {
		// Emitted by the hold executor: its source is the execution.
		source = e.CorrelationKey.Source
	}
	if e.Type == TypeClaimRequested {
		// Only the claim action claims by hand; the key names the entry's issue.
		if e.Source != extension.ActionSource || e.CorrelationKey == nil {
			return extension.Skip("a claim comes only from the claim action"), nil
		}
		if _, generation, ok := ParseIssueKey(e.CorrelationKey.ExternalID); !ok || generation != "" {
			return extension.Skip("a claim names an issue number"), nil
		}
		source = e.CorrelationKey.Source
	}
	s, ok := settingsFor(in.Scope, source)
	if !ok {
		return extension.Skip("no github entry in this scope reads " + source), nil
	}
	if e.CorrelationKey == nil {
		return extension.Skip("the event names no issue or pull request"), nil
	}
	if e.Type == TypeClaimRequested {
		// A manual claim overrides the admission labels (v1 claim by hand).
		// It never feeds existing Work, which already owns the issue.
		if in.Work != nil {
			return extension.Skip("the issue already has Work"), nil
		}
		return extension.RouteDecision{Match: true, Kind: WorkKind, Reason: "claimed by hand"}, nil
	}
	if in.Work == nil {
		if e.Type != TypeIssueOpen || e.Data.Schema != SchemaIssue {
			return extension.Skip("only an opened issue starts Work"), nil
		}
		var p IssuePayload
		if json.Unmarshal(e.Data.Data, &p) != nil {
			return extension.Skip("the issue data is unreadable"), nil
		}
		if reason := s.Labels.refusal(p); reason != "" {
			return extension.Skip(reason), nil
		}
		return extension.RouteDecision{Match: true, Kind: WorkKind, Reason: "an open issue the labels admit"}, nil
	}
	switch e.Type {
	case TypeIssueClosed, TypePullMerged, TypePullClosed, TypeChecksPassed, TypeChecksFailed, TypeChecksNone, TypeChecksMissing:
		return extension.RouteDecision{Match: true, Reason: "news for the issue's agent"}, nil
	case TypeHoldReleased:
		if in.Event.Event.WorkID != in.Work.ID {
			return extension.Skip("released for other Work"), nil
		}
		return extension.RouteDecision{Match: true, Reason: "the hold ended; the agent starts"}, nil
	case TypeCommentHuman, TypeCommentAgent:
		var p CommentPayload
		if json.Unmarshal(e.Data.Data, &p) != nil {
			return extension.Skip("the comment data is unreadable"), nil
		}
		if p.Edited {
			return extension.Skip("an edited comment is not news"), nil
		}
		if p.Kind == KindReview && strings.TrimSpace(p.Body) == "" {
			return extension.Skip("a review with no text"), nil
		}
		if e.Type == TypeCommentAgent && selfAuthored(p.Agent, in.Work) {
			return extension.Skip("the agent's own comment"), nil
		}
		return extension.RouteDecision{Match: true, Reason: "a comment for the issue's agent"}, nil
	}
	return extension.Skip(e.Type + " is not passed to the agent"), nil
}
