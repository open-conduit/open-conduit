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
	// Format is how a message's text is laid out: markdown (default) sends
	// it as a markdown block, so it is read as standard markdown; mrkdwn
	// sends it as plain message text, read as Slack's own mrkdwn.
	Format string `json:"format,omitempty"`
}

// Text formats (projection.format).
const (
	FormatMarkdown = "markdown"
	FormatMrkdwn   = "mrkdwn"
)

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
	// Identities give the messages of named bindings' Work their own name
	// and icon (chat.postMessage username and icon_emoji or icon_url, which
	// need the chat:write.customize scope). A binding without one posts as
	// the app.
	Identities []BindingIdentity `json:"identities,omitempty"`
	// DirectMessages, when set, makes a direct message with the app a
	// place where Work starts: every person's top-level message there
	// starts a conversation, whatever start says, if the limit admits its
	// sender.
	DirectMessages *DirectMessages `json:"direct_messages,omitempty"`
	// Uploads, when set, lets slack.upload put files into a Work's thread
	// within its limits. Without it every upload is refused.
	Uploads *UploadsConfig `json:"uploads,omitempty"`
	// Progress, when enabled, keeps one progress message in a
	// conversation's thread for each run a person's message starts or
	// wakes, updated from the run's durable state. Off by default.
	Progress *ProgressConfig `json:"progress,omitempty"`
	// Feedback, when enabled, puts Slack's thumbs up and down below the
	// replies of the bindings it names (every binding when it names none).
	// A press is recorded on the Work as slack.feedback. Off by default.
	Feedback *FeedbackConfig `json:"feedback,omitempty"`
	// ChildRequests, when set, shows the requests of a conversation's
	// child Work, at any depth (an approval its gated call waits on, a
	// question it asked), in the conversation's thread: as a task of the
	// run's progress message, and, when the request's responders include
	// a Slack person, as a prompt with its buttons. Off by default.
	ChildRequests bool `json:"child_requests,omitempty"`
}

// FeedbackConfig turns on feedback buttons below replies.
type FeedbackConfig struct {
	Enabled bool `json:"enabled"`
	// Bindings narrows the buttons to these bindings' replies; empty is
	// every binding's.
	Bindings []string `json:"bindings,omitempty"`
}

// ProgressConfig turns on the progress message and says who may stop a run
// from it.
type ProgressConfig struct {
	Enabled bool `json:"enabled"`
	// Stop lists who may press the progress message's Stop button, each
	// slack:<user ID>, the form a gate's responders take. Empty shows no
	// Stop button.
	Stop []string `json:"stop,omitempty"`
}

// UploadsConfig is the host's limits on slack.upload, checked before
// anything is sent to Slack.
type UploadsConfig struct {
	// MaxBytes is the largest file accepted, at most MaxUpload; default
	// MaxUpload.
	MaxBytes int `json:"max_bytes,omitempty"`
	// MediaTypes lists the media types accepted, each exact (image/png),
	// a type's every subtype (image/*) or anything (*/*). A file's type
	// is the one its content sniffs as, not its name.
	MediaTypes []string `json:"media_types"`
}

// MaxUpload bounds a file slack.upload accepts: what one call to the
// extension carries once base64 grows it by a third, within the protocol's
// 16 MiB message bound (protocol.MaxMessageBytes), since the bundled
// extension runs out of process.
const MaxUpload = 10 << 20

// FilesScope is the bot scope slack.upload needs.
const FilesScope = "files:write"

var mediaTypePattern = regexp.MustCompile(`^([a-z0-9][a-z0-9!#$&^_.+-]{0,126}|\*)/([a-z0-9][a-z0-9!#$&^_.+-]{0,126}|\*)$`)

// DirectMessages is who may start Work by direct message. Exactly one of
// Users and Anyone is set, so turning DMs on always says who they admit.
type DirectMessages struct {
	// Users lists the Slack user IDs (U… or W…) whose direct messages
	// start Work.
	Users []string `json:"users,omitempty"`
	// Anyone admits every person who can message the app.
	Anyone bool `json:"anyone,omitempty"`
}

