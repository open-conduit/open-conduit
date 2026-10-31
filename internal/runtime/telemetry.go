package runtime

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

// TelemetryOptions configures lifecycle instrumentation. Observer must not
// block: production composition passes the buffered telemetry pipeline, whose
// Observe only validates, redacts and enqueues.
type TelemetryOptions struct {
	Observer observability.Observer
	// Diagnostic receives safe codes for records that could not be correlated
	// or that the observer refused. It must not block.
	Diagnostic func(code string)
}

// Instrument wraps coordination storage so every committed lifecycle change is
// reported once, after its commit, with trusted correlation. Records are
// derived from the durable writes themselves (before/after state of Work,
// Executions, Timers, Waits, attempt/retry/ingress ledgers and accepted or
// routed Events), so no runtime path can change state without telemetry. A
// rolled-back transaction emits nothing. Telemetry failure never fails the
// transaction or the caller.
func Instrument(store storage.Store, o TelemetryOptions) (storage.Store, error) {
	if store == nil || o.Observer == nil {
		return nil, storage.ErrInvalid
	}
	return &instrumentedStore{inner: store, o: o}, nil
}

type instrumentedStore struct {
	inner storage.Store
	o     TelemetryOptions
}

func (s *instrumentedStore) diagnostic(code string) {
	if s.o.Diagnostic != nil {
		s.o.Diagnostic(code)
	}
}

func (s *instrumentedStore) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	var records []observability.Record
	err := s.inner.Transact(ctx, g, func(tx storage.Tx) error {
		r := &recorder{Tx: tx, at: g.Now}
		if err := fn(r); err != nil {
			return err
		}
		records = r.resolveSafely(s.diagnostic)
		return nil
	})
	if err != nil {
		return err
	}
	observeAll(ctx, s.o.Observer, records, s.diagnostic)
	return nil
}

func observeAll(ctx context.Context, o observability.Observer, records []observability.Record, diagnostic func(string)) {
	if o == nil || len(records) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, record := range records {
		if err := o.Observe(ctx, record); err != nil && diagnostic != nil {
			diagnostic("observer_rejected")
		}
	}
}

type acceptedFact struct {
	id    domain.ID
	added bool
}
type leaseFact struct {
	name string
	ref  domain.LeaseRef
}

// recorder captures the pre-state of every touched record during a
// transaction. resolve runs inside the same transaction after the callback
// succeeded and compares it with the post-state, so intermediate states that
// were never committed are never reported.
type recorder struct {
	storage.Tx
	at time.Time

	works      map[domain.ID]*domain.Work
	workOrder  []domain.ID
	execs      map[domain.ID]*domain.Execution
	execOrder  []domain.ID
	timers     map[domain.ID]*domain.Timer
	timerOrder []domain.ID
	fired      map[domain.ID]bool
	waits      map[domain.ID]*domain.Wait
	waitOrder  []domain.ID
	accepted   []acceptedFact
	routed     []domain.ID
	leases     []leaseFact

	attempts     map[string]*AttemptState
	attemptOrder []string
	retries      map[string]*RetryState
	retryOrder   []string
	ingress      map[string]*ingressState
	ingressOrder []string
	rejections   []string
	expirations  []string

	traces map[domain.ID]string
}

func (r *recorder) touchWork(id domain.ID) {
	if r.works == nil {
		r.works = map[domain.ID]*domain.Work{}
	}
	if _, ok := r.works[id]; ok || id == "" {
		return
	}
	var before *domain.Work
	if w, err := r.Tx.Work(id); err == nil {
		before = &w
	}
	r.works[id] = before
	r.workOrder = append(r.workOrder, id)
}
func (r *recorder) touchExecution(id domain.ID) *domain.Execution {
	if r.execs == nil {
		r.execs = map[domain.ID]*domain.Execution{}
	}
	if before, ok := r.execs[id]; ok || id == "" {
		return before
	}
	var before *domain.Execution
	if x, err := r.Tx.Execution(id); err == nil {
		before = &x
	}
	r.execs[id] = before
	r.execOrder = append(r.execOrder, id)
	return before
}
func (r *recorder) touchTimer(id domain.ID) {
	if r.timers == nil {
		r.timers = map[domain.ID]*domain.Timer{}
	}
	if _, ok := r.timers[id]; ok || id == "" {
		return
	}
	var before *domain.Timer
	if t, err := r.Tx.Timer(id); err == nil {
		before = &t
	}
	r.timers[id] = before
	r.timerOrder = append(r.timerOrder, id)
}
func (r *recorder) touchWait(id domain.ID) {
	if r.waits == nil {
		r.waits = map[domain.ID]*domain.Wait{}
	}
	if _, ok := r.waits[id]; ok || id == "" {
		return
	}
	var before *domain.Wait
	if w, err := r.Tx.Wait(id); err == nil {
		before = &w
	}
	r.waits[id] = before
	r.waitOrder = append(r.waitOrder, id)
}

