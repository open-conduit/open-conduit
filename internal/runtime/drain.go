package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// A drain stops new attempts so a daemon can be moved: attempts already
// running finish or park, and everything else (Events, timers, Waits, input)
// keeps being recorded. It is durable, so a restart of the same daemon can't
// quietly start taking work again.

const (
	drainNamespace = "core.drain"
	drainKey       = "state"
)

// ErrDraining refuses a new attempt while the daemon is draining.
var ErrDraining = errors.New("daemon is draining")

// Drain is a drain in force. Origin names the installation's place (host and
// state directory) when it began, so a daemon started somewhere else can tell
// it has been moved.
type Drain struct {
	Since  time.Time `json:"since"`
	Origin string    `json:"origin"`
	Reason string    `json:"reason,omitempty"`
}

// DrainInTx reads the drain in force, if any.
func DrainInTx(tx storage.Tx) (Drain, bool, error) {
	var d Drain
	err := getJSON(tx, drainNamespace, drainKey, &d)
	if errors.Is(err, storage.ErrNotFound) {
		return Drain{}, false, nil
	}
	return d, err == nil, err
}

func refuseWhileDraining(tx storage.Tx) error {
	if _, on, err := DrainInTx(tx); err != nil {
		return err
	} else if on {
		return ErrDraining
	}
	return nil
}

// BeginDrainInTx starts a drain, keeping the original one if already draining.
func BeginDrainInTx(tx storage.Tx, now time.Time, origin, reason string) (Drain, error) {
	if d, on, err := DrainInTx(tx); err != nil || on {
		return d, err
	}
	d := Drain{Since: now.UTC(), Origin: origin, Reason: reason}
	data, err := json.Marshal(d)
	if err != nil {
		return d, err
	}
	return d, tx.Put(drainNamespace, drainKey, data)
}

// EndDrainInTx ends a drain, reporting whether one was in force.
func EndDrainInTx(tx storage.Tx) (bool, error) {
	_, on, err := DrainInTx(tx)
	if err != nil || !on {
		return false, err
	}
	return true, tx.Delete(drainNamespace, drainKey)
}

// EndMovedDrain ends a drain begun at another origin: the daemon has been
// moved, which is what the drain was for. The ledger records it. A drain from
// this origin stays. It reports the drain it ended, if any.
func EndMovedDrain(ctx context.Context, store storage.Store, now time.Time, origin string) (*Drain, error) {
	var ended *Drain
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		ended = nil
		d, on, err := DrainInTx(tx)
		if err != nil || !on || d.Origin == origin {
			return err
		}
		if err = tx.Delete(drainNamespace, drainKey); err != nil {
			return err
		}
		ended = &d
		_, err = tx.AppendAudit(domain.AuditRecord{Actor: "system:daemon", Action: "core.drain.ended_by_move", Outcome: domain.AuditOK, Detail: map[string]string{"from": d.Origin, "to": origin}})
		return err
	})
	return ended, err
}

// InFlight is whether an attempt holds a process or is about to: a parked
// attempt holds nothing and resumes on its wake wherever the state is.
func InFlight(x domain.Execution) bool {
	return !x.State.Terminal() && x.State != domain.ExecutionParked
}

// DrainScope is one scope's attempts and Waits during a drain.
type DrainScope struct {
	ID          string   `json:"id"`
	InFlight    int      `json:"in_flight"`
	Parked      int      `json:"parked"`
	ActiveWaits int      `json:"active_waits"`
	Blocking    []string `json:"blocking_work,omitempty"`
}

// DrainStatus is what a drain has left to do.
type DrainStatus struct {
	Draining bool       `json:"draining"`
	Since    *time.Time `json:"since,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	// Safe is whether the state can be moved now: draining, with nothing
	// that needs this process.
	Safe bool `json:"safe_to_move"`
	// Blocking is what has to settle first, and Carried what is durable and
	// is picked up by the daemon that starts on the moved state.
	Blocking []QuiescenceReason `json:"blocking"`
	Carried  []QuiescenceReason `json:"carried"`
	Scopes   []DrainScope       `json:"scopes"`
}

// drainBlocks are the quiescence reasons that need this process: an attempt
// with a process, or a settlement only it can make.
var drainBlocks = map[string]bool{
	QuiescentLiveExecution:   true,
	QuiescentUnconfirmedStop: true,
	QuiescentPendingControl:  true,
	QuiescentUnsettledUsage:  true,
}

// DrainStatusInTx reports the drain from the reads quiescence and `state
// check` use.
func DrainStatusInTx(tx storage.Tx) (DrainStatus, error) {
	out := DrainStatus{Blocking: []QuiescenceReason{}, Carried: []QuiescenceReason{}, Scopes: []DrainScope{}}
	d, on, err := DrainInTx(tx)
	if err != nil {
		return out, err
	}
	if on {
		since := d.Since
		out.Draining, out.Since, out.Reason = true, &since, d.Reason
	}
	reasons, _, err := QuiescenceInTx(tx)
	if err != nil {
		return out, err
	}
	for _, r := range reasons {
		if drainBlocks[r.Code] {
			out.Blocking = append(out.Blocking, r)
		} else {
			out.Carried = append(out.Carried, r)
		}
	}
	scopes := map[domain.ID]*DrainScope{}
	scope := func(id domain.ID) *DrainScope {
		if scopes[id] == nil {
			scopes[id] = &DrainScope{ID: string(id)}
		}
		return scopes[id]
	}
	live, err := tx.LiveExecutions()
	if err != nil {
		return out, err
	}
	workScope := map[domain.ID]domain.ID{}
	scopeOf := func(id domain.ID) (domain.ID, error) {
		if s, ok := workScope[id]; ok {
			return s, nil
		}
		w, e := tx.Work(id)
		if e != nil {
			return "", e
		}
		workScope[id] = w.ScopeID
		return w.ScopeID, nil
	}
	for _, x := range live {
		id, e := scopeOf(x.WorkID)
		if e != nil {
			return out, e
		}
		s := scope(id)
		if InFlight(x) {
			s.InFlight++
			s.Blocking = append(s.Blocking, string(x.WorkID))
		} else {
			s.Parked++
		}
	}
	waits, err := tx.ActiveWaits()
	if err != nil {
		return out, err
	}
	for _, w := range waits {
		id, e := scopeOf(w.WorkID)
		if e != nil {
			return out, e
		}
		scope(id).ActiveWaits++
	}
	for _, s := range scopes {
		sort.Strings(s.Blocking)
		out.Scopes = append(out.Scopes, *s)
	}
	sort.Slice(out.Scopes, func(i, j int) bool { return out.Scopes[i].ID < out.Scopes[j].ID })
	out.Safe = out.Draining && len(out.Blocking) == 0
	return out, nil
}
