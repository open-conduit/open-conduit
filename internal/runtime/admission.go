package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// Admit is the admission pass. In creation order, it admits each open
// root Work not yet admitted while its scope and every ancestor are under
// their limits, and holds the rest; held Work is released, oldest first,
// as places free. It forgets finished Work's admission. The daemon runs it
// before it starts ready Work.
func (s *Scheduler) Admit(ctx context.Context) (out AdmissionResult, err error) {
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		out = AdmissionResult{}
		now := s.now()
		works, e := tx.LiveWorks()
		if e != nil {
			return e
		}
		live := make(map[string]bool, len(works))
		for _, w := range works {
			live[identity(string(w.ID))] = true
		}
		rows, e := tx.List(admittedNamespace, "")
		if e != nil {
			return e
		}
		for _, row := range rows {
			if !live[row.Key] {
				if e = tx.Delete(admittedNamespace, row.Key); e != nil {
					return e
				}
			}
		}
		if len(s.options.AdmissionLimits) == 0 {
			// With no limit left, nothing stays held for one.
			for _, w := range works {
				h, held, e := HoldInTx(tx, w.ID)
				if e != nil {
					return e
				}
				if held && h.Kind == HoldKindAdmission {
					if e = releaseAdmissionInTx(tx, now, w); e != nil {
						return e
					}
					out.Released = append(out.Released, w.ID)
				}
			}
			return nil
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
		type candidate struct {
			work domain.Work
			hold *Hold
		}
		var waiting []candidate
		for _, w := range works {
			if !s.subjectToAdmission(w) {
				continue
			}
			ok, e := admittedInTx(tx, w.ID)
			if e != nil {
				return e
			}
			if !ok {
				// Work that has had an attempt was admitted before
				// admission was enforced.
				xs, e := tx.ListExecutions(w.ID)
				if e != nil {
					return e
				}
				if len(xs) > 0 {
					if e = admitInTx(tx, now, w.ID, ""); e != nil {
						return e
					}
					ok = true
				}
			}
			if ok {
				chain, e := chainOf(w.ScopeID)
				if e != nil {
					return e
				}
				for _, scope := range chain {
					counts[scope]++
				}
				continue
			}
			h, held, e := HoldInTx(tx, w.ID)
			if e != nil {
				return e
			}
			switch {
			case !held:
				waiting = append(waiting, candidate{work: w})
			case h.Kind == HoldKindAdmission:
				waiting = append(waiting, candidate{work: w, hold: &h})
			}
			// Work another hold holds waits for that hold first.
		}
		// The oldest Work is let in first: held Work by when it was held,
		// which is its creation order too.
		sort.SliceStable(waiting, func(i, j int) bool {
			a, b := waiting[i].work, waiting[j].work
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.ID < b.ID
		})
		for _, c := range waiting {
			chain, e := chainOf(c.work.ScopeID)
			if e != nil {
				return e
			}
			full := domain.ID("")
			limit := 0
			for _, scope := range chain {
				if l := s.options.AdmissionLimits[scope]; l > 0 && counts[scope] >= l {
					full, limit = scope, l
					break
				}
			}
			if full != "" {
				if c.hold == nil {
					if e = holdForAdmissionInTx(tx, now, c.work, full, limit, counts[full]); e != nil {
						return e
					}
					out.Held = append(out.Held, c.work.ID)
				}
				continue
			}
			for _, scope := range chain {
				counts[scope]++
			}
			if c.hold == nil {
				if e = admitInTx(tx, now, c.work.ID, ""); e != nil {
					return e
				}
				out.Admitted = append(out.Admitted, c.work.ID)
				continue
			}
			if e = releaseAdmissionInTx(tx, now, c.work); e != nil {
				return e
			}
			out.Released = append(out.Released, c.work.ID)
		}
		return nil
	})
	return out, err
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
func admissionEventInTx(tx storage.Tx, now time.Time, w domain.Work, kind string, n AdmissionNotice) (domain.ID, error) {
	data, _ := json.Marshal(n)
	delivery := kind + "/" + identity(string(w.ID), now.UTC().Format(time.RFC3339Nano))
	e := domain.Event{ID: domain.ID("admission-" + delivery), Type: "core.admission." + kind, Source: "core.admission", DeliveryID: delivery, WorkID: w.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.admission.v1", Data: data}}
	accepted, _, err := AcceptInTx(tx, w.ScopeID, e)
	if err != nil {
		return "", err
	}
	return accepted.Event.ID, tx.MarkRouted(accepted.Event.ID, "", "control")
}

// holdForAdmissionInTx holds Work beyond the admission limit of scope.
func holdForAdmissionInTx(tx storage.Tx, now time.Time, w domain.Work, scope domain.ID, limit, open int) error {
	reason := fmt.Sprintf("Waiting for a place: scope %s has %d open Work at its admission limit of %d. The oldest held Work starts as open Work finishes; release the hold to admit it now.", scope, open, limit)
	event, err := admissionEventInTx(tx, now, w, "held", AdmissionNotice{Reason: reason, ScopeID: scope, Limit: limit, Open: open})
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
	event, err := admissionEventInTx(tx, now, w, "released", AdmissionNotice{Reason: "A place opened in its scope"})
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
