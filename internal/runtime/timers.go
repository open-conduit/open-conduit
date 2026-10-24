package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

const timerNamespace = "core.timers"
const timerFiringNamespace = "core.timers.firings"

type TimerOptions struct{ CausalLimits CausalLimits }
type Timers struct {
	store   storage.Store
	now     func() time.Time
	options TimerOptions
}
type timerMetadata struct {
	ScopeID  domain.ID
	Interval time.Duration
}
type timerFiring struct {
	TimerID  domain.ID
	Revision domain.Revision
	EventID  domain.ID
	ParentID domain.ID
	NextHash string
	Rejected bool
}
type TimerFire struct {
	TimerID         domain.ID
	Revision        domain.Revision
	Event           domain.AcceptedEvent
	Fired, Rejected bool
	WakeEventID     domain.ID
	Diagnostic      string
}

func NewTimers(store storage.Store, now func() time.Time, options TimerOptions) (*Timers, error) {
	if store == nil || now == nil {
		return nil, storage.ErrInvalid
	}
	limits, err := normalizeCausalLimits(options.CausalLimits)
	if err != nil {
		return nil, err
	}
	options.CausalLimits = limits
	return &Timers{store, now, options}, nil
}
func timerKey(id domain.ID) string { return identity(string(id)) }
func firingKey(id domain.ID, revision domain.Revision) string {
	return identity(string(id), fmt.Sprint(revision))
}
func (t *Timers) arm(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, interval time.Duration, lease *domain.LeaseRef) error {
	now := t.now()
	return t.store.Transact(ctx, storage.Guard{Now: now, Lease: lease}, func(tx storage.Tx) error {
		if interval < 0 || interval > 365*24*time.Hour {
			return storage.ErrInvalid
		}
		if lease != nil {
			x, err := tx.Execution(lease.ExecutionID)
			if err != nil {
				return err
			}
			w, err := tx.Work(x.WorkID)
			if err != nil {
				return err
			}
			parent, err := executionTriggerInTx(tx, x)
			if err != nil {
				return err
			}
			if strings.HasPrefix(timer.Event.Type, "core.") || strings.HasPrefix(timer.Event.Source, "core.") || timer.Event.WorkID != "" && timer.Event.WorkID != w.ID || timer.Event.ExecutionID != "" && timer.Event.ExecutionID != x.ID || timer.Event.CausationID != "" && timer.Event.CausationID != parent {
				return storage.ErrInvalid
			}
			timer.Event.WorkID, timer.Event.ExecutionID, timer.Event.CausationID = w.ID, x.ID, parent
			scope = w.ScopeID
		}
		resolved, err := resolveScope(tx, timer.Event, scope)
		if err != nil {
			return err
		}
		if resolved == "" {
			return storage.ErrInvalid
		}
		timer.Event.OccurredAt = timer.FireAt
		timer.Event.ReceivedAt = now
		if err = tx.PutTimer(timer, expected); err != nil {
			return err
		}
		return putJSON(tx, timerNamespace, timerKey(timer.ID), timerMetadata{ScopeID: resolved, Interval: interval})
	})
}

// Arm is a trusted control-plane entrypoint. Only configured independent
// schedules/operator actions may supply a root; extension callers use ArmDerived.
func (t *Timers) Arm(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision) error {
	return t.arm(ctx, scope, timer, expected, 0, nil)
}

