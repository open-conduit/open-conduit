// Package slack is the first-party Slack extension: an ingress pair (Events
// API push and conversation-history reconcile) that normalizes channel and
// thread messages into canonical observations correlated by thread; a
// deterministic route handler that starts `slack.conversation` Work for a thread and
// selects its later input; a projector that publishes marked replies exactly
// once through durable intents; and the reply tools agents call.
//
// It depends only on public contracts. It has no workspace, repository or
// tracker concept. Credentials are resolved lazily through an injected secret
// function and never form identity, payloads or errors.
package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Type is the registered connection, extension and route handler name.
const Type = "slack"

// Connection secrets (keys of a connection's secret_refs).
const (
	// SecretBotToken holds the bot token (xoxb-…) for Web API calls.
	SecretBotToken = "bot_token"
	// SecretAppToken holds the app-level token (xapp-…, connections:write)
	// that opens Socket Mode connections.
	SecretAppToken = "app_token"
)

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	slackIDPattern = regexp.MustCompile(`^[A-Z0-9]{2,32}$`)
)

// ConnectionConfig is the nonsecret config of a `slack` connection. The bot
// token comes only from the connection's secret_refs (`bot_token`).
type ConnectionConfig struct {
	APIURL string `json:"api_url,omitempty"`
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
		return c, fmt.Errorf("slack connection: %w", err)
	}
	if c.APIURL == "" {
		c.APIURL = "https://slack.com/api"
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	u, err := url.Parse(c.APIURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return c, errors.New("slack connection: api_url must be an http(s) URL without credentials")
	}
	return c, nil
}

// ValidateConnection is the config.Registry connection validator.
func ValidateConnection(data json.RawMessage) error {
	_, err := ParseConnection(data)
	return err
}

// Receivers: how push deliveries arrive.
const (
	ReceiverSocket = "socket"
	ReceiverHTTP   = "http"
	ReceiverNone   = "none"
)

// Start policies: which unowned messages start a conversation.
const (
	// StartAll starts a conversation for every human top-level message.
	StartAll = "all"
	// StartMention starts one for a human message (top-level or in a thread
	// no Work owns) that mentions bot_user_id.
	StartMention = "mention"
)

// Projection controls reply publication.
type Projection struct {
	Disabled bool `json:"disabled,omitempty"`
	DryRun   bool `json:"dry_run,omitempty"`
}

// ExtensionConfig is the opaque config of a `slack` extension entry: one
// Slack app installation in one workspace, observing a set of channels.
type ExtensionConfig struct {
	Namespace string   `json:"namespace"`
	Scope     string   `json:"scope"`
	Team      string   `json:"team"`
	Channels  []string `json:"channels"`
	// Start is all (default) or mention.
	Start string `json:"start,omitempty"`
	// BotUserID is the app's bot user (required for mention starts; also
	// recognizes the app's own unmarked messages).
	BotUserID string `json:"bot_user_id,omitempty"`
	// StartWindowHours bounds how old a message may be, when first
	// received, and still start a conversation (default 24), so a first
	// reconcile does not turn history into Work.
	StartWindowHours int `json:"start_window_hours,omitempty"`
	// HistoryHours is the reconcile horizon for top-level messages and their
	// threads (default 336, 14 days).
	HistoryHours int `json:"history_hours,omitempty"`
	// RefreshMinutes bounds how long an unchanged thread's replies are taken
	// from memory instead of re-read (default 60).
	RefreshMinutes int `json:"refresh_minutes,omitempty"`
	// Receiver is how push deliveries arrive: socket (default; Socket Mode
	// with the connection's app_token, no public endpoint), http (Events API
	// Request URL on the webhook listener, verified with signing_secret) or
	// none (reconcile only).
	Receiver string `json:"receiver,omitempty"`
	// SigningSecret names a selected scope secret: the app's signing secret
	// used to verify HTTP Events API deliveries (receiver http).
	SigningSecret string     `json:"signing_secret,omitempty"`
	Projection    Projection `json:"projection,omitempty"`
}

