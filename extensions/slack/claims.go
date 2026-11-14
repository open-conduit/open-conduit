package slack

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// ClaimNamespace is the extension's durable KV namespace for what limits
// the threads an entry claims: each channel's start point (when this
// daemon first registered it) and the threads found with replies marked by
// Work this daemon has no record of. It lives in the coordination database,
// so a backup bundle carries it and a restore keeps it.
const ClaimNamespace = "slack.claims"

// claimRecord is one stored start point or marked thread.
type claimRecord struct {
	Namespace string    `json:"namespace"`
	Channel   string    `json:"channel"`
	Thread    string    `json:"thread_ts,omitempty"`
	At        time.Time `json:"at"`
}

func startKey(namespace, channel string) string {
	return "start/" + url.PathEscape(namespace) + "/" + channel
}

func foreignPrefix(namespace string) string {
	return "foreign/" + url.PathEscape(namespace) + "/"
}

func foreignKey(namespace, channel, thread string) string {
	return foreignPrefix(namespace) + channel + "/" + thread
}

// StartBook is what the route reads to decide whether an entry may claim
// a thread no Work owns: the channel's recorded start point and whether
// the thread holds replies marked by Work this daemon has no record of.
// Both are write-once facts the extension recorded durably before any
// Event they bear on was accepted, so a route evaluated again decides the
// same way.
type StartBook interface {
	StartPoint(namespace, channel string) (time.Time, bool)
	Foreign(namespace, channel, thread string) bool
}

// Book is the extension's in-memory copy of its claim records, loaded at
// registration and kept in step with every record it writes, so the route
// reads it without a transaction.
type Book struct {
	mu      sync.RWMutex
	starts  map[string]time.Time
	foreign map[string]bool
}

var _ StartBook = (*Book)(nil)

// NewBook returns an empty book.
func NewBook() *Book {
	return &Book{starts: map[string]time.Time{}, foreign: map[string]bool{}}
}

// StartPoint is the time the channel was first registered for the entry.
func (b *Book) StartPoint(namespace, channel string) (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	t, ok := b.starts[startKey(namespace, channel)]
	return t, ok
}

// Foreign reports whether the thread was found with replies marked by
// Work this daemon has no record of.
func (b *Book) Foreign(namespace, channel, thread string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.foreign[foreignKey(namespace, channel, thread)]
}

