package slack

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// IngressOptions tune scan pacing (zero values select defaults).
type IngressOptions struct {
	// BatchUnits bounds Web API calls per reconcile page.
	BatchUnits int
	// PageBudget bounds wall time spent on one page before yielding a cursor.
	PageBudget time.Duration
	// MaxScanAge restarts a generation that has not completed in time.
	MaxScanAge time.Duration
	// MaxRepeats restarts a generation whose same page keeps being
	// re-requested after a successful answer (the runtime rejected it).
	MaxRepeats int
	// PageSize is the history page size (default 200).
	PageSize int
	// ActiveThreads lists, per channel, the threads live Work owns. Their
	// replies are reconciled even when the parent is older than
	// history_hours, so a multi-day wait still recovers missed replies. It is
	// a read of durable state supplied by the host; nil means none.
	ActiveThreads func(context.Context) (map[string][]string, error)
}

// Ingress is one app installation's (push, reconcile) pair.
type Ingress struct {
	s   Settings
	c   *Client
	now func() time.Time
	o   IngressOptions

	mu       sync.Mutex
	threads  map[string]*threadMemo // by thread key, across generations
	page     *pageMemo
	answered map[string]int
	memoID   string
	lastID   string
}

type threadMemo struct {
	latest string
	count  int
	readAt time.Time
	seen   string // generation that last used it
	obs    []extension.Observation
}

type pageMemo struct {
	key      string
	messages []Message
	next     string
}

var _ extension.Ingress = (*Ingress)(nil)

// NewIngress binds an extension entry to a client.
func NewIngress(s Settings, c *Client, now func() time.Time, o IngressOptions) *Ingress {
	if now == nil {
		now = time.Now
	}
	if o.BatchUnits <= 0 {
		o.BatchUnits = 20
	}
	if o.PageBudget <= 0 {
		o.PageBudget = 10 * time.Second
	}
	if o.MaxScanAge <= 0 {
		o.MaxScanAge = 6 * time.Hour
	}
	if o.MaxRepeats <= 0 {
		o.MaxRepeats = 3
	}
	if o.PageSize <= 0 || o.PageSize > 999 {
		o.PageSize = 200
	}
	return &Ingress{s: s, c: c, now: now, o: o, threads: map[string]*threadMemo{}, answered: map[string]int{}}
}

// Settings returns the bound extension settings.
func (g *Ingress) Settings() Settings { return g.s }

// Spec declares every family; only push-only deletions are non-re-derivable.
func (g *Ingress) Spec() extension.IngressSpec { return extension.IngressSpec{Families: Families()} }

func (g *Ingress) checkSource(src extension.Source) error {
	if src.Namespace != g.s.Namespace || string(src.ScopeID) != g.s.Scope {
		return extension.ErrInvalid
	}
	return nil
}

type pushEvent struct {
	Message
	Channel   string   `json:"channel"`
	Changed   *Message `json:"message"`
	Previous  *Message `json:"previous_message"`
	DeletedTS string   `json:"deleted_ts"`
}

type callback struct {
	Type      string    `json:"type"`
	TeamID    string    `json:"team_id"`
	EventID   string    `json:"event_id"`
	EventTime int64     `json:"event_time"`
	Event     pushEvent `json:"event"`
}

