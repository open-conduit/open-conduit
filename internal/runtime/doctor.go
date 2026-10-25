package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// Check statuses. Info carries declared limitations that are not faults.
const (
	CheckOK   = "ok"
	CheckInfo = "info"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// Check is one safe health/doctor finding. Code is stable; Detail and Items
// carry identifiers and counts only, never payloads, references or secrets.
type Check struct {
	Name, Status, Code, Detail string
	Items                      []string
}

// DoctorOptions selects generic runtime diagnostics. Exporter- or host-
// specific checks are contributed by their extensions through management.
type DoctorOptions struct {
	Ingress *Ingress
	// LagFactor marks a source lagging when its last complete scan is older
	// than LagFactor reconcile intervals (default 2).
	LagFactor int
	// Limit bounds identifiers listed per check (default 20).
	Limit int
}

const doctorNamespace = "core.doctor"

// Diagnose runs generic runtime checks: storage read/write, reconcile lag,
// lease recovery backlog, unconfirmed resource holds, retention backlog and
// declared non-re-derivable delivery families. It never mutates domain state.
func Diagnose(ctx context.Context, store storage.Store, now time.Time, o DoctorOptions) []Check {
	if o.LagFactor <= 0 {
		o.LagFactor = 2
	}
	if o.Limit <= 0 {
		o.Limit = 20
	}
	limit := func(items []string) []string {
		sort.Strings(items)
		if len(items) > o.Limit {
			return append(items[:o.Limit:o.Limit], fmt.Sprintf("(+%d more)", len(items)-o.Limit))
		}
		return items
	}
	var checks []Check
	storageCheck := Check{Name: "storage", Status: CheckOK, Code: "ready", Detail: "coordination storage read/write transaction committed"}
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if _, err := tx.ListScopes(); err != nil {
			return err
		}
		if err := tx.Put(doctorNamespace, "probe", []byte(`"`+now.UTC().Format(time.RFC3339Nano)+`"`)); err != nil {
			return err
		}
		return tx.Delete(doctorNamespace, "probe")
	}); err != nil {
		storageCheck = Check{Name: "storage", Status: CheckFail, Code: "storage_unavailable", Detail: "coordination storage transaction failed"}
		return append(checks, storageCheck)
	}
	checks = append(checks, storageCheck)

	var expired, holds, retention []string
	parked := 0
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions("")
		if err != nil {
			return err
		}
		for _, x := range xs {
			switch {
			case x.State == domain.ExecutionRunning && !now.Before(x.Lease.ExpiresAt):
				expired = append(expired, string(x.ID))
			case x.State == domain.ExecutionParked:
				parked++
			case x.State.Terminal():
				state, err := attemptState(tx, x.ID)
				if err == nil && !state.Excluded {
					holds = append(holds, string(x.ID))
				} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
					return err
				}
			}
		}
		rows, err := tx.List(retentionNamespace, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var r retentionRecord
			if err = getJSON(tx, retentionNamespace, row.Key, &r); err != nil {
				return err
			}
			if !r.ArtifactsReleased || !r.Forgotten {
				retention = append(retention, string(r.ExecutionID))
			}
		}
		return nil
	})
	if err != nil {
		return append(checks, Check{Name: "executions", Status: CheckFail, Code: "storage_unavailable", Detail: "execution history could not be read"})
	}
	if len(expired) > 0 {
		checks = append(checks, Check{Name: "lease_recovery", Status: CheckWarn, Code: "expired_leases", Detail: fmt.Sprintf("%d running attempt(s) hold expired leases and await recovery", len(expired)), Items: limit(expired)})
	} else {
		checks = append(checks, Check{Name: "lease_recovery", Status: CheckOK, Code: "current", Detail: fmt.Sprintf("no expired leases; %d parked attempt(s)", parked)})
	}
	if len(holds) > 0 {
		checks = append(checks, Check{Name: "resource_holds", Status: CheckWarn, Code: "unconfirmed_stop", Detail: fmt.Sprintf("%d finished attempt(s) still hold capacity until a verified stop is confirmed", len(holds)), Items: limit(holds)})
	} else {
		checks = append(checks, Check{Name: "resource_holds", Status: CheckOK, Code: "released", Detail: "every finished attempt has confirmed resource exclusion"})
	}
	if len(retention) > 0 {
		checks = append(checks, Check{Name: "retention", Status: CheckWarn, Code: "release_pending", Detail: fmt.Sprintf("%d expired attempt(s) still have content or executor state to release", len(retention)), Items: limit(retention)})
	} else {
		checks = append(checks, Check{Name: "retention", Status: CheckOK, Code: "current", Detail: "no pending retention releases"})
	}

	if o.Ingress == nil {
		return append(checks, Check{Name: "reconcile_lag", Status: CheckInfo, Code: "no_ingress", Detail: "no ingress sources are registered"})
	}
	statuses, err := o.Ingress.Inspect(ctx)
	if err != nil {
		return append(checks, Check{Name: "reconcile_lag", Status: CheckFail, Code: "storage_unavailable", Detail: "ingress state could not be read"})
	}
	var lagging, failing, limited []string
	for _, s := range statuses {
		switch {
		case s.LastError != "":
			failing = append(failing, fmt.Sprintf("%s: %s (failures %d, next %s)", s.Name, s.LastError, s.Failures, s.NextDue.UTC().Format(time.RFC3339)))
		case s.LastComplete.IsZero() && now.Sub(s.NextDue) > s.Interval:
			lagging = append(lagging, s.Name+": no complete scan yet")
		case !s.LastComplete.IsZero() && now.Sub(s.LastComplete) > time.Duration(o.LagFactor)*s.Interval:
			lagging = append(lagging, fmt.Sprintf("%s: last complete %s ago", s.Name, now.Sub(s.LastComplete).Truncate(time.Second)))
		}
		for _, f := range s.Families {
			if !f.ReDerivable {
				limited = append(limited, fmt.Sprintf("%s %s: %s", s.Name, f.Type, f.Loss))
			}
		}
	}
	switch {
	case len(failing) > 0:
		checks = append(checks, Check{Name: "reconcile_lag", Status: CheckWarn, Code: "scan_failing", Detail: fmt.Sprintf("%d source(s) failing reconcile", len(failing)), Items: limit(append(failing, lagging...))})
	case len(lagging) > 0:
		checks = append(checks, Check{Name: "reconcile_lag", Status: CheckWarn, Code: "lagging", Detail: fmt.Sprintf("%d source(s) behind their reconcile interval", len(lagging)), Items: limit(lagging)})
	default:
		checks = append(checks, Check{Name: "reconcile_lag", Status: CheckOK, Code: "current", Detail: fmt.Sprintf("%d source(s) within their reconcile interval", len(statuses))})
	}
	if len(limited) > 0 {
		checks = append(checks, Check{Name: "non_rederivable", Status: CheckInfo, Code: "declared", Detail: fmt.Sprintf("%d declared delivery famil(ies) cannot be recovered by reconcile if push is lost", len(limited)), Items: limit(limited)})
	} else {
		checks = append(checks, Check{Name: "non_rederivable", Status: CheckOK, Code: "none", Detail: "every registered delivery family is recoverable by reconcile"})
	}
	return checks
}

// PinnedExecutorCheck reports attempts that still need their pinned executor
// (live, parked or holding an unconfirmed resource) but whose generation and
// registration are not installed, so recovery, resume or stop cannot run.
func (s *Scheduler) PinnedExecutorCheck(ctx context.Context) Check {
	var missing []string
	err := s.transact(ctx, nil, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions("")
		if err != nil {
			return err
		}
		for _, x := range xs {
			state, err := attemptState(tx, x.ID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if x.State.Terminal() && state.Excluded {
				continue
			}
			if _, err := s.executorFor(state); err != nil {
				missing = append(missing, fmt.Sprintf("%s (generation %s, executor %s)", x.ID, state.Generation, state.Executor))
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return Check{Name: "pinned_executors", Status: CheckFail, Code: "storage_unavailable", Detail: "attempt pins could not be read"}
	case len(missing) > 0:
		sort.Strings(missing)
		return Check{Name: "pinned_executors", Status: CheckFail, Code: "executor_missing", Detail: fmt.Sprintf("%d attempt(s) pin an executor that is not installed", len(missing)), Items: missing}
	}
	return Check{Name: "pinned_executors", Status: CheckOK, Code: "installed", Detail: "every live or held attempt has its pinned executor installed"}
}
