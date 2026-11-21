package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Admission (#706) caps the open Work a scope has in flight, where
// concurrency caps the attempts running at once. It never refuses an
// Event: a route creates its Work as always, and the Work owns its keys
// and records its trigger. Work admitted before its first attempt counts
// against the admission limit of its scope and every ancestor's
// (SchedulerOptions.AdmissionLimits) until it finishes, whatever state it
// is in: ready, running or waiting (Work handed back to a reviewer still
// holds its place). Work beyond a limit is held with a hold of kind
// "admission", recorded by a core.admission.held Event in its history.
// When admitted Work finishes, or a limit is raised, the admission pass
// (Scheduler.Admit) releases the oldest held Work first, with a
// core.admission.released Event, so the platform does the waiting. An
// operator's release admits held Work at once, past the limit.
//
// Child Work is part of the Work that created it and standing Work
// (WorkKindPolicy.Standing) is an objective that never finishes, so
// neither is subject to admission nor counts against a limit. Work that
// already had an attempt (before admission was enforced) is admitted as
// it is.

const (
	admittedNamespace = "core.admission.admitted"
	// HoldKindAdmission marks a hold admission placed on Work beyond its
	// scope's admission limit.
	HoldKindAdmission = "admission"
)

// ErrNotAdmitted is a start refused because the Work has not been admitted
// yet; the admission pass admits or holds it. It matches ErrNotEligible.
var ErrNotAdmitted = fmt.Errorf("%w: not admitted", ErrNotEligible)

// admitted is the record of Work admission let in: when, and by which
// Event (a release's) when it was held first.
type admitted struct {
	At      time.Time `json:"at"`
	EventID domain.ID `json:"event_id,omitempty"`
}

// subjectToAdmission reports whether Work is admitted at all: root Work
// that is not standing, while some scope has an admission limit.
func (s *Scheduler) subjectToAdmission(w domain.Work) bool {
	return len(s.options.AdmissionLimits) > 0 && w.ParentID == "" && !s.options.WorkKinds[w.Kind].Standing
}

func admittedInTx(tx storage.Tx, work domain.ID) (bool, error) {
	_, err := tx.Get(admittedNamespace, identity(string(work)))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func admitInTx(tx storage.Tx, now time.Time, work, event domain.ID) error {
	return putJSON(tx, admittedNamespace, identity(string(work)), admitted{At: now.UTC(), EventID: event})
}

// checkAdmittedInTx refuses the first attempt of Work admission has not
// let in. Work that has had an attempt is past admission.
func (s *Scheduler) checkAdmittedInTx(tx storage.Tx, w domain.Work, attempts int) error {
	if attempts > 0 || !s.subjectToAdmission(w) {
		return nil
	}
	ok, err := admittedInTx(tx, w.ID)
	if err != nil || ok {
		return err
	}
	return ErrNotAdmitted
}

// AdmissionResult is what one admission pass did.
type AdmissionResult struct {
	Admitted, Held, Released []domain.ID
}

// admissionPlan is what one admission pass would change.
type admissionPlan struct {
	stale               []string
	legacy              []domain.ID
	admit, hold         []domain.Work
	release             []domain.Work
	holdScope           []domain.ID
	holdLimit, holdOpen []int
}

func (p admissionPlan) empty() bool {
	return len(p.stale)+len(p.legacy)+len(p.admit)+len(p.hold)+len(p.release) == 0
}

// Admit is the admission pass. In creation order, it admits each open
// root Work not yet admitted while its scope and every ancestor are under
// their limits, and holds the rest; held Work is released, oldest first,
// as places free. It forgets finished Work's admission. The daemon runs it
// before it starts ready Work. It plans in a read-only view and writes only
// when the plan changes something, planning again in the write.
func (s *Scheduler) Admit(ctx context.Context) (out AdmissionResult, err error) {
	var plan admissionPlan
	if err = storage.View(ctx, s.store, s.now(), func(tx storage.Tx) (e error) { plan, e = s.planAdmission(tx); return }); err != nil || plan.empty() {
		return out, err
	}
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		out = AdmissionResult{}
		now := s.now()
		plan, e := s.planAdmission(tx)
		if e != nil {
			return e
		}
		for _, key := range plan.stale {
			if e = tx.Delete(admittedNamespace, key); e != nil {
				return e
			}
		}
		// Work admitted before admission was enforced is recorded, not news.
		for _, id := range plan.legacy {
			if e = admitInTx(tx, now, id, ""); e != nil {
				return e
			}
		}
		for _, w := range plan.admit {
			if e = admitInTx(tx, now, w.ID, ""); e != nil {
				return e
			}
			out.Admitted = append(out.Admitted, w.ID)
		}
		for i, w := range plan.hold {
			if e = holdForAdmissionInTx(tx, now, w, plan.holdScope[i], plan.holdLimit[i], plan.holdOpen[i]); e != nil {
				return e
			}
			out.Held = append(out.Held, w.ID)
		}
		for _, w := range plan.release {
			if e = releaseAdmissionInTx(tx, now, w); e != nil {
				return e
			}
			out.Released = append(out.Released, w.ID)
		}
		return nil
	})
	return out, err
}