// Push normalizes one verified Events API callback. It makes no service calls
// and no coordination writes. The caller must have verified the signature
// (VerifyDelivery). Retries of one callback share its event ID, and every
// message's identity is its own observed state, so a retried or overlapping
// delivery deduplicates.
func (g *Ingress) Push(_ context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var out extension.PushResult
	if err := g.checkSource(src); err != nil {
		return out, err
	}
	if d.ContentType != DeliveryContentType {
		return out, extension.ErrInvalid
	}
	var cb callback
	if err := json.Unmarshal(d.Body, &cb); err != nil || cb.Type != "event_callback" || cb.EventID == "" || cb.EventID != d.ID {
		return out, extension.ErrInvalid
	}
	e := cb.Event
	if cb.TeamID != g.s.Team || e.Type != "message" || !g.s.Watches(e.Channel) {
		return out, nil
	}
	received := d.ReceivedAt.UTC()
	if received.IsZero() {
		received = g.now().UTC()
	}
	var m Message
	switch e.Subtype {
	case "message_changed":
		if e.Changed == nil {
			return out, nil
		}
		m = *e.Changed
	case "message_deleted":
		ts := e.DeletedTS
		thread := ""
		if e.Previous != nil {
			if ts == "" {
				ts = e.Previous.TS
			}
			thread = e.Previous.ThreadTS
		}
		if thread == "" {
			thread = ts
		}
		if !tsPattern.MatchString(ts) || !tsPattern.MatchString(thread) {
			return out, nil
		}
		at := received
		if cb.EventTime > 0 {
			at = time.Unix(cb.EventTime, 0)
		}
		out.Events = []domain.Event{removedEvent(g.s, e.Channel, thread, ts, at, received)}
		return out, nil
	default:
		m = e.Message
	}
	o, err := normalize(g.s, e.Channel, m, received)
	if errors.Is(err, errSkip) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Observations = []extension.Observation{o}
	return out, nil
}

// ---- reconcile ------------------------------------------------------------

const (
	phaseHistory = "h"
	phaseActive  = "a"
)

// cursor is "<channel index>:h:<offset>:<slack cursor>" for a history page
// (re-read deterministically from its Slack cursor, resumed at offset) or
// "<channel index>:a:<after ts>" for owned threads older than the window.
type cursor struct {
	channel int
	phase   string
	offset  int
	slack   string
	after   string
}

func (c cursor) String() string {
	if c.phase == phaseActive {
		return strconv.Itoa(c.channel) + ":a:" + c.after
	}
	return strconv.Itoa(c.channel) + ":h:" + strconv.Itoa(c.offset) + ":" + c.slack
}

func parseCursor(s string, channels int) (cursor, bool) {
	parts := strings.SplitN(s, ":", 4)
	if len(parts) < 3 || len(s) > 4096 {
		return cursor{}, false
	}
	ci, err := strconv.Atoi(parts[0])
	if err != nil || ci < 0 || ci >= channels {
		return cursor{}, false
	}
	switch parts[1] {
	case phaseActive:
		if len(parts) != 3 || (parts[2] != "" && !tsPattern.MatchString(parts[2])) {
			return cursor{}, false
		}
		return cursor{channel: ci, phase: phaseActive, after: parts[2]}, true
	case phaseHistory:
		if len(parts) != 4 {
			return cursor{}, false
		}
		off, err := strconv.Atoi(parts[2])
		if err != nil || off < 0 || off > 1000 {
			return cursor{}, false
		}
		return cursor{channel: ci, phase: phaseHistory, offset: off, slack: parts[3]}, true
	}
	return cursor{}, false
}

func scanStart(id string) (time.Time, bool) {
	v, ok := strings.CutPrefix(id, "gen-")
	if !ok {
		return time.Time{}, false
	}
	nanos, err := strconv.ParseInt(v, 10, 64)
	if err != nil || nanos <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, nanos).UTC(), true
}

type budget struct {
	started time.Time
	limit   time.Duration
	units   int
	max     int
}

func (b *budget) spent() bool { return b.units >= b.max || time.Since(b.started) >= b.limit }