// BindingIdentity is the name and icon a binding's Work posts under: its replies,
// posts and the prompts of its runs. At least one field besides Binding is
// set, and at most one icon.
type BindingIdentity struct {
	Binding   string `json:"binding"`
	Username  string `json:"username,omitempty"`
	IconEmoji string `json:"icon_emoji,omitempty"`
	IconURL   string `json:"icon_url,omitempty"`
}

// CustomizeScope is the bot scope that lets a message carry its own name
// and icon (BindingIdentity).
const CustomizeScope = "chat:write.customize"

var emojiPattern = regexp.MustCompile(`^:[a-z0-9_+'-]{1,100}:$`)

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
	// Identities maps each binding that posts under its own name and icon
	// to that identity.
	Identities map[string]BindingIdentity
	// DirectMessages turns on direct messages with the app. DMUsers (sorted)
	// is who may start Work by one; DMAnyone admits everyone instead.
	DirectMessages bool
	DMUsers        []string
	DMAnyone       bool
	// Uploads turns slack.upload on, up to UploadMax bytes of the
	// UploadTypes media types.
	Uploads     bool
	UploadMax   int
	UploadTypes []string
	// Progress turns on the progress message; StopUsers (sorted) are the
	// Slack users who may press its Stop button.
	Progress  bool
	StopUsers []string
	// Feedback turns on feedback buttons below replies; FeedbackBindings
	// (sorted) narrows them to those bindings' replies, empty for all.
	Feedback         bool
	FeedbackBindings []string
	// ChildRequests shows child Work's requests in the conversation's
	// thread.
	ChildRequests bool
}

// AsksFeedback reports whether a reply posted on binding carries feedback
// buttons.
func (s Settings) AsksFeedback(binding string) bool {
	if !s.Feedback {
		return false
	}
	if len(s.FeedbackBindings) == 0 {
		return true
	}
	_, ok := slices.BinarySearch(s.FeedbackBindings, binding)
	return ok && binding != ""
}

// MayStop reports whether user may press a progress message's Stop button.
func (s Settings) MayStop(user string) bool {
	if !s.Progress || user == "" {
		return false
	}
	i := sort.SearchStrings(s.StopUsers, user)
	return i < len(s.StopUsers) && s.StopUsers[i] == user
}

// MayUpload reports whether a file of size bytes that sniffs as
// mediaType (its parameters are ignored) is within the entry's uploads.
func (s Settings) MayUpload(size int, mediaType string) bool {
	if !s.Uploads || size <= 0 || size > s.UploadMax {
		return false
	}
	return s.UploadsType(mediaType)
}

