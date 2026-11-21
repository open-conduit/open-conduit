package sqlite

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
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
func (t *transaction) UnroutedInput(target storage.InputTarget, after, before uint64) ([]domain.AcceptedEvent, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	var attempts []string
	if target.Work != "" {
		var err error
		if attempts, err = t.strings("SELECT id FROM executions WHERE work_id=? ORDER BY id", string(target.Work)); err != nil {
			return nil, err
		}
	}
	query, args := unroutedInput(target, attempts, after, before)
	if query == "" {
		return []domain.AcceptedEvent{}, nil
	}
	return list[domain.AcceptedEvent](t, query, args...)
}

// unroutedInput is UnroutedInput's query: a union of index searches, one
// for the events naming the Work, one for those naming its attempts and one
// per key, so each reads only its own entries of the unrouted backlog. The
// attempts are given as values: SQLite searches an expression index only
// for values, never for another table's column. Empty when nothing can
// match.
func unroutedInput(target storage.InputTarget, attempts []string, after, before uint64) (string, []any) {
	if after > 1<<63-1 {
		return "", nil
	}
	if before == 0 || before > 1<<63-1 {
		before = 1<<63 - 1
	}
	if before <= after+1 {
		return "", nil
	}
	bound := " AND " + unrouted + " AND sequence>? AND sequence<?"
	var arms []string
	var args []any
	if target.Work != "" {
		arms = append(arms, "SELECT sequence FROM events WHERE "+eventWork+"=?"+bound)
		args = append(args, string(target.Work), after, before)
	}
	for chunk := range slices.Chunk(attempts, 500) {
		arms = append(arms, "SELECT sequence FROM events WHERE "+eventExecution+" IN ("+strings.Repeat("?,", len(chunk)-1)+"?)"+bound)
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, after, before)
	}
	seen := map[domain.CorrelationKey]bool{}
	for _, k := range target.Keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		arms = append(arms, "SELECT sequence FROM events WHERE "+eventKeySource+"=? AND "+eventKeyID+"=?"+bound)
		args = append(args, k.Source, k.ExternalID, after, before)
	}
	if len(arms) == 0 {
		return "", nil
	}
	return "SELECT record FROM events WHERE sequence IN (" + strings.Join(arms, " UNION ") + ") ORDER BY sequence", args
}
func (t *transaction) UnroutedOfType(types []string) ([]domain.AcceptedEvent, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	query, args := unroutedOfType(types)
	if query == "" {
		return []domain.AcceptedEvent{}, nil
	}
	return list[domain.AcceptedEvent](t, query, args...)
}
func unroutedOfType(types []string) (string, []any) {
	types = slices.Compact(slices.Sorted(slices.Values(types)))
	if len(types) == 0 {
		return "", nil
	}
	marks := make([]string, 0, len(types))
	args := make([]any, 0, len(types))
	for _, ty := range types {
		marks = append(marks, "?")
		args = append(args, ty)
	}
	return "SELECT record FROM events WHERE " + eventType + " IN (" + strings.Join(marks, ",") + ") AND " + unrouted + " ORDER BY sequence", args
}

// earlierInput is whether an unrouted event before sequence is input for
// work, as MarkRouted judges it: one naming work, or one naming no Work
// whose key work owns (keys). It reads the indexes alone, decoding no
// record.
func earlierInput(work domain.ID, keys []domain.CorrelationKey, sequence uint64) (string, []any) {
	query := "SELECT EXISTS(SELECT 1 FROM events WHERE " + eventWork + "=? AND " + unrouted + " AND sequence<?)"
	args := []any{string(work), sequence}
	for _, k := range keys {
		query += " OR EXISTS(SELECT 1 FROM events WHERE " + eventKeySource + "=? AND " + eventKeyID + "=? AND " + unrouted + " AND sequence<? AND " + eventWork + "='')"
		args = append(args, k.Source, k.ExternalID, sequence)
	}
	return query, args
}

