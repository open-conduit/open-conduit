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
	"slices"
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

// Start points (claim_from): which threads no Work owns an entry may claim.
const (
	// ClaimRegistration claims threads begun at or after the channel's
	// first registration on this daemon.
	ClaimRegistration = "registration"
	// ClaimAll claims any thread, however old.
	ClaimAll = "all"
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
	// ClaimFrom is the entry's start point: which threads no Work owns it
	// may claim. registration (default) claims only threads begun at or
	// after the time this daemon first registered the entry's channel (a
	// durable record, so a restart does not move it and a restored backup
	// keeps it); an RFC 3339 time claims threads begun from then; all
	// claims any thread reconcile reads.
	ClaimFrom string `json:"claim_from,omitempty"`
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
	// Post lists the bindings whose Work may post a new top-level message
	// (slack.post) and the channels each may post in. A channel must be one
	// the entry watches, so replies in the new thread are observed.
	Post []PostGrant `json:"post,omitempty"`
	// DirectMessages, when set, makes a direct message with the app a
	// place where Work starts: every person's top-level message there
	// starts a conversation, whatever start says, if the limit admits its
	// sender.
	DirectMessages *DirectMessages `json:"direct_messages,omitempty"`
}

// DirectMessages is who may start Work by direct message. Exactly one of
// Users and Anyone is set, so turning DMs on always says who they admit.
type DirectMessages struct {
	// Users lists the Slack user IDs (U… or W…) whose direct messages
	// start Work.
	Users []string `json:"users,omitempty"`
	// Anyone admits every person who can message the app.
	Anyone bool `json:"anyone,omitempty"`
}

// PostGrant lets Work running on Binding post to Channels (slack.post).
type PostGrant struct {
	Binding  string   `json:"binding"`
	Channels []string `json:"channels"`
	// FollowUp, when set, lets a person's message in the thread of a post
	// made by this binding's Work, once that Work has finished, open its
	// successor on the binding. Without it such a message starts nothing.
	FollowUp *FollowUpGrant `json:"follow_up,omitempty"`
}

// FollowUpGrant is what a successor a reply opens may use: only the
// binding grants it names (domain.Binding.Grants), none by default, never
// the finished Work's own.
type FollowUpGrant struct {
	Grants []string `json:"grants,omitempty"`
}

// Settings is a parsed, defaulted extension entry.
type Settings struct {
	Namespace, Scope, Team string
	Channels               []string
	Start, BotUser         string
	Receiver               string
	StartWindow            time.Duration
	// Claim is ClaimRegistration, ClaimAll or "" with ClaimSince set (an
	// explicit start point).
	Claim            string
	ClaimSince       time.Time
	History, Refresh time.Duration
	SigningSecret    string
	Projection       Projection
	// Post maps each binding that may post (slack.post) to the sorted
	// channels it may post in.
	Post map[string][]string
	// FollowUp maps each binding whose posts' replies open a successor
	// once its Work has finished to the grants that successor gets.
	FollowUp map[string][]string
	// DirectMessages turns on direct messages with the app. DMUsers (sorted)
	// is who may start Work by one; DMAnyone admits everyone instead.
	DirectMessages bool
	DMUsers        []string
	DMAnyone       bool
}

// IsDirect reports whether channel is a direct message conversation (D…).
func IsDirect(channel string) bool {
	return strings.HasPrefix(channel, "D") && slackIDPattern.MatchString(channel)
}

// Observes reports whether messages in channel are observed: a watched
// channel, or a direct message with the app when the entry turns them on.
func (s Settings) Observes(channel string) bool {
	return s.Watches(channel) || s.DirectMessages && IsDirect(channel)
}

// AdmitsDirect reports whether user may start Work by direct message.
func (s Settings) AdmitsDirect(user string) bool {
	if !s.DirectMessages || user == "" {
		return false
	}
	if s.DMAnyone {
		return true
	}
	i := sort.SearchStrings(s.DMUsers, user)
	return i < len(s.DMUsers) && s.DMUsers[i] == user
}

// FollowsUp reports whether a reply to binding's post in channel, once
// the posting Work has finished, opens a successor, and with which grants.
func (s Settings) FollowsUp(binding, channel string) ([]string, bool) {
	grants, ok := s.FollowUp[binding]
	if !ok || !s.MayPost(binding, channel) {
		return nil, false
	}
	return slices.Clone(grants), true
}

// MayPost reports whether Work running on binding may post to channel.
func (s Settings) MayPost(binding, channel string) bool {
	return slices.Contains(s.Post[binding], channel)
}