func (r *recorder) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	a, added, err := r.Tx.AcceptEvent(e)
	if err == nil {
		r.accepted = append(r.accepted, acceptedFact{a.Event.ID, added})
	}
	return a, added, err
}
func (r *recorder) MarkRouted(eventID, workID domain.ID, disposition string) error {
	before, err := r.Tx.Event(eventID)
	err2 := r.Tx.MarkRouted(eventID, workID, disposition)
	if err2 == nil && (err != nil || !before.Routed) {
		r.routed = append(r.routed, eventID)
	}
	return err2
}
func (r *recorder) PutWork(w domain.Work, expected domain.Revision) error {
	r.touchWork(w.ID)
	return r.Tx.PutWork(w, expected)
}
func (r *recorder) PutExecution(x domain.Execution, expected domain.Revision) error {
	r.touchExecution(x.ID)
	r.touchWork(x.WorkID)
	return r.Tx.PutExecution(x, expected)
}
func (r *recorder) Claim(id domain.ID, owner string, ttl time.Duration) (domain.LeaseRef, error) {
	if before := r.touchExecution(id); before != nil {
		r.touchWork(before.WorkID)
	}
	return r.Tx.Claim(id, owner, ttl)
}
func (r *recorder) Renew(ref domain.LeaseRef, ttl time.Duration) error {
	r.touchExecution(ref.ExecutionID)
	err := r.Tx.Renew(ref, ttl)
	if err == nil {
		r.leases = append(r.leases, leaseFact{"lease.renewed", ref})
	}
	return err
}
func (r *recorder) Release(ref domain.LeaseRef) error {
	r.touchExecution(ref.ExecutionID)
	err := r.Tx.Release(ref)
	if err == nil {
		r.leases = append(r.leases, leaseFact{"lease.released", ref})
	}
	return err
}
func (r *recorder) PutTimer(t domain.Timer, expected domain.Revision) error {
	r.touchTimer(t.ID)
	return r.Tx.PutTimer(t, expected)
}
func (r *recorder) DeleteTimer(id domain.ID, expected domain.Revision) error {
	r.touchTimer(id)
	return r.Tx.DeleteTimer(id, expected)
}
func (r *recorder) FireTimer(id domain.ID, expected domain.Revision, next *domain.Timer) error {
	r.touchTimer(id)
	before := r.timers[id]
	err := r.Tx.FireTimer(id, expected, next)
	if err == nil {
		if r.fired == nil {
			r.fired = map[domain.ID]bool{}
		}
		r.fired[id] = true
		// The firing path accepts the template through AcceptEvent first
		// (05-03 fix: it was reported twice, as two event.accepted records).
		if before != nil && !slices.ContainsFunc(r.accepted, func(f acceptedFact) bool { return f.id == before.Event.ID }) {
			r.accepted = append(r.accepted, acceptedFact{before.Event.ID, true})
		}
	}
	return err
}
func (r *recorder) PutWait(w domain.Wait, expected domain.Revision) error {
	r.touchWait(w.ID)
	return r.Tx.PutWait(w, expected)
}
func (r *recorder) Put(ns, key string, value []byte) error {
	switch {
	case ns == attemptNamespace:
		if r.attempts == nil {
			r.attempts = map[string]*AttemptState{}
		}
		if _, ok := r.attempts[key]; !ok {
			var before *AttemptState
			var state AttemptState
			if err := getJSON(r.Tx, ns, key, &state); err == nil {
				before = &state
			}
			r.attempts[key] = before
			r.attemptOrder = append(r.attemptOrder, key)
		}
	case ns == retryNamespace:
		if r.retries == nil {
			r.retries = map[string]*RetryState{}
		}
		if _, ok := r.retries[key]; !ok {
			var before *RetryState
			var state RetryState
			if err := getJSON(r.Tx, ns, key, &state); err == nil {
				before = &state
			}
			r.retries[key] = before
			r.retryOrder = append(r.retryOrder, key)
		}
	case strings.HasPrefix(ns, "core.ingress.") && key == "state":
		if r.ingress == nil {
			r.ingress = map[string]*ingressState{}
		}
		if _, ok := r.ingress[ns]; !ok {
			var before *ingressState
			var state ingressState
			if err := getJSON(r.Tx, ns, key, &state); err == nil {
				before = &state
			}
			r.ingress[ns] = before
			r.ingressOrder = append(r.ingressOrder, ns)
		}
	case ns == causalRejections:
		if _, err := r.Tx.Get(ns, key); errors.Is(err, storage.ErrNotFound) {
			r.rejections = append(r.rejections, key)
		}
	case ns == retentionNamespace:
		if _, err := r.Tx.Get(ns, key); errors.Is(err, storage.ErrNotFound) {
			r.expirations = append(r.expirations, key)
		}
	}
	return r.Tx.Put(ns, key, value)
}

