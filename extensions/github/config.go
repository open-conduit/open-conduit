// Package github is the first-party GitHub extension: an ingress pair (push
// webhooks and reconcile scans) that normalizes issues, comments, reviews,
// checks, merges, closes and reopenings into canonical observations; a
// deterministic route handler holding the opt-in/skip claim policy; and a
// projector that converges claim/status labels and marked comments from durable
// Work state. Labels are projections: the runtime never reads them as state.
//
// It depends only on public contracts. Credentials are resolved lazily through
// an injected secret function and never form identity, payloads or errors.
package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Type is the registered connection, extension and route handler name.
const Type = "github"

// Connection secret names (keys of a connection's secret_refs).
const (
	SecretToken         = "token"
	SecretAppPrivateKey = "app_private_key"
	SecretBotPrivateKey = "bot_private_key"
)

var (
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	idPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	digitPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
)

// ConnectionConfig is the nonsecret config of a `github` connection. Token
// auth reads secret `token` (use `cmd:gh auth token` for the gh CLI login);
// app auth reads `app_private_key`. An optional bot App performs writes.
type ConnectionConfig struct {
	APIURL   string `json:"api_url,omitempty"`
	Auth     string `json:"auth,omitempty"`
	AppID    string `json:"app_id,omitempty"`
	BotAppID string `json:"bot_app_id,omitempty"`
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

// ParseConnection decodes and defaults connection config.
func ParseConnection(data []byte) (ConnectionConfig, error) {
	var c ConnectionConfig
	if err := strict(data, &c); err != nil {
		return c, fmt.Errorf("github connection: %w", err)
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.github.com"
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	if c.Auth == "" {
		c.Auth = "token"
	}
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return c, errors.New("github connection: api_url must be an http(s) URL without credentials")
	}
	switch c.Auth {
	case "token":
		if c.AppID != "" {
			return c, errors.New("github connection: app_id requires auth app")
		}
	case "app":
		if !digitPattern.MatchString(c.AppID) {
			return c, errors.New("github connection: auth app requires a numeric app_id")
		}
	default:
		return c, errors.New("github connection: auth must be token or app")
	}
	if c.BotAppID != "" && !digitPattern.MatchString(c.BotAppID) {
		return c, errors.New("github connection: bot_app_id must be numeric")
	}
	return c, nil
}

// ValidateConnection is the config.Registry connection validator.
func ValidateConnection(data json.RawMessage) error {
	_, err := ParseConnection(data)
	return err
}

// Labels are the projected and admission labels (v1 names by default).
type Labels struct {
	Claimed   string   `json:"claimed,omitempty"`
	Scheduled string   `json:"scheduled,omitempty"`
	Help      string   `json:"help,omitempty"`
	Review    string   `json:"review,omitempty"`
	Done      string   `json:"done,omitempty"`
	Skip      []string `json:"skip,omitempty"`
	Require   []string `json:"require,omitempty"`
}

// DefaultLabels matches Open Supervisor so one owner is visible to both.
func DefaultLabels() Labels {
	return Labels{Claimed: "agent-in-progress", Scheduled: "scheduled", Help: "agent-needs-help",
		Review: "ready-for-review", Done: "might-be-done", Skip: []string{"backlog", "human", "milestone"}}
}

func (l Labels) merge(base Labels) Labels {
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	out := Labels{Claimed: pick(l.Claimed, base.Claimed), Scheduled: pick(l.Scheduled, base.Scheduled),
		Help: pick(l.Help, base.Help), Review: pick(l.Review, base.Review), Done: pick(l.Done, base.Done),
		Skip: l.Skip, Require: l.Require}
	if out.Skip == nil {
		out.Skip = base.Skip
	}
	return out
}

// Statuses are the projected status labels; they are excluded from issue
// observations so status projection never generates input.
func (l Labels) Statuses() []string { return []string{l.Help, l.Review, l.Done} }

// Status maps a projected status name onto its label.
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

// Tracker names a shared issue tracker repository whose issues PRs here
// reference as owner/repo#N.
type Tracker struct {
	Repo      string `json:"repo"`
	Namespace string `json:"namespace"`
}

// Projection controls label/comment convergence.
type Projection struct {
	Disabled      bool `json:"disabled,omitempty"`
	VerifySeconds int  `json:"verify_seconds,omitempty"`
	DryRun        bool `json:"dry_run,omitempty"`
}

// ExtensionConfig is the opaque config of a `github` extension entry.
type ExtensionConfig struct {
	Namespace string   `json:"namespace"`
	Scope     string   `json:"scope"`
	Repo      string   `json:"repo"`
	Tracker   *Tracker `json:"tracker,omitempty"`
	// TrackIssues=false observes only PRs/comments/reviews/checks (a code repo
	// whose issues live in a shared tracker).
	TrackIssues        *bool      `json:"track_issues,omitempty"`
	Labels             *Labels    `json:"labels,omitempty"`
	ClosedHorizonHours int        `json:"closed_horizon_hours,omitempty"`
	BranchPattern      *string    `json:"branch_pattern,omitempty"`
	PreviewPattern     *string    `json:"preview_pattern,omitempty"`
	Projection         Projection `json:"projection,omitempty"`
	// WebhookSecret names a selected scope secret used to verify deliveries.
	WebhookSecret string `json:"webhook_secret,omitempty"`
	// ManageWebhook lets the host create or re-point the repository's
	// webhook at the listener's public URL (needs hook admin permission).
	ManageWebhook bool `json:"manage_webhook,omitempty"`
	// BaseBranch is the branch PRs target (default: the repository's).
	BaseBranch string `json:"base_branch,omitempty"`
	// NoChecksMinutes is how long a PR head with no checks at all waits
	// before the agent hears none ran (default 10; -1 disables).
	NoChecksMinutes int `json:"no_checks_minutes,omitempty"`
}

// Settings is a parsed, defaulted extension entry.
type Settings struct {
	Namespace, Scope, Repo     string
	TrackerRepo, TrackerNS     string
	SharedTracker, TrackIssues bool
	Labels                     Labels
	ClosedHorizon              time.Duration
	Branch, Preview            *regexp.Regexp
	Projection                 Projection
	VerifyInterval             time.Duration
	WebhookSecret              string
	ManageWebhook              bool
	BaseBranch                 string
	NoChecks                   time.Duration
}

func validNamespace(ns string) bool {
	return ns != "" && len(ns) <= 1024 && !strings.HasPrefix(ns, "core.") && !strings.ContainsAny(ns, " \t\r\n")
}

// ParseExtension decodes, validates and defaults an extension entry.
func ParseExtension(data []byte, secretNames []string) (Settings, error) {
	return parseSettings(data, secretNames, true)
}

func parseSettings(data []byte, secretNames []string, checkSecrets bool) (Settings, error) {
	var c ExtensionConfig
	var s Settings
	if err := strict(data, &c); err != nil {
		return s, fmt.Errorf("github extension: %w", err)
	}
	if !validNamespace(c.Namespace) {
		return s, errors.New("github extension: namespace must be a stable secret-free identifier outside core.*")
	}
	if !idPattern.MatchString(c.Scope) {
		return s, errors.New("github extension: scope must be a scope id")
	}
	if !repoPattern.MatchString(c.Repo) {
		return s, errors.New("github extension: repo must be owner/name")
	}
	s = Settings{Namespace: c.Namespace, Scope: c.Scope, Repo: c.Repo, TrackerRepo: c.Repo, TrackerNS: c.Namespace, TrackIssues: true,
		Labels: DefaultLabels(), ClosedHorizon: 168 * time.Hour, Projection: c.Projection, WebhookSecret: c.WebhookSecret, ManageWebhook: c.ManageWebhook}
	if c.Tracker != nil {
		if !repoPattern.MatchString(c.Tracker.Repo) || !validNamespace(c.Tracker.Namespace) {
			return s, errors.New("github extension: tracker needs repo owner/name and a namespace")
		}
		s.TrackerRepo, s.TrackerNS = c.Tracker.Repo, c.Tracker.Namespace
		s.SharedTracker = !strings.EqualFold(c.Tracker.Repo, c.Repo) || c.Tracker.Namespace != c.Namespace
	}
	if c.TrackIssues != nil {
		s.TrackIssues = *c.TrackIssues
	}
	if c.Labels != nil {
		s.Labels = c.Labels.merge(DefaultLabels())
	}
	for _, l := range append(append([]string{s.Labels.Claimed, s.Labels.Scheduled}, s.Labels.Statuses()...), append(s.Labels.Skip, s.Labels.Require...)...) {
		if strings.TrimSpace(l) == "" || len(l) > 100 {
			return s, errors.New("github extension: labels must be nonempty names")
		}
	}
	if c.ClosedHorizonHours != 0 {
		if c.ClosedHorizonHours < 1 || c.ClosedHorizonHours > 24*365 {
			return s, errors.New("github extension: closed_horizon_hours must be 1-8760")
		}
		s.ClosedHorizon = time.Duration(c.ClosedHorizonHours) * time.Hour
	}
	branch := `^agent/issue-(\d+)$`
	if c.BranchPattern != nil {
		branch = *c.BranchPattern
	}
	if branch != "" {
		re, err := regexp.Compile(branch)
		if err != nil || re.NumSubexp() < 1 {
			return s, errors.New("github extension: branch_pattern must be a regex capturing the issue number")
		}
		s.Branch = re
	}
	preview := "deploy-preview"
	if c.PreviewPattern != nil {
		preview = *c.PreviewPattern
	}
	if preview != "" {
		re, err := regexp.Compile(preview)
		if err != nil {
			return s, errors.New("github extension: preview_pattern must be a regex")
		}
		s.Preview = re
	}
	s.VerifyInterval = 15 * time.Minute
	if c.Projection.VerifySeconds != 0 {
		if c.Projection.VerifySeconds < 10 || c.Projection.VerifySeconds > 86400 {
			return s, errors.New("github extension: projection.verify_seconds must be 10-86400")
		}
		s.VerifyInterval = time.Duration(c.Projection.VerifySeconds) * time.Second
	}
	if c.BaseBranch != "" {
		if strings.ContainsAny(c.BaseBranch, " \t\r\n~^:?*[\\") || len(c.BaseBranch) > 255 {
			return s, errors.New("github extension: base_branch must be a branch name")
		}
		s.BaseBranch = c.BaseBranch
	}
	s.NoChecks = 10 * time.Minute
	switch {
	case c.NoChecksMinutes == -1:
		s.NoChecks = 0
	case c.NoChecksMinutes < -1 || c.NoChecksMinutes > 24*60:
		return s, errors.New("github extension: no_checks_minutes must be 1-1440, or -1 to disable")
	case c.NoChecksMinutes > 0:
		s.NoChecks = time.Duration(c.NoChecksMinutes) * time.Minute
	}
	if c.ManageWebhook && c.WebhookSecret == "" {
		return s, errors.New("github extension: manage_webhook needs webhook_secret")
	}
	if c.WebhookSecret != "" && checkSecrets {
		found := false
		for _, n := range secretNames {
			found = found || n == c.WebhookSecret
		}
		if !found {
			return s, errors.New("github extension: webhook_secret must name a selected secret")
		}
	}
	return s, nil
}

// ValidateExtension is the config.Registry extension validator.
func ValidateExtension(data json.RawMessage, secretNames []string) error {
	_, err := ParseExtension(data, secretNames)
	return err
}
