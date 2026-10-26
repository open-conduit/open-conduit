package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

type canonicalIdentity struct{ Item, Version, Source, DeliveryID, Signature string }

func validateObservation(tx storage.Tx, r ingressRegistration, o extension.Observation) error {
	if o.ID == "" || o.Version == "" || len(o.ID) > 4096 || len(o.Version) > 4096 {
		return extension.ErrInvalid
	}
	if err := validateEvent(r, o.Event, true); err != nil {
		return err
	}
	if err := validateObservationEnvelope(tx, r, o.Event); err != nil {
		return err
	}
	// Canonical payload is opaque. Hash its exact normalized envelope, excluding
	// internal ID and actual receipt time, which may differ across transports.
	normalized := o.Event
	normalized.ID = ""
	normalized.ReceivedAt = time.Time{}
	b, err := json.Marshal(normalized)
	if err != nil {
		return extension.ErrInvalid
	}
	canonical := canonicalIdentity{Item: o.ID, Version: o.Version, Source: o.Event.Source, DeliveryID: o.Event.DeliveryID, Signature: identity(string(b))}
	ns := sourceNamespace(r.source)
	for _, key := range []string{"identity/" + identity(o.ID, o.Version), "delivery/" + identity(o.Event.Source, o.Event.DeliveryID)} {
		var previous canonicalIdentity
		err = getJSON(tx, ns, key, &previous)
		if err == nil {
			if previous != canonical {
				return extension.ErrConflict
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		} else if err = putJSON(tx, ns, key, canonical); err != nil {
			return err
		}
	}
	return nil
}

// Validate before assigning a canonical identity or persisting partial staging.
// This mirrors the public B02 envelope preconditions without accepting an event.
func validateObservationEnvelope(tx storage.Tx, r ingressRegistration, event domain.Event) error {
	if event.OccurredAt.IsZero() || event.ReceivedAt.IsZero() {
		return extension.ErrInvalid
	}
	p := event.Data
	if !(p.Schema == "" && len(p.Data) == 0 || validType(p.Schema) && json.Valid(p.Data)) {
		return extension.ErrInvalid
	}
	if event.CorrelationKey != nil && (event.CorrelationKey.Source == "" || event.CorrelationKey.ExternalID == "") {
		return extension.ErrInvalid
	}
	scope, err := resolveScope(tx, event, r.source.ScopeID)
	if err != nil {
		return err
	}
	if scope != r.source.ScopeID {
		return extension.ErrInvalid
	}
	return nil
}

// Staged records have already passed envelope validation. encoding/json writes
// nil RawMessage as JSON null, then decodes it to literal "null" bytes. Restore
// the absent representation here only; incoming explicit null still fails the
// schema-empty payload precondition in validateObservationEnvelope.
func decodeStagedObservation(data []byte, observation *extension.Observation) error {
	if err := json.Unmarshal(data, observation); err != nil {
		return err
	}
	if observation.Event.Data.Schema == "" && string(observation.Event.Data.Data) == "null" {
		observation.Event.Data.Data = nil
	}
	return nil
}

type ReconcileResult struct {
	Name              string
	Accepted          []domain.AcceptedEvent
	Complete, Partial bool
	ScanID, ErrorCode string
}

func (i *Ingress) Reconcile(ctx context.Context, name string) (ReconcileResult, error) {
	return i.reconcile(ctx, name, false)
}
func (i *Ingress) reconcile(ctx context.Context, name string, onlyDue bool) (result ReconcileResult, err error) {
	result.Name = name
	result.Accepted = []domain.AcceptedEvent{}
	r, err := i.registration(name)
	if err != nil {
		return result, err
	}
	token, err := randomToken()
	if err != nil {
		return result, err
	}
	var claimed ingressState
	ns := sourceNamespace(r.source)
	err = i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		if e := getJSON(tx, ns, "state", &claimed); e != nil {
			return e
		}
		now := i.now()
		if onlyDue && now.Before(claimed.NextDue) {
			return ErrIngressBusy
		}
		if claimed.LeaseToken != "" && now.Before(claimed.LeaseUntil) {
			return ErrIngressBusy
		}
		claimed.LeaseToken = token
		claimed.LeaseUntil = now.Add(i.options.ScanLease)
		if claimed.ScanID == "" {
			claimed.GenerationRequests = claimed.Requests
		}
		return putJSON(tx, ns, "state", claimed)
	})
	if err != nil {
		return result, err
	}
	scan := extension.Scan{Source: r.source, ScanID: claimed.ScanID, Cursor: claimed.Cursor, Checkpoint: claimed.Checkpoint}
	if claimed.ScanID != "" {
		scan.Checkpoint = claimed.PageCheckpoint
	}
	callCtx, cancel := context.WithTimeout(ctx, i.options.PageTimeout)
	page, providerErr := r.adapter.Reconcile(callCtx, scan)
	cancel()
	if providerErr != nil {
		result.ErrorCode = "provider_failed"
		return result, i.finishFailure(ctx, r, token, result.ErrorCode, ErrIngressProvider)
	}
	// Shape checks precede staging; semantic checks and checkpoints join acceptance.
	if err = i.validatePage(claimed, page); err != nil {
		result.ErrorCode = "invalid_snapshot"
		return result, i.finishFailure(ctx, r, token, result.ErrorCode, err)
	}
	result.ScanID = page.ScanID
	err = i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		var state ingressState
		if e := getJSON(tx, ns, "state", &state); e != nil {
			return e
		}
		now := i.now()
		if state.LeaseToken != token || !now.Before(state.LeaseUntil) {
			return ErrScanLeaseLost
		}
		prefix := "stage/" + identity(page.ScanID) + "/"
		staged, e := tx.List(ns, prefix)
		if e != nil {
			return e
		}
		versions := map[string]string{}
		for _, entry := range staged {
			var o extension.Observation
			if e = decodeStagedObservation(entry.Value, &o); e != nil {
				return e
			}
			versions[o.ID] = o.Version
		}
		for _, o := range page.Observations {
			if previous, ok := versions[o.ID]; ok && previous != o.Version {
				return extension.ErrConflict
			}
			versions[o.ID] = o.Version
			if e = validateObservation(tx, r, o); e != nil {
				return e
			}
			key := prefix + identity(o.ID)
			var previous extension.Observation
			e = getJSON(tx, ns, key, &previous)
			if e == nil {
				if previous.Version != o.Version {
					return extension.ErrConflict
				}
			} else if !errors.Is(e, storage.ErrNotFound) {
				return e
			}
			if e = putJSON(tx, ns, key, o); e != nil {
				return e
			}
		}
		if len(versions) > i.options.MaxScanItems {
			return scanError("scan_item_limit")
		}
		stagedEntries, listErr := tx.List(ns, prefix)
		if listErr != nil {
			return listErr
		}
		stagedBytes := 0
		for _, entry := range stagedEntries {
			stagedBytes += len(entry.Value)
		}
		if stagedBytes > i.options.MaxScanBytes {
			return scanError("scan_byte_limit")
		}

		state.ScanID = page.ScanID
		state.Cursor = page.NextCursor
		state.PageCheckpoint = page.Checkpoint
		state.LeaseToken = ""
		state.LeaseUntil = time.Time{}
		state.LastError = ""
		state.Failures = 0
		if page.NextCursor != "" {
			state.SeenCursors = append(state.SeenCursors, page.NextCursor)
		}
		state.NextDue = now
		if page.Complete {
			events, e := i.publishGeneration(tx, r, page.ScanID)
			if e != nil {
				return e
			}
			result.Accepted = events
			result.Complete = true
			state.Checkpoint = page.Checkpoint
			state.LastComplete = now
			state.NextDue = now.Add(r.interval)
			if state.Requests != state.GenerationRequests {
				// An operator request arrived during this generation.
				state.NextDue = now
			}
			state.ScanID = ""
			state.Cursor = ""
			state.PageCheckpoint = ""
			state.SeenCursors = nil
		} else if page.NextCursor == "" {
			// An exhausted partial generation cannot certify absence. Keep its canonical
			// identity ownership but discard only staging; begin afresh after backoff.
			if e = deletePrefix(tx, ns, prefix); e != nil {
				return e
			}
			result.Partial = true
			state.ScanID = ""
			state.Cursor = ""
			state.PageCheckpoint = ""
			state.SeenCursors = nil
			state.LastError = "partial_snapshot"
			state.NextDue = now.Add(retryDelay(1, r.interval))
			state.Failures = 1
		}
		return putJSON(tx, ns, "state", state)
	})
	if err != nil {
		result.Accepted = nil
		result.Complete = false
		result.Partial = false
		result.ErrorCode = "invalid_snapshot"
		if errors.Is(err, ErrScanLeaseLost) {
			result.ErrorCode = "scan_lease_lost"
			return result, err
		}
		return result, i.finishFailure(ctx, r, token, result.ErrorCode, err)
	}
	return result, nil
}
func (i *Ingress) validatePage(state ingressState, page extension.Snapshot) error {
	encoded, err := json.Marshal(page)
	if err != nil || len(encoded) > i.options.MaxPageBytes || len(page.ScanID) > 4096 || len(page.NextCursor) > 4096 || len(page.Checkpoint) > 4096 {
		return extension.ErrInvalid
	}
	if page.ScanID == "" || state.ScanID != "" && state.ScanID != page.ScanID || page.Complete && page.NextCursor != "" || len(page.Observations) > i.options.MaxPageItems {
		return extension.ErrInvalid
	}
	if len(state.SeenCursors) >= i.options.MaxScanPages {
		return scanError("scan_page_limit")
	}
	if page.NextCursor != "" {
		if page.NextCursor == state.Cursor {
			return extension.ErrInvalid
		}
		for _, cursor := range state.SeenCursors {
			if cursor == page.NextCursor {
				return extension.ErrInvalid
			}
		}
	}
	return nil
}
func (i *Ingress) finishFailure(ctx context.Context, r ingressRegistration, token, code string, cause error) error {
	err := i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		var state ingressState
		ns := sourceNamespace(r.source)
		if e := getJSON(tx, ns, "state", &state); e != nil {
			return e
		}
		now := i.now()
		if state.LeaseToken != token || !now.Before(state.LeaseUntil) {
			return ErrScanLeaseLost
		}
		state.LeaseToken = ""
		state.LeaseUntil = time.Time{}
		state.LastError = code
		if state.Failures < 32 {
			state.Failures++
		}
		state.NextDue = now.Add(retryDelay(state.Failures, r.interval))
		return putJSON(tx, ns, "state", state)
	})
	if err != nil {
		return err
	}
	return cause
}
func retryDelay(failures uint32, interval time.Duration) time.Duration {
	delay := 5 * time.Second
	for failures > 1 && delay < interval {
		delay *= 2
		failures--
	}
	if delay > interval {
		delay = interval
	}
	return delay
}
func deletePrefix(tx storage.Tx, ns, prefix string) error {
	entries, err := tx.List(ns, prefix)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = tx.Delete(ns, entry.Key); err != nil {
			return err
		}
	}
	return nil
}