// resolveSafely never lets instrumentation fail or abort the transaction.
func (r *recorder) resolveSafely(diagnostic func(string)) (out []observability.Record) {
	defer func() {
		if recover() != nil {
			out = nil
			diagnostic("telemetry_resolve_panic")
		}
	}()
	return r.resolve(diagnostic)
}

// emit appends a record when correlation is complete. A record that cannot be
// anchored to a trusted causal Event is dropped with a diagnostic rather than
// fabricated.
func (r *recorder) emit(out *[]observability.Record, diagnostic func(string), rec observability.Record) {
	rec.At = r.at
	if rec.Attributes == nil {
		rec.Attributes = map[string]string{}
	}
	if err := observability.Validate(rec); err != nil {
		diagnostic("uncorrelated_record")
		return
	}
	*out = append(*out, rec)
}

func (r *recorder) resolve(diagnostic func(string)) []observability.Record {
	var out []observability.Record
	add := func(rec observability.Record) { r.emit(&out, diagnostic, rec) }

	// Events first: acceptance precedes any route or state change it causes.
	for _, f := range r.accepted {
		name := "event.accepted"
		if !f.added {
			name = "event.duplicate"
		}
		a, err := r.Tx.Event(f.id)
		if err != nil {
			continue
		}
		c := r.eventCorrelation(a)
		attrs := map[string]string{"event.type": a.Event.Type, "event.source": a.Event.Source, "event.sequence": strconv.FormatUint(a.Sequence, 10), "record_id": identity(name, string(a.Event.ID))}
		if a.Event.CausationID != "" {
			attrs["event.causation_id"] = string(a.Event.CausationID)
		}
		if a.Event.ExecutionID != "" {
			attrs["emitted_by_execution"] = "true"
		}
		add(observability.Record{Name: name, Correlation: c, Attributes: attrs})
	}
	for _, id := range r.routed {
		a, err := r.Tx.Event(id)
		if err != nil {
			continue
		}
		c := r.eventCorrelation(a)
		if a.RoutedWorkID != "" {
			c.WorkID = a.RoutedWorkID
		}
		attrs := map[string]string{"event.type": a.Event.Type, "route.disposition": a.Disposition, "record_id": identity("route.decided", string(id))}
		var rec RouteRecord
		if getJSON(r.Tx, routeNamespace, identity(string(id)), &rec) == nil {
			attrs["route.name"], attrs["route.binding"], attrs["route.generation"], attrs["route.reason"] = rec.Route, rec.Binding, rec.Generation, rec.Reason
		}
		add(observability.Record{Name: "route.decided", Correlation: c, To: a.Disposition, Attributes: attrs})
	}
	for _, key := range r.rejections {
		var d CausalRejection
		if getJSON(r.Tx, causalRejections, key, &d) != nil {
			continue
		}
		parent, err := r.Tx.Event(d.ParentID)
		if err != nil {
			diagnostic("uncorrelated_record")
			continue
		}
		c := r.eventCorrelation(parent)
		if d.WorkID != "" {
			c.WorkID = d.WorkID
		}
		if d.ExecutionID != "" {
			c.ExecutionID = d.ExecutionID
			if x, err := r.Tx.Execution(d.ExecutionID); err == nil {
				c.ExecutorBinding, c.TraceID = x.ExecutorBinding, x.TraceID
			}
		}
		add(observability.Record{Name: "causal.rejected", Correlation: c, Attributes: map[string]string{"reason": d.Reason, "causal.depth": strconv.Itoa(d.Depth), "causal.count": strconv.Itoa(d.Count), "causal.root_id": string(d.RootID), "causal.max_depth": strconv.Itoa(d.Limits.MaxDepth), "causal.per_minute": strconv.Itoa(d.Limits.PerMinute), "record_id": identity("causal.rejected", key)}})
	}
	for _, id := range r.workOrder {
		before := r.works[id]
		after, err := r.Tx.Work(id)
		if err != nil {
			continue
		}
		name, from := "", ""
		switch {
		case before == nil:
			name = "work.created"
		case before.State != after.State:
			name, from = "work.transitioned", string(before.State)
		default:
			continue
		}
		c := r.workCorrelation(after)
		attrs := map[string]string{"work.kind": after.Kind, "work.revision": strconv.FormatUint(uint64(after.Revision), 10), "record_id": identity(name, string(id), strconv.FormatUint(uint64(after.Revision), 10))}
		if after.ParentID != "" {
			attrs["work.parent_id"] = string(after.ParentID)
		}
		add(observability.Record{Name: name, Correlation: c, From: from, To: string(after.State), Attributes: attrs})
	}
	for _, id := range r.execOrder {
		before := r.execs[id]
		after, err := r.Tx.Execution(id)
		if err != nil {
			continue
		}
		r.executionRecords(add, before, after)
	}
	for _, f := range r.leases {
		if x, err := r.Tx.Execution(f.ref.ExecutionID); err == nil {
			add(observability.Record{Name: f.name, Correlation: r.executionCorrelation(x), Attributes: map[string]string{"lease.epoch": strconv.FormatUint(f.ref.Epoch, 10), "lease.owner": f.ref.Owner, "lease.expires_at": timeAttr(x.Lease.ExpiresAt), "record_id": identity(f.name, string(x.ID), strconv.FormatUint(uint64(x.Revision), 10))}})
		}
	}
	for _, key := range r.attemptOrder {
		before := r.attempts[key]
		var after AttemptState
		if getJSON(r.Tx, attemptNamespace, key, &after) != nil || after.ExecutionID == "" {
			continue
		}
		x, err := r.Tx.Execution(after.ExecutionID)
		if err != nil {
			continue
		}
		c := r.executionCorrelation(x)
		epoch := strconv.FormatUint(after.Epoch, 10)
		if after.LaunchState == "running" && (before == nil || before.LaunchState != "running") {
			mode := "run"
			if before != nil {
				switch before.LaunchState {
				case "recovering":
					mode = "recover"
				case "resuming":
					mode = "resume"
				}
			}
			add(observability.Record{Name: "execution.launched", Correlation: c, Attributes: map[string]string{"launch.mode": mode, "lease.epoch": epoch, "executor": after.Executor, "config.generation": after.Generation, "record_id": identity("execution.launched", string(x.ID), epoch)}})
		}
		if prior := r.execs[x.ID]; after.Excluded && before != nil && !before.Excluded && x.State.Terminal() && (prior == nil || prior.State == x.State) {
			add(observability.Record{Name: "execution.stop_confirmed", Correlation: c, Attributes: map[string]string{"lease.epoch": epoch, "record_id": identity("execution.stop_confirmed", string(x.ID), epoch)}})
		}
		if after.Declaration != nil && (before == nil || before.Declaration == nil) {
			kind := "outcome"
			if after.Declaration.Wait != nil {
				kind = "wait"
			}
			add(observability.Record{Name: "toolbelt.declared", Correlation: c, Attributes: map[string]string{"declaration.kind": kind, "lease.epoch": epoch, "record_id": identity("toolbelt.declared", string(x.ID), epoch)}})
		}
	}
	for _, key := range r.retryOrder {
		before := r.retries[key]
		var after RetryState
		if getJSON(r.Tx, retryNamespace, key, &after) != nil {
			continue
		}
		name := ""
		switch {
		case before == nil || after.FailureCount > before.FailureCount || after.Exhausted && !before.Exhausted:
			name = "retry.scheduled"
		case before.Blocked && !after.Blocked:
			name = "retry.unblocked"
		case !before.Blocked && after.Blocked:
			name = "retry.blocked"
		default:
			continue
		}
		x, err := r.Tx.Execution(after.FailedExecutionID)
		if err != nil {
			diagnostic("uncorrelated_record")
			continue
		}
		attrs := map[string]string{"retry.failure_count": strconv.Itoa(after.FailureCount), "retry.blocked": strconv.FormatBool(after.Blocked), "retry.exhausted": strconv.FormatBool(after.Exhausted), "record_id": identity(name, key, strconv.Itoa(after.FailureCount), strconv.FormatUint(after.FailureSequence, 10), timeAttr(r.at))}
		if !after.EligibleAt.IsZero() {
			attrs["retry.eligible_at"] = timeAttr(after.EligibleAt)
		}
		if after.BlockedReason != "" {
			attrs["retry.blocked_reason"] = after.BlockedReason
		}
		if after.RetryEventID != "" {
			attrs["retry.event_id"] = string(after.RetryEventID)
		} else if before != nil && before.RetryEventID != "" {
			attrs["retry.event_id"] = string(before.RetryEventID)
		}
		add(observability.Record{Name: name, Correlation: r.executionCorrelation(x), Attributes: attrs})
	}
	for _, id := range r.timerOrder {
		before := r.timers[id]
		after, err := r.Tx.Timer(id)
		var afterPtr *domain.Timer
		if err == nil {
			afterPtr = &after
		}
		if r.fired[id] && before != nil {
			c, ok := r.timerCorrelation(*before, before.Event.ID)
			if ok {
				add(observability.Record{Name: "timer.fired", Correlation: c, Attributes: map[string]string{"timer.id": string(id), "event.type": before.Event.Type, "timer.fire_at": timeAttr(before.FireAt), "record_id": identity("timer.fired", string(id), string(before.Event.ID))}})
			}
			if afterPtr != nil {
				if c, ok := r.timerCorrelation(*afterPtr, before.Event.ID); ok {
					add(observability.Record{Name: "timer.armed", Correlation: c, Attributes: map[string]string{"timer.id": string(id), "event.type": after.Event.Type, "timer.fire_at": timeAttr(after.FireAt), "timer.recurring": "true", "record_id": identity("timer.armed", string(id), string(after.Event.ID))}})
				}
			}
			continue
		}
		switch {
		case before == nil && afterPtr != nil:
			if c, ok := r.timerCorrelation(after, after.Event.CausationID); ok {
				add(observability.Record{Name: "timer.armed", Correlation: c, Attributes: map[string]string{"timer.id": string(id), "event.type": after.Event.Type, "timer.fire_at": timeAttr(after.FireAt), "record_id": identity("timer.armed", string(id), string(after.Event.ID))}})
			}
		case before != nil && afterPtr == nil:
			if c, ok := r.timerCorrelation(*before, before.Event.CausationID); ok {
				add(observability.Record{Name: "timer.cancelled", Correlation: c, Attributes: map[string]string{"timer.id": string(id), "event.type": before.Event.Type, "record_id": identity("timer.cancelled", string(id), string(before.Event.ID))}})
			}
		case before != nil && afterPtr != nil && (before.Revision != after.Revision):
			if c, ok := r.timerCorrelation(after, after.Event.CausationID); ok {
				add(observability.Record{Name: "timer.armed", Correlation: c, Attributes: map[string]string{"timer.id": string(id), "event.type": after.Event.Type, "timer.fire_at": timeAttr(after.FireAt), "record_id": identity("timer.armed", string(id), string(after.Event.ID))}})
			}
		}
	}
	for _, id := range r.waitOrder {
		before := r.waits[id]
		after, err := r.Tx.Wait(id)
		if err != nil {
			continue
		}
		name, from := "", ""
		switch {
		case before == nil:
			name = "wait.registered"
		case before.State != after.State && after.State == domain.WaitWoken:
			name, from = "wait.woken", string(before.State)
		case before.State != after.State && after.State == domain.WaitCancelled:
			name, from = "wait.cancelled", string(before.State)
		default:
			continue
		}
		x, err := r.Tx.Execution(after.ExecutionID)
		if err != nil {
			diagnostic("uncorrelated_record")
			continue
		}
		c := r.executionCorrelation(x)
		attrs := map[string]string{"wait.id": string(id), "wait.types": strings.Join(after.Match.Types, ","), "wait.keys": strconv.Itoa(len(after.Match.Keys)), "record_id": identity(name, string(id))}
		if after.TimeoutTimerID != "" {
			attrs["wait.timeout_timer_id"] = string(after.TimeoutTimerID)
		}
		if name == "wait.woken" && after.WakeEventID != "" {
			c.EventID = after.WakeEventID
			attrs["wait.wake_event_id"] = string(after.WakeEventID)
			if a, err := r.Tx.Event(after.WakeEventID); err == nil {
				attrs["wait.wake_event_type"] = a.Event.Type
			}
		}
		add(observability.Record{Name: name, Correlation: c, From: from, To: string(after.State), Attributes: attrs})
	}
	for _, ns := range r.ingressOrder {
		r.ingressRecords(add, r.ingress[ns], ns)
	}
	for _, key := range r.expirations {
		var e retentionRecord
		if getJSON(r.Tx, retentionNamespace, key, &e) != nil {
			continue
		}
		if x, err := r.Tx.Execution(e.ExecutionID); err == nil {
			add(observability.Record{Name: "execution.retention_expired", Correlation: r.executionCorrelation(x), Attributes: map[string]string{"retention.artifacts": strconv.Itoa(len(e.Artifacts)), "retention.kept": strconv.Itoa(e.Kept), "record_id": identity("execution.retention_expired", string(x.ID))}})
		}
	}
	return out
}