// Reconcile returns one bounded page of a generation. A generation reads a
// fixed window [start-history_hours, start] of every channel: each page of
// top-level messages (newest first, cursor-stable) and, for each thread
// parent, the thread's replies up to start; then the threads live Work owns
// whose parent is older than the window. Unchanged threads (same reply count
// and latest reply) are taken from memory for refresh_minutes, so a quiet
// channel costs one history read per page. Slack has no snapshot token:
// Complete certifies that every listing was read to its end within this
// generation's window. Rate limits and failed reads return errors, so no
// absence is inferred.
func (g *Ingress) Reconcile(ctx context.Context, scan extension.Scan) (extension.Snapshot, error) {
	if err := g.checkSource(scan.Source); err != nil {
		return extension.Snapshot{}, err
	}
	now := g.now().UTC()
	scanID := scan.ScanID
	start, ok := scanStart(scanID)
	if scanID == "" {
		start, scanID = now, "gen-"+strconv.FormatInt(now.UnixNano(), 10)
	} else if !ok {
		return extension.Snapshot{ScanID: scanID}, nil // foreign generation: restart
	}
	c := cursor{phase: phaseHistory}
	if scan.Cursor != "" {
		if c, ok = parseCursor(scan.Cursor, len(g.s.Channels)); !ok {
			return extension.Snapshot{ScanID: scanID}, nil
		}
	}
	g.mu.Lock()
	if scan.ScanID == "" {
		// A new generation never reuses an earlier one's identity.
		for scanID == g.lastID {
			start = start.Add(time.Microsecond)
			scanID = "gen-" + strconv.FormatInt(start.UnixNano(), 10)
		}
		g.lastID = scanID
	}
	answerKey := scanID + "|" + c.String()
	if g.memoID != scanID {
		g.memoID, g.answered, g.page = scanID, map[string]int{}, nil
	}
	repeats := g.answered[answerKey]
	g.mu.Unlock()
	if scan.ScanID != "" && (now.Sub(start) > g.o.MaxScanAge || repeats >= g.o.MaxRepeats) {
		// An exhausted partial page discards staging; the next scan starts afresh.
		return extension.Snapshot{ScanID: scanID}, nil
	}
	b := &budget{started: time.Now(), limit: g.o.PageBudget, max: g.o.BatchUnits}
	var snap extension.Snapshot
	var err error
	if c.phase == phaseActive {
		snap, err = g.activePage(ctx, scanID, start, c, b)
	} else {
		snap, err = g.historyPage(ctx, scanID, start, c, b)
	}
	if err != nil {
		return extension.Snapshot{}, err
	}
	snap.ScanID, snap.Checkpoint = scanID, scanID
	snap.Observations = dedupe(snap.Observations)
	g.mu.Lock()
	if g.memoID == scanID {
		g.answered[answerKey]++
	}
	if snap.Complete {
		for key, m := range g.threads {
			if m.seen != scanID {
				delete(g.threads, key)
			}
		}
	}
	g.mu.Unlock()
	return snap, nil
}

func (g *Ingress) window(start time.Time) (oldest, latest time.Time) {
	return start.Add(-g.s.History), start
}

// next advances past a finished phase of channel ci.
func (g *Ingress) next(ci int, phase string) (string, bool) {
	if phase == phaseHistory {
		return cursor{channel: ci, phase: phaseActive}.String(), false
	}
	if ci+1 < len(g.s.Channels) {
		return cursor{channel: ci + 1, phase: phaseHistory}.String(), false
	}
	return "", true
}

func (g *Ingress) historyPage(ctx context.Context, scanID string, start time.Time, c cursor, b *budget) (extension.Snapshot, error) {
	channel := g.s.Channels[c.channel]
	oldest, latest := g.window(start)
	key := scanID + "|" + strconv.Itoa(c.channel) + "|" + c.slack
	g.mu.Lock()
	memo := g.page
	g.mu.Unlock()
	var messages []Message
	var next string
	if memo != nil && memo.key == key {
		messages, next = memo.messages, memo.next
	} else {
		var err error
		messages, next, err = g.c.History(ctx, channel, TS(oldest), TS(latest), c.slack, g.o.PageSize)
		b.units++
		if err != nil {
			return extension.Snapshot{}, err
		}
		g.mu.Lock()
		g.page = &pageMemo{key: key, messages: messages, next: next}
		g.mu.Unlock()
	}
	if c.offset > len(messages) {
		return extension.Snapshot{}, extension.ErrInvalid
	}
	var out []extension.Observation
	i := c.offset
	for ; i < len(messages); i++ {
		if i > c.offset && b.spent() {
			break
		}
		m := messages[i]
		if m.ThreadTS != "" && m.ThreadTS != m.TS {
			continue // a broadcast reply: observed with its thread
		}
		if o, err := normalize(g.s, channel, m, g.now()); err == nil {
			out = append(out, o)
		} else if !errors.Is(err, errSkip) {
			return extension.Snapshot{}, err
		}
		if m.ReplyCount == 0 && m.LatestReply == "" {
			continue
		}
		obs, err := g.thread(ctx, scanID, channel, m.TS, m, latest, false, b)
		if err != nil {
			return extension.Snapshot{}, err
		}
		out = append(out, obs...)
	}
	snap := extension.Snapshot{Observations: out}
	switch {
	case i < len(messages):
		snap.NextCursor = cursor{channel: c.channel, phase: phaseHistory, offset: i, slack: c.slack}.String()
	case next != "":
		snap.NextCursor = cursor{channel: c.channel, phase: phaseHistory, slack: next}.String()
	default:
		snap.NextCursor, snap.Complete = g.next(c.channel, phaseHistory)
	}
	return snap, nil
}