func (t *transaction) WorkEvents(work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	if work == "" || after > 1<<63-1 {
		return []domain.AcceptedEvent{}, nil
	}
	if limit <= 0 {
		limit = -1
	}
	return list[domain.AcceptedEvent](t, "SELECT record FROM events WHERE sequence>? AND ("+eventWork+"=? OR "+routedWork+"=?) ORDER BY sequence LIMIT ?", after, string(work), string(work), limit)
}
func (t *transaction) ScopeEvents(scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	if len(scopes) == 0 || after > 1<<63-1 {
		return []domain.AcceptedEvent{}, nil
	}
	if limit <= 0 {
		limit = -1
	}
	args := []any{after}
	marks := make([]string, 0, len(scopes))
	for _, s := range slices.Compact(slices.Sorted(slices.Values(scopes))) {
		marks = append(marks, "?")
		args = append(args, string(s))
	}
	args = append(args, limit)
	return list[domain.AcceptedEvent](t, "SELECT record FROM events WHERE sequence>? AND "+eventScope+" IN ("+strings.Join(marks, ",")+") ORDER BY sequence LIMIT ?", args...)
}

// TypeEvents walks events_type, named so that a scope filter can't move the
// read onto events_scope and its cost off the Events of the type.
func (t *transaction) TypeEvents(eventTypeName string, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	if eventTypeName == "" || after > 1<<63-1 || scopes != nil && len(scopes) == 0 {
		return []domain.AcceptedEvent{}, nil
	}
	if limit <= 0 {
		limit = -1
	}
	query, args := typeEventsQuery(eventTypeName, scopes, after, limit)
	return list[domain.AcceptedEvent](t, query, args...)
}

