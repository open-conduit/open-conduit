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

type TimerOptions struct {
	CausalLimits CausalLimits
	// Recurrences are the registered calendar recurrence policies by kind
	// (05-01). Core stores a recurring timer's opaque spec with it and, inside
	// the firing transaction, asks the kind's pure function what this firing
	// publishes and when the timer next fires. Calendar, time-zone and
	// catch-up rules stay in the registering extension.
	Recurrences map[string]RecurrenceFunc
}

// RecurrenceFunc is a pure, deterministic recurrence policy. Given the stored
// spec, the timer being fired and the trusted firing time, it returns the
// occurrence Event to accept (nil skips this firing: no Event is published,
// as for a missed slot outside a catch-up window) and the replacement timer
// (nil ends the recurrence). It performs no I/O and runs inside the firing
// transaction, so acceptance, the firing ledger and the rearm stay atomic.
// The occurrence keeps the timer's source and causal parent; the replacement
// keeps its ID, fires strictly later and carries a distinct event identity.
type RecurrenceFunc func(spec []byte, fired domain.Timer, now time.Time) (occurrence *domain.Event, next *domain.Timer, err error)

type Timers struct {
	store   storage.Store
	now     func() time.Time
	options TimerOptions
}
type timerMetadata struct {
	ScopeID  domain.ID
	Interval time.Duration
	// Recurrence names a registered RecurrenceFunc; Spec is its opaque input.
	Recurrence string          `json:",omitempty"`
	Spec       json.RawMessage `json:",omitempty"`
}
type timerFiring struct {
	TimerID  domain.ID
	Revision domain.Revision
	EventID  domain.ID
	ParentID domain.ID
	NextHash string
	Rejected bool
	Skipped  bool `json:",omitempty"`
}
type TimerFire struct {
	TimerID         domain.ID
	Revision        domain.Revision
	Event           domain.AcceptedEvent
	Fired, Rejected bool
	// Skipped reports a recurrence firing that published no Event by policy.
	Skipped     bool
	WakeEventID domain.ID
	Diagnostic  string
}

// maxRecurrenceSpec bounds a stored recurrence spec.
const maxRecurrenceSpec = 64 << 10