// planAdmission reads, once each, the open Work, the admission records and
// the holds, and works out the pass. Only Work with neither a record nor a
// hold (new Work, or Work from before admission was enforced) has its
// attempts listed, so held Work costs no reads beyond the listings.
func (s *Scheduler) planAdmission(tx storage.Tx) (p admissionPlan, err error) {
	works, err := tx.LiveWorks()
	if err != nil {
		return p, err
	}
	live := make(map[string]bool, len(works))
	for _, w := range works {
		live[identity(string(w.ID))] = true
	}
	rows, err := tx.List(admittedNamespace, "")
	if err != nil {
		return p, err
	}
	admittedSet := make(map[string]bool, len(rows))
	for _, row := range rows {
		if live[row.Key] {
			admittedSet[row.Key] = true
		} else {
			p.stale = append(p.stale, row.Key)
		}
	}
	holdRows, err := tx.List(holdNamespace, "")
	if err != nil {
		return p, err
	}
	holds := make(map[string]Hold, len(holdRows))
	for _, row := range holdRows {
		var h Hold
		if err = json.Unmarshal(row.Value, &h); err != nil {
			return p, err
		}
		holds[row.Key] = h
	}
	if len(s.options.AdmissionLimits) == 0 {
		// With no limit left, nothing stays held for one.
		for _, w := range works {
			if h, ok := holds[identity(string(w.ID))]; ok && h.Kind == HoldKindAdmission {
				p.release = append(p.release, w)
			}
		}
		return p, nil
	}
	chains := map[domain.ID][]domain.ID{}
	chainOf := func(scope domain.ID) ([]domain.ID, error) {
		if c, ok := chains[scope]; ok {
			return c, nil
		}
		c, err := scopeChain(tx, scope)
		chains[scope] = c
		return c, err
	}
	counts := map[domain.ID]int{}
	count := func(w domain.Work) error {
		chain, err := chainOf(w.ScopeID)
		for _, scope := range chain {
			counts[scope]++
		}
		return err
	}
	var waiting []domain.Work
	for _, w := range works {
		if !s.subjectToAdmission(w) {
			continue
		}
		key := identity(string(w.ID))
		if admittedSet[key] {
			if err = count(w); err != nil {
				return p, err
			}
			continue
		}
		h, held := holds[key]
		switch {
		case held && h.Kind == HoldKindAdmission:
			waiting = append(waiting, w)
		case held:
			// Work another hold holds waits for that hold first.
		default:
			// Work that has had an attempt was admitted before admission
			// was enforced; it is recorded once and counted.
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return p, err
			}
			if len(xs) > 0 {
				p.legacy = append(p.legacy, w.ID)
				if err = count(w); err != nil {
					return p, err
				}
				continue
			}
			waiting = append(waiting, w)
		}
	}
	// The oldest Work is let in first, in creation order.
	sort.SliceStable(waiting, func(i, j int) bool {
		a, b := waiting[i], waiting[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	for _, w := range waiting {
		chain, err := chainOf(w.ScopeID)
		if err != nil {
			return p, err
		}
		full, limit := domain.ID(""), 0
		for _, scope := range chain {
			if l := s.options.AdmissionLimits[scope]; l > 0 && counts[scope] >= l {
				full, limit = scope, l
				break
			}
		}
		held := holds[identity(string(w.ID))].Kind == HoldKindAdmission
		if full != "" {
			if !held {
				p.hold = append(p.hold, w)
				p.holdScope = append(p.holdScope, full)
				p.holdLimit = append(p.holdLimit, limit)
				p.holdOpen = append(p.holdOpen, counts[full])
			}
			continue
		}
		for _, scope := range chain {
			counts[scope]++
		}
		if held {
			p.release = append(p.release, w)
		} else {
			p.admit = append(p.admit, w)
		}
	}
	return p, nil
}

// AdmissionNotice is the data of a core.admission.held or
// core.admission.released Event (schema core.admission.v1).
type AdmissionNotice struct {
	Reason  string    `json:"reason"`
	ScopeID domain.ID `json:"scope_id,omitempty"`
	Limit   int       `json:"limit,omitempty"`
	Open    int       `json:"open,omitempty"`
}

// admissionEventInTx accepts a core.admission.* control Event addressed to
// Work, so its history records the hold or the release.
func admissionEventInTx(tx storage.Tx, now time.Time, w domain.Work, typ string, n AdmissionNotice) (domain.ID, error) {
	data, _ := json.Marshal(n)
	delivery := strings.TrimPrefix(typ, "core.admission.") + "/" + identity(string(w.ID), now.UTC().Format(time.RFC3339Nano))
	e := domain.Event{ID: domain.ID("admission-" + delivery), Type: typ, Source: "core.admission", DeliveryID: delivery, WorkID: w.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.admission.v1", Data: data}}
	accepted, _, err := AcceptInTx(tx, w.ScopeID, e)
	if err != nil {
		return "", err
	}
	return accepted.Event.ID, tx.MarkRouted(accepted.Event.ID, "", "control")
}

// holdForAdmissionInTx holds Work beyond the admission limit of scope.
func holdForAdmissionInTx(tx storage.Tx, now time.Time, w domain.Work, scope domain.ID, limit, open int) error {
	reason := fmt.Sprintf("Waiting for a place: scope %s has %d open Work at its admission limit of %d. The oldest held Work starts as open Work finishes; release the hold to admit it now.", scope, open, limit)
	event, err := admissionEventInTx(tx, now, w, domain.EventAdmissionHeld, AdmissionNotice{Reason: reason, ScopeID: scope, Limit: limit, Open: open})
	if err != nil {
		return err
	}
	if err = putJSON(tx, holdNamespace, identity(string(w.ID)), Hold{EventID: event, Reason: reason, At: now.UTC(), Kind: HoldKindAdmission}); err != nil {
		return err
	}
	// Advance the revision, as an operator hold does, so views see it.
	return tx.PutWork(w, w.Revision)
}

// releaseAdmissionInTx admits held Work now that its scope has a place.
func releaseAdmissionInTx(tx storage.Tx, now time.Time, w domain.Work) error {
	event, err := admissionEventInTx(tx, now, w, domain.EventAdmissionReleased, AdmissionNotice{Reason: "A place opened in its scope"})
	if err != nil {
		return err
	}
	if err = tx.Delete(holdNamespace, identity(string(w.ID))); err != nil {
		return err
	}
	if err = admitInTx(tx, now, w.ID, event); err != nil {
		return err
	}
	return tx.PutWork(w, w.Revision)
}