func (r *recorder) executionRecords(add func(observability.Record), before *domain.Execution, after domain.Execution) {
	c := r.executionCorrelation(after)
	rev := strconv.FormatUint(uint64(after.Revision), 10)
	epoch := strconv.FormatUint(after.Lease.Epoch, 10)
	base := func(name string) map[string]string {
		attrs := map[string]string{"execution.attempt": strconv.FormatUint(uint64(after.Attempt), 10), "lease.epoch": epoch, "record_id": identity(name, string(after.ID), rev)}
		return attrs
	}
	if before == nil {
		attrs := base("execution.queued")
		attrs["trigger.id"] = string(after.TriggerID)
		if a, err := r.Tx.Event(after.TriggerID); err == nil {
			attrs["trigger.type"] = a.Event.Type
			attrs["trigger.trace_id"] = r.traceOf(a.Event.ID)
		}
		add(observability.Record{Name: "execution.queued", Correlation: c, To: string(domain.ExecutionPending), Attributes: attrs})
		before = &domain.Execution{State: domain.ExecutionPending}
	}
	if after.Lease.Epoch > before.Lease.Epoch && after.State == domain.ExecutionRunning {
		name := "execution.started"
		switch before.State {
		case domain.ExecutionParked:
			name = "execution.resumed"
		case domain.ExecutionRunning:
			name = "execution.recovered"
		}
		attrs := base(name)
		trigger := c.EventID
		attrs["trigger.id"] = string(trigger)
		if a, err := r.Tx.Event(trigger); err == nil {
			attrs["trigger.type"] = a.Event.Type
			attrs["trigger.source"] = a.Event.Source
		}
		attrs["lease.owner"] = after.Lease.Owner
		if name == "execution.recovered" {
			attrs["lease.previous_epoch"] = strconv.FormatUint(before.Lease.Epoch, 10)
			attrs["lease.previous_owner"] = before.Lease.Owner
		}
		if state, err := attemptState(r.Tx, after.ID); err == nil {
			attrs["executor"] = state.Executor
			attrs["config.generation"] = state.Generation
			if !state.Deadline.IsZero() {
				attrs["execution.deadline"] = timeAttr(state.Deadline)
			}
		}
		add(observability.Record{Name: name, Correlation: c, From: string(before.State), To: string(after.State), Attributes: attrs})
		return
	}
	if before.State == after.State {
		return
	}
	switch {
	case after.State == domain.ExecutionParked:
		add(observability.Record{Name: "execution.parked", Correlation: c, From: string(before.State), To: string(after.State), Attributes: base("execution.parked")})
	case after.State.Terminal():
		name := "execution." + string(after.State)
		attrs := base(name)
		var usage *observability.Measurement
		if after.Outcome != nil {
			attrs["outcome.kind"] = string(after.Outcome.Kind)
			attrs["outcome.summary"] = truncate(after.Outcome.Summary, 256)
		}
		if d, ok := domain.TurnOf(after.Outcome); ok && d.Status != "" {
			attrs["turn.status"] = d.Status
		}
		if after.StartedAt != nil {
			attrs["execution.started_at"] = timeAttr(*after.StartedAt)
		}
		if after.StartedAt != nil && after.FinishedAt != nil {
			attrs["execution.duration_ms"] = strconv.FormatInt(after.FinishedAt.Sub(*after.StartedAt).Milliseconds(), 10)
		}
		// What started the attempt and when: the gap from the Work's
		// previous turn to this trigger is the time it spent waiting.
		if a, err := r.Tx.Event(after.TriggerID); err == nil {
			attrs["trigger.type"] = a.Event.Type
			if !a.Event.OccurredAt.IsZero() {
				attrs["trigger.occurred_at"] = timeAttr(a.Event.OccurredAt)
			}
		}
		if w, err := r.Tx.Work(after.WorkID); err == nil {
			attrs["work.kind"] = w.Kind
		}
		attrs["execution.artifacts"] = strconv.Itoa(len(after.Artifacts))
		if state, err := attemptState(r.Tx, after.ID); err == nil {
			attrs["executor"] = state.Executor
			if state.LaunchState != "" {
				attrs["execution.stop_state"] = state.LaunchState
			}
			attrs["execution.resource_excluded"] = strconv.FormatBool(state.Excluded)
			u := state.Usage()
			usage = measurement(u.InputTokens, u.OutputTokens, u.CachedTokens, u.Cost, u.Currency)
		}
		add(observability.Record{Name: name, Correlation: c, From: string(before.State), To: string(after.State), Attributes: attrs, Usage: usage})
	}
}