type observationHead struct {
	ID, Version    string
	CorrelationKey *domain.CorrelationKey
	WorkID         domain.ID
}

func (i *Ingress) publishGeneration(tx storage.Tx, r ingressRegistration, scanID string) ([]domain.AcceptedEvent, error) {
	ns := sourceNamespace(r.source)
	prefix := "stage/" + identity(scanID) + "/"
	staged, err := tx.List(ns, prefix)
	if err != nil {
		return nil, err
	}
	current, err := tx.List(ns, "current/")
	if err != nil {
		return nil, err
	}
	old := map[string]observationHead{}
	for _, entry := range current {
		var head observationHead
		if err = json.Unmarshal(entry.Value, &head); err != nil {
			return nil, err
		}
		old[head.ID] = head
	}
	observations := make([]extension.Observation, 0, len(staged))
	for _, entry := range staged {
		var o extension.Observation
		if err = decodeStagedObservation(entry.Value, &o); err != nil {
			return nil, err
		}
		observations = append(observations, o)
	}
	sort.Slice(observations, func(a, b int) bool { return observations[a].ID < observations[b].ID })
	events := []domain.AcceptedEvent{}
	for _, o := range observations {
		previous, exists := old[o.ID]
		if !exists || previous.Version != o.Version {
			accepted, _, e := AcceptInTx(tx, r.source.ScopeID, o.Event)
			if e != nil {
				return nil, e
			}
			events = append(events, accepted)
		}
		delete(old, o.ID)
		if err = putJSON(tx, ns, "current/"+identity(o.ID), observationHead{ID: o.ID, Version: o.Version, CorrelationKey: o.Event.CorrelationKey, WorkID: o.Event.WorkID}); err != nil {
			return nil, err
		}
	}
	absent := make([]string, 0, len(old))
	for id := range old {
		absent = append(absent, id)
	}
	sort.Strings(absent)
	for _, id := range absent {
		head := old[id]
		data, err := json.Marshal(struct {
			Source      string `json:"source"`
			Item        string `json:"item"`
			LastVersion string `json:"last_version"`
		}{r.source.Namespace, id, head.Version})
		if err != nil {
			return nil, err
		}
		delivery := identity(r.source.Namespace, scanID, id)
		event := domain.Event{ID: domain.ID("ingress-absent-" + delivery), Type: "core.ingress.absent", Source: "core.ingress", DeliveryID: "core.absent/" + delivery, CorrelationKey: head.CorrelationKey, WorkID: head.WorkID, OccurredAt: i.now(), ReceivedAt: i.now(), Data: domain.Payload{Schema: "core.ingress.absent.v1", Data: data}}
		accepted, _, err := AcceptInTx(tx, r.source.ScopeID, event)
		if err != nil {
			return nil, err
		}
		events = append(events, accepted)
		if err = tx.Delete(ns, "current/"+identity(id)); err != nil {
			return nil, err
		}
	}
	if err = deletePrefix(tx, ns, prefix); err != nil {
		return nil, err
	}
	return events, nil
}

