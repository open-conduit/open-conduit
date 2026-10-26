package introspection

import (
	"slices"
	"sort"
	"strings"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// Removal policy for existing Work (05-03). Removing an `introspection`
// entry (or one of its schedules) takes effect on the next generation:
//
//   - its operations are undeclared, its route matches nothing and Ensure
//     cancels its timers (no new report, sweep, triage or fix Work);
//   - Work of the extension's kinds that the configuration no longer has a
//     use for is retired: the host cancels it with a recorded reason once no
//     attempt of it is running. An attempt already going finishes first (its
//     calls to the operations are undeclared); a standing sweep or triage is
//     idle afterwards and is cancelled by the next pass;
//   - reports, fixes, notes, Events and run history stay readable: retiring
//     cancels Work, it never deletes records.
//
// A disabled scope is not a removal: its timers and Work wait for it to be
// enabled again (B05), as scheduled jobs do.

// RetireReason is the cancellation reason the host records.
const RetireReason = "introspection removed from configuration"

// Retired lists the non-terminal, not-running Work of the extension's kinds
// that the configuration no longer has a use for. configured maps every
// configured scope to whether it has an `introspection` entry (enabled or
// not); a scope missing from it is left alone. Standing sweep and triage
// Work (one owning a run key) is also retired when its key is no longer a
// live slot timer's: its schedule was removed, or replaced by a new epoch.
func Retired(tx storage.Tx, configured map[domain.ID]bool) ([]domain.Work, error) {
	live := map[domain.CorrelationKey]bool{}
	timers, err := tx.ListTimers()
	if err != nil {
		return nil, err
	}
	for _, t := range timers {
		if (t.Event.Type == TypeSweepDue || t.Event.Type == TypeTriageDue) && t.Event.CorrelationKey != nil {
			live[*t.Event.CorrelationKey] = true
		}
	}
	works, err := tx.ListWorks()
	if err != nil {
		return nil, err
	}
	var out []domain.Work
	for _, w := range works {
		if w.State.Terminal() || w.State == domain.WorkRunning || !slices.Contains(WorkKinds(), w.Kind) {
			continue
		}
		entry, ok := configured[w.ScopeID]
		if !ok {
			continue
		}
		if !entry {
			out = append(out, w)
			continue
		}
		if run, ok := runKey(w); ok && !live[run] {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// runKey is the run key standing sweep or triage Work owns.
func runKey(w domain.Work) (domain.CorrelationKey, bool) {
	if w.Kind != WorkKindSweep && w.Kind != WorkKindTriage {
		return domain.CorrelationKey{}, false
	}
	run := WorkSweep
	if w.Kind == WorkKindTriage {
		run = WorkTriage
	}
	for _, k := range w.CorrelationKeys {
		if k.Source == SourceNamespace(w.ScopeID) && strings.HasPrefix(k.ExternalID, run+"/") {
			return k, true
		}
	}
	return domain.CorrelationKey{}, false
}