// ArmRecurring stores a fixed-duration recurrence with the timer. Each occurrence
// keeps the arming parent; executor-created recurrences never become new roots.
func (t *Timers) ArmRecurring(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, interval time.Duration) error {
	if interval <= 0 {
		return storage.ErrInvalid
	}
	return t.arm(ctx, scope, timer, expected, interval, nil)
}
func (t *Timers) ArmDerived(ctx context.Context, lease domain.LeaseRef, timer domain.Timer, expected domain.Revision) error {
	return t.arm(ctx, "", timer, expected, 0, &lease)
}
func (t *Timers) ArmDerivedRecurring(ctx context.Context, lease domain.LeaseRef, timer domain.Timer, expected domain.Revision, interval time.Duration) error {
	if interval <= 0 {
		return storage.ErrInvalid
	}
	return t.arm(ctx, "", timer, expected, interval, &lease)
}
func (t *Timers) Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error {
	return t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		timer, err := tx.Timer(id)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if timer.Revision != expected {
			return storage.ErrConflict
		}
		if err = tx.DeleteTimer(id, expected); err != nil {
			return err
		}
		return tx.Delete(timerNamespace, timerKey(id))
	})
}
func nextTimer(timer domain.Timer, meta timerMetadata, replacement *domain.Timer) (*domain.Timer, error) {
	var next *domain.Timer
	if replacement != nil {
		copy := *replacement
		next = &copy
	} else if meta.Interval > 0 {
		copy := timer
		copy.Revision = 0
		copy.FireAt = timer.FireAt.Add(meta.Interval)
		copy.Event.ID = domain.ID("timer-event/" + identity(string(timer.Event.ID), copy.FireAt.UTC().Format(time.RFC3339Nano)))
		copy.Event.DeliveryID = "timer/" + identity(timer.Event.Source, timer.Event.DeliveryID, copy.FireAt.UTC().Format(time.RFC3339Nano))
		next = &copy
	}
	if next == nil {
		return nil, nil
	}
	if next.ID != timer.ID || !next.FireAt.After(timer.FireAt) || next.Event.ID == timer.Event.ID || next.Event.Source == timer.Event.Source && next.Event.DeliveryID == timer.Event.DeliveryID {
		return nil, storage.ErrInvalid
	}
	// Derived recurrence may neither sever nor replace its trusted arming parent.
	if next.Event.CausationID != "" && next.Event.CausationID != timer.Event.CausationID {
		return nil, storage.ErrInvalid
	}
	next.Event.CausationID = timer.Event.CausationID
	next.Event.OccurredAt = next.FireAt
	next.Event.ReceivedAt = next.FireAt
	return next, nil
}
func replacementHash(next *domain.Timer) string {
	if next == nil {
		return ""
	}
	copy := *next
	copy.Revision = 0
	copy.CreatedAt = time.Time{}
	copy.UpdatedAt = time.Time{}
	b, _ := json.Marshal(copy)
	return identity(string(b))
}

