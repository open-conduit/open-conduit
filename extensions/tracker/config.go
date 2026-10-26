package tracker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	idPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	attrPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
)

// Strict decodes JSON rejecting unknown fields and trailing data; an empty
// document is an empty object.
func Strict(data []byte, out any) error {
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

// Labels are the projected and admission labels (Open Supervisor's names by
// default, so either runtime sees the other's claim).
type Labels struct {
	Claimed string   `json:"claimed,omitempty"`
	Help    string   `json:"help,omitempty"`
	Review  string   `json:"review,omitempty"`
	Done    string   `json:"done,omitempty"`
	Skip    []string `json:"skip,omitempty"`
	Require []string `json:"require,omitempty"`
}

// DefaultLabels matches Open Supervisor.
func DefaultLabels() Labels {
	return Labels{Claimed: "agent-in-progress", Help: "agent-needs-help", Review: "ready-for-review", Done: "might-be-done",
		Skip: []string{"backlog", "human", "milestone"}}
}

func (l Labels) merge(base Labels) Labels {
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	out := Labels{Claimed: pick(l.Claimed, base.Claimed), Help: pick(l.Help, base.Help), Review: pick(l.Review, base.Review),
		Done: pick(l.Done, base.Done), Skip: l.Skip, Require: l.Require}
	if out.Skip == nil {
		out.Skip = base.Skip
	}
	return out
}

// Statuses are the projected status labels; they are excluded from issue
// observations so status projection never generates input.
func (l Labels) Statuses() []string { return []string{l.Help, l.Review, l.Done} }

// Status maps a projected status onto its label.
func (l Labels) Status(status string) string {
	switch status {
	case "help":
		return l.Help
	case "review":
		return l.Review
	case "done":
		return l.Done
	}
	return ""
}

// TransitionStates are the states the projector can move an issue's workflow
// status for (Open Supervisor's stateClaimed…stateFinished fields).
var TransitionStates = []string{"claimed", "building", "review", "help", "done", "finished"}

// Projection controls label, transition and comment convergence.
type Projection struct {
	Disabled      bool `json:"disabled,omitempty"`
	VerifySeconds int  `json:"verify_seconds,omitempty"`
	DryRun        bool `json:"dry_run,omitempty"`
}

// Common is the part of every tracker entry the kit owns. Tracker packages
// embed it in their entry config beside their own fields.
type Common struct {
	// Namespace is the stable source identity and correlation-key source.
	Namespace string `json:"namespace"`
	// Scope must be the containing scope (an ingress entry).
	Scope  string  `json:"scope"`
	Labels *Labels `json:"labels,omitempty"`
	// Transitions name the workflow status to move the issue to for each
	// state (claimed, building, review, help, done, finished); a missing
	// state leaves the status alone.
	Transitions        map[string]string `json:"transitions,omitempty"`
	ClosedHorizonHours int               `json:"closed_horizon_hours,omitempty"`
	Projection         Projection        `json:"projection,omitempty"`
	// WebhookSecret names a selected scope secret used to verify deliveries.
	WebhookSecret string `json:"webhook_secret,omitempty"`
	// ManageWebhook lets the host create or re-point the tracker's webhook
	// at the listener's public URL (needs admin permission on the service).
	ManageWebhook bool `json:"manage_webhook,omitempty"`
}

// Settings is a parsed, defaulted entry (the kit's view).
type Settings struct {
	// Type is the tracker type ("linear", "jira"): the event family and
	// operation prefix.
	Type, Namespace, Scope string
	Labels                 Labels
	Transitions            map[string]string
	ClosedHorizon          time.Duration
	Projection             Projection
	VerifyInterval         time.Duration
	WebhookSecret          string
	ManageWebhook          bool
}

func validNamespace(ns string) bool {
	return ns != "" && len(ns) <= 1024 && !strings.HasPrefix(ns, "core.") && !strings.ContainsAny(ns, " \t\r\n")
}

// Settings validates and defaults the common part of an entry. With
// checkSecrets, webhook_secret must name one of secretNames.
func (c Common) Settings(typ string, secretNames []string, checkSecrets bool) (Settings, error) {
	var s Settings
	if !validNamespace(c.Namespace) {
		return s, fmt.Errorf("%s extension: namespace must be a stable secret-free identifier outside core.*", typ)
	}
	if !idPattern.MatchString(c.Scope) {
		return s, fmt.Errorf("%s extension: scope must be a scope id", typ)
	}
	s = Settings{Type: typ, Namespace: c.Namespace, Scope: c.Scope, Labels: DefaultLabels(), ClosedHorizon: 168 * time.Hour,
		Projection: c.Projection, VerifyInterval: 15 * time.Minute, WebhookSecret: c.WebhookSecret, ManageWebhook: c.ManageWebhook,
		Transitions: map[string]string{}}
	if c.Labels != nil {
		s.Labels = c.Labels.merge(DefaultLabels())
	}
	for _, l := range append(append([]string{s.Labels.Claimed}, s.Labels.Statuses()...), append(s.Labels.Skip, s.Labels.Require...)...) {
		if strings.TrimSpace(l) == "" || len(l) > 100 {
			return s, fmt.Errorf("%s extension: labels must be nonempty names", typ)
		}
	}
	for state, status := range c.Transitions {
		known := false
		for _, t := range TransitionStates {
			known = known || t == state
		}
		if !known {
			return s, fmt.Errorf("%s extension: transitions keys are %s", typ, strings.Join(TransitionStates, ", "))
		}
		if status = strings.TrimSpace(status); status == "" || len(status) > 200 {
			return s, fmt.Errorf("%s extension: a transition names a workflow status", typ)
		}
		s.Transitions[state] = status
	}
	if c.ClosedHorizonHours != 0 {
		if c.ClosedHorizonHours < 1 || c.ClosedHorizonHours > 24*365 {
			return s, fmt.Errorf("%s extension: closed_horizon_hours must be 1-8760", typ)
		}
		s.ClosedHorizon = time.Duration(c.ClosedHorizonHours) * time.Hour
	}
	if c.Projection.VerifySeconds != 0 {
		if c.Projection.VerifySeconds < 10 || c.Projection.VerifySeconds > 86400 {
			return s, fmt.Errorf("%s extension: projection.verify_seconds must be 10-86400", typ)
		}
		s.VerifyInterval = time.Duration(c.Projection.VerifySeconds) * time.Second
	}
	if c.ManageWebhook && c.WebhookSecret == "" {
		return s, fmt.Errorf("%s extension: manage_webhook needs webhook_secret", typ)
	}
	if c.WebhookSecret != "" && checkSecrets {
		found := false
		for _, n := range secretNames {
			found = found || n == c.WebhookSecret
		}
		if !found {
			return s, fmt.Errorf("%s extension: webhook_secret must name a selected secret", typ)
		}
	}
	return s, nil
}

// ParseLenient reads only the common part of an entry (ignoring the tracker's
// own fields), for route evaluation over scope metadata.
func ParseLenient(typ string, data []byte) (Settings, error) {
	var c Common
	if err := json.Unmarshal(data, &c); err != nil {
		return Settings{}, err
	}
	return c.Settings(typ, nil, false)
}

// RouteConfig is the opaque config of a tracker route. Match narrows the
// issues the route claims by their attributes (multi-scope routing: one
// entry per scope, each claiming the issues its match selects); every listed
// attribute must have one of its values. "labels", "status", "title" and the
// tracker's own attributes (team, project, priority, assignee, components,
// custom fields) are available. Values compare case-insensitively.
type RouteConfig struct {
	Match map[string][]string `json:"match,omitempty"`
}

// ParseRoute decodes and validates a route config.
func ParseRoute(data []byte) (RouteConfig, error) {
	var r RouteConfig
	if err := Strict(data, &r); err != nil {
		return r, err
	}
	for name, values := range r.Match {
		if !attrPattern.MatchString(name) || len(values) == 0 {
			return r, errors.New("match takes attribute names and nonempty value lists")
		}
		for _, v := range values {
			if strings.TrimSpace(v) == "" || len(v) > 200 {
				return r, errors.New("match values must be nonempty")
			}
		}
	}
	return r, nil
}

// ValidateRoute is the config.Registry route-config validator.
func ValidateRoute(data json.RawMessage) error {
	_, err := ParseRoute(data)
	return err
}

// Matches reports whether an issue payload satisfies the match.
func (r RouteConfig) Matches(p IssuePayload) bool {
	names := make([]string, 0, len(r.Match))
	for n := range r.Match {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		var have []string
		switch name {
		case "labels":
			have = p.Labels
		case "status":
			have = []string{p.Status}
		case "title":
			have = []string{p.Title}
		default:
			have = p.Attrs[name]
		}
		found := false
		for _, want := range r.Match[name] {
			for _, h := range have {
				found = found || strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(want))
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SplitKeys parses a list of upper-case keys ("ENG", "OPS") with a pattern.
func SplitKeys(keys []string, pattern *regexp.Regexp) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, k := range keys {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if !pattern.MatchString(k) {
			return nil, fmt.Errorf("%q is not a valid key", k)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no keys")
	}
	return out, nil
}
