// Package reference contains a deliberately simple, volatile contract double.
// It is not a production backend and provides no process-crash durability.
package reference

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

type state struct {
	TimerVersions map[domain.ID]domain.Revision
	Events        map[domain.ID]domain.AcceptedEvent
	Works         map[domain.ID]domain.Work
	Executions    map[domain.ID]domain.Execution
	Timers        map[domain.ID]domain.Timer
	Waits         map[domain.ID]domain.Wait
	Scopes        map[domain.ID]domain.Scope
	KV            map[string]map[string][]byte
	Sequence      uint64
}
type Memory struct {
	mu   sync.Mutex
	data state
}

var _ storage.Store = (*Memory)(nil)

func New() *Memory {
	return &Memory{data: state{
		TimerVersions: map[domain.ID]domain.Revision{},
		Events:        map[domain.ID]domain.AcceptedEvent{}, Works: map[domain.ID]domain.Work{},
		Executions: map[domain.ID]domain.Execution{}, Timers: map[domain.ID]domain.Timer{},
		Waits: map[domain.ID]domain.Wait{}, Scopes: map[domain.ID]domain.Scope{}, KV: map[string]map[string][]byte{},
	}}
}
func copyOf[T any](v T) T {
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(v); err != nil {
		panic(err)
	}
	var out T
	if err := gob.NewDecoder(&buffer).Decode(&out); err != nil {
		panic(err)
	}
	return out
}
func (m *Memory) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.Now.IsZero() {
		return storage.ErrInvalid
	}
	t := &transaction{data: copyOf(m.data), now: g.Now}
	if g.Lease != nil {
		if err := t.authorize(*g.Lease); err != nil {
			return err
		}
	}
	if err := fn(t); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.data = copyOf(t.data)
	return nil
}

type transaction struct {
	data state
	now  time.Time
}

