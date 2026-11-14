package slack

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	// DirectPages bounds one listing of the app's direct messages, in
	// pages of 200 (default 50).
	DirectPages int
	// ActiveThreads lists, per channel, the threads live Work owns. Their
	// replies are reconciled even when the parent is older than
	// history_hours, so a multi-day wait still recovers missed replies. It is
	// a read of durable state supplied by the host; nil means none.
	ActiveThreads func(context.Context) (map[string][]string, error)
	// StartPoint is a channel's start point (Settings.StartPoint); nil or
	// false means none is known, and every thread counts as begun after it.
	StartPoint func(channel string) (time.Time, bool)
	// Unowned reports which of a channel's threads no Work here holds a
	// key for. It reads durable state; nil means every thread is owned, so
	// nothing is marked or counted.
	Unowned func(ctx context.Context, channel string, threads []string) (map[string]bool, error)
	// Foreign is given a channel's threads no Work here owns that hold
	// replies marked by a Work, with the Work IDs the markers name. It
	// records, durably, those whose markers name Work this daemon has no
	// record of, before the page they are in is returned, so the route
	// refuses to claim them, and returns the threads it recorded. An error
	// fails the page.
	Foreign func(ctx context.Context, channel string, marked map[string][]string) ([]string, error)
}

// Unclaimed is what a channel's last complete reconcile found that no Work
// here owns and the entry will not claim for its start point: threads that
// began before it and hold a message the start policy asks for, other than
// those with marked replies (which Foreign records durably).
type Unclaimed struct {
	BeforeStart int
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
	directs  *directMemo
	answered map[string]int
	memoID   string
	lastID   string
	// staged collects a generation's unclaimed threads per channel;
	// unclaimed is the last complete generation's.
	stagedID  string
	staged    map[string]map[string]bool
	unclaimed map[string]Unclaimed
}

type threadMemo struct {
	// asks says whether the replies hold a message the start policy asks
	// for, and marked the Work IDs their markers name.
	asks   bool
	marked []string
	latest string
	count  int
	readAt time.Time
	seen   string // generation that last used it
	obs    []extension.Observation
}

