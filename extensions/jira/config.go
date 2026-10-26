// Package jira is the first-party Jira extension (06-01): the issue tracker
// of one or more Jira projects, Cloud or Data Center, through REST API v2
// (whose bodies are wiki markup, converted to and from the Markdown agents
// write), ported from Open Supervisor's adapter onto the shared tracker kit.
// An agent's comment carries the `open-supervisor` entity property (which
// Open Supervisor still reads, so both runtimes recognize each other's notes
// during the fallback release); workflow transitions move the issue as the
// agent's state changes.
package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// Type is the registered connection, extension and route handler name.
const Type = "jira"

// SecretToken is the connection secret: an API token (basic, with the
// account's email) or a personal access token (bearer, Data Center).
const SecretToken = "token"

var (
	projectPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
	fieldPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
)

// ConnectionConfig is the nonsecret config of a `jira` connection.
type ConnectionConfig struct {
	BaseURL string `json:"base_url"`
	// Auth is basic (email + API token, Cloud; the default) or bearer (a
	// personal access token, Data Center).
	Auth  string `json:"auth,omitempty"`
	Email string `json:"email,omitempty"`
	// AgentAccount is an account id or name whose comments always count as
	// an agent's (leave empty when agents post as you).
	AgentAccount string `json:"agent_account,omitempty"`
}

// ParseConnection decodes and defaults connection config.
func ParseConnection(data []byte) (ConnectionConfig, error) {
	var c ConnectionConfig
	if err := tracker.Strict(data, &c); err != nil {
		return c, fmt.Errorf("jira connection: %w", err)
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	u, err := url.Parse(c.BaseURL)
	if c.BaseURL == "" || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return c, errors.New("jira connection: base_url must be the site's http(s) URL without credentials")
	}
	if c.Auth == "" {
		c.Auth = "basic"
	}
	switch c.Auth {
	case "basic":
		if c.Email == "" || len(c.Email) > 254 || strings.ContainsAny(c.Email, " \r\n") {
			return c, errors.New("jira connection: basic auth needs the account's email")
		}
	case "bearer":
	default:
		return c, errors.New("jira connection: auth must be basic or bearer")
	}
	if len(c.AgentAccount) > 200 || strings.ContainsAny(c.AgentAccount, "\r\n") {
		return c, errors.New("jira connection: agent_account is an account id or name")
	}
	return c, nil
}

// ValidateConnection is the config.Registry connection validator.
func ValidateConnection(data json.RawMessage) error {
	_, err := ParseConnection(data)
	return err
}

// ExtensionConfig is a `jira` extension entry: the kit's common fields plus
// the projects, extra JQL and the extra fields routes match on.
type ExtensionConfig struct {
	tracker.Common
	// Projects are project keys ("PROJ"); several make a source whose
	// routes match issues to scopes.
	Projects []string `json:"projects"`
	// JQL is ANDed with the project and open filter ("component = Web").
	JQL string `json:"jql,omitempty"`
	// Fields are extra field ids routes can match on ("customfield_10010").
	Fields []string `json:"fields,omitempty"`
}

// Settings is a parsed, defaulted entry.
type Settings struct {
	tracker.Settings
	Projects []string
	JQL      string
	Fields   []string
}

// ParseExtension decodes, validates and defaults an entry.
func ParseExtension(data []byte, secretNames []string) (Settings, error) {
	var c ExtensionConfig
	var s Settings
	if err := tracker.Strict(data, &c); err != nil {
		return s, fmt.Errorf("jira extension: %w", err)
	}
	common, err := c.Common.Settings(Type, secretNames, true)
	if err != nil {
		return s, err
	}
	projects, err := tracker.SplitKeys(c.Projects, projectPattern)
	if err != nil {
		return s, errors.New("jira extension: projects must list Jira project keys (PROJ)")
	}
	for _, l := range append(append([]string{common.Labels.Claimed}, common.Labels.Statuses()...), append(common.Labels.Skip, common.Labels.Require...)...) {
		if strings.ContainsAny(l, " \t") {
			return s, errors.New("jira extension: Jira labels cannot contain spaces")
		}
	}
	if len(c.JQL) > 2000 || strings.ContainsAny(c.JQL, "\r\n") {
		return s, errors.New("jira extension: jql must be one line")
	}
	for _, f := range c.Fields {
		if !fieldPattern.MatchString(f) {
			return s, errors.New("jira extension: fields are field ids (customfield_10010)")
		}
	}
	return Settings{Settings: common, Projects: projects, JQL: strings.TrimSpace(c.JQL), Fields: c.Fields}, nil
}

// ValidateExtension is the config.Registry extension validator.
func ValidateExtension(data json.RawMessage, secretNames []string) error {
	_, err := ParseExtension(data, secretNames)
	return err
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