// FireTimerInTx atomically accepts a due event through the scoped causal boundary
// and removes/rearms its timer. On causal refusal only the diagnostic, disabled
// timer and firing ledger commit; no event or recurrence is published. The outer
// transaction must commit before acknowledging any returned result.
func FireTimerInTx(tx storage.Tx, now time.Time, limits CausalLimits, scope domain.ID, id domain.ID, expected domain.Revision, replacement *domain.Timer) (result TimerFire, err error) {
	result.TimerID, result.Revision = id, expected
	var prior timerFiring
	err = getJSON(tx, timerFiringNamespace, firingKey(id, expected), &prior)
	if err == nil {
		// Replays preserve the original firing. An explicit different rearm cannot
		// turn a committed one-shot into another operation.
		if replacement != nil {
			if replacement.Event.CausationID != "" && replacement.Event.CausationID != prior.ParentID {
				return result, storage.ErrConflict
			}
			copy := *replacement
			copy.Event.CausationID = prior.ParentID
			copy.Event.OccurredAt = copy.FireAt
			copy.Event.ReceivedAt = copy.FireAt
			if replacementHash(&copy) != prior.NextHash {
				return result, storage.ErrConflict
			}
		}
		result.Rejected = prior.Rejected
		result.Fired = !prior.Rejected
		if prior.EventID != "" {
			result.Event, err = tx.Event(prior.EventID)
		}
		return result, err
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return result, err
	}
	timer, err := tx.Timer(id)
	if err != nil {
		return result, err
	}
	if timer.Revision != expected {
		return result, storage.ErrConflict
	}
	if now.Before(timer.FireAt) {
		return result, storage.ErrInvalid
	}
	var meta timerMetadata
	err = getJSON(tx, timerNamespace, timerKey(id), &meta)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return result, err
	}
	if meta.ScopeID != "" {
		if scope != "" && scope != meta.ScopeID {
			return result, storage.ErrConflict
		}
		scope = meta.ScopeID
	}
	scope, err = resolveScope(tx, timer.Event, scope)
	if err != nil {
		return result, err
	}
	if scope == "" {
		return result, storage.ErrInvalid
	}
	next, err := nextTimer(timer, meta, replacement)
	if err != nil {
		return result, err
	}
	if next != nil {
		if _, err = resolveScope(tx, next.Event, scope); err != nil {
			return result, err
		}
	}
	event := timer.Event
	event.OccurredAt = timer.FireAt
	event.ReceivedAt = now
	if event.CausationID != "" {
		result.Event, _, err = AcceptDerivedInTx(tx, now, limits, scope, event.CausationID, event)
	} else {
		result.Event, _, err = AcceptInTx(tx, scope, event)
	}
	record := timerFiring{TimerID: id, Revision: expected, ParentID: event.CausationID, NextHash: replacementHash(next)}
	if errors.Is(err, ErrCausalRejected) {
		if err = rejectExecutionTimeoutInTx(tx, now, event); err != nil {
			return result, err
		}
		if err = tx.DeleteTimer(id, expected); err != nil {
			return result, err
		}
		if err = tx.Delete(timerNamespace, timerKey(id)); err != nil {
			return result, err
		}
		record.Rejected = true
		result.Rejected = true
		err = putJSON(tx, timerFiringNamespace, firingKey(id, expected), record)
		return result, err
	}
	if err != nil {
		return result, err
	}
	if err = tx.FireTimer(id, expected, next); err != nil {
		return result, err
	}
	if next == nil {
		if err = tx.Delete(timerNamespace, timerKey(id)); err != nil {
			return result, err
		}
	}
	record.EventID = result.Event.Event.ID
	result.Fired = true
	err = putJSON(tx, timerFiringNamespace, firingKey(id, expected), record)
	return result, err
}
func (t *Timers) Fire(ctx context.Context, id domain.ID, expected domain.Revision, replacement *domain.Timer) (result TimerFire, err error) {
	now := t.now()
	err = t.store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var previous timerFiring
		ledgerErr := getJSON(tx, timerFiringNamespace, firingKey(id, expected), &previous)
		if ledgerErr == nil {
			var e error
			result, e = FireTimerInTx(tx, now, t.options.CausalLimits, "", id, expected, replacement)
			return e
		}
		if !errors.Is(ledgerErr, storage.ErrNotFound) {
			return ledgerErr
		}
		current, currentErr := tx.Timer(id)
		if currentErr != nil && !errors.Is(currentErr, storage.ErrNotFound) {
			return currentErr
		}
		if currentErr == nil && current.Revision != expected {
			return storage.ErrConflict
		}
		waits, e := tx.ListWaits("")
		if e != nil {
			return e
		}
		for _, wait := range waits {
			if wait.TimeoutTimerID != id {
				continue
			}
			if replacement != nil {
				return storage.ErrInvalid
			}
			if wait.State == domain.WaitActive {
				winner, e := WakeWaitInTx(tx, now, t.options.CausalLimits, wait.ID)
				result = TimerFire{TimerID: id, Revision: expected, WakeEventID: winner}
				if e != nil {
					return e
				}
				var fired timerFiring
				e = getJSON(tx, timerFiringNamespace, firingKey(id, expected), &fired)
				if e == nil {
					result.Rejected = fired.Rejected
					result.Fired = !fired.Rejected
					if fired.EventID != "" {
						result.Event, e = tx.Event(fired.EventID)
					}
					return e
				}
				if errors.Is(e, storage.ErrNotFound) {
					return nil
				}
				return e
			}
		}
		result, e = FireTimerInTx(tx, now, t.options.CausalLimits, "", id, expected, replacement)
		return e
	})
	if err != nil {
		return TimerFire{}, err
	}
	return
}

// Due processes at most limit distinct due timers per poll. A durable lexical
// round-robin cursor prevents an overdue recurrence or invalid timer from
// starving independent timers when the clock stays fixed and limit is one.
func (t *Timers) Due(ctx context.Context, limit int) (results []TimerFire, err error) {
	if limit < 1 || limit > 200 {
		return nil, storage.ErrInvalid
	}
	var due []domain.Timer
	err = t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		all, e := tx.ListTimers()
		if e != nil {
			return e
		}
		var cursor string
		e = getJSON(tx, timerNamespace, "due_cursor", &cursor)
		if e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		for _, timer := range all {
			if !t.now().Before(timer.FireAt) {
				due = append(due, timer)
			}
		}
		sort.Slice(due, func(i, j int) bool {
			a, b := string(due[i].ID), string(due[j].ID)
			aa, bb := a > cursor, b > cursor
			if aa != bb {
				return aa
			}
			return a < b
		})
		if len(due) > limit {
			due = due[:limit]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, timer := range due {
		result, e := t.Fire(ctx, timer.ID, timer.Revision, nil)
		if cursorErr := t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error { return putJSON(tx, timerNamespace, "due_cursor", string(timer.ID)) }); cursorErr != nil {
			return results, cursorErr
		}
		if e != nil {
			if errors.Is(e, storage.ErrNotFound) || errors.Is(e, storage.ErrConflict) {
				continue
			}
			if errors.Is(e, storage.ErrInvalid) {
				result = TimerFire{TimerID: timer.ID, Revision: timer.Revision, Diagnostic: "invalid_timer"}
			} else {
				return results, e
			}
		}
		results = append(results, result)
	}
	return results, nil
}
