// Package linear is the first-party Linear extension (06-01): a tracker over
// one or more Linear teams' issues through Linear's GraphQL API and its
// webhooks, ported from Open Supervisor's adapter onto the shared tracker
// kit. Linear keeps Markdown but drops HTML comments, so an agent's comment
// carries a visible footer (Open Supervisor's, plus Open Conduit's intent
// line). Workflow states are moved by name as the agent's state changes.
package linear

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
const Type = "linear"

// SecretToken is the connection secret: a personal API key, or an OAuth
// token (lin_oauth_…), sent as a bearer token.
const SecretToken = "token"

// DefaultAPIURL is Linear's GraphQL endpoint.
const DefaultAPIURL = "https://api.linear.app/graphql"

var teamPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)

// ConnectionConfig is the nonsecret config of a `linear` connection.
type ConnectionConfig struct {
	APIURL string `json:"api_url,omitempty"`
	// AgentAccount is a user's name, email or id whose comments always count
	// as an agent's (leave empty when agents post as you).
	AgentAccount string `json:"agent_account,omitempty"`
}

// ParseConnection decodes and defaults connection config.
func ParseConnection(data []byte) (ConnectionConfig, error) {
	var c ConnectionConfig
	if err := tracker.Strict(data, &c); err != nil {
		return c, fmt.Errorf("linear connection: %w", err)
	}
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return c, errors.New("linear connection: api_url must be an http(s) URL without credentials")
	}
	if len(c.AgentAccount) > 200 || strings.ContainsAny(c.AgentAccount, "\r\n") {
		return c, errors.New("linear connection: agent_account is a name, email or id")
	}
	return c, nil
}

// ValidateConnection is the config.Registry connection validator.
func ValidateConnection(data json.RawMessage) error {
	_, err := ParseConnection(data)
	return err
}

// ExtensionConfig is a `linear` extension entry: the kit's common fields plus
// the teams and the states issues are claimed from.
type ExtensionConfig struct {
	tracker.Common
	// Teams are team keys ("ENG"); several make a source whose routes match
	// issues to scopes.
	Teams []string `json:"teams"`
	// States are the workflow state types (triage, backlog, unstarted,
	// started) or names whose issues are claimed; issues elsewhere wait
	// (default unstarted, started).
	States []string `json:"states,omitempty"`
}

// Settings is a parsed, defaulted entry.
type Settings struct {
	tracker.Settings
	Teams  []string
	States []string
}

// ParseExtension decodes, validates and defaults an entry.
func ParseExtension(data []byte, secretNames []string) (Settings, error) {
	var c ExtensionConfig
	var s Settings
	if err := tracker.Strict(data, &c); err != nil {
		return s, fmt.Errorf("linear extension: %w", err)
	}
	common, err := c.Common.Settings(Type, secretNames, true)
	if err != nil {
		return s, err
	}
	teams, err := tracker.SplitKeys(c.Teams, teamPattern)
	if err != nil {
		return s, errors.New("linear extension: teams must list Linear team keys (ENG)")
	}
	s = Settings{Settings: common, Teams: teams, States: []string{"unstarted", "started"}}
	if len(c.States) > 0 {
		s.States = nil
		for _, st := range c.States {
			if st = strings.ToLower(strings.TrimSpace(st)); st != "" {
				s.States = append(s.States, st)
			}
		}
		if len(s.States) == 0 {
			return s, errors.New("linear extension: states must name state types or names")
		}
	}
	return s, nil
}

// ValidateExtension is the config.Registry extension validator.
func ValidateExtension(data json.RawMessage, secretNames []string) error {
	_, err := ParseExtension(data, secretNames)
	return err
}
