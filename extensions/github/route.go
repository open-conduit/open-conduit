package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// WorkKind is the kind of Work claimed for an issue generation.
const WorkKind = "github.issue"

// WorkKindPull is the kind of Work claimed for a pull request: it owns the
// PR's own key (PullRequestKey), never its issue's key, so it runs alongside
// any Work that owns the issue the PR belongs to.
const WorkKindPull = "github.pull_request"

// What a `github` route claims (RouteConfig.Claim).
const (
	ClaimIssues       = "issues"
	TrackPullRequests = "pull_requests"
)

// RouteConfig is the opaque config of a `github` route. Claim is what it
// claims as Work: issues (the default) or pull_requests, each open PR's
// own Work, from the copies an entry with track_pull_requests
// observes. Drafts lets a pull_requests route claim draft PRs too; by
// default a draft is claimed once it is marked ready. Authors limits a
// pull_requests route to PRs opened by those logins (dependabot[bot],
// renovate[bot]), compared without case; a PR by anyone else falls through
// to the next route.
type RouteConfig struct {
	Claim   string   `json:"claim,omitempty"`
	Drafts  bool     `json:"drafts,omitempty"`
	Authors []string `json:"authors,omitempty"`
}

// ParseRoute decodes and defaults a `github` route's config.
func ParseRoute(data []byte) (RouteConfig, error) {
	var c RouteConfig
	if err := strict(data, &c); err != nil {
		return c, fmt.Errorf("github route: %w", err)
	}
	switch c.Claim {
	case "":
		c.Claim = ClaimIssues
	case ClaimIssues, TrackPullRequests:
	default:
		return c, errors.New("github route: claim must be issues or pull_requests")
	}
	if c.Drafts && c.Claim != TrackPullRequests {
		return c, errors.New("github route: drafts applies only to claim pull_requests")
	}
	if len(c.Authors) > 0 && c.Claim != TrackPullRequests {
		return c, errors.New("github route: authors applies only to claim pull_requests")
	}
	for _, a := range c.Authors {
		if strings.TrimSpace(a) == "" || strings.ContainsAny(a, " \t\n/") {
			return c, errors.New("github route: authors are GitHub logins, such as dependabot[bot]")
		}
	}
	return c, nil
}

// ValidateRoute is the config.Registry validator of a `github` route.
func ValidateRoute(data json.RawMessage) error {
	_, err := ParseRoute(data)
	return err
}

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
	rc, err := ParseRoute(in.Route.Config.Data)
	if err != nil {
		return extension.Skip("the route's config is invalid"), nil
	}
	if rc.Claim == TrackPullRequests {
		return matchPull(in, rc), nil
	}
	if in.Work != nil && in.Work.Kind == WorkKindPull {
		return extension.Skip("a PR's own Work takes input from a pull_requests route"), nil
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
	case TypeIssueClosed, TypePullMerged, TypePullClosed, TypePullConflicts, TypeChecksPassed, TypeChecksFailed, TypeChecksNone, TypeChecksMissing:
		return extension.RouteDecision{Match: true, Reason: "news for the issue's agent"}, nil
	case TypeHoldReleased:
		if in.Event.Event.WorkID != in.Work.ID {
			return extension.Skip("released for other Work"), nil
		}
		return extension.RouteDecision{Match: true, Reason: "the hold ended; the agent starts"}, nil
	case TypeCommentHuman, TypeCommentAgent:
		return commentNews(e, in.Work), nil
	}
	return extension.Skip(e.Type + " is not passed to the agent"), nil
}

// commentNews selects a human or agent comment for existing Work: new, with
// text, and not the Work's own.
func commentNews(e domain.Event, w *domain.Work) extension.RouteDecision {
	var p CommentPayload
	if json.Unmarshal(e.Data.Data, &p) != nil {
		return extension.Skip("the comment data is unreadable")
	}
	if p.Edited {
		return extension.Skip("an edited comment is not news")
	}
	if p.Kind == KindReview && strings.TrimSpace(p.Body) == "" {
		return extension.Skip("a review with no text")
	}
	if e.Type == TypeCommentAgent && selfAuthored(p.Agent, w) {
		return extension.Skip("the agent's own comment")
	}
	return extension.RouteDecision{Match: true, Reason: "a comment for the agent"}
}

// matchPull is a pull_requests route: it creates Work for an open PR's
// copy and selects that Work's input. It never touches an issue's Work, so
// the issue's Work and the PR's each hear their own copy of the PR's news.
func matchPull(in extension.RouteInput, rc RouteConfig) extension.RouteDecision {
	e := in.Event.Event
	if e.CorrelationKey == nil {
		return extension.Skip("the event names no pull request")
	}
	number, ok := ParsePullRequestKey(e.CorrelationKey.ExternalID)
	if !ok {
		return extension.Skip("not a pull request's own copy")
	}
	s, ok := settingsFor(in.Scope, e.Source)
	if !ok || !s.TrackPulls {
		return extension.Skip("no github entry in this scope tracks the pull requests of " + e.Source + " (track_pull_requests)")
	}
	if in.Work == nil {
		if e.Type != TypePullOpen || e.Data.Schema != SchemaPull {
			return extension.Skip("only an open PR starts Work")
		}
		var p PullPayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Number != number {
			return extension.Skip("the PR data is unreadable")
		}
		if p.State != "open" {
			return extension.Skip("the PR is " + p.State)
		}
		if p.Draft && !rc.Drafts {
			return extension.Skip("the PR is a draft; it is claimed once it is marked ready")
		}
		if len(rc.Authors) > 0 && !slices.ContainsFunc(rc.Authors, func(a string) bool { return strings.EqualFold(a, p.Author) }) {
			return extension.Skip("the PR is by " + p.Author + ", not one of this route's authors (" + strings.Join(rc.Authors, ", ") + ")")
		}
		return extension.RouteDecision{Match: true, Kind: WorkKindPull, Reason: "an open PR"}
	}
	if in.Work.Kind != WorkKindPull {
		return extension.Skip("not a PR's own Work")
	}
	// The same news an issue's Work hears about its PRs, and new versions
	// of the open PR (a push, a retitle, a draft change) as well.
	switch e.Type {
	case TypePullOpen:
		return extension.RouteDecision{Match: true, Reason: "the PR changed (a push, or its title, body or draft state)"}
	case TypePullMerged, TypePullClosed, TypePullConflicts, TypeChecksPassed, TypeChecksFailed, TypeChecksNone, TypeChecksMissing:
		return extension.RouteDecision{Match: true, Reason: "news for the PR's agent"}
	case TypeCommentHuman, TypeCommentAgent:
		return commentNews(e, in.Work)
	}
	return extension.Skip(e.Type + " is not passed to the agent")
}
