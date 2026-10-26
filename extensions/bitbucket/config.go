// Package bitbucket is the first-party Bitbucket Cloud extension (06-01): the
// code host for a repository whose issues live in a tracker (Jira or
// Linear). Pull requests, their comments and commit statuses are observed
// through the 2.0 REST API and its webhooks, and correlated with the
// tracker issue they belong to (the branch first, then a key in the title
// or description), so PR state reaches the issue's Work without a route
// attaching keys. Agents open and comment on PRs through its tools; comments
// carry an invisible Markdown mark (Open Supervisor's line plus Open
// Conduit's intent) so a lost answer is never posted twice. Ported from Open
// Supervisor's adapter; it reaches the tracker only through the public
// tracker kit.
package bitbucket

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/extensions/tracker"
)

// Type is the registered connection, extension and route handler name.
const Type = "bitbucket"

// SecretToken is the connection secret: an API token (basic, with the
// Atlassian account email) or a repository/project/workspace access token
// (bearer).
const SecretToken = "token"

// DefaultAPIURL is Bitbucket Cloud's API.
const DefaultAPIURL = "https://api.bitbucket.org/2.0"

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	keyPrefix   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
	idPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
)

// ConnectionConfig is the nonsecret config of a `bitbucket` connection.
type ConnectionConfig struct {
	APIURL   string `json:"api_url,omitempty"`
	Auth     string `json:"auth,omitempty"`
	Username string `json:"username,omitempty"`
}

// ParseConnection decodes and defaults connection config.
func ParseConnection(data []byte) (ConnectionConfig, error) {
	var c ConnectionConfig
	if err := tracker.Strict(data, &c); err != nil {
		return c, fmt.Errorf("bitbucket connection: %w", err)
	}
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return c, errors.New("bitbucket connection: api_url must be an http(s) URL without credentials")
	}
	if c.Auth == "" {
		c.Auth = "basic"
	}
	switch c.Auth {
	case "basic":
		if c.Username == "" || strings.ContainsAny(c.Username, " \r\n:") || len(c.Username) > 254 {
			return c, errors.New("bitbucket connection: basic auth needs the account email or username")
		}
	case "bearer":
	default:
		return c, errors.New("bitbucket connection: auth must be basic or bearer")
	}
	return c, nil
}

// ValidateConnection is the config.Registry connection validator.
func ValidateConnection(data json.RawMessage) error {
	_, err := ParseConnection(data)
	return err
}

// TrackerRef names the tracker whose issues the repository's PRs belong to:
// its entry's namespace and the key prefixes (Jira project or Linear team
// keys) a PR may mention.
type TrackerRef struct {
	Namespace string   `json:"namespace"`
	Keys      []string `json:"keys"`
}

// ExtensionConfig is the opaque config of a `bitbucket` extension entry.
type ExtensionConfig struct {
	Namespace string      `json:"namespace"`
	Scope     string      `json:"scope"`
	Repo      string      `json:"repo"`
	Tracker   *TrackerRef `json:"tracker,omitempty"`
	// BranchPattern captures the issue key from a PR's source branch
	// (default `^agent/issue/([A-Za-z][A-Za-z0-9_]*-[0-9]+)(?:/|$)`, the
	// claude-code `agent/{key}` branch); "" disables it.
	BranchPattern *string `json:"branch_pattern,omitempty"`
	// PreviewPattern matches commit status keys and names; a successful
	// match's URL is the deploy preview (default `preview|deploy`).
	PreviewPattern *string `json:"preview_pattern,omitempty"`
	// IssueRef is how a PR body refers to its issue ({key}); default
	// "{key}" (Jira links it), "Closes {key}" for Linear.
	IssueRef           string `json:"issue_ref,omitempty"`
	ClosedHorizonHours int    `json:"closed_horizon_hours,omitempty"`
	BaseBranch         string `json:"base_branch,omitempty"`
	WebhookSecret      string `json:"webhook_secret,omitempty"`
	ManageWebhook      bool   `json:"manage_webhook,omitempty"`
	// DryRun makes the tools write nothing (the daemon's --dry-run sets it).
	DryRun bool `json:"dry_run,omitempty"`
}

// Settings is a parsed, defaulted entry.
type Settings struct {
	Namespace, Scope, Repo string
	Workspace, Slug        string
	TrackerNS              string
	TrackerKeys            []string
	Branch, Preview, Refs  *regexp.Regexp
	IssueRef               string
	ClosedHorizon          time.Duration
	BaseBranch             string
	WebhookSecret          string
	ManageWebhook          bool
	DryRun                 bool
}

func validNamespace(ns string) bool {
	return ns != "" && len(ns) <= 1024 && !strings.HasPrefix(ns, "core.") && !strings.ContainsAny(ns, " \t\r\n")
}

// ParseExtension decodes, validates and defaults an entry.
func ParseExtension(data []byte, secretNames []string) (Settings, error) {
	return parseSettings(data, secretNames, true)
}