func measurement(in, out, cached *uint64, cost *float64, currency string) *observability.Measurement {
	if in == nil && out == nil && cached == nil && cost == nil {
		return nil
	}
	return &observability.Measurement{InputTokens: in, OutputTokens: out, CachedTokens: cached, Cost: cost, Currency: currency}
}

func (r *recorder) ingressRecords(add func(observability.Record), before *ingressState, ns string) {
	var after ingressState
	if getJSON(r.Tx, ns, "state", &after) != nil || after.Source.ScopeID == "" {
		return
	}
	if before == nil {
		before = &ingressState{}
	}
	source := after.Source.Namespace
	scanID := after.ScanID
	if scanID == "" {
		scanID = before.ScanID
	}
	record := func(name string, attrs map[string]string) {
		anchor := scanID
		if anchor == "" {
			anchor = "attempt/" + timeAttr(r.at)
		}
		attrs["ingress.source"] = source
		attrs["ingress.name"] = after.Name
		if scanID != "" {
			attrs["ingress.scan_id"] = scanID
		}
		attrs["record_id"] = identity(name, source, anchor, timeAttr(r.at))
		add(observability.Record{Name: name, Correlation: observability.Correlation{ScopeID: after.Source.ScopeID, TraceID: identity("ingress", source, anchor)}, Attributes: attrs})
	}
	switch {
	case !after.LastComplete.Equal(before.LastComplete):
		if before.ScanID == "" {
			record("ingress.scan_started", map[string]string{})
		}
		record("ingress.scan_completed", map[string]string{"ingress.checkpoint_advanced": strconv.FormatBool(after.Checkpoint != before.Checkpoint)})
	case after.LastError != "" && (after.LastError != before.LastError || after.Failures != before.Failures || !after.NextDue.Equal(before.NextDue)):
		name := "ingress.scan_failed"
		if after.LastError == "partial_snapshot" {
			name = "ingress.scan_partial"
		}
		record(name, map[string]string{"ingress.error": after.LastError, "ingress.failures": strconv.FormatUint(uint64(after.Failures), 10), "ingress.next_due": timeAttr(after.NextDue)})
	case after.ScanID != "" && before.ScanID == "":
		record("ingress.scan_started", map[string]string{})
		record("ingress.scan_page", map[string]string{"ingress.more": strconv.FormatBool(after.Cursor != "")})
	case after.ScanID != "" && after.Cursor != before.Cursor:
		record("ingress.scan_page", map[string]string{"ingress.more": strconv.FormatBool(after.Cursor != "")})
	}
}

