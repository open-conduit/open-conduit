package sqlite

import (
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

func namespace(s string) bool {
	if !strings.Contains(s, ".") || strings.TrimSpace(s) != s || strings.ContainsAny(s, " \t\n") {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			return false
		}
	}
	return true
}
func (t *transaction) Event(id domain.ID) (domain.AcceptedEvent, error) {
	return read[domain.AcceptedEvent](t, "SELECT record FROM events WHERE id=?", id)
}
func (t *transaction) Events(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	if after > 1<<63-1 {
		return []domain.AcceptedEvent{}, nil
	}
	if limit <= 0 {
		limit = -1
	}
	return list[domain.AcceptedEvent](t, "SELECT record FROM events WHERE sequence>? ORDER BY sequence LIMIT ?", after, limit)
}
func (t *transaction) UnroutedEvents(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	if after > 1<<63-1 {
		return []domain.AcceptedEvent{}, nil
	}
	if limit <= 0 {
		limit = -1
	}
	return list[domain.AcceptedEvent](t, "SELECT record FROM events WHERE sequence>? AND "+unrouted+" ORDER BY sequence LIMIT ?", after, limit)
}
func (t *transaction) LatestSequence() (uint64, error) {
	if !t.live {
		return 0, storage.ErrInvalid
	}
	var sequence uint64
	err := t.conn.QueryRowContext(t.ctx, "SELECT coalesce(max(sequence),0) FROM events").Scan(&sequence)
	return sequence, mapError(err)
}
func (t *transaction) EventByDelivery(source, deliveryID string) (domain.AcceptedEvent, error) {
	return read[domain.AcceptedEvent](t, "SELECT record FROM events WHERE source=? AND delivery_id=?", source, deliveryID)
}
func (t *transaction) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	fail := func(err error) (domain.AcceptedEvent, bool, error) { return domain.AcceptedEvent{}, false, err }
	if e.ID == "" || !namespaced(e.Type) || e.Source == "" || e.DeliveryID == "" || e.OccurredAt.IsZero() || e.ReceivedAt.IsZero() || !payload(e.Data) || e.CorrelationKey != nil && !keyOK(*e.CorrelationKey) {
		return fail(storage.ErrInvalid)
	}
	old, err := t.EventByDelivery(e.Source, e.DeliveryID)
	if err == nil {
		return old, false, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return fail(err)
	}
	if _, err = t.Event(e.ID); err == nil {
		return fail(storage.ErrConflict)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return fail(err)
	}
	if e.CausationID != "" {
		if _, err = t.Event(e.CausationID); err != nil {
			return fail(err)
		}
	}
	if e.WorkID != "" {
		if _, err = t.Work(e.WorkID); err != nil {
			return fail(err)
		}
	}
	if e.ExecutionID != "" {
		x, err := t.Execution(e.ExecutionID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				err = storage.ErrInvalid
			}
			return fail(err)
		}
		if e.WorkID != "" && x.WorkID != e.WorkID {
			return fail(storage.ErrInvalid)
		}
	}
	a := domain.AcceptedEvent{Event: e}
	b, err := encode(a)
	if err != nil {
		return fail(err)
	}
	r, err := t.exec("INSERT INTO events(id,source,delivery_id,record) VALUES(?,?,?,?)", e.ID, e.Source, e.DeliveryID, b)
	if err != nil {
		return fail(err)
	}
	sequence, err := r.LastInsertId()
	if err != nil {
		return fail(err)
	}
	a.Sequence = uint64(sequence)
	b, err = encode(a)
	if err != nil {
		return fail(err)
	}
	if _, err = t.exec("UPDATE events SET record=? WHERE id=?", b, e.ID); err != nil {
		return fail(err)
	}
	// Decode to detach caller-owned pointers/raw payloads before returning.
	a, err = t.Event(e.ID)
	return a, true, err
}
func (t *transaction) MarkRouted(id, work domain.ID, disposition string) error {
	a, err := t.Event(id)
	if err != nil {
		return err
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
		if _, err = t.Work(work); err != nil {
			return err
		}
		// Only an earlier unrouted input for this Work blocks the mark.
		earlier, err := list[domain.AcceptedEvent](t, "SELECT record FROM events WHERE sequence<? AND "+unrouted+" ORDER BY sequence", a.Sequence)
		if err != nil {
			return err
		}
		for _, other := range earlier {
			target := other.Event.WorkID == work
			if other.Event.WorkID == "" && other.Event.CorrelationKey != nil {
				w, e := t.WorkByCorrelation(*other.Event.CorrelationKey)
				if e != nil && !errors.Is(e, storage.ErrNotFound) {
					return e
				}
				target = e == nil && w.ID == work
			}
			if target && !other.Routed {
				return storage.ErrConflict
			}
		}
	}
	a.Routed = true
	a.RoutedWorkID = work
	a.Disposition = disposition
	b, err := encode(a)
	if err != nil {
		return err
	}
	_, err = t.exec("UPDATE events SET record=? WHERE id=?", b, id)
	return err
}
func (t *transaction) Work(id domain.ID) (domain.Work, error) {
	return read[domain.Work](t, "SELECT record FROM works WHERE id=?", id)
}
func (t *transaction) WorkByCorrelation(k domain.CorrelationKey) (domain.Work, error) {
	return read[domain.Work](t, "SELECT w.record FROM works w JOIN correlations c ON c.work_id=w.id WHERE c.source=? AND c.external_id=?", k.Source, k.ExternalID)
}
func (t *transaction) ListWorks() ([]domain.Work, error) {
	return list[domain.Work](t, "SELECT record FROM works ORDER BY id")
}
func (t *transaction) PutWork(w domain.Work, expected domain.Revision) error {
	old, err := t.Work(w.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err = revision(exists, old.Revision, expected); err != nil {
		return err
	}
	if w.ID == "" || !namespaced(w.Kind) || w.ScopeID == "" || !payload(w.Context) {
		return storage.ErrInvalid
	}
	if _, err = t.Scope(w.ScopeID); err != nil {
		return err
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
		if _, err = t.Work(w.ParentID); err != nil {
			return err
		}
	}
	seen := map[domain.CorrelationKey]bool{}
	for _, k := range w.CorrelationKeys {
		if !keyOK(k) || seen[k] {
			return storage.ErrInvalid
		}
		seen[k] = true
		owner, e := t.WorkByCorrelation(k)
		if e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		if e == nil && owner.ID != w.ID {
			return storage.ErrConflict
		}
	}
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
	b, err := encode(w)
	if err != nil {
		return err
	}
	if exists {
		_, err = t.exec("UPDATE works SET record=? WHERE id=?", b, w.ID)
	} else {
		_, err = t.exec("INSERT INTO works(id,scope_id,parent_id,record) VALUES(?,?,?,?)", w.ID, w.ScopeID, nullable(w.ParentID), b)
	}
	if err != nil {
		return err
	}
	for _, k := range w.CorrelationKeys {
		if _, err = t.exec("INSERT INTO correlations(source,external_id,work_id) VALUES(?,?,?) ON CONFLICT(source,external_id) DO NOTHING", k.Source, k.ExternalID, w.ID); err != nil {
			return err
		}
	}
	return nil
}
func (t *transaction) Scope(id domain.ID) (domain.Scope, error) {
	return read[domain.Scope](t, "SELECT record FROM scopes WHERE id=?", id)
}
func (t *transaction) ListScopes() ([]domain.Scope, error) {
	return list[domain.Scope](t, "SELECT record FROM scopes ORDER BY id")
}
func (t *transaction) PutScope(v domain.Scope, expected domain.Revision) error {
	old, err := t.Scope(v.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err = revision(exists, old.Revision, expected); err != nil {
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
		p, e := t.Scope(id)
		if e != nil {
			return e
		}
		id = p.ParentID
	}
	v.Revision = expected + 1
	v.CreatedAt = old.CreatedAt
	if !exists {
		v.CreatedAt = t.now
	}
	v.UpdatedAt = t.now
	b, err := encode(v)
	if err != nil {
		return err
	}
	if exists {
		_, err = t.exec("UPDATE scopes SET parent_id=?,record=? WHERE id=?", nullable(v.ParentID), b, v.ID)
	} else {
		_, err = t.exec("INSERT INTO scopes(id,parent_id,record) VALUES(?,?,?)", v.ID, nullable(v.ParentID), b)
	}
	return err
}
func (t *transaction) Wait(id domain.ID) (domain.Wait, error) {
	return read[domain.Wait](t, "SELECT record FROM waits WHERE id=?", id)
}
func (t *transaction) ListWaits(work domain.ID) ([]domain.Wait, error) {
	if work == "" {
		return list[domain.Wait](t, "SELECT record FROM waits ORDER BY id")
	}
	return list[domain.Wait](t, "SELECT record FROM waits WHERE work_id=? ORDER BY id", work)
}
func (t *transaction) PutWait(v domain.Wait, expected domain.Revision) error {
	old, err := t.Wait(v.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err = revision(exists, old.Revision, expected); err != nil {
		return err
	}
	w, err := t.Work(v.WorkID)
	if err != nil {
		return err
	}
	x, err := t.Execution(v.ExecutionID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return storage.ErrInvalid
		}
		return err
	}
	var sequence uint64
	if err = t.conn.QueryRowContext(t.ctx, "SELECT COALESCE(MAX(sequence),0) FROM events").Scan(&sequence); err != nil {
		return err
	}
	if v.ID == "" || x.WorkID != w.ID || w.State != domain.WorkWaiting || x.State != domain.ExecutionParked && x.State != domain.ExecutionWaiting || len(v.Match.Keys) == 0 || v.AfterSequence > sequence {
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
	if !exists && v.TimeoutTimerID != "" {
		if _, err = t.Timer(v.TimeoutTimerID); err != nil {
			return err
		}
	}
	if !exists && (v.WakeEventID != "" || v.State != domain.WaitActive) {
		return storage.ErrInvalid
	}
	if exists && (old.State != domain.WaitActive || old.WorkID != v.WorkID || old.ExecutionID != v.ExecutionID || old.AfterSequence != v.AfterSequence || old.TimeoutTimerID != v.TimeoutTimerID || old.Description != v.Description || !reflect.DeepEqual(old.Match, v.Match)) {
		return storage.ErrInvalid
	}
	if exists && v.State != domain.WaitWoken && v.State != domain.WaitCancelled {
		return storage.ErrInvalid
	}
	if (v.State == domain.WaitWoken) != (v.WakeEventID != "") {
		return storage.ErrInvalid
	}
	if v.WakeEventID != "" {
		if _, err = t.Event(v.WakeEventID); err != nil {
			return err
		}
	}
	v.Revision = expected + 1
	v.CreatedAt = old.CreatedAt
	if !exists {
		v.CreatedAt = t.now
	}
	v.UpdatedAt = t.now
	b, err := encode(v)
	if err != nil {
		return err
	}
	if exists {
		_, err = t.exec("UPDATE waits SET record=? WHERE id=?", b, v.ID)
	} else {
		_, err = t.exec("INSERT INTO waits(id,work_id,execution_id,record) VALUES(?,?,?,?)", v.ID, v.WorkID, v.ExecutionID, b)
	}
	return err
}
func (t *transaction) Execution(id domain.ID) (domain.Execution, error) {
	return read[domain.Execution](t, "SELECT record FROM executions WHERE id=?", id)
}
func (t *transaction) ListExecutions(work domain.ID) ([]domain.Execution, error) {
	if work == "" {
		return list[domain.Execution](t, "SELECT record FROM executions ORDER BY id")
	}
	return list[domain.Execution](t, "SELECT record FROM executions WHERE work_id=? ORDER BY id", work)
}
func expectedOutcome(x domain.Execution) bool {
	if x.State == domain.ExecutionPending || x.State == domain.ExecutionCancelled || x.State == domain.ExecutionRunning {
		return x.Outcome == nil
	}
	kind := domain.OutcomeKind(x.State)
	if x.State == domain.ExecutionParked {
		kind = domain.OutcomeWaiting
	}
	return x.Outcome != nil && x.Outcome.Kind == kind && payload(x.Outcome.Data)
}
func (t *transaction) PutExecution(x domain.Execution, expected domain.Revision) error {
	old, err := t.Execution(x.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err = revision(exists, old.Revision, expected); err != nil {
		return err
	}
	w, err := t.Work(x.WorkID)
	if err != nil {
		return err
	}
	if x.ID == "" || w.State.Terminal() || x.ExecutorBinding == "" || x.Attempt == 0 || !expectedOutcome(x) {
		return storage.ErrInvalid
	}
	if _, err = t.Event(x.TriggerID); err != nil {
		return err
	}
	if !exists {
		if x.State != domain.ExecutionPending || x.Lease != (domain.Lease{}) {
			return storage.ErrInvalid
		}
		xs, err := t.ListExecutions(x.WorkID)
		if err != nil {
			return err
		}
		var latest uint32
		for _, other := range xs {
			if !other.State.Terminal() {
				return storage.ErrConflict
			}
			if other.Attempt > latest {
				latest = other.Attempt
			}
		}
		if latest == ^uint32(0) || x.Attempt != latest+1 {
			return storage.ErrConflict
		}
	} else {
		if x.WorkID != old.WorkID || x.Attempt != old.Attempt || x.TriggerID != old.TriggerID || x.ExecutorBinding != old.ExecutorBinding || x.Lease != old.Lease || old.State.Terminal() {
			return storage.ErrInvalid
		}
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
	return t.writeExecution(x, exists)
}
func (t *transaction) writeExecution(x domain.Execution, exists bool) error {
	b, err := encode(x)
	if err != nil {
		return err
	}
	active := 0
	if !x.State.Terminal() {
		active = 1
	}
	if exists {
		_, err = t.exec("UPDATE executions SET active=?,record=? WHERE id=?", active, b, x.ID)
	} else {
		_, err = t.exec("INSERT INTO executions(id,work_id,attempt,active,record) VALUES(?,?,?,?,?)", x.ID, x.WorkID, x.Attempt, active, b)
	}
	return err
}
func (t *transaction) authorize(ref domain.LeaseRef) error {
	x, err := t.Execution(ref.ExecutionID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return storage.ErrLeaseLost
		}
		return err
	}
	w, err := t.Work(x.WorkID)
	if err != nil {
		return err
	}
	if w.State.Terminal() || ref.Owner == "" || x.State != domain.ExecutionRunning || x.Lease.Owner != ref.Owner || x.Lease.Epoch != ref.Epoch || !t.now.Before(x.Lease.ExpiresAt) {
		return storage.ErrLeaseLost
	}
	return nil
}
func (t *transaction) Claim(id domain.ID, owner string, ttl time.Duration) (domain.LeaseRef, error) {
	fail := func(err error) (domain.LeaseRef, error) { return domain.LeaseRef{}, err }
	x, err := t.Execution(id)
	if err != nil {
		return fail(err)
	}
	w, err := t.Work(x.WorkID)
	if err != nil {
		return fail(err)
	}
	if owner == "" || ttl <= 0 || x.State.Terminal() || w.State.Terminal() || x.Lease.Epoch == ^uint64(0) {
		return fail(storage.ErrInvalid)
	}
	if x.Lease.Owner != "" && t.now.Before(x.Lease.ExpiresAt) {
		return fail(storage.ErrClaimed)
	}
	xs, err := t.ListExecutions(x.WorkID)
	if err != nil {
		return fail(err)
	}
	for _, other := range xs {
		if other.ID != id && !other.State.Terminal() {
			return fail(storage.ErrClaimed)
		}
	}
	if w.State != domain.WorkRunning {
		if !domain.WorkTransition(w.State, domain.WorkRunning) {
			return fail(storage.ErrInvalid)
		}
		w.State = domain.WorkRunning
		if err = t.PutWork(w, w.Revision); err != nil {
			return fail(err)
		}
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
	if err = t.writeExecution(x, true); err != nil {
		return fail(err)
	}
	return domain.LeaseRef{ExecutionID: id, Owner: owner, Epoch: x.Lease.Epoch}, nil
}
func (t *transaction) Renew(ref domain.LeaseRef, ttl time.Duration) error {
	if err := t.authorize(ref); err != nil {
		return err
	}
	if ttl <= 0 {
		return storage.ErrInvalid
	}
	x, err := t.Execution(ref.ExecutionID)
	if err != nil {
		return err
	}
	expires := t.now.Add(ttl)
	if expires.Before(x.Lease.ExpiresAt) {
		return storage.ErrInvalid
	}
	x.Lease.ExpiresAt = expires
	x.Revision++
	x.UpdatedAt = t.now
	return t.writeExecution(x, true)
}
func (t *transaction) Release(ref domain.LeaseRef) error {
	if err := t.authorize(ref); err != nil {
		return err
	}
	x, err := t.Execution(ref.ExecutionID)
	if err != nil {
		return err
	}
	x.Lease.Owner = ""
	x.Lease.ExpiresAt = time.Time{}
	x.Revision++
	x.UpdatedAt = t.now
	return t.writeExecution(x, true)
}