// Posts reports whether any binding may post to channel.
func (s Settings) Posts(channel string) bool {
	for _, chs := range s.Post {
		if slices.Contains(chs, channel) {
			return true
		}
	}
	return false
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
	if dm := c.DirectMessages; dm != nil {
		if dm.Anyone == (len(dm.Users) > 0) {
			return s, errors.New("slack extension: direct_messages must list users or set anyone, not both")
		}
		if len(dm.Users) > 1000 {
			return s, errors.New("slack extension: direct_messages users must list at most 1000 user IDs")
		}
		users := slices.Clone(dm.Users)
		sort.Strings(users)
		for i, u := range users {
			if !slackIDPattern.MatchString(u) || !strings.HasPrefix(u, "U") && !strings.HasPrefix(u, "W") || i > 0 && users[i-1] == u {
				return s, errors.New("slack extension: direct_messages users must be distinct Slack user IDs (U… or W…)")
			}
		}
		s.DirectMessages, s.DMUsers, s.DMAnyone = true, users, dm.Anyone
	}
	if len(c.Channels) > 100 || len(c.Channels) == 0 && !s.DirectMessages {
		return s, errors.New("slack extension: channels must list 1-100 channel IDs (or none, with direct_messages)")
	}
	seen := map[string]bool{}
	for _, ch := range c.Channels {
		if !slackIDPattern.MatchString(ch) || IsDirect(ch) || seen[ch] {
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
	switch c.ClaimFrom {
	case "", ClaimRegistration:
		s.Claim = ClaimRegistration
	case ClaimAll:
		s.Claim = ClaimAll
	default:
		t, err := time.Parse(time.RFC3339, c.ClaimFrom)
		if err != nil {
			return s, errors.New("slack extension: claim_from must be registration, all or an RFC 3339 time")
		}
		s.ClaimSince = t.UTC()
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
	if len(c.Post) > 100 {
		return s, errors.New("slack extension: post must list at most 100 bindings")
	}
	for _, g := range c.Post {
		if !idPattern.MatchString(g.Binding) {
			return s, errors.New("slack extension: each post entry must name a binding")
		}
		if _, dup := s.Post[g.Binding]; dup {
			return s, fmt.Errorf("slack extension: post lists binding %s more than once", g.Binding)
		}
		if len(g.Channels) == 0 || len(g.Channels) > 100 {
			return s, fmt.Errorf("slack extension: post for binding %s must list 1-100 channels", g.Binding)
		}
		chs := slices.Clone(g.Channels)
		sort.Strings(chs)
		if len(slices.Compact(slices.Clone(chs))) != len(chs) {
			return s, fmt.Errorf("slack extension: post for binding %s lists a channel more than once", g.Binding)
		}
		for _, ch := range chs {
			if !s.Watches(ch) {
				return s, fmt.Errorf("slack extension: post for binding %s names channel %q, which channels does not list; a posted thread's replies are only seen in a watched channel", g.Binding, ch)
			}
		}
		if s.Post == nil {
			s.Post = map[string][]string{}
		}
		s.Post[g.Binding] = chs
		if g.FollowUp != nil {
			grants := slices.Clone(g.FollowUp.Grants)
			sort.Strings(grants)
			if len(grants) > 100 || len(slices.Compact(slices.Clone(grants))) != len(grants) {
				return s, fmt.Errorf("slack extension: follow_up for binding %s must list at most 100 grants, each once", g.Binding)
			}
			for _, name := range grants {
				if strings.TrimSpace(name) == "" {
					return s, fmt.Errorf("slack extension: follow_up for binding %s names an empty grant", g.Binding)
				}
			}
			if s.FollowUp == nil {
				s.FollowUp = map[string][]string{}
			}
			s.FollowUp[g.Binding] = append([]string{}, grants...)
		}
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

// RouteConfig is the opaque config of a `slack` route. Channels narrows the
// threads the route claims to those in the listed channels, and
// DirectMessages to direct messages with the app; with neither, the route
// claims every thread the entry observes. One entry can so serve several
// routes, each with its own binding and playbook; first match wins as usual.
type RouteConfig struct {
	Channels       []string `json:"channels,omitempty"`
	DirectMessages bool     `json:"direct_messages,omitempty"`
}

// ParseRoute decodes and validates a route config.
func ParseRoute(data []byte) (RouteConfig, error) {
	var r RouteConfig
	if err := strict(data, &r); err != nil {
		return r, fmt.Errorf("slack route: %w", err)
	}
	if len(r.Channels) > 100 {
		return r, errors.New("slack route: channels must list at most 100 channel IDs")
	}
	seen := map[string]bool{}
	for _, ch := range r.Channels {
		if !slackIDPattern.MatchString(ch) || IsDirect(ch) || seen[ch] {
			return r, errors.New("slack route: channels must be distinct Slack channel IDs (C… or G…)")
		}
		seen[ch] = true
	}
	return r, nil
}

// ValidateRoute is the config.Registry route-config validator.
func ValidateRoute(data json.RawMessage) error {
	_, err := ParseRoute(data)
	return err
}

// Selects reports whether the route claims a thread in channel.
func (r RouteConfig) Selects(channel string) bool {
	if len(r.Channels) == 0 && !r.DirectMessages {
		return true
	}
	if IsDirect(channel) {
		return r.DirectMessages
	}
	return slices.Contains(r.Channels, channel)
}