// directMemo is a generation's listing of the app's direct messages.
type directMemo struct {
	scanID   string
	channels []string
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
	if o.DirectPages <= 0 {
		o.DirectPages = 50
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
	if cb.TeamID != g.s.Team || e.Type != "message" || !g.s.Observes(e.Channel) {
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

// cursor is "<channel>:h:<offset>:<slack cursor>" for a history page
// (re-read deterministically from its Slack cursor, resumed at offset) or
// "<channel>:a:<after ts>" for owned threads older than the window.
// <channel> is a watched channel's index, or a direct message's own ID: the
// app's direct messages are listed afresh, so a cursor names one by ID and
// a DM opened since cannot shift it.
type cursor struct {
	channel int
	direct  string
	phase   string
	offset  int
	slack   string
	after   string
}

func (c cursor) at() string {
	if c.direct != "" {
		return c.direct
	}
	return strconv.Itoa(c.channel)
}

func (c cursor) String() string {
	if c.phase == phaseActive {
		return c.at() + ":a:" + c.after
	}
	return c.at() + ":h:" + strconv.Itoa(c.offset) + ":" + c.slack
}

func parseCursor(s string, channels int, directs bool) (cursor, bool) {
	parts := strings.SplitN(s, ":", 4)
	if len(parts) < 3 || len(s) > 4096 {
		return cursor{}, false
	}
	ci, direct := 0, ""
	if IsDirect(parts[0]) && directs {
		direct = parts[0]
	} else if n, err := strconv.Atoi(parts[0]); err != nil || n < 0 || n >= channels {
		return cursor{}, false
	} else {
		ci = n
	}
	switch parts[1] {
	case phaseActive:
		if len(parts) != 3 || (parts[2] != "" && !tsPattern.MatchString(parts[2])) {
			return cursor{}, false
		}
		return cursor{channel: ci, direct: direct, phase: phaseActive, after: parts[2]}, true
	case phaseHistory:
		if len(parts) != 4 {
			return cursor{}, false
		}
		off, err := strconv.Atoi(parts[2])
		if err != nil || off < 0 || off > 1000 {
			return cursor{}, false
		}
		return cursor{channel: ci, direct: direct, phase: phaseHistory, offset: off, slack: parts[3]}, true
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
		if c, ok = parseCursor(scan.Cursor, len(g.s.Channels), g.s.DirectMessages); !ok {
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
	if len(g.s.Channels) == 0 && c.direct == "" {
		// An entry of direct messages only starts at the first of them.
		var done bool
		if c, done, err = g.nextDirect(ctx, scanID, "", b); err != nil {
			return extension.Snapshot{}, err
		}
		if done {
			c.phase = ""
		}
	}
	switch {
	case c.phase == "":
		snap.Complete = true
	case c.phase == phaseActive:
		snap, err = g.activePage(ctx, scanID, start, c, b)
	default:
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
		if g.stagedID == scanID {
			out := map[string]Unclaimed{}
			for ch, u := range g.staged {
				out[ch] = Unclaimed{BeforeStart: len(u)}
			}
			g.unclaimed = out
		}
	}
	g.mu.Unlock()
	return snap, nil
}

func (g *Ingress) window(start time.Time) (oldest, latest time.Time) {
	return start.Add(-g.s.History), start
}

// channelOf is the channel a cursor reads.
func (g *Ingress) channelOf(c cursor) string {
	if c.direct != "" {
		return c.direct
	}
	return g.s.Channels[c.channel]
}

// next advances past a finished phase of c's channel: its owned threads
// after its history, then the next watched channel, then the app's direct
// messages in ID order.
func (g *Ingress) next(ctx context.Context, scanID string, c cursor, b *budget) (string, bool, error) {
	if c.phase == phaseHistory {
		return cursor{channel: c.channel, direct: c.direct, phase: phaseActive}.String(), false, nil
	}
	if c.direct == "" && c.channel+1 < len(g.s.Channels) {
		return cursor{channel: c.channel + 1, phase: phaseHistory}.String(), false, nil
	}
	n, done, err := g.nextDirect(ctx, scanID, c.direct, b)
	if err != nil || done {
		return "", done, err
	}
	return n.String(), false, nil
}

// nextDirect is the history cursor of the first direct message after
// after (the first of all when empty), or done when there is none. The
// DMs read are computed once per generation (directsFor).
func (g *Ingress) nextDirect(ctx context.Context, scanID, after string, b *budget) (cursor, bool, error) {
	if !g.s.DirectMessages {
		return cursor{}, true, nil
	}
	g.mu.Lock()
	memo := g.directs
	g.mu.Unlock()
	var list []string
	if memo != nil && memo.scanID == scanID {
		list = memo.channels
	} else {
		var err error
		if list, err = g.directsFor(ctx, b); err != nil {
			return cursor{}, false, err
		}
		g.mu.Lock()
		g.directs = &directMemo{scanID: scanID, channels: list}
		g.mu.Unlock()
	}
	for _, ch := range list {
		if IsDirect(ch) && ch > after {
			return cursor{direct: ch, phase: phaseHistory}, false, nil
		}
	}
	return cursor{}, true, nil
}

// directsFor is the sorted DMs a generation reads: those with a person
// direct_messages admits, since only they can start Work or be its input,
// and every DM live Work owns a thread in, whoever it is with. Reconcile
// therefore costs one history read per admitted person's DM, not per
// person who ever wrote to the app. A listing longer than DirectPages
// keeps what it read rather than failing the generation, so the watched
// channels are still reconciled and live conversations still recovered;
// DMs past the bound go unread and are recovered only by push.
func (g *Ingress) directsFor(ctx context.Context, b *budget) ([]string, error) {
	listed, calls, _, err := g.c.DirectMessages(ctx, g.o.DirectPages)
	b.units += calls
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(ch string) {
		if IsDirect(ch) && !seen[ch] {
			seen[ch] = true
			out = append(out, ch)
		}
	}
	for _, d := range listed {
		if g.s.AdmitsDirect(d.User) {
			add(d.ID)
		}
	}
	if g.o.ActiveThreads != nil {
		active, err := g.o.ActiveThreads(ctx)
		if err != nil {
			return nil, err
		}
		for ch := range active {
			add(ch)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (g *Ingress) historyPage(ctx context.Context, scanID string, start time.Time, c cursor, b *budget) (extension.Snapshot, error) {
	channel := g.channelOf(c)
	oldest, latest := g.window(start)
	key := scanID + "|" + c.at() + "|" + c.slack
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
	var before []string
	marked := map[string][]string{}
	start, bounded := time.Time{}, false
	if g.o.StartPoint != nil {
		start, bounded = g.o.StartPoint(channel)
	}
	i := c.offset
	for ; i < len(messages); i++ {
		if i > c.offset && b.spent() {
			break
		}
		m := messages[i]
		if m.ThreadTS != "" && m.ThreadTS != m.TS {
			continue // a broadcast reply: observed with its thread
		}
		asks := false
		if o, err := normalize(g.s, channel, m, g.now()); err == nil {
			out = append(out, o)
			asks = g.asks(o)
		} else if !errors.Is(err, errSkip) {
			return extension.Snapshot{}, err
		}
		if m.ReplyCount != 0 || m.LatestReply != "" {
			obs, err := g.thread(ctx, scanID, channel, m.TS, m, latest, false, b)
			if err != nil {
				return extension.Snapshot{}, err
			}
			out = append(out, obs...)
			g.mu.Lock()
			if memo := g.threads[ThreadKey(channel, m.TS)]; memo != nil {
				asks = asks || memo.asks
				if len(memo.marked) > 0 {
					marked[m.TS] = memo.marked
				}
			}
			g.mu.Unlock()
		}
		if began, ok := TSTime(m.TS); ok && asks && bounded && began.Before(start) {
			before = append(before, m.TS)
		}
	}
	if err := g.unclaimedThreads(ctx, scanID, channel, before, marked); err != nil {
		return extension.Snapshot{}, err
	}
	snap := extension.Snapshot{Observations: out}
	switch {
	case i < len(messages):
		snap.NextCursor = cursor{channel: c.channel, direct: c.direct, phase: phaseHistory, offset: i, slack: c.slack}.String()
	case next != "":
		snap.NextCursor = cursor{channel: c.channel, direct: c.direct, phase: phaseHistory, slack: next}.String()
	default:
		var err error
		if snap.NextCursor, snap.Complete, err = g.next(ctx, scanID, c, b); err != nil {
			return extension.Snapshot{}, err
		}
	}
	return snap, nil
}

func (g *Ingress) activePage(ctx context.Context, scanID string, start time.Time, c cursor, b *budget) (extension.Snapshot, error) {
	channel := g.channelOf(c)
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
		snap.NextCursor = cursor{channel: c.channel, direct: c.direct, phase: phaseActive, after: threads[done-1]}.String()
	} else {
		var err error
		if snap.NextCursor, snap.Complete, err = g.next(ctx, scanID, c, b); err != nil {
			return extension.Snapshot{}, err
		}
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
	asks, marked := false, []string(nil)
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
		asks = asks || g.asks(o)
		if a := markerOf(msg.Metadata); a != nil && a.Work != "" && !slices.Contains(marked, a.Work) {
			marked = append(marked, a.Work)
		}
	}
	if !withParent {
		g.mu.Lock()
		g.threads[key] = &threadMemo{asks: asks, marked: marked, latest: parent.LatestReply, count: parent.ReplyCount, readAt: now, seen: scanID, obs: out}
		g.mu.Unlock()
	}
	return out, nil
}

// asks reports whether an observation is a person's message the start
// policy asks for (Settings.asks), whatever its age.
func (g *Ingress) asks(o extension.Observation) bool {
	if o.Event.Type != TypeMessageHuman {
		return false
	}
	var p MessagePayload
	return json.Unmarshal(o.Event.Data.Data, &p) == nil && g.s.asks(p)
}

// unclaimedThreads finds which of a page's threads (begun before the start
// point and asking for a conversation, or holding marked replies) no Work
// here owns, records the marked ones before the page is returned, and
// stages both for doctor (Unclaimed).
func (g *Ingress) unclaimedThreads(ctx context.Context, scanID, channel string, before []string, marked map[string][]string) error {
	if g.o.Unowned == nil || len(before)+len(marked) == 0 {
		return nil
	}
	threads := append([]string{}, before...)
	for t := range marked {
		threads = append(threads, t)
	}
	unowned, err := g.o.Unowned(ctx, channel, threads)
	if err != nil {
		return err
	}
	candidates := map[string][]string{}
	for t, works := range marked {
		if unowned[t] {
			candidates[t] = works
		}
	}
	var foreign []string
	if len(candidates) > 0 && g.o.Foreign != nil {
		if foreign, err = g.o.Foreign(ctx, channel, candidates); err != nil {
			return err
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stagedID != scanID {
		g.stagedID, g.staged = scanID, map[string]map[string]bool{}
	}
	u := g.staged[channel]
	if u == nil {
		u = map[string]bool{}
		g.staged[channel] = u
	}
	for _, t := range before {
		if unowned[t] && !slices.Contains(foreign, t) {
			u[t] = true
		}
	}
	return nil
}

// Unclaimed is, per channel, what the last complete reconcile found no
// Work here owns and the entry will not claim.
func (g *Ingress) Unclaimed() map[string]Unclaimed {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]Unclaimed, len(g.unclaimed))
	for ch, u := range g.unclaimed {
		out[ch] = u
	}
	return out
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