func namespaced(s string) bool {
	if !strings.Contains(s, ".") || strings.TrimSpace(s) != s || strings.ContainsAny(s, " \t\n") {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
	}
	return true
}
func payload(p domain.Payload) bool {
	return p.Schema == "" && len(p.Data) == 0 || namespaced(p.Schema) && json.Valid(p.Data)
}
func keyOK(k domain.CorrelationKey) bool { return k.Source != "" && k.ExternalID != "" }
func revision(exists bool, current, expected domain.Revision) error {
	if !exists && expected == 0 {
		return nil
	}
	if exists && expected != 0 && current == expected {
		return nil
	}
	return storage.ErrConflict
}
func (t *transaction) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	if e.ID == "" || !namespaced(e.Type) || e.Source == "" || e.DeliveryID == "" || e.OccurredAt.IsZero() || e.ReceivedAt.IsZero() || !payload(e.Data) || e.CorrelationKey != nil && !keyOK(*e.CorrelationKey) {
		return domain.AcceptedEvent{}, false, storage.ErrInvalid
	}
	for _, old := range t.data.Events {
		if old.Event.Source == e.Source && old.Event.DeliveryID == e.DeliveryID {
			return copyOf(old), false, nil
		}
	}
	if _, ok := t.data.Events[e.ID]; ok {
		return domain.AcceptedEvent{}, false, storage.ErrConflict
	}
	if e.CausationID != "" {
		if _, ok := t.data.Events[e.CausationID]; !ok {
			return domain.AcceptedEvent{}, false, storage.ErrNotFound
		}
	}
	if e.WorkID != "" {
		if _, ok := t.data.Works[e.WorkID]; !ok {
			return domain.AcceptedEvent{}, false, storage.ErrNotFound
		}
	}
	if e.ExecutionID != "" {
		x, ok := t.data.Executions[e.ExecutionID]
		if !ok || e.WorkID != "" && x.WorkID != e.WorkID {
			return domain.AcceptedEvent{}, false, storage.ErrInvalid
		}
	}
	t.data.Sequence++
	a := domain.AcceptedEvent{Event: copyOf(e), Sequence: t.data.Sequence}
	t.data.Events[e.ID] = a
	return copyOf(a), true, nil
}
func (t *transaction) Event(id domain.ID) (domain.AcceptedEvent, error) {
	v, ok := t.data.Events[id]
	if !ok {
		return v, storage.ErrNotFound
	}
	return copyOf(v), nil
}
func (t *transaction) Events(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	out := []domain.AcceptedEvent{}
	for _, v := range t.data.Events {
		if v.Sequence > after {
			out = append(out, copyOf(v))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (t *transaction) UnroutedEvents(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	all, _ := t.Events(after, 0)
	out := []domain.AcceptedEvent{}
	for _, v := range all {
		if !v.Routed && (limit <= 0 || len(out) < limit) {
			out = append(out, v)
		}
	}
	return out, nil
}
func (t *transaction) LatestSequence() (uint64, error) {
	var latest uint64
	for _, v := range t.data.Events {
		latest = max(latest, v.Sequence)
	}
	return latest, nil
}
func (t *transaction) EventByDelivery(source, deliveryID string) (domain.AcceptedEvent, error) {
	for _, v := range t.data.Events {
		if v.Event.Source == source && v.Event.DeliveryID == deliveryID {
			return copyOf(v), nil
		}
	}
	return domain.AcceptedEvent{}, storage.ErrNotFound
}
func (t *transaction) MarkRouted(id, work domain.ID, disposition string) error {
	a, ok := t.data.Events[id]
	if !ok {
		return storage.ErrNotFound
	}
	if disposition == "" {
		return storage.ErrInvalid
	}
	if a.Routed {
		if a.RoutedWorkID == work && a.Disposition == disposition {
			return nil
		}
		return storage.ErrConflict
	}
	if work != "" {
		if _, ok := t.data.Works[work]; !ok {
			return storage.ErrNotFound
		}
		for _, other := range t.data.Events {
			if other.Sequence < a.Sequence && t.targets(other.Event, work) && !other.Routed {
				return storage.ErrConflict
			}
		}
	}
	a.Routed = true
	a.RoutedWorkID = work
	a.Disposition = disposition
	t.data.Events[id] = a
	return nil
}
func (t *transaction) targets(e domain.Event, work domain.ID) bool {
	if e.WorkID != "" {
		return e.WorkID == work
	}
	if e.CorrelationKey != nil {
		w, err := t.WorkByCorrelation(*e.CorrelationKey)
		return err == nil && w.ID == work
	}
	return false
}
func (t *transaction) Work(id domain.ID) (domain.Work, error) {
	v, ok := t.data.Works[id]
	if !ok {
		return v, storage.ErrNotFound
	}
	return copyOf(v), nil
}
func (t *transaction) WorkByCorrelation(k domain.CorrelationKey) (domain.Work, error) {
	for _, w := range t.data.Works {
		for _, key := range w.CorrelationKeys {
			if key == k {
				return copyOf(w), nil
			}
		}
	}
	return domain.Work{}, storage.ErrNotFound
}
func (t *transaction) PutWork(w domain.Work, expected domain.Revision) error {
	old, exists := t.data.Works[w.ID]
	if err := revision(exists, old.Revision, expected); err != nil {
		return err
	}
	if w.ID == "" || !namespaced(w.Kind) || w.ScopeID == "" || !payload(w.Context) {
		return storage.ErrInvalid
	}
	if _, ok := t.data.Scopes[w.ScopeID]; !ok {
		return storage.ErrNotFound
	}
	if !exists && w.State != domain.WorkReady || exists && w.State != old.State && !domain.WorkTransition(old.State, w.State) {
		return storage.ErrInvalid
	}
	if exists && (old.State.Terminal() || old.Kind != w.Kind || old.ScopeID != w.ScopeID || old.ParentID != w.ParentID) {
		return storage.ErrInvalid
	}
	if w.ParentID != "" {
		if w.ParentID == w.ID {
			return storage.ErrInvalid
		}
		if _, ok := t.data.Works[w.ParentID]; !ok {
			return storage.ErrNotFound
		}
	}
	seen := map[domain.CorrelationKey]bool{}
	for _, k := range w.CorrelationKeys {
		if !keyOK(k) || seen[k] {
			return storage.ErrInvalid
		}
		seen[k] = true
		if owner, err := t.WorkByCorrelation(k); err == nil && owner.ID != w.ID {
			return storage.ErrConflict
		}
	}
	// Keys remain owned for the entire lifetime, including terminal tombstones.
	for _, k := range old.CorrelationKeys {
		if !seen[k] {
			return storage.ErrInvalid
		}
	}
	w.Revision = expected + 1
	w.CreatedAt = old.CreatedAt
	if !exists {
		w.CreatedAt = t.now
	}
	w.UpdatedAt = t.now
	t.data.Works[w.ID] = copyOf(w)
	return nil
}
func (t *transaction) Execution(id domain.ID) (domain.Execution, error) {
	v, ok := t.data.Executions[id]
	if !ok {
		return v, storage.ErrNotFound
	}
	return copyOf(v), nil
}
func expectedOutcome(x domain.Execution) bool {
	if x.State == domain.ExecutionPending || x.State == domain.ExecutionCancelled {
		return x.Outcome == nil
	}
	kind := domain.OutcomeKind(x.State)
	if x.State == domain.ExecutionParked {
		kind = domain.OutcomeWaiting
	}
	if x.State == domain.ExecutionRunning {
		return x.Outcome == nil
	}
	return x.Outcome != nil && x.Outcome.Kind == kind && payload(x.Outcome.Data)
}
func (t *transaction) PutExecution(x domain.Execution, expected domain.Revision) error {
	old, exists := t.data.Executions[x.ID]
	if err := revision(exists, old.Revision, expected); err != nil {
		return err
	}
	w, ok := t.data.Works[x.WorkID]
	if !ok {
		return storage.ErrNotFound
	}
	if x.ID == "" || w.State.Terminal() || x.ExecutorBinding == "" || x.Attempt == 0 || !expectedOutcome(x) {
		return storage.ErrInvalid
	}
	if _, ok := t.data.Events[x.TriggerID]; !ok {
		return storage.ErrNotFound
	}
	if !exists {
		if x.State != domain.ExecutionPending || x.Lease != (domain.Lease{}) {
			return storage.ErrInvalid
		}
		var latest uint32
		for _, other := range t.data.Executions {
			if other.WorkID == x.WorkID {
				if !other.State.Terminal() {
					return storage.ErrConflict
				}
				if other.Attempt > latest {
					latest = other.Attempt
				}
			}
		}
		if x.Attempt != latest+1 {
			return storage.ErrConflict
		}
	} else {
		if x.WorkID != old.WorkID || x.Attempt != old.Attempt || x.TriggerID != old.TriggerID || x.ExecutorBinding != old.ExecutorBinding || x.Lease != old.Lease || old.State.Terminal() {
			return storage.ErrInvalid
		}
		// Running is entered only by Claim, including parked continuation/recovery.
		if x.State == domain.ExecutionRunning && old.State != x.State {
			return storage.ErrInvalid
		}
		if x.State != old.State && !domain.ExecutionTransition(old.State, x.State) {
			return storage.ErrInvalid
		}
	}
	x.StartedAt = old.StartedAt
	x.FinishedAt = old.FinishedAt
	if x.State.Terminal() {
		now := t.now
		x.FinishedAt = &now
	}
	if x.State.Terminal() || x.State == domain.ExecutionParked {
		x.Lease.Owner = ""
		x.Lease.ExpiresAt = time.Time{}
	}
	x.Revision = expected + 1
	x.CreatedAt = old.CreatedAt
	if !exists {
		x.CreatedAt = t.now
	}
	x.UpdatedAt = t.now
	t.data.Executions[x.ID] = copyOf(x)
	return nil
}
func (t *transaction) authorize(ref domain.LeaseRef) error {
	x, ok := t.data.Executions[ref.ExecutionID]
	if !ok || t.data.Works[x.WorkID].State.Terminal() || ref.Owner == "" || x.State != domain.ExecutionRunning || x.Lease.Owner != ref.Owner || x.Lease.Epoch != ref.Epoch || !t.now.Before(x.Lease.ExpiresAt) {
		return storage.ErrLeaseLost
	}
	return nil
}
func (t *transaction) Claim(id domain.ID, owner string, ttl time.Duration) (domain.LeaseRef, error) {
	x, ok := t.data.Executions[id]
	if !ok {
		return domain.LeaseRef{}, storage.ErrNotFound
	}
	w := t.data.Works[x.WorkID]
	if owner == "" || ttl <= 0 || x.State.Terminal() || w.State.Terminal() {
		return domain.LeaseRef{}, storage.ErrInvalid
	}
	if x.Lease.Owner != "" && t.now.Before(x.Lease.ExpiresAt) {
		return domain.LeaseRef{}, storage.ErrClaimed
	}
	for _, other := range t.data.Executions {
		if other.WorkID == x.WorkID && other.ID != id && !other.State.Terminal() {
			return domain.LeaseRef{}, storage.ErrClaimed
		}
	}
	if w.State != domain.WorkRunning {
		if !domain.WorkTransition(w.State, domain.WorkRunning) {
			return domain.LeaseRef{}, storage.ErrInvalid
		}
		w.State = domain.WorkRunning
		w.Revision++
		w.UpdatedAt = t.now
		t.data.Works[w.ID] = w
	}
	x.Lease = domain.Lease{Owner: owner, Epoch: x.Lease.Epoch + 1, ExpiresAt: t.now.Add(ttl)}
	x.State = domain.ExecutionRunning
	if x.StartedAt == nil {
		now := t.now
		x.StartedAt = &now
	}
	x.Outcome = nil
	x.Revision++
	x.UpdatedAt = t.now
	t.data.Executions[id] = x
	return domain.LeaseRef{ExecutionID: id, Owner: owner, Epoch: x.Lease.Epoch}, nil
}
func (t *transaction) Renew(ref domain.LeaseRef, ttl time.Duration) error {
	if err := t.authorize(ref); err != nil {
		return err
	}
	if ttl <= 0 {
		return storage.ErrInvalid
	}
	x := t.data.Executions[ref.ExecutionID]
	expires := t.now.Add(ttl)
	if expires.Before(x.Lease.ExpiresAt) {
		return storage.ErrInvalid
	}
	x.Lease.ExpiresAt = expires
	x.Revision++
	x.UpdatedAt = t.now
	t.data.Executions[x.ID] = x
	return nil
}
func (t *transaction) Release(ref domain.LeaseRef) error {
	if err := t.authorize(ref); err != nil {
		return err
	}
	x := t.data.Executions[ref.ExecutionID]
	x.Lease.Owner = ""
	x.Lease.ExpiresAt = time.Time{}
	x.Revision++
	x.UpdatedAt = t.now
	t.data.Executions[x.ID] = x
	return nil
}
func (t *transaction) Timer(id domain.ID) (domain.Timer, error) {
	v, ok := t.data.Timers[id]
	if !ok {
		return v, storage.ErrNotFound
	}
	return copyOf(v), nil
}
func (t *transaction) PutTimer(v domain.Timer, expected domain.Revision) error {
	old, exists := t.data.Timers[v.ID]
	if err := revision(exists, old.Revision, expected); err != nil {
		return err
	}
	if v.ID == "" || v.FireAt.IsZero() || v.Event.ID == "" || !namespaced(v.Event.Type) || v.Event.Source == "" || v.Event.DeliveryID == "" || !payload(v.Event.Data) {
		return storage.ErrInvalid
	}
	if v.Event.CorrelationKey != nil && !keyOK(*v.Event.CorrelationKey) {
		return storage.ErrInvalid
	}
	if v.Event.CausationID != "" {
		if _, ok := t.data.Events[v.Event.CausationID]; !ok {
			return storage.ErrNotFound
		}
	}
	for _, a := range t.data.Events {
		if a.Event.ID == v.Event.ID || a.Event.Source == v.Event.Source && a.Event.DeliveryID == v.Event.DeliveryID {
			return storage.ErrConflict
		}
	}

	v.Revision = expected + 1
	if !exists {
		v.Revision = t.data.TimerVersions[v.ID] + 1
	}
	t.data.TimerVersions[v.ID] = v.Revision
	v.CreatedAt = old.CreatedAt
	if !exists {
		v.CreatedAt = t.now
	}
	v.UpdatedAt = t.now
	t.data.Timers[v.ID] = copyOf(v)
	return nil
}
func (t *transaction) FireTimer(id domain.ID, expected domain.Revision, next *domain.Timer) error {
	v, ok := t.data.Timers[id]
	if !ok {
		return storage.ErrNotFound
	}
	if v.Revision != expected {
		return storage.ErrConflict
	}
	if t.now.Before(v.FireAt) {
		return storage.ErrInvalid
	}
	if next != nil && (next.ID != id || !next.FireAt.After(v.FireAt) || next.Event.ID == v.Event.ID || next.Event.Source == v.Event.Source && next.Event.DeliveryID == v.Event.DeliveryID) {
		return storage.ErrInvalid
	}
	before := copyOf(t.data)
	if next != nil {
		if err := t.PutTimer(*next, expected); err != nil {
			return err
		}
	} else {
		delete(t.data.Timers, id)
	}
	e := v.Event
	e.OccurredAt = v.FireAt
	e.ReceivedAt = t.now
	if _, _, err := t.AcceptEvent(e); err != nil {
		t.data = before
		return err
	}
	return nil
}
func (t *transaction) Wait(id domain.ID) (domain.Wait, error) {
	v, ok := t.data.Waits[id]
	if !ok {
		return v, storage.ErrNotFound
	}
	return copyOf(v), nil
}
func (t *transaction) PutWait(v domain.Wait, expected domain.Revision) error {
	old, exists := t.data.Waits[v.ID]
	if err := revision(exists, old.Revision, expected); err != nil {
		return err
	}
	w, ok := t.data.Works[v.WorkID]
	if !ok {
		return storage.ErrNotFound
	}
	x, ok := t.data.Executions[v.ExecutionID]
	if !ok || v.ID == "" || x.WorkID != w.ID || w.State != domain.WorkWaiting || x.State != domain.ExecutionParked && x.State != domain.ExecutionWaiting || len(v.Match.Keys) == 0 || v.AfterSequence > t.data.Sequence {
		return storage.ErrInvalid
	}
	for _, key := range v.Match.Keys {
		found := false
		for _, owned := range w.CorrelationKeys {
			if key == owned {
				found = true
			}
		}
		if !found {
			return storage.ErrInvalid
		}
	}
	for _, typ := range v.Match.Types {
		if !namespaced(typ) {
			return storage.ErrInvalid
		}
	}
	// A timeout may have fired/deleted before the historical wait transitions.
	if !exists && v.TimeoutTimerID != "" {
		if _, ok := t.data.Timers[v.TimeoutTimerID]; !ok {
			return storage.ErrNotFound
		}
	}
	if !exists && (v.WakeEventID != "" || v.State != domain.WaitActive) {
		return storage.ErrInvalid
	}
	if exists && (old.State != domain.WaitActive || old.WorkID != v.WorkID || old.ExecutionID != v.ExecutionID || old.AfterSequence != v.AfterSequence || old.TimeoutTimerID != v.TimeoutTimerID || old.Description != v.Description || old.Owner != v.Owner || !reflect.DeepEqual(old.Match, v.Match)) {
		return storage.ErrInvalid
	}
	// An active Wait changes once, to woken or cancelled, keeping its
	// progress; before that its owner may only change the progress.
	if exists && v.State == domain.WaitActive && (v.Owner == "" || v.WakeEventID != "") {
		return storage.ErrInvalid
	}
	if exists && v.State != domain.WaitActive && (v.State != domain.WaitWoken && v.State != domain.WaitCancelled || v.Progress != old.Progress) {
		return storage.ErrInvalid
	}
	if (v.State == domain.WaitWoken) != (v.WakeEventID != "") {
		return storage.ErrInvalid
	}

	if v.WakeEventID != "" {
		if _, ok := t.data.Events[v.WakeEventID]; !ok {
			return storage.ErrNotFound
		}
	}
	v.Revision = expected + 1
	v.CreatedAt = old.CreatedAt
	if !exists {
		v.CreatedAt = t.now
	}
	v.UpdatedAt = t.now
	t.data.Waits[v.ID] = copyOf(v)
	return nil
}
func (t *transaction) Scope(id domain.ID) (domain.Scope, error) {
	v, ok := t.data.Scopes[id]
	if !ok {
		return v, storage.ErrNotFound
	}
	return copyOf(v), nil
}
func (t *transaction) PutScope(v domain.Scope, expected domain.Revision) error {
	old, exists := t.data.Scopes[v.ID]
	if err := revision(exists, old.Revision, expected); err != nil {
		return err
	}
	if v.ID == "" || v.Name == "" || !payload(v.Config) {
		return storage.ErrInvalid
	}
	seen := map[domain.ID]bool{v.ID: true}
	for id := v.ParentID; id != ""; {
		if seen[id] {
			return storage.ErrInvalid
		}
		seen[id] = true
		p, ok := t.data.Scopes[id]
		if !ok {
			return storage.ErrNotFound
		}
		id = p.ParentID
	}
	v.Revision = expected + 1
	v.CreatedAt = old.CreatedAt
	if !exists {
		v.CreatedAt = t.now
	}
	v.UpdatedAt = t.now
	t.data.Scopes[v.ID] = copyOf(v)
	return nil
}
func kvOK(ns, key string) bool { return ns != "" && key != "" }
func (t *transaction) Get(ns, key string) ([]byte, error) {
	if !kvOK(ns, key) {
		return nil, storage.ErrInvalid
	}
	v, ok := t.data.KV[ns][key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}
func (t *transaction) Put(ns, key string, value []byte) error {
	if !kvOK(ns, key) {
		return storage.ErrInvalid
	}
	if t.data.KV[ns] == nil {
		t.data.KV[ns] = map[string][]byte{}
	}
	t.data.KV[ns][key] = append([]byte(nil), value...)
	return nil
}
func (t *transaction) Delete(ns, key string) error {
	if !kvOK(ns, key) {
		return storage.ErrInvalid
	}
	delete(t.data.KV[ns], key)
	return nil
}
func (t *transaction) List(ns, prefix string) ([]storage.Entry, error) {
	if ns == "" {
		return nil, storage.ErrInvalid
	}
	out := []storage.Entry{}
	for k, v := range t.data.KV[ns] {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.Entry{Key: k, Value: append([]byte(nil), v...)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Enumeration is deterministic. A production backend can add paged query APIs
// while preserving these small-fixture/recovery contract operations.
func sortedValues[K ~string, V any](values map[K]V) []V {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]V, 0, len(keys))
	for _, key := range keys {
		out = append(out, copyOf(values[key]))
	}
	return out
}
func (t *transaction) ListWorks() ([]domain.Work, error)   { return sortedValues(t.data.Works), nil }
func (t *transaction) ListScopes() ([]domain.Scope, error) { return sortedValues(t.data.Scopes), nil }
func (t *transaction) ListTimers() ([]domain.Timer, error) { return sortedValues(t.data.Timers), nil }
func (t *transaction) ListExecutions(work domain.ID) ([]domain.Execution, error) {
	out := []domain.Execution{}
	for _, x := range sortedValues(t.data.Executions) {
		if work == "" || x.WorkID == work {
			out = append(out, x)
		}
	}
	return out, nil
}
func (t *transaction) ListWaits(work domain.ID) ([]domain.Wait, error) {
	out := []domain.Wait{}
	for _, w := range sortedValues(t.data.Waits) {
		if work == "" || w.WorkID == work {
			out = append(out, w)
		}
	}
	return out, nil
}
func (t *transaction) DeleteTimer(id domain.ID, expected domain.Revision) error {
	v, ok := t.data.Timers[id]
	if !ok {
		return storage.ErrNotFound
	}
	if v.Revision != expected {
		return storage.ErrConflict
	}
	delete(t.data.Timers, id)
	return nil
}
