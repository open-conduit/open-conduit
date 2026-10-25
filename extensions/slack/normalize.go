package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// Event families. Author class is in the type, so a Wait can filter out the
// agent's own and other bots' messages without a payload predicate.
const (
	TypeMessageHuman = "slack.message.human"
	TypeMessageBot   = "slack.message.bot"
	TypeMessageAgent = "slack.message.agent"
	// TypeMessageDeleted is a deleted message Slack still shows as a
	// tombstone (a thread parent with replies): recoverable observed state.
	TypeMessageDeleted = "slack.message.deleted"
	// TypeMessageRemoved is a push-only deletion notice for a message Slack
	// no longer shows at all. It is not re-derivable.
	TypeMessageRemoved = "slack.message.removed"
)

// Payload schemas.
const (
	SchemaMessage = "slack.message.v1"
	SchemaRemoved = "slack.message.removed.v1"
)

// RemovedLoss is the declared loss limitation of TypeMessageRemoved.
const RemovedLoss = "a message deleted without a remaining tombstone is only announced by push; reconcile shows it as generic core.ingress.absent within history_hours, without its deletion time"

// Families declares every ingress family.
func Families() []extension.EventFamily {
	return []extension.EventFamily{
		{Type: TypeMessageHuman, ReDerivable: true},
		{Type: TypeMessageBot, ReDerivable: true},
		{Type: TypeMessageAgent, ReDerivable: true},
		{Type: TypeMessageDeleted, ReDerivable: true},
		{Type: TypeMessageRemoved, Loss: RemovedLoss},
	}
}

// Agent is the provenance an agent reply's marker carries: the Work and
// Execution that wrote it and the marker form of its intent.
type Agent struct {
	Work      string `json:"work,omitempty"`
	Execution string `json:"execution,omitempty"`
	Intent    string `json:"intent,omitempty"`
}

// MessagePayload is the normalized SchemaMessage payload. Thread is the
// thread's parent ts (equal to TS for a top-level message). Reply counts and
// latest-reply times are deliberately omitted, so a reply never changes its
// parent's observation and push equals reconcile.
type MessagePayload struct {
	Team     string   `json:"team"`
	Channel  string   `json:"channel"`
	Thread   string   `json:"thread_ts"`
	TS       string   `json:"ts"`
	Root     bool     `json:"root"`
	User     string   `json:"user,omitempty"`
	BotID    string   `json:"bot_id,omitempty"`
	Subtype  string   `json:"subtype,omitempty"`
	Text     string   `json:"text,omitempty"`
	Mentions []string `json:"mentions,omitempty"`
	Files    []File   `json:"files,omitempty"`
	// Edited is the edit's ts when the message was edited.
	Edited  string `json:"edited,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Agent   *Agent `json:"agent,omitempty"`
}

// RemovedPayload is the SchemaRemoved payload.
type RemovedPayload struct {
	Team    string `json:"team"`
	Channel string `json:"channel"`
	Thread  string `json:"thread_ts"`
	TS      string `json:"ts"`
}

var (
	tsPattern      = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,9}$`)
	mentionPattern = regexp.MustCompile(`<@([A-Z0-9]{2,32})(?:\|[^>]*)?>`)
	threadKey      = regexp.MustCompile(`^channel/([A-Z0-9]{2,32})/thread/([0-9]{1,12}\.[0-9]{1,9})$`)
)

// ThreadKey is the correlation identity of a thread within a namespace.
func ThreadKey(channel, thread string) string { return "channel/" + channel + "/thread/" + thread }