func (r *recorder) executionCorrelation(x domain.Execution) observability.Correlation {
	c := observability.Correlation{WorkID: x.WorkID, ExecutionID: x.ID, ExecutorBinding: x.ExecutorBinding, TraceID: x.TraceID, EventID: x.TriggerID}
	if w, err := r.Tx.Work(x.WorkID); err == nil {
		c.ScopeID = w.ScopeID
	}
	if state, err := attemptState(r.Tx, x.ID); err == nil && state.CurrentTriggerID != "" {
		c.EventID = state.CurrentTriggerID
	}
	return c
}

func (r *recorder) eventCorrelation(a domain.AcceptedEvent) observability.Correlation {
	c := observability.Correlation{EventID: a.Event.ID, WorkID: a.Event.WorkID}
	if c.WorkID == "" {
		c.WorkID = a.RoutedWorkID
	}
	if scope, err := scopeForEvent(r.Tx, a); err == nil {
		c.ScopeID = scope
	}
	if a.Event.ExecutionID != "" {
		if x, err := r.Tx.Execution(a.Event.ExecutionID); err == nil {
			c.ExecutionID, c.ExecutorBinding = x.ID, x.ExecutorBinding
		}
	}
	c.TraceID = r.traceOf(a.Event.ID)
	if c.ScopeID == "" && c.WorkID != "" {
		if w, err := r.Tx.Work(c.WorkID); err == nil {
			c.ScopeID = w.ScopeID
		}
	}
	return c
}

