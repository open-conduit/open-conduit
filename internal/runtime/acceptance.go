// Package runtime coordinates durable, domain-free orchestration operations.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

const acceptanceNamespace = "core.acceptance"

func identity(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func putJSON(tx storage.Tx, ns, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Put(ns, key, b)
}
func getJSON(tx storage.Tx, ns, key string, value any) error {
	b, err := tx.Get(ns, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

// AcceptInTx is the shared trusted coordination boundary. Authentication,
// declarations and derived-event budgets are caller responsibilities. It joins
// the caller's event/KV/timer/wait transaction and does not acknowledge a commit.
func AcceptInTx(tx storage.Tx, scopeID domain.ID, event domain.Event) (domain.AcceptedEvent, bool, error) {
	fail := func(err error) (domain.AcceptedEvent, bool, error) { return domain.AcceptedEvent{}, false, err }
	resolved, err := resolveScope(tx, event, scopeID)
	if err != nil {
		return fail(err)
	}
	if resolved == "" {
		return fail(storage.ErrInvalid)
	}
	accepted, added, err := tx.AcceptEvent(event)
	if err != nil {
		return fail(err)
	}
	// Delivery duplicates preserve their original target as well as envelope.
	original, err := resolveScope(tx, accepted.Event, resolved)
	if err != nil {
		return fail(err)
	}
	key := identity(string(accepted.Event.ID))
	var old domain.ID
	err = getJSON(tx, acceptanceNamespace, key, &old)
	if err == nil && old != original {
		return fail(storage.ErrConflict)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fail(err)
	}
	if errors.Is(err, storage.ErrNotFound) {
		if err = putJSON(tx, acceptanceNamespace, key, original); err != nil {
			return fail(err)
		}
	}
	return accepted, added, nil
}
func resolveScope(tx storage.Tx, event domain.Event, requested domain.ID) (domain.ID, error) {
	var owner domain.ID
	check := func(w domain.Work) error {
		if owner != "" && owner != w.ID {
			return storage.ErrConflict
		}
		owner = w.ID
		if requested != "" && requested != w.ScopeID {
			return storage.ErrConflict
		}
		requested = w.ScopeID
		return nil
	}
	if event.WorkID != "" {
		w, err := tx.Work(event.WorkID)
		if err != nil {
			return "", err
		}
		if err = check(w); err != nil {
			return "", err
		}
	}
	if event.ExecutionID != "" {
		x, err := tx.Execution(event.ExecutionID)
		if err != nil {
			return "", err
		}
		w, err := tx.Work(x.WorkID)
		if err != nil {
			return "", err
		}
		if err = check(w); err != nil {
			return "", err
		}
	}
	if event.CorrelationKey != nil {
		w, err := tx.WorkByCorrelation(*event.CorrelationKey)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return "", err
		}
		if err == nil {
			if err = check(w); err != nil {
				return "", err
			}
		}
	}
	if requested != "" {
		if _, err := tx.Scope(requested); err != nil {
			return "", err
		}
	}
	return requested, nil
}
func (i *Ingress) Accept(ctx context.Context, scope domain.ID, event domain.Event) (accepted domain.AcceptedEvent, added bool, err error) {
	err = i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error { var e error; accepted, added, e = AcceptInTx(tx, scope, event); return e })
	if err != nil {
		return domain.AcceptedEvent{}, false, err
	}
	return
}

type PendingEvent struct {
	Event      domain.AcceptedEvent
	ScopeID    domain.ID
	Diagnostic string
}
type PendingPage struct {
	Items        []PendingEvent
	NextSequence uint64
	More         bool
}

// Pending scans every unrouted event, including events accepted by storage's
// atomic FireTimer. Its cursor counts examined rows, not only pending results.
// Start again at sequence zero after a traversal to find still-pending old rows.
func (i *Ingress) Pending(ctx context.Context, after uint64, limit int) (page PendingPage, err error) {
	if limit < 1 || limit > 200 {
		return page, storage.ErrInvalid
	}
	page.Items = []PendingEvent{}
	page.NextSequence = after
	err = i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		events, e := tx.UnroutedEvents(after, limit+1)
		if e != nil {
			return e
		}
		page.More = len(events) > limit
		if page.More {
			events = events[:limit]
		}
		for _, accepted := range events {
			page.NextSequence = accepted.Sequence
			if accepted.Routed {
				continue
			}
			var scope domain.ID
			e = getJSON(tx, acceptanceNamespace, identity(string(accepted.Event.ID)), &scope)
			if e != nil && !errors.Is(e, storage.ErrNotFound) {
				return e
			}
			storedScope := scope
			resolved, resolveErr := resolveScope(tx, accepted.Event, scope)
			pending := PendingEvent{Event: accepted, ScopeID: scope}
			if resolveErr != nil {
				if errors.Is(resolveErr, storage.ErrConflict) || errors.Is(resolveErr, storage.ErrInvalid) || errors.Is(resolveErr, storage.ErrNotFound) {
					pending.Diagnostic = "identity_conflict"
				} else {
					return resolveErr
				}
			} else {
				pending.ScopeID = resolved
				if storedScope != "" {
					pending.ScopeID = storedScope
				}
				if pending.ScopeID == "" {
					pending.Diagnostic = "unbound_scope"
				}
			}

			page.Items = append(page.Items, pending)
		}
		return nil
	})
	return
}