// ForeignCount is how many threads of the entry's channel are so marked.
func (b *Book) ForeignCount(namespace, channel string) int {
	if b == nil {
		return 0
	}
	prefix := foreignPrefix(namespace) + channel + "/"
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for k := range b.foreign {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// Register records, once, the start point of each of the entry's channels
// that has none, and loads the entry's records into the book. A channel
// registered before keeps its first start point, so neither a restart nor
// a config change moves it. A channel's first start point is now, or, when
// Work here already holds one of its threads (the entry ran before start
// points were recorded), when the earliest such Work was created, so a
// message missed during the upgrade is still claimed.
func (b *Book) Register(tx storage.Tx, namespace string, channels []string, now time.Time) error {
	starts := map[string]time.Time{}
	var earliest map[string]time.Time
	for _, ch := range channels {
		if _, err := tx.Get(ClaimNamespace, startKey(namespace, ch)); errors.Is(err, storage.ErrNotFound) {
			var err error
			if earliest, err = earliestWork(tx, namespace); err != nil {
				return err
			}
			break
		}
	}
	for _, ch := range channels {
		key := startKey(namespace, ch)
		raw, err := tx.Get(ClaimNamespace, key)
		var rec claimRecord
		switch {
		case err == nil:
			if json.Unmarshal(raw, &rec) != nil || rec.At.IsZero() {
				return storage.ErrInvalid
			}
		case errors.Is(err, storage.ErrNotFound):
			at := now.UTC()
			if t, ok := earliest[ch]; ok && t.Before(at) {
				at = t.UTC()
			}
			rec = claimRecord{Namespace: namespace, Channel: ch, At: at}
			data, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := tx.Put(ClaimNamespace, key, data); err != nil {
				return err
			}
		default:
			return err
		}
		starts[key] = rec.At
	}
	entries, err := tx.List(ClaimNamespace, foreignPrefix(namespace))
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range starts {
		b.starts[k] = t
	}
	for _, e := range entries {
		b.foreign[e.Key] = true
	}
	return nil
}

// earliestWork is, per channel, when the earliest Work holding one of the
// channel's threads in the namespace was created.
func earliestWork(tx storage.Tx, namespace string) (map[string]time.Time, error) {
	works, err := tx.ListWorks()
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, w := range works {
		for _, k := range w.CorrelationKeys {
			if k.Source != namespace || w.CreatedAt.IsZero() {
				continue
			}
			if ch, _, ok := ParseOwnedKey(k.ExternalID); ok {
				if t, seen := out[ch]; !seen || w.CreatedAt.Before(t) {
					out[ch] = w.CreatedAt
				}
			}
		}
	}
	return out, nil
}

// MarkForeign records threads found with replies marked by Work this
// daemon has no record of. The caller puts them in the book once tx has
// committed (Remember).
func MarkForeign(tx storage.Tx, namespace, channel string, threads []string, now time.Time) error {
	for _, t := range threads {
		key := foreignKey(namespace, channel, t)
		if _, err := tx.Get(ClaimNamespace, key); err == nil {
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		data, err := json.Marshal(claimRecord{Namespace: namespace, Channel: channel, Thread: t, At: now.UTC()})
		if err != nil {
			return err
		}
		if err := tx.Put(ClaimNamespace, key, data); err != nil {
			return err
		}
	}
	return nil
}

// Remember puts committed foreign threads in the book.
func (b *Book) Remember(namespace, channel string, threads []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range threads {
		b.foreign[foreignKey(namespace, channel, t)] = true
	}
}

// SetStart puts a start point in the book without recording it, for a
// route evaluated away from a daemon (tests, fixtures).
func (b *Book) SetStart(namespace, channel string, t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts[startKey(namespace, channel)] = t.UTC()
}

// StartPoint is the start point of the entry's channel: the zero time for
// claim_from all (everything), the configured time, or the channel's
// recorded first registration. false when it is registration and none is
// recorded, so nothing is claimed.
func (s Settings) StartPoint(book StartBook, channel string) (time.Time, bool) {
	switch {
	case s.Claim == ClaimAll:
		return time.Time{}, true
	case s.Claim == "":
		return s.ClaimSince, true
	case book == nil:
		return time.Time{}, false
	}
	return book.StartPoint(s.Namespace, channel)
}

// claimRefusal says why the entry may not claim the thread ("" when it
// may): the thread holds replies marked by Work this daemon has no record
// of, or it began before the channel's start point. note qualifies a claim
// made with no recorded start point: the channel is not registered yet,
// as when a route trace evaluates a config not yet applied. Its start
// point will be recorded when it registers, before its source ingests
// anything, so no real Event of the channel reaches the route without one
// (an entry that cannot record its start points never registers its
// source), and the trace answers for a thread begun after registration.
func (s Settings) claimRefusal(book StartBook, channel, thread string) (refusal, note string) {
	if book != nil && book.Foreign(s.Namespace, channel, thread) {
		return "the thread has replies marked by Work this daemon has no record of: a daemon whose state was not moved here owned it", ""
	}
	start, ok := s.StartPoint(book, channel)
	if !ok {
		return "", "channel " + channel + " has no start point yet: it is recorded when the entry registers, and only threads begun after it are claimed"
	}
	began, ok := TSTime(thread)
	if !ok {
		return "the thread has no readable time", ""
	}
	if began.Before(start) {
		return "the thread began before channel " + channel + "'s start point " + start.Format(time.RFC3339) + " (claim_from " + s.claimFrom() + ")", ""
	}
	return "", ""
}

func (s Settings) claimFrom() string {
	if s.Claim == "" {
		return s.ClaimSince.Format(time.RFC3339)
	}
	return s.Claim
}