func NewTimers(store storage.Store, now func() time.Time, options TimerOptions) (*Timers, error) {
	if store == nil || now == nil {
		return nil, storage.ErrInvalid
	}
	limits, err := normalizeCausalLimits(options.CausalLimits)
	if err != nil {
		return nil, err
	}
	options.CausalLimits = limits
	recurrences := map[string]RecurrenceFunc{}
	for kind, fn := range options.Recurrences {
		if kind == "" || fn == nil {
			return nil, storage.ErrInvalid
		}
		recurrences[kind] = fn
	}
	options.Recurrences = recurrences
	return &Timers{store, now, options}, nil
}
func timerKey(id domain.ID) string { return identity(string(id)) }
func firingKey(id domain.ID, revision domain.Revision) string {
	return identity(string(id), fmt.Sprint(revision))
}
func (t *Timers) arm(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, interval time.Duration, lease *domain.LeaseRef, recurrence ...timerMetadata) error {
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
		meta := timerMetadata{ScopeID: resolved, Interval: interval}
		if len(recurrence) == 1 {
			meta.Recurrence, meta.Spec = recurrence[0].Recurrence, recurrence[0].Spec
		}
		return putJSON(tx, timerNamespace, timerKey(timer.ID), meta)
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

// ArmRecurrence stores a calendar recurrence with a trusted configured root
// timer (05-01): kind names a registered RecurrenceFunc and spec is its opaque
// bounded input. Each firing asks that function for the occurrence and the
// replacement inside the firing transaction, so a crash can never separate
// acceptance from the rearm. Only configured independent schedules use it.
func (t *Timers) ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error {
	if t.options.Recurrences[kind] == nil || len(spec) > maxRecurrenceSpec || !json.Valid(spec) || timer.Event.CausationID != "" {
		return storage.ErrInvalid
	}
	return t.arm(ctx, scope, timer, expected, 0, nil, timerMetadata{Recurrence: kind, Spec: append(json.RawMessage(nil), spec...)})
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
	return fireTimerInTx(tx, now, limits, nil, scope, id, expected, replacement)
}

// recurrenceStep validates a registered recurrence policy's answer for one
// firing. The occurrence keeps the timer's source and causal parent.
func recurrenceStep(fn RecurrenceFunc, meta timerMetadata, timer domain.Timer, now time.Time, replacement *domain.Timer) (occurrence *domain.Event, next *domain.Timer, err error) {
	if fn == nil || replacement != nil {
		return nil, nil, storage.ErrInvalid
	}
	occurrence, next, err = fn(append([]byte(nil), meta.Spec...), timer, now)
	if err != nil {
		return nil, nil, storage.ErrInvalid
	}
	if occurrence != nil {
		copy := *occurrence
		if copy.ID == "" || copy.DeliveryID == "" || copy.Source != timer.Event.Source || strings.HasPrefix(copy.Type, "core.") || copy.CausationID != "" && copy.CausationID != timer.Event.CausationID {
			return nil, nil, storage.ErrInvalid
		}
		copy.CausationID = timer.Event.CausationID
		occurrence = &copy
	}
	return occurrence, next, nil
}

func fireTimerInTx(tx storage.Tx, now time.Time, limits CausalLimits, recurrences map[string]RecurrenceFunc, scope domain.ID, id domain.ID, expected domain.Revision, replacement *domain.Timer) (result TimerFire, err error) {
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
		result.Skipped = prior.Skipped
		result.Fired = !prior.Rejected && !prior.Skipped
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
	event := timer.Event
	var next *domain.Timer
	skip := false
	if meta.Recurrence != "" {
		occurrence, planned, e := recurrenceStep(recurrences[meta.Recurrence], meta, timer, now, replacement)
		if e != nil {
			return result, e
		}
		if occurrence == nil {
			skip = true
		} else {
			event = *occurrence
		}
		if planned != nil {
			if !planned.FireAt.After(now) {
				return result, storage.ErrInvalid
			}
			replacement = planned
		}
		next, err = nextTimer(timer, timerMetadata{}, replacement)
	} else {
		next, err = nextTimer(timer, meta, replacement)
	}
	if err != nil {
		return result, err
	}
	if next != nil {
		if _, err = resolveScope(tx, next.Event, scope); err != nil {
			return result, err
		}
	}
	if skip {
		// A recurrence policy declined this firing (for example a missed slot
		// outside its catch-up window): no Event, only the rearm and ledger.
		// storage.FireTimer would accept the template, so replace or delete
		// the timer directly in this same transaction.
		if err = rearmInTx(tx, id, expected, next); err != nil {
			return result, err
		}
		if next == nil {
			if err = tx.Delete(timerNamespace, timerKey(id)); err != nil {
				return result, err
			}
		}
		result.Skipped = true
		return result, putJSON(tx, timerFiringNamespace, firingKey(id, expected), timerFiring{TimerID: id, Revision: expected, ParentID: event.CausationID, NextHash: replacementHash(next), Skipped: true})
	}
	if event.ID == timer.Event.ID {
		event.OccurredAt = timer.FireAt
	} else if event.OccurredAt.IsZero() {
		event.OccurredAt = now
	}
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
	if event.ID == timer.Event.ID {
		err = tx.FireTimer(id, expected, next)
	} else {
		// A recurrence substituted its occurrence (the latest slot within its
		// catch-up window); the template is not an occurrence of its own.
		err = rearmInTx(tx, id, expected, next)
	}
	if err != nil {
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

// rearmInTx replaces (next) or deletes a due timer without accepting its
// template Event, for recurrence firings that publish another occurrence or
// none. The caller already checked revision and due time.
func rearmInTx(tx storage.Tx, id domain.ID, expected domain.Revision, next *domain.Timer) error {
	if next != nil {
		return tx.PutTimer(*next, expected)
	}
	return tx.DeleteTimer(id, expected)
}
func (t *Timers) Fire(ctx context.Context, id domain.ID, expected domain.Revision, replacement *domain.Timer) (result TimerFire, err error) {
	now := t.now()
	err = t.store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var previous timerFiring
		ledgerErr := getJSON(tx, timerFiringNamespace, firingKey(id, expected), &previous)
		if ledgerErr == nil {
			var e error
			result, e = fireTimerInTx(tx, now, t.options.CausalLimits, t.options.Recurrences, "", id, expected, replacement)
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
		result, e = fireTimerInTx(tx, now, t.options.CausalLimits, t.options.Recurrences, "", id, expected, replacement)
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