func parseSettings(data []byte, secretNames []string, checkSecrets bool) (Settings, error) {
	var c ExtensionConfig
	var s Settings
	if err := tracker.Strict(data, &c); err != nil {
		return s, fmt.Errorf("bitbucket extension: %w", err)
	}
	if !validNamespace(c.Namespace) {
		return s, errors.New("bitbucket extension: namespace must be a stable secret-free identifier outside core.*")
	}
	if !idPattern.MatchString(c.Scope) {
		return s, errors.New("bitbucket extension: scope must be a scope id")
	}
	if !repoPattern.MatchString(c.Repo) {
		return s, errors.New("bitbucket extension: repo must be workspace/slug")
	}
	ws, slug, _ := strings.Cut(c.Repo, "/")
	s = Settings{Namespace: c.Namespace, Scope: c.Scope, Repo: c.Repo, Workspace: ws, Slug: slug, IssueRef: "{key}",
		ClosedHorizon: 168 * time.Hour, BaseBranch: c.BaseBranch, WebhookSecret: c.WebhookSecret, ManageWebhook: c.ManageWebhook, DryRun: c.DryRun}
	if c.Tracker != nil {
		if !validNamespace(c.Tracker.Namespace) {
			return s, errors.New("bitbucket extension: tracker needs its entry's namespace")
		}
		keys, err := tracker.SplitKeys(c.Tracker.Keys, keyPrefix)
		if err != nil {
			return s, errors.New("bitbucket extension: tracker keys are project or team keys (PROJ, ENG)")
		}
		s.TrackerNS, s.TrackerKeys = c.Tracker.Namespace, keys
		quoted := make([]string, len(keys))
		for i, k := range keys {
			quoted[i] = regexp.QuoteMeta(k)
		}
		s.Refs = regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)-[0-9]+\b`)
	}
	branch := `^agent/issue/([A-Za-z][A-Za-z0-9_]*-[0-9]+)(?:/|$)`
	if c.BranchPattern != nil {
		branch = *c.BranchPattern
	}
	if branch != "" {
		re, err := regexp.Compile(branch)
		if err != nil || re.NumSubexp() < 1 {
			return s, errors.New("bitbucket extension: branch_pattern must be a regex capturing the issue key")
		}
		s.Branch = re
	}
	preview := "preview|deploy"
	if c.PreviewPattern != nil {
		preview = *c.PreviewPattern
	}
	if preview != "" {
		re, err := regexp.Compile("(?i)" + preview)
		if err != nil {
			return s, errors.New("bitbucket extension: preview_pattern must be a regex")
		}
		s.Preview = re
	}
	if c.IssueRef != "" {
		if !strings.Contains(c.IssueRef, "{key}") || len(c.IssueRef) > 200 || strings.ContainsAny(c.IssueRef, "\r\n") {
			return s, errors.New("bitbucket extension: issue_ref must contain {key}")
		}
		s.IssueRef = c.IssueRef
	}
	if c.ClosedHorizonHours != 0 {
		if c.ClosedHorizonHours < 1 || c.ClosedHorizonHours > 24*365 {
			return s, errors.New("bitbucket extension: closed_horizon_hours must be 1-8760")
		}
		s.ClosedHorizon = time.Duration(c.ClosedHorizonHours) * time.Hour
	}
	if c.BaseBranch != "" && (strings.ContainsAny(c.BaseBranch, " \t\r\n~^:?*[\\") || len(c.BaseBranch) > 255) {
		return s, errors.New("bitbucket extension: base_branch must be a branch name")
	}
	if c.ManageWebhook && c.WebhookSecret == "" {
		return s, errors.New("bitbucket extension: manage_webhook needs webhook_secret")
	}
	if c.WebhookSecret != "" && checkSecrets {
		found := false
		for _, n := range secretNames {
			found = found || n == c.WebhookSecret
		}
		if !found {
			return s, errors.New("bitbucket extension: webhook_secret must name a selected secret")
		}
	}
	return s, nil
}

// ValidateExtension is the config.Registry extension validator.
func ValidateExtension(data json.RawMessage, secretNames []string) error {
	_, err := ParseExtension(data, secretNames)
	return err
}

// IssueOf finds the tracker issue key a PR belongs to: the branch first,
// then a key of the tracker in the title or description.
func (s Settings) IssueOf(branch, title, body string) string {
	if s.TrackerNS == "" {
		return ""
	}
	if s.Branch != nil {
		if m := s.Branch.FindStringSubmatch(branch); m != nil && tracker.ValidKey(m[1]) && s.ownsKey(m[1]) {
			return strings.ToUpper(m[1])
		}
	}
	for _, text := range []string{title, body} {
		if m := s.Refs.FindString(text); m != "" {
			return strings.ToUpper(m)
		}
	}
	return ""
}

func (s Settings) ownsKey(key string) bool {
	for _, k := range s.TrackerKeys {
		if strings.HasPrefix(strings.ToUpper(key), k+"-") {
			return true
		}
	}
	return false
}
