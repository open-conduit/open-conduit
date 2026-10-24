package reference

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

// Runtime is an executable specification over public storage. It has no daemon,
// process controls, throughput guarantees or crash durability. KV holds the
// reference scheduler's queue/eligibility, never executor-private metadata.
type Runtime struct {
	store   storage.Store
	clock   *conformance.Clock
	options conformance.RuntimeOptions
}

func NewRuntime(o conformance.RuntimeOptions) conformance.RuntimeFixture {
	s, c := New(), conformance.NewClock()
	return NewRuntimeWithStore(o, s, c)
}

// NewRuntimeWithStore runs the executable specification over supplied storage.
// Backend integration tests use it to prove persistence/reopen compatibility;
// it is still a reference coordinator, never a production runtime adapter.
func NewRuntimeWithStore(o conformance.RuntimeOptions, s storage.Store, c *conformance.Clock) conformance.RuntimeFixture {
	r := &Runtime{store: s, clock: c, options: o}
	if r.options.MaxAttempts == 0 {
		r.options.MaxAttempts = 3
	}
	if r.options.MaxDepth == 0 {
		r.options.MaxDepth = 32
	}
	if r.options.PerMinute == 0 {
		r.options.PerMinute = 128
	}
	if err := r.tx(nil, func(tx storage.Tx) error { return tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0) }); err != nil {
		panic(err)
	}
	return conformance.RuntimeFixture{Driver: r, Store: s, Clock: c, Restart: func() conformance.RuntimeDriver { return &Runtime{store: s, clock: c, options: r.options} }}
}
func (r *Runtime) tx(lease *domain.LeaseRef, fn func(storage.Tx) error) error {
	return r.store.Transact(context.Background(), storage.Guard{Now: r.clock.Now(), Lease: lease}, fn)
}
func (r *Runtime) Accept(e domain.Event) error {
	return r.tx(nil, func(tx storage.Tx) error { _, _, err := tx.AcceptEvent(e); return err })
}
func owner(tx storage.Tx, e domain.Event) (domain.Work, error) {
	if e.WorkID != "" {
		return tx.Work(e.WorkID)
	}
	if e.CorrelationKey != nil {
		return tx.WorkByCorrelation(*e.CorrelationKey)
	}
	return domain.Work{}, storage.ErrNotFound
}
func (r *Runtime) Route(id domain.ID) error {
	var input extension.RouteInput
	if err := r.tx(nil, func(tx storage.Tx) error {
		a, err := tx.Event(id)
		if err != nil {
			return err
		}
		input.Event = a
		input.Scope, err = tx.Scope("scope")
		if err != nil {
			return err
		}
		w, err := owner(tx, a.Event)
		if err == nil {
			input.Work = &w
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if input.Event.Routed {
		return nil
	}
	var decision extension.RouteDecision
	var selected *conformance.RuntimeRoute
	// Deliberately outside Tx: blocked evaluation of one key cannot hold storage
	// or a global routing lock. Runtime adapters must retain this property.
	for i := range r.options.Routes {
		candidate := &r.options.Routes[i]
		d, err := candidate.Handler.Match(context.Background(), input)
		if err != nil {
			return err
		}
		if d.Match {
			decision = d
			selected = candidate
			break
		}
	}
	return r.tx(nil, func(tx storage.Tx) error {
		a, err := tx.Event(id)
		if err != nil {
			return err
		}
		if a.Routed {
			return nil
		}
		if selected == nil {
			return tx.MarkRouted(id, "", "unmatched")
		}
		w, err := owner(tx, a.Event)
		if errors.Is(err, storage.ErrNotFound) {
			if a.Event.WorkID != "" {
				return err
			}
			if a.Event.CorrelationKey == nil && decision.CreateID == "" {
				return tx.MarkRouted(id, "", "unmatched")
			}
			w = domain.Work{ID: decision.CreateID, Kind: decision.Kind, ScopeID: "scope", State: domain.WorkReady}
			if a.Event.CorrelationKey != nil {
				w.CorrelationKeys = append(w.CorrelationKeys, *a.Event.CorrelationKey)
			}
		} else if err != nil {
			return err
		}
		if a.Event.WorkID != "" && a.Event.CorrelationKey != nil {
			correlated, err := tx.WorkByCorrelation(*a.Event.CorrelationKey)
			if err != nil || correlated.ID != w.ID {
				return storage.ErrInvalid
			}
		}
		if w.State.Terminal() {
			return tx.MarkRouted(id, w.ID, "terminal")
		}
		for _, key := range decision.AttachKeys {
			found := false
			for _, old := range w.CorrelationKeys {
				if old == key {
					found = true
				}
			}
			if !found {
				w.CorrelationKeys = append(w.CorrelationKeys, key)
			}
		}
		if err := tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		policy := selected.Route.ActivePolicy
		if policy == "" {
			policy = "queue"
		}
		if policy != "queue" && policy != "inject" && policy != "interrupt" && policy != "drop" {
			return extension.ErrInvalid
		}
		supported := policy != "inject" || r.options.Capabilities.Inject
		supported = supported && (policy != "interrupt" || r.options.Capabilities.Interrupt)
		if !supported {
			policy = r.options.Fallback
			if policy != "queue" && policy != "drop" {
				return extension.ErrUnsupported
			}
		}
		// Active policies apply only to a running attempt, not to parked waits.
		if w.State != domain.WorkRunning {
			policy = "queue"
		}
		if policy != "drop" {
			if err := tx.Put("runtime.queue", string(w.ID)+"/"+string(id), []byte(id)); err != nil {
				return err
			}
		}
		// inject/interrupt are durable delivery intents here. This double does not
		// pretend that committing intent has called or interrupted a real process.
		return tx.MarkRouted(id, w.ID, policy)
	})
}
func queue(tx storage.Tx, id domain.ID) ([]storage.Entry, error) {
	return tx.List("runtime.queue", string(id)+"/")
}
func (r *Runtime) Start(id domain.ID) (domain.LeaseRef, error) {
	var ref domain.LeaseRef
	err := r.tx(nil, func(tx storage.Tx) error {
		w, err := tx.Work(id)
		if err != nil {
			return err
		}
		if w.State != domain.WorkReady {
			return storage.ErrInvalid
		}
		if _, err := tx.Get("runtime.blocked", string(id)); err == nil {
			return storage.ErrInvalid
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		entries, err := queue(tx, id)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return storage.ErrInvalid
		}
		events, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		var trigger domain.ID
		for _, e := range events {
			for _, entry := range entries {
				if string(e.Event.ID) == string(entry.Value) {
					trigger = e.Event.ID
					break
				}
			}
			if trigger != "" {
				break
			}
		}
		attempts, err := tx.ListExecutions(id)
		if err != nil {
			return err
		}
		var latest uint32
		var parked domain.ID
		for _, x := range attempts {
			if x.Attempt > latest {
				latest = x.Attempt
			}
			if x.State == domain.ExecutionParked {
				parked = x.ID
			}
		}
		executionID := parked
		if executionID == "" {
			executionID = domain.ID(fmt.Sprintf("%s/attempt/%d", id, latest+1))
			if err := tx.PutExecution(domain.Execution{ID: executionID, WorkID: id, Attempt: latest + 1, State: domain.ExecutionPending, ExecutorBinding: "fixture", TriggerID: trigger, TraceID: string(id)}, 0); err != nil {
				return err
			}
		}
		ref, err = tx.Claim(executionID, "fixture", conformance.RuntimeLease)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := tx.Delete("runtime.queue", entry.Key); err != nil {
				return err
			}
		}
		return nil
	})
	return ref, err
}
func matches(w domain.Wait, a domain.AcceptedEvent) bool {
	if a.Sequence <= w.AfterSequence || a.Event.CorrelationKey == nil {
		return false
	}
	found := false
	for _, key := range w.Match.Keys {
		if key == *a.Event.CorrelationKey {
			found = true
		}
	}
	if !found {
		return false
	}
	if len(w.Match.Types) == 0 {
		return true
	}
	for _, typ := range w.Match.Types {
		if typ == a.Event.Type {
			return true
		}
	}
	return false
}
func (r *Runtime) Wait(ref domain.LeaseRef, w domain.Wait, resume bool) error {
	return r.tx(&ref, func(tx storage.Tx) error {
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		work, err := tx.Work(x.WorkID)
		if err != nil {
			return err
		}
		w.WorkID, w.ExecutionID, w.State = work.ID, x.ID, domain.WaitActive
		x.State = domain.ExecutionWaiting
		if resume {
			x.State = domain.ExecutionParked
		}
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeWaiting}
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		work.State = domain.WorkWaiting
		if err := tx.PutWork(work, work.Revision); err != nil {
			return err
		}
		if resume && !r.options.Capabilities.Resume {
			return extension.ErrUnsupported
		}
		if w.TimeoutTimerID != "" {
			timer, err := tx.Timer(w.TimeoutTimerID)
			if err != nil {
				return err
			}
			var encoded bytes.Buffer
			if err := gob.NewEncoder(&encoded).Encode(timer); err != nil {
				return err
			}
			if err := tx.Put("runtime.timeout", string(w.ID), encoded.Bytes()); err != nil {
				return err
			}
		}
		if err := tx.PutWait(w, 0); err != nil {
			return err
		}
		return r.wake(tx, w.ID)
	})
}
func (r *Runtime) Wake(id domain.ID) error {
	return r.tx(nil, func(tx storage.Tx) error { return r.wake(tx, id) })
}
func (r *Runtime) wake(tx storage.Tx, id domain.ID) error {
	w, err := tx.Wait(id)
	if err != nil {
		return err
	}
	if w.State != domain.WaitActive {
		return nil
	}
	events, err := tx.Events(0, 0)
	if err != nil {
		return err
	}
	var timer domain.Timer
	if w.TimeoutTimerID != "" {
		data, err := tx.Get("runtime.timeout", string(w.ID))
		if err != nil {
			return err
		}
		if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&timer); err != nil {
			return err
		}
	}
	var winner domain.ID
	// Lowest acceptance sequence wins, including already accepted timeouts.
	for _, a := range events {
		if matches(w, a) || timer.ID != "" && a.Event.ID == timer.Event.ID {
			winner = a.Event.ID
			break
		}
	}
	if winner == "" && timer.ID != "" && !r.clock.Now().Before(timer.FireAt) {
		if err := r.fire(tx, timer); err != nil {
			return err
		}
		winner = timer.Event.ID
		// The accepted firing remains identifiable after one-shot timer deletion.
	}
	if winner == "" {
		return nil
	}
	// See B04: a fired timer may already be absent. Preserve its immutable ID on
	// historical waits; storage validates presence at registration, not wake.
	a, err := tx.Event(winner)
	if err != nil {
		return err
	}
	if !a.Routed {
		if err := tx.MarkRouted(winner, w.WorkID, "wake"); err != nil {
			return err
		}
	}
	w.State, w.WakeEventID = domain.WaitWoken, winner
	if err := tx.PutWait(w, w.Revision); err != nil {
		return err
	}
	if timer.ID != "" {
		if current, err := tx.Timer(timer.ID); err == nil {
			if err := tx.DeleteTimer(current.ID, current.Revision); err != nil {
				return err
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	work, err := tx.Work(w.WorkID)
	if err != nil {
		return err
	}
	work.State = domain.WorkReady
	if err := tx.PutWork(work, work.Revision); err != nil {
		return err
	}
	return tx.Put("runtime.queue", string(work.ID)+"/"+string(winner), []byte(winner))
}
func (r *Runtime) Complete(ref domain.LeaseRef) error {
	return r.tx(&ref, func(tx storage.Tx) error {
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		all, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		for _, a := range all {
			target, err := owner(tx, a.Event)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			if !a.Routed && target.ID == x.WorkID {
				return storage.ErrConflict
			}
		}
		x.State = domain.ExecutionCompleted
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeCompleted}
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		w, err := tx.Work(x.WorkID)
		if err != nil {
			return err
		}
		pending, err := queue(tx, w.ID)
		if err != nil {
			return err
		}
		w.State = domain.WorkCompleted
		if len(pending) > 0 {
			w.State = domain.WorkReady
		}
		if err := tx.Delete("runtime.failures", string(w.ID)); err != nil {
			return err
		}
		return tx.PutWork(w, w.Revision)
	})
}
func (r *Runtime) fail(tx storage.Tx, x domain.Execution) error {
	x.State = domain.ExecutionFailed
	x.Outcome = &domain.Outcome{Kind: domain.OutcomeFailed}
	if err := tx.PutExecution(x, x.Revision); err != nil {
		return err
	}
	w, err := tx.Work(x.WorkID)
	if err != nil {
		return err
	}
	w.State = domain.WorkReady
	if err := tx.PutWork(w, w.Revision); err != nil {
		return err
	}
	// Failure-budget state is independent of monotonic attempt numbers.
	failures := 0
	if data, err := tx.Get("runtime.failures", string(w.ID)); err == nil {
		failures, err = strconv.Atoi(string(data))
		if err != nil {
			return err
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	failures++
	if err := tx.Put("runtime.failures", string(w.ID), []byte(strconv.Itoa(failures))); err != nil {
		return err
	}
	if err := tx.Put("runtime.blocked", string(w.ID), []byte("exhausted")); err != nil {
		return err
	}
	if failures >= r.options.MaxAttempts {
		return nil
	}
	e := domain.Event{ID: domain.ID(fmt.Sprintf("%s/retry/%d", w.ID, x.Attempt)), Type: "core.retry.due", Source: "core.retry", DeliveryID: fmt.Sprintf("%s/%d", w.ID, x.Attempt), CausationID: x.TriggerID, WorkID: w.ID, OccurredAt: r.clock.Now(), ReceivedAt: r.clock.Now()}
	return tx.PutTimer(domain.Timer{ID: domain.ID("retry/" + string(w.ID)), FireAt: r.clock.Now().Add(time.Duration(failures) * 5 * time.Minute), Event: e}, 0)
}
func (r *Runtime) Fail(ref domain.LeaseRef) error {
	return r.tx(&ref, func(tx storage.Tx) error {
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		return r.fail(tx, x)
	})
}
func (r *Runtime) Retry(id, eventID domain.ID, manual bool) error {
	return r.tx(nil, func(tx storage.Tx) error {
		w, err := tx.Work(id)
		if err != nil {
			return err
		}
		if w.State != domain.WorkReady {
			return storage.ErrInvalid
		}
		if manual {
			a, err := tx.Event(eventID)
			if err != nil {
				return err
			}
			if a.Event.WorkID != id || a.Event.Type != "core.retry.requested" {
				return storage.ErrInvalid
			}
			if a.Routed {
				return nil
			}
			if err := tx.Delete("runtime.failures", string(id)); err != nil {
				return err
			}
			if err := tx.MarkRouted(eventID, id, "retry"); err != nil {
				return err
			}
			if t, err := tx.Timer(domain.ID("retry/" + string(id))); err == nil {
				if err := tx.DeleteTimer(t.ID, t.Revision); err != nil {
					return err
				}
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		} else {
			timer, err := tx.Timer(domain.ID("retry/" + string(id)))
			if err != nil {
				return err
			}
			if err := r.fire(tx, timer); err != nil {
				return err
			}
			eventID = timer.Event.ID
			if err := tx.MarkRouted(eventID, id, "retry"); err != nil {
				return err
			}
		}
		if err := tx.Delete("runtime.blocked", string(id)); err != nil {
			return err
		}
		return tx.Put("runtime.queue", string(id)+"/"+string(eventID), []byte(eventID))
	})
}
func (r *Runtime) Cancel(id domain.ID) error {
	return r.tx(nil, func(tx storage.Tx) error {
		w, err := tx.Work(id)
		if err != nil {
			return err
		}
		if w.State.Terminal() {
			return nil
		}
		waits, err := tx.ListWaits(id)
		if err != nil {
			return err
		}
		for _, wait := range waits {
			if wait.State == domain.WaitActive {
				wait.State = domain.WaitCancelled
				if err := tx.PutWait(wait, wait.Revision); err != nil {
					return err
				}
			}
		}
		attempts, err := tx.ListExecutions(id)
		if err != nil {
			return err
		}
		for _, x := range attempts {
			if !x.State.Terminal() {
				x.State = domain.ExecutionCancelled
				x.Outcome = nil
				if err := tx.PutExecution(x, x.Revision); err != nil {
					return err
				}
			}
		}
		timers, err := tx.ListTimers()
		if err != nil {
			return err
		}
		for _, t := range timers {
			if t.Event.WorkID == id {
				if err := tx.DeleteTimer(t.ID, t.Revision); err != nil {
					return err
				}
			}
		}
		entries, err := queue(tx, id)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := tx.Delete("runtime.queue", e.Key); err != nil {
				return err
			}
		}
		w.State = domain.WorkCancelled
		return tx.PutWork(w, w.Revision)
	})
}
func (r *Runtime) Recover(id domain.ID, adoptable bool) (domain.LeaseRef, error) {
	var ref domain.LeaseRef
	err := r.tx(nil, func(tx storage.Tx) error {
		var err error
		ref, err = tx.Claim(id, "recovery", conformance.RuntimeLease)
		return err
	})
	if err != nil {
		return ref, err
	}
	// Represents Recover outside the claim transaction; never calls Run. A real
	// adapter replaces credentials before accepting the old process's writes.
	if !adoptable {
		return ref, r.Fail(ref)
	}
	return ref, nil
}
func (r *Runtime) Derive(e domain.Event) error {
	err := r.tx(nil, func(tx storage.Tx) error { return r.derive(tx, e) })
	if err != nil {
		auditErr := r.tx(nil, func(tx storage.Tx) error {
			data, _ := json.Marshal(struct {
				Event, Parent domain.ID
				Reason        string
			}{e.ID, e.CausationID, "derived event rejected"})
			return tx.Put("runtime.rejections", string(e.ID), data)
		})
		return errors.Join(err, auditErr)
	}
	return nil
}
func (r *Runtime) derive(tx storage.Tx, e domain.Event) error {
	// Dedup before quota checks: a replay never spends another chain token.
	all, err := tx.Events(0, 0)
	if err != nil {
		return err
	}
	for _, old := range all {
		if old.Event.Source == e.Source && old.Event.DeliveryID == e.DeliveryID {
			return nil
		}
	}
	if e.CausationID == "" {
		return extension.ErrInvalid
	}
	root := e.CausationID
	depth := 1
	for {
		a, err := tx.Event(root)
		if err != nil {
			return err
		}
		if a.Event.CausationID == "" {
			break
		}
		root = a.Event.CausationID
		depth++
	}
	if depth > r.options.MaxDepth {
		return extension.ErrInvalid
	}
	bucket := fmt.Sprintf("%s/%d", root, r.clock.Now().Unix()/60)
	count := 0
	if data, err := tx.Get("runtime.chains", bucket); err == nil {
		count, err = strconv.Atoi(string(data))
		if err != nil {
			return err
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if count >= r.options.PerMinute {
		return extension.ErrInvalid
	}
	if _, _, err := tx.AcceptEvent(e); err != nil {
		return err
	}
	return tx.Put("runtime.chains", bucket, []byte(strconv.Itoa(count+1)))
}

func (r *Runtime) fire(tx storage.Tx, timer domain.Timer) error {
	if r.clock.Now().Before(timer.FireAt) {
		return storage.ErrInvalid
	}
	e := timer.Event
	e.OccurredAt = timer.FireAt
	e.ReceivedAt = r.clock.Now()
	if err := r.derive(tx, e); err != nil {
		return err
	}
	return tx.FireTimer(timer.ID, timer.Revision, nil)
}