// workCorrelation anchors a Work change to the Event that caused it in this
// transaction: an Event routed or accepted for that Work, then the trigger of
// an attempt touched in this transaction, then any Event committed here.
func (r *recorder) workCorrelation(w domain.Work) observability.Correlation {
	c := observability.Correlation{WorkID: w.ID, ScopeID: w.ScopeID}
	cause := domain.ID("")
	for _, id := range r.routed {
		if a, err := r.Tx.Event(id); err == nil && (a.RoutedWorkID == w.ID || a.Event.WorkID == w.ID) {
			cause = id
			break
		}
	}
	if cause == "" {
		for _, f := range r.accepted {
			if a, err := r.Tx.Event(f.id); err == nil && a.Event.WorkID == w.ID {
				cause = f.id
				break
			}
		}
	}
	if cause == "" {
		for _, id := range r.execOrder {
			if x, err := r.Tx.Execution(id); err == nil && x.WorkID == w.ID {
				xc := r.executionCorrelation(x)
				cause = xc.EventID
				c.ExecutionID, c.ExecutorBinding = x.ID, x.ExecutorBinding
				break
			}
		}
	}
	if cause == "" && len(r.routed) > 0 {
		cause = r.routed[0]
	}
	if cause == "" && len(r.accepted) > 0 {
		cause = r.accepted[0].id
	}
	if cause != "" {
		c.EventID = cause
		c.TraceID = r.traceOf(cause)
	}
	return c
}

