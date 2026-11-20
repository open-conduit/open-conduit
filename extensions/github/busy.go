package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// busyNamespace keeps, per Work, when a work_active refusal last told it
// that an agent on a PR hadn't handed back, for the turn it was told in.
// A wait on that agent's hand-back in the same turn counts any hand-back
// since it was seen busy, so one that lands between the refusal and the
// wait still meets it (#648). Without a refusal a hand-back condition is
// the agent's next turn from the wait on, as before: a supervisor that has
// just asked an agent for a change mustn't be woken by its earlier report.
const busyNamespace = "github.tools.busy"

// busyRecord is a Work's observations in one turn: the item each busy
// agent's hand-back conditions name (busyItem), and when it was first seen
// busy in that turn.
type busyRecord struct {
	Execution domain.ID            `json:"execution"`
	Trigger   domain.ID            `json:"trigger"`
	Seen      map[string]time.Time `json:"seen"`
}

// busyItem names the Work whose hand-back conditions name an issue's or a
// PR's agent: issue:<scope>/<n> or pr:<scope>/<n>.
func busyItem(kind CondKind, scope domain.ID, number int) string {
	return fmt.Sprintf("%s:%s/%d", kind, scope, number)
}

// sawBusy records that b's caller was told that the agents behind items
// haven't handed back, as of seen: a time taken before their state was
// read, so a turn ending after that read is never earlier than seen. An
// earlier sighting in the same turn stands.
func (t *Tools) sawBusy(ctx context.Context, b *bound, seen time.Time, items []string) error {
	key := string(b.work.ID)
	return t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var rec busyRecord
		data, err := tx.Get(busyNamespace, key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
		case err != nil:
			return err
		default:
			_ = json.Unmarshal(data, &rec)
		}
		if rec.Execution != b.a.ExecutionID || rec.Trigger != b.a.TriggerID || rec.Seen == nil {
			rec = busyRecord{Execution: b.a.ExecutionID, Trigger: b.a.TriggerID, Seen: map[string]time.Time{}}
		}
		for _, item := range items {
			if _, ok := rec.Seen[item]; !ok {
				rec.Seen[item] = seen
			}
		}
		out, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return tx.Put(busyNamespace, key, out)
	})
}

// busySince is when b's caller was told, in its current turn, that the
// agent behind item hadn't handed back; nil when it wasn't.
func (t *Tools) busySince(ctx context.Context, b *bound, item string) (*time.Time, error) {
	if t.o.Store == nil {
		return nil, nil
	}
	var since *time.Time
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		since = nil
		data, err := tx.Get(busyNamespace, string(b.work.ID))
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var rec busyRecord
		if json.Unmarshal(data, &rec) != nil || rec.Execution != b.a.ExecutionID || rec.Trigger != b.a.TriggerID {
			return nil
		}
		if at, ok := rec.Seen[item]; ok {
			since = &at
		}
		return nil
	})
	return since, err
}