// UploadsType reports whether the entry's uploads accept mediaType.
func (s Settings) UploadsType(mediaType string) bool {
	mt, _, _ := strings.Cut(mediaType, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	major, _, _ := strings.Cut(mt, "/")
	for _, want := range s.UploadTypes {
		if want == "*/*" || want == mt || strings.HasSuffix(want, "/*") && strings.TrimSuffix(want, "/*") == major {
			return true
		}
	}
	return false
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

// IdentityOf is the name and icon Work running on binding posts under, if
// the entry gives it one.
func (s Settings) IdentityOf(binding string) (BindingIdentity, bool) {
	id, ok := s.Identities[binding]
	return id, ok && binding != ""
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
	if pc := c.Progress; pc != nil {
		if len(pc.Stop) > 1000 {
			return s, errors.New("slack extension: progress stop must list at most 1000 people")
		}
		var users []string
		for _, r := range pc.Stop {
			u, ok := strings.CutPrefix(r, "slack:")
			if !ok || !slackIDPattern.MatchString(u) || !strings.HasPrefix(u, "U") && !strings.HasPrefix(u, "W") {
				return s, fmt.Errorf("slack extension: progress stop entry %q must be slack:<user ID> (U… or W…)", r)
			}
			users = append(users, u)
		}
		sort.Strings(users)
		if len(slices.Compact(slices.Clone(users))) != len(users) {
			return s, errors.New("slack extension: progress stop lists a person more than once")
		}
		if len(users) > 0 && !pc.Enabled {
			return s, errors.New("slack extension: progress stop needs progress enabled")
		}
		s.Progress, s.StopUsers = pc.Enabled, users
	}
	if fc := c.Feedback; fc != nil {
		if len(fc.Bindings) > 1000 {
			return s, errors.New("slack extension: feedback bindings must list at most 1000 bindings")
		}
		bindings := slices.Clone(fc.Bindings)
		sort.Strings(bindings)
		for i, b := range bindings {
			if strings.TrimSpace(b) == "" || i > 0 && bindings[i-1] == b {
				return s, errors.New("slack extension: feedback bindings must be distinct binding names")
			}
		}
		if len(bindings) > 0 && !fc.Enabled {
			return s, errors.New("slack extension: feedback bindings needs feedback enabled")
		}
		s.Feedback, s.FeedbackBindings = fc.Enabled, bindings
	}
	s.ChildRequests = c.ChildRequests
	if u := c.Uploads; u != nil {
		s.Uploads, s.UploadMax = true, MaxUpload
		if u.MaxBytes != 0 {
			if u.MaxBytes < 1 || u.MaxBytes > MaxUpload {
				return s, fmt.Errorf("slack extension: uploads max_bytes must be 1-%d", MaxUpload)
			}
			s.UploadMax = u.MaxBytes
		}
		if len(u.MediaTypes) == 0 || len(u.MediaTypes) > 100 {
			return s, errors.New("slack extension: uploads media_types must list 1-100 media types (*/* accepts any)")
		}
		types := slices.Clone(u.MediaTypes)
		sort.Strings(types)
		for i, t := range types {
			if !mediaTypePattern.MatchString(t) || strings.HasPrefix(t, "*/") && t != "*/*" || i > 0 && types[i-1] == t {
				return s, fmt.Errorf("slack extension: uploads media_types must be distinct lower-case media types (image/png, image/* or */*), not %q", t)
			}
		}
		s.UploadTypes = types
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
	switch s.Projection.Format {
	case "":
		s.Projection.Format = FormatMarkdown
	case FormatMarkdown, FormatMrkdwn:
	default:
		return s, errors.New("slack extension: projection format must be markdown or mrkdwn")
	}
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
	if len(c.Identities) > 100 {
		return s, errors.New("slack extension: identities must list at most 100 bindings")
	}
	for _, id := range c.Identities {
		if !idPattern.MatchString(id.Binding) {
			return s, errors.New("slack extension: each identities entry must name a binding")
		}
		if _, dup := s.Identities[id.Binding]; dup {
			return s, fmt.Errorf("slack extension: identities lists binding %s more than once", id.Binding)
		}
		if err := validIdentity(id); err != nil {
			return s, fmt.Errorf("slack extension: identity for binding %s: %w", id.Binding, err)
		}
		if s.Identities == nil {
			s.Identities = map[string]BindingIdentity{}
		}
		s.Identities[id.Binding] = id
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

func validIdentity(id BindingIdentity) error {
	if id.Username == "" && id.IconEmoji == "" && id.IconURL == "" {
		return errors.New("set a username, an icon_emoji or an icon_url")
	}
	if id.Username != "" && (strings.TrimSpace(id.Username) != id.Username || len([]rune(id.Username)) > 80 || strings.ContainsAny(id.Username, "\r\n\t")) {
		return errors.New("username must be 1-80 characters on one line, without surrounding spaces")
	}
	if id.IconEmoji != "" && id.IconURL != "" {
		return errors.New("set icon_emoji or icon_url, not both")
	}
	if id.IconEmoji != "" && !emojiPattern.MatchString(id.IconEmoji) {
		return errors.New("icon_emoji must be an emoji code between colons (:wrench:)")
	}
	if id.IconURL != "" {
		u, err := url.Parse(id.IconURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(id.IconURL) > 2000 {
			return errors.New("icon_url must be an https URL without credentials")
		}
	}
	return nil
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