func typeEventsQuery(eventTypeName string, scopes []domain.ID, after uint64, limit int) (string, []any) {
	query := "SELECT record FROM events INDEXED BY events_type WHERE " + eventType + "=? AND sequence>?"
	args := []any{eventTypeName, after}
	if scopes != nil {
		marks := make([]string, 0, len(scopes))
		for _, s := range slices.Compact(slices.Sorted(slices.Values(scopes))) {
			marks = append(marks, "?")
			args = append(args, string(s))
		}
		query += " AND " + eventScope + " IN (" + strings.Join(marks, ",") + ")"
	}
	return query + " ORDER BY sequence LIMIT ?", append(args, limit)
}
func (t *transaction) LatestSequence() (uint64, error) {
	if !t.live {
		return 0, storage.ErrInvalid
	}
	var sequence uint64
	err := t.queryRow("SELECT coalesce(max(sequence),0) FROM events").Scan(&sequence)
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
		rows, err := t.query("SELECT source, external_id FROM correlations WHERE work_id=? ORDER BY source, external_id", string(work))
		if err != nil {
			return mapError(err)
		}
		var keys []domain.CorrelationKey
		for rows.Next() {
			var k domain.CorrelationKey
			if err = rows.Scan(&k.Source, &k.ExternalID); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return mapError(err)
		}
		query, args := earlierInput(work, keys, a.Sequence)
		var blocked bool
		if err = t.queryRow(query, args...).Scan(&blocked); err != nil {
			return mapError(err)
		}
		if blocked {
			return storage.ErrConflict
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
func (t *transaction) SetEventScope(id, scope domain.ID) error {
	a, err := t.Event(id)
	if err != nil {
		return err
	}
	if scope == "" {
		return storage.ErrInvalid
	}
	if a.ScopeID != "" {
		if a.ScopeID == scope {
			return nil
		}
		return storage.ErrConflict
	}
	if _, err = t.Scope(scope); err != nil {
		return err
	}
	a.ScopeID = scope
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
func (t *transaction) LiveWorks() ([]domain.Work, error) {
	return list[domain.Work](t, "SELECT record FROM works WHERE "+liveWork+" ORDER BY id")
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
	if exists && (old.State.Terminal() || old.Kind != w.Kind || old.ScopeID != w.ScopeID || old.ParentID != w.ParentID || !slices.Equal(old.Grants, w.Grants)) {
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
	w.LatestExecutor = old.LatestExecutor
	if w.UpdatedAt, err = t.workStampInTx(); err != nil {
		return err
	}
	w.CreatedAt = old.CreatedAt
	if !exists {
		w.CreatedAt = w.UpdatedAt
	}
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

// workStampInTx is the UpdatedAt a Work written in this transaction
// carries: the transaction's now, or the latest UpdatedAt
// already committed when the clock has stepped back. Transactions hold the
// writer in turn, so every Work written later carries a later time than
// any a reader has seen, which a change cursor (storage.LiveOrChanged)
// depends on. It reads the latest from works_updated, once a transaction.
func (t *transaction) workStampInTx() (time.Time, error) {
	if !t.workStamp.IsZero() {
		return t.workStamp, nil
	}
	var latest sql.NullInt64
	if err := t.queryRow("SELECT max(" + workUpdated + ") FROM works").Scan(&latest); err != nil {
		return time.Time{}, mapError(err)
	}
	t.workStamp = t.now
	if floor := time.Unix(0, latest.Int64).UTC(); latest.Valid && t.workStamp.Before(floor) {
		t.workStamp = floor
	}
	return t.workStamp, nil
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
func (t *transaction) ActiveWaits() ([]domain.Wait, error) {
	return list[domain.Wait](t, "SELECT record FROM waits WHERE "+activeWait+" ORDER BY id")
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
	if err = t.queryRow("SELECT COALESCE(MAX(sequence),0) FROM events").Scan(&sequence); err != nil {
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
func (t *transaction) LiveExecutions() ([]domain.Execution, error) {
	return list[domain.Execution](t, "SELECT record FROM executions WHERE active=1 ORDER BY id")
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
		if x.WorkID != old.WorkID || x.Attempt != old.Attempt || x.TriggerID != old.TriggerID || x.ExecutorBinding != old.ExecutorBinding || x.Executor != "" && x.Executor != old.Executor || x.Lease != old.Lease || old.State.Terminal() {
			return storage.ErrInvalid
		}
		if x.State == domain.ExecutionRunning && old.State != x.State {
			return storage.ErrInvalid
		}
		if x.State != old.State && !domain.ExecutionTransition(old.State, x.State) {
			return storage.ErrInvalid
		}
	}
	if exists {
		// An update need not repeat the recorded executor (it may have been
		// read before SetExecutionExecutor set it); it keeps it.
		x.Executor = old.Executor
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
	if err = t.writeExecution(x, exists); err != nil || exists {
		return err
	}
	// The new attempt is the Work's latest.
	return t.setLatestExecutor(w, x.Executor)
}

// setLatestExecutor records w's LatestExecutor, which storage keeps
// without a new revision, as it does the record's other derived fields.
func (t *transaction) setLatestExecutor(w domain.Work, executor string) error {
	if w.LatestExecutor == executor {
		return nil
	}
	w.LatestExecutor = executor
	b, err := encode(w)
	if err != nil {
		return err
	}
	_, err = t.exec("UPDATE works SET record=? WHERE id=?", b, w.ID)
	return err
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
	// A wall clock stepped back (an NTP correction, a VM resume) makes the
	// new expiry earlier than the stored one; keep the later, so a renewal
	// never shortens a lease and never fails a healthy holder.
	if expires := t.now.Add(ttl); expires.After(x.Lease.ExpiresAt) {
		x.Lease.ExpiresAt = expires
	}
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

func (t *transaction) ReferencedArtifacts(ids []string) ([]string, error) {
	if err := storage.ValidReferenceQuery(ids); err != nil {
		return nil, err
	}
	out := []string{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := t.query("SELECT DISTINCT artifact_id FROM execution_artifacts WHERE artifact_id IN (?"+strings.Repeat(",?", len(ids)-1)+") ORDER BY artifact_id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
