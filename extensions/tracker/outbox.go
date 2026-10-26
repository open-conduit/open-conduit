package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Outbox publishes non-re-derivable writes (comments) exactly once as
// observed through their mark. Each item has a stable ID and a durable
// record that moves pending → attempted (recorded before the write) →
// posted. A write whose answer was lost stays attempted; after SearchDelay
// the item's mark is searched for and the record completed instead of
// writing again. A changed body under a reused ID is refused.
type Outbox struct {
	Store storage.Store
	// Namespace is the KV namespace of the records.
	Namespace   string
	Now         func() time.Time
	SearchDelay time.Duration
	DryRun      bool
	// AfterPost runs after a write succeeded and before local completion is
	// recorded (tests crash at exactly that boundary).
	AfterPost func(id string)
}

// OutboxItem is one write.
type OutboxItem struct {
	ID   string
	Work domain.ID
	// Target names where it goes (an issue key, "pr/12", "pr/12/reply/34").
	Target string
	Body   string
	// Durable keeps the body in the record so a later pass finishes a write
	// whose caller is gone (tool calls); other items are re-derived from
	// durable outcomes instead.
	Durable bool
}

type outboxRecord struct {
	ID          string    `json:"id"`
	Work        domain.ID `json:"work"`
	Target      string    `json:"target"`
	BodyDigest  string    `json:"body_digest"`
	State       string    `json:"state"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	PostedID    string    `json:"posted_id,omitempty"`
	URL         string    `json:"url,omitempty"`
	Body        string    `json:"body,omitempty"`
}

// Outbox record states (Deliver also returns "recovered" when a lost write
// was found through its mark).
const (
	StatePending   = "pending"
	StateAttempted = "attempted"
	StatePosted    = "posted"
	StateRecovered = "recovered"
)

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Outbox) delay() time.Duration {
	if o.SearchDelay > 0 {
		return o.SearchDelay
	}
	return 30 * time.Second
}

func (o *Outbox) transact(ctx context.Context, fn func(storage.Tx) error) error {
	return o.Store.Transact(ctx, storage.Guard{Now: o.now()}, fn)
}

func (o *Outbox) get(tx storage.Tx, key string, v any) (bool, error) {
	data, err := tx.Get(o.Namespace, key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

func (o *Outbox) put(tx storage.Tx, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(o.Namespace, key, data)
}

func outboxKey(id string) string { return "intent/" + digest(id)[:40] }

// Deliver writes an item at most once. post performs the write carrying the
// mark; find searches for a write carrying the mark (used after an
// ambiguous attempt). It returns the record state.
func (o *Outbox) Deliver(ctx context.Context, it OutboxItem, post func(ctx context.Context, mark string) (Posted, error), find func(ctx context.Context, mark string) (Posted, bool, error)) (string, error) {
	if it.ID == "" || it.Target == "" || strings.TrimSpace(it.Body) == "" {
		return "", storage.ErrInvalid
	}
	key, bodyDigest := outboxKey(it.ID), digest(it.Body)
	var rec outboxRecord
	err := o.transact(ctx, func(tx storage.Tx) error {
		found, err := o.get(tx, key, &rec)
		if err != nil {
			return err
		}
		if !found {
			rec = outboxRecord{ID: it.ID, Work: it.Work, Target: it.Target, BodyDigest: bodyDigest, State: StatePending, CreatedAt: o.now().UTC()}
			if it.Durable {
				rec.Body = it.Body
			}
			return o.put(tx, key, rec)
		}
		if rec.BodyDigest != bodyDigest || rec.Target != it.Target {
			return ErrIntentConflict
		}
		return nil
	})
	if err != nil || rec.State == StatePosted {
		return rec.State, err
	}
	mark := MarkerID(it.ID)
	if rec.State == StateAttempted {
		if o.now().Sub(rec.AttemptedAt) < o.delay() {
			return rec.State, nil
		}
		p, ok, err := find(ctx, mark)
		if err != nil {
			return rec.State, err
		}
		if ok {
			return o.complete(ctx, key, p, true)
		}
	}
	if o.DryRun {
		return rec.State, nil
	}
	err = o.transact(ctx, func(tx storage.Tx) error {
		var cur outboxRecord
		if _, err := o.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.State, cur.Attempts, cur.AttemptedAt = StateAttempted, cur.Attempts+1, o.now().UTC()
		return o.put(tx, key, cur)
	})
	if err != nil {
		return rec.State, err
	}
	p, err := post(ctx, mark)
	if err != nil {
		// Ambiguous: the mark search decides after SearchDelay.
		return StateAttempted, err
	}
	if o.AfterPost != nil {
		o.AfterPost(it.ID)
	}
	return o.complete(ctx, key, p, false)
}

func (o *Outbox) complete(ctx context.Context, key string, p Posted, recovered bool) (string, error) {
	err := o.transact(ctx, func(tx storage.Tx) error {
		var cur outboxRecord
		if _, err := o.get(tx, key, &cur); err != nil {
			return err
		}
		cur.State, cur.PostedID, cur.URL = StatePosted, p.ID, p.URL
		return o.put(tx, key, cur)
	})
	if err != nil {
		return StateAttempted, err
	}
	if recovered {
		return StateRecovered, nil
	}
	return StatePosted, nil
}

// Posted returns what an item published once its record is posted.
func (o *Outbox) Posted(ctx context.Context, id string) (Posted, bool, error) {
	var rec outboxRecord
	var ok bool
	err := o.transact(ctx, func(tx storage.Tx) error {
		found, err := o.get(tx, outboxKey(id), &rec)
		ok = found && rec.State == StatePosted
		return err
	})
	return Posted{ID: rec.PostedID, URL: rec.URL}, ok, err
}

// State returns an item's record state ("" when there is none).
func (o *Outbox) State(ctx context.Context, id string) (string, error) {
	var rec outboxRecord
	err := o.transact(ctx, func(tx storage.Tx) error { _, e := o.get(tx, outboxKey(id), &rec); return e })
	return rec.State, err
}

// Unfinished lists the durable items whose write is not yet posted.
func (o *Outbox) Unfinished(ctx context.Context) ([]OutboxItem, error) {
	var out []OutboxItem
	err := o.transact(ctx, func(tx storage.Tx) error {
		out = nil
		rows, err := tx.List(o.Namespace, "intent/")
		for _, row := range rows {
			var rec outboxRecord
			if json.Unmarshal(row.Value, &rec) == nil && rec.Body != "" && rec.State != StatePosted {
				out = append(out, OutboxItem{ID: rec.ID, Work: rec.Work, Target: rec.Target, Body: rec.Body, Durable: true})
			}
		}
		return err
	})
	return out, err
}