// timerCorrelation anchors a timer to the Event that armed it (its causation)
// or, for a fire, to the Event it emitted. Root timers armed without any
// committed Event cannot be anchored and are reported as uncorrelated.
func (r *recorder) timerCorrelation(t domain.Timer, anchor domain.ID) (observability.Correlation, bool) {
	if anchor == "" {
		for _, f := range r.accepted {
			anchor = f.id
			break
		}
	}
	if anchor == "" && len(r.routed) > 0 {
		anchor = r.routed[0]
	}
	if anchor == "" {
		return observability.Correlation{}, false
	}
	a, err := r.Tx.Event(anchor)
	if err != nil {
		return observability.Correlation{}, false
	}
	c := r.eventCorrelation(a)
	if t.Event.WorkID != "" {
		c.WorkID = t.Event.WorkID
	}
	if t.Event.ExecutionID != "" {
		if x, err := r.Tx.Execution(t.Event.ExecutionID); err == nil {
			c.ExecutionID, c.ExecutorBinding, c.TraceID = x.ID, x.ExecutorBinding, x.TraceID
		}
	}
	if c.ScopeID == "" && c.WorkID != "" {
		if w, err := r.Tx.Work(c.WorkID); err == nil {
			c.ScopeID = w.ScopeID
		}
	}
	return c, true
}

// traceOf returns the trace of an Event: the producing attempt's trace for an
// execution emission, otherwise a deterministic trace of its causal root, so
// records emitted before and after a restart join the same trace.
func (r *recorder) traceOf(id domain.ID) string {
	if r.traces == nil {
		r.traces = map[domain.ID]string{}
	}
	if t, ok := r.traces[id]; ok {
		return t
	}
	trace := ""
	current := id
	for depth := 0; depth < 4096 && current != ""; depth++ {
		a, err := r.Tx.Event(current)
		if err != nil {
			break
		}
		if a.Event.ExecutionID != "" {
			if x, err := r.Tx.Execution(a.Event.ExecutionID); err == nil && x.TraceID != "" {
				trace = x.TraceID
				break
			}
		}
		if a.Event.CausationID == "" {
			trace = identity("trace", string(current))
			break
		}
		current = a.Event.CausationID
	}
	r.traces[id] = trace
	return trace
}

func timeAttr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// observeDirect reports a record that has no storage mutation of its own (for
// example a refused toolbelt call). It is used only with trusted correlation.
func observeDirect(ctx context.Context, o observability.Observer, rec observability.Record) {
	if o == nil {
		return
	}
	if rec.Attributes == nil {
		rec.Attributes = map[string]string{}
	}
	if observability.Validate(rec) != nil {
		return
	}
	_ = o.Observe(context.WithoutCancel(ctx), rec)
}