func (g *Ingress) activePage(ctx context.Context, scanID string, start time.Time, c cursor, b *budget) (extension.Snapshot, error) {
	channel := g.s.Channels[c.channel]
	oldest, latest := g.window(start)
	var threads []string
	if g.o.ActiveThreads != nil {
		all, err := g.o.ActiveThreads(ctx)
		if err != nil {
			return extension.Snapshot{}, err
		}
		after, _ := TSTime(c.after)
		for _, ts := range all[channel] {
			t, ok := TSTime(ts)
			// Threads whose parent is inside the window were read with history.
			if !ok || !t.Before(oldest) || (c.after != "" && !t.After(after)) {
				continue
			}
			threads = append(threads, ts)
		}
	}
	sort.Slice(threads, func(i, j int) bool {
		a, _ := TSTime(threads[i])
		b, _ := TSTime(threads[j])
		return a.Before(b)
	})
	var out []extension.Observation
	done := 0
	for ; done < len(threads); done++ {
		if done > 0 && b.spent() {
			break
		}
		obs, err := g.thread(ctx, scanID, channel, threads[done], Message{}, latest, true, b)
		if err != nil {
			return extension.Snapshot{}, err
		}
		out = append(out, obs...)
	}
	snap := extension.Snapshot{Observations: out}
	if done < len(threads) {
		snap.NextCursor = cursor{channel: c.channel, phase: phaseActive, after: threads[done-1]}.String()
	} else {
		snap.NextCursor, snap.Complete = g.next(c.channel, phaseActive)
	}
	return snap, nil
}

// thread returns a thread's observations up to latest. withParent includes
// the parent (owned threads older than the window, whose parent history did
// not list); otherwise history supplied the parent and its reply summary
// decides whether the remembered read is still current.
func (g *Ingress) thread(ctx context.Context, scanID, channel, thread string, parent Message, latest time.Time, withParent bool, b *budget) ([]extension.Observation, error) {
	key := ThreadKey(channel, thread)
	now := g.now()
	g.mu.Lock()
	m := g.threads[key]
	if !withParent && m != nil && m.latest == parent.LatestReply && m.count == parent.ReplyCount && now.Sub(m.readAt) < g.s.Refresh {
		m.seen = scanID
		obs := m.obs
		g.mu.Unlock()
		return obs, nil
	}
	g.mu.Unlock()
	messages, calls, err := g.c.Thread(ctx, channel, thread, "", TS(latest))
	b.units += calls
	if err != nil {
		return nil, err
	}
	var out []extension.Observation
	for _, msg := range messages {
		if msg.ThreadTS == "" {
			msg.ThreadTS = thread
		}
		if msg.ThreadTS != thread || (msg.TS == thread && !withParent) {
			continue
		}
		o, err := normalize(g.s, channel, msg, now)
		if errors.Is(err, errSkip) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if !withParent {
		g.mu.Lock()
		g.threads[key] = &threadMemo{latest: parent.LatestReply, count: parent.ReplyCount, readAt: now, seen: scanID, obs: out}
		g.mu.Unlock()
	}
	return out, nil
}

// dedupe drops exact repeats within one page (same item and version).
func dedupe(obs []extension.Observation) []extension.Observation {
	seen := map[string]bool{}
	out := []extension.Observation{}
	for _, o := range obs {
		if !seen[o.ID+"@"+o.Version] {
			seen[o.ID+"@"+o.Version] = true
			out = append(out, o)
		}
	}
	return out
}
