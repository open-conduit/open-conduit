package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// ToolbeltSweep counts the records one SweepToolbelt pass deleted.
type ToolbeltSweep struct {
	Tokens, Replays int
}

// SweepToolbelt deletes toolbelt records nothing can use any more, at most
// limit of each kind per pass:
//
//   - an execution token revoked or expired more than grace ago, with its ID
//     and per-execution index rows. A deleted token is unknown, which every
//     check refuses as it refused the revoked or expired one.
//   - a call's replay record older than grace whose execution has finished
//     (or no longer exists). Only that execution's tokens can replay it, and
//     they are revoked when it finishes. Replay records written before they
//     named their execution are kept.
//
// The token and replay namespaces then hold the live executions' records and
// those within grace, not every call ever made.
func (t *Toolbelt) SweepToolbelt(ctx context.Context, grace time.Duration, limit int) (result ToolbeltSweep, err error) {
	if grace < 0 || limit < 1 || limit > 1000 {
		return result, storage.ErrInvalid
	}
	now := t.now()
	cutoff := now.Add(-grace)
	err = t.store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		result = ToolbeltSweep{}
		rows, e := tx.List(tokenNamespace, "")
		if e != nil {
			return e
		}
		for _, row := range rows {
			if result.Tokens >= limit {
				break
			}
			var record tokenRecord
			if e = json.Unmarshal(row.Value, &record); e != nil {
				return e
			}
			revoked := record.Revoked && record.RevokedAt.Before(cutoff)
			expired := !record.Authority.ExpiresAt.IsZero() && record.Authority.ExpiresAt.Before(cutoff)
			if !revoked && !expired {
				continue
			}
			if e = tx.Delete(tokenNamespace, row.Key); e != nil {
				return e
			}
			if record.TokenID != "" {
				if e = tx.Delete(tokenIDNamespace, record.TokenID); e != nil {
					return e
				}
				if e = tx.Delete(tokenNamespace+".execution", identity(string(record.Authority.ExecutionID))+"/"+record.TokenID); e != nil {
					return e
				}
			}
			result.Tokens++
		}
		rows, e = tx.List(requestNamespace, "")
		if e != nil {
			return e
		}
		finished := map[domain.ID]bool{}
		for _, row := range rows {
			if result.Replays >= limit {
				break
			}
			var replay toolbeltReplay
			if e = json.Unmarshal(row.Value, &replay); e != nil {
				return e
			}
			if replay.Execution == "" || !replay.At.Before(cutoff) {
				continue
			}
			done, seen := finished[replay.Execution]
			if !seen {
				x, e := tx.Execution(replay.Execution)
				if e != nil && !errors.Is(e, storage.ErrNotFound) {
					return e
				}
				done = e != nil || x.State.Terminal()
				finished[replay.Execution] = done
			}
			if !done {
				continue
			}
			if e = tx.Delete(requestNamespace, row.Key); e != nil {
				return e
			}
			result.Replays++
		}
		return nil
	})
	return result, err
}