// Settings is a parsed, defaulted extension entry.
type Settings struct {
	Namespace, Scope, Team string
	Channels               []string
	Start, BotUser         string
	Receiver               string
	StartWindow            time.Duration
	History, Refresh       time.Duration
	SigningSecret          string
	Projection             Projection
}

// Watches reports whether channel is observed.
func (s Settings) Watches(channel string) bool {
	i := sort.SearchStrings(s.Channels, channel)
	return i < len(s.Channels) && s.Channels[i] == channel
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
		return s, fmt.Errorf("slack extension: %w", err)
	}
	if !validNamespace(c.Namespace) {
		return s, errors.New("slack extension: namespace must be a stable secret-free identifier outside core.*")
	}
	if !idPattern.MatchString(c.Scope) {
		return s, errors.New("slack extension: scope must be a scope id")
	}
	if !slackIDPattern.MatchString(c.Team) {
		return s, errors.New("slack extension: team must be a Slack team ID (T…)")
	}
	if len(c.Channels) == 0 || len(c.Channels) > 100 {
		return s, errors.New("slack extension: channels must list 1-100 channel IDs")
	}
	seen := map[string]bool{}
	for _, ch := range c.Channels {
		if !slackIDPattern.MatchString(ch) || seen[ch] {
			return s, errors.New("slack extension: channels must be distinct Slack channel IDs (C… or G…)")
		}
		seen[ch] = true
		s.Channels = append(s.Channels, ch)
	}
	sort.Strings(s.Channels)
	s.Namespace, s.Scope, s.Team = c.Namespace, c.Scope, c.Team
	s.SigningSecret, s.Projection = c.SigningSecret, c.Projection
	s.Start = c.Start
	switch s.Start {
	case "":
		s.Start = StartAll
	case StartAll, StartMention:
	default:
		return s, errors.New("slack extension: start must be all or mention")
	}
	if c.BotUserID != "" && !slackIDPattern.MatchString(c.BotUserID) {
		return s, errors.New("slack extension: bot_user_id must be a Slack user ID (U…)")
	}
	s.BotUser = c.BotUserID
	if s.Start == StartMention && s.BotUser == "" {
		return s, errors.New("slack extension: start mention needs bot_user_id")
	}
	s.History = 336 * time.Hour
	if c.HistoryHours != 0 {
		if c.HistoryHours < 1 || c.HistoryHours > 24*90 {
			return s, errors.New("slack extension: history_hours must be 1-2160")
		}
		s.History = time.Duration(c.HistoryHours) * time.Hour
	}
	s.StartWindow = 24 * time.Hour
	if c.StartWindowHours != 0 {
		if c.StartWindowHours < 1 || time.Duration(c.StartWindowHours)*time.Hour > s.History {
			return s, errors.New("slack extension: start_window_hours must be at least 1 and within history_hours")
		}
		s.StartWindow = time.Duration(c.StartWindowHours) * time.Hour
	}
	if s.StartWindow > s.History {
		s.StartWindow = s.History
	}
	s.Refresh = time.Hour
	if c.RefreshMinutes != 0 {
		if c.RefreshMinutes < 1 || c.RefreshMinutes > 24*60 {
			return s, errors.New("slack extension: refresh_minutes must be 1-1440")
		}
		s.Refresh = time.Duration(c.RefreshMinutes) * time.Minute
	}
	s.Receiver = c.Receiver
	switch s.Receiver {
	case "":
		s.Receiver = ReceiverSocket
	case ReceiverSocket, ReceiverHTTP, ReceiverNone:
	default:
		return s, errors.New("slack extension: receiver must be socket, http or none")
	}
	if s.Receiver == ReceiverHTTP && c.SigningSecret == "" {
		return s, errors.New("slack extension: receiver http needs signing_secret")
	}
	if c.SigningSecret != "" && checkSecrets {
		found := false
		for _, n := range secretNames {
			found = found || n == c.SigningSecret
		}
		if !found {
			return s, errors.New("slack extension: signing_secret must name a selected secret")
		}
	}
	return s, nil
}

// ValidateExtension is the config.Registry extension validator.
func ValidateExtension(data json.RawMessage, secretNames []string) error {
	_, err := ParseExtension(data, secretNames)
	return err
}