// ReconcileDue performs at most limit external page calls in stable source-name
// order. Each source appears at most once per call; failures cannot block peers.
func (i *Ingress) ReconcileDue(ctx context.Context, limit int) ([]ReconcileResult, error) {
	if limit < 1 || limit > 200 {
		return nil, storage.ErrInvalid
	}
	statuses, err := i.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	var last string
	err = i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		e := getJSON(tx, "core.ingress.scheduler", "last_source", &last)
		if errors.Is(e, storage.ErrNotFound) {
			return nil
		}
		return e
	})
	if err != nil {
		return nil, err
	}
	// Rotate a durable lexical ring so one many-page source cannot starve peers
	// even when deterministic clocks do not advance between bounded polls.
	pivot := sort.Search(len(statuses), func(n int) bool { return statuses[n].Name > last })
	statuses = append(statuses[pivot:], statuses[:pivot]...)
	out := []ReconcileResult{}
	for _, status := range statuses {
		if len(out) >= limit {
			break
		}
		if i.now().Before(status.NextDue) {
			continue
		}
		result, e := i.reconcile(ctx, status.Name, true)
		if errors.Is(e, ErrIngressBusy) {
			continue
		}
		if e != nil && !errors.Is(e, ErrIngressProvider) && !errors.Is(e, extension.ErrInvalid) && !errors.Is(e, extension.ErrConflict) && !errors.Is(e, ErrScanLeaseLost) && !errors.Is(e, storage.ErrInvalid) && !errors.Is(e, storage.ErrConflict) && !errors.Is(e, storage.ErrNotFound) {
			return out, e
		}
		if e != nil && result.ErrorCode == "" {
			result.ErrorCode = "reconcile_failed"
		}
		out = append(out, result)
		if err = i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error { return putJSON(tx, "core.ingress.scheduler", "last_source", status.Name) }); err != nil {
			return out, err
		}
	}
	return out, nil
}

// RestartScan discards only resumable staging after a provider invalidates its
// snapshot/cursor. Accepted events, identity ownership and the last complete
// checkpoint remain intact. It cannot steal a live external call's lease.
func (i *Ingress) RestartScan(ctx context.Context, name, expectedScanID string) error {
	r, err := i.registration(name)
	if err != nil {
		return err
	}
	return i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		ns := sourceNamespace(r.source)
		var state ingressState
		if err := getJSON(tx, ns, "state", &state); err != nil {
			return err
		}
		now := i.now()
		if state.ScanID != expectedScanID {
			return storage.ErrConflict
		}
		if state.LeaseToken != "" && now.Before(state.LeaseUntil) {
			return ErrIngressBusy
		}
		if state.ScanID != "" {
			if err := deletePrefix(tx, ns, "stage/"+identity(state.ScanID)+"/"); err != nil {
				return err
			}
		}
		state.ScanID = ""
		state.Cursor = ""
		state.PageCheckpoint = ""
		state.SeenCursors = nil
		state.LeaseToken = ""
		state.LeaseUntil = time.Time{}
		state.NextDue = now
		state.LastError = ""
		state.Failures = 0
		return putJSON(tx, ns, "state", state)
	})
}