// ParseThreadKey splits a ThreadKey.
func ParseThreadKey(key string) (channel, thread string, ok bool) {
	m := threadKey.FindStringSubmatch(key)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func itemID(channel, thread, ts string) string {
	return ThreadKey(channel, thread) + "/message/" + ts
}

// TSTime parses a Slack ts ("seconds.micros") into a time.
func TSTime(ts string) (time.Time, bool) {
	if !tsPattern.MatchString(ts) {
		return time.Time{}, false
	}
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	frac = (frac + "000000000")[:9]
	n, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(s, n).UTC(), true
}

// TS formats a time as a Slack ts bound (for oldest/latest parameters).
func TS(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// observed lists the subtypes that are conversation content. Everything else
// (joins, topic changes, pins…) is ignored by push and reconcile alike.
var observed = map[string]bool{"": true, "thread_broadcast": true, "file_share": true, "me_message": true, "bot_message": true, "tombstone": true}

// maxText bounds a normalized message body (Slack caps text at 40,000).
const maxText = 40000

// errSkip means the message is not conversation content.
var errSkip = errors.New("not observed")

// MarkerEventType is the metadata event_type an agent reply carries.
const MarkerEventType = "open_conduit_reply"

// markerOf decodes an agent marker from message metadata.
func markerOf(m *Metadata) *Agent {
	if m == nil || m.EventType != MarkerEventType || len(m.EventPayload) == 0 {
		return nil
	}
	var a Agent
	if json.Unmarshal(m.EventPayload, &a) != nil || a.Intent == "" {
		return nil
	}
	return &a
}

func mentions(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionPattern.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// normalize turns one message into its canonical observation. Push and
// reconcile both call it, so equal external state yields equal identity: the
// version hashes the whole normalized envelope (type, key, occurrence time,
// payload) and the delivery ID is item@version.
func normalize(s Settings, channel string, m Message, received time.Time) (extension.Observation, error) {
	if !observed[m.Subtype] || !tsPattern.MatchString(m.TS) {
		return extension.Observation{}, errSkip
	}
	thread := m.ThreadTS
	if thread == "" {
		thread = m.TS
	}
	if !tsPattern.MatchString(thread) {
		return extension.Observation{}, errSkip
	}
	p := MessagePayload{Team: s.Team, Channel: channel, Thread: thread, TS: m.TS, Root: thread == m.TS, User: m.User, BotID: m.BotID}
	occurred, _ := TSTime(m.TS)
	typ := TypeMessageHuman
	switch {
	case m.Subtype == "tombstone":
		typ, p.Deleted, p.User, p.BotID = TypeMessageDeleted, true, "", ""
	default:
		p.Text = m.Text
		if len(p.Text) > maxText {
			p.Text = p.Text[:maxText]
		}
		p.Mentions = mentions(p.Text)
		if m.Subtype == "me_message" || m.Subtype == "thread_broadcast" || m.Subtype == "file_share" {
			p.Subtype = m.Subtype
		}
		for i, f := range m.Files {
			if i >= 20 {
				break
			}
			p.Files = append(p.Files, File{ID: f.ID, Name: f.Name})
		}
		if m.Edited != nil && tsPattern.MatchString(m.Edited.TS) {
			p.Edited = m.Edited.TS
			if t, ok := TSTime(m.Edited.TS); ok {
				occurred = t
			}
		}
		switch {
		case markerOf(m.Metadata) != nil:
			typ, p.Agent = TypeMessageAgent, markerOf(m.Metadata)
		case s.BotUser != "" && m.User == s.BotUser:
			// The app's own message without a marker (posted by other
			// tooling with this token): an agent note of no known Work.
			typ, p.Agent = TypeMessageAgent, &Agent{}
		case m.BotID != "" || m.Subtype == "bot_message":
			typ = TypeMessageBot
		case m.User == "":
			return extension.Observation{}, errSkip
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return extension.Observation{}, err
	}
	key := ThreadKey(channel, thread)
	item := itemID(channel, thread, m.TS)
	version := digest(typ, key, occurred.Format(time.RFC3339Nano), SchemaMessage, string(data))[:32]
	delivery := item + "@" + version
	e := domain.Event{ID: domain.ID("slack-" + digest(s.Namespace, delivery)[:40]), Type: typ, Source: s.Namespace, DeliveryID: delivery,
		OccurredAt: occurred, ReceivedAt: received.UTC(), CorrelationKey: &domain.CorrelationKey{Source: s.Namespace, ExternalID: key},
		Data: domain.Payload{Schema: SchemaMessage, Data: data}}
	return extension.Observation{ID: item, Version: version, Event: e}, nil
}

// removedEvent is the direct, non-re-derivable deletion notice.
func removedEvent(s Settings, channel, thread, ts string, at, received time.Time) domain.Event {
	data, _ := json.Marshal(RemovedPayload{Team: s.Team, Channel: channel, Thread: thread, TS: ts})
	delivery := "removed/" + channel + "/" + ts
	return domain.Event{ID: domain.ID("slack-" + digest(s.Namespace, delivery)[:40]), Type: TypeMessageRemoved, Source: s.Namespace, DeliveryID: delivery,
		OccurredAt: at.UTC(), ReceivedAt: received.UTC(), CorrelationKey: &domain.CorrelationKey{Source: s.Namespace, ExternalID: ThreadKey(channel, thread)},
		Data: domain.Payload{Schema: SchemaRemoved, Data: data}}
}
