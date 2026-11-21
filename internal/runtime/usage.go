package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// meteredUnits is a usage in the units its records carry: its named amounts
// and the token counts by kind, which a price depends on.
func meteredUnits(u extension.Usage) map[string]float64 {
	out := u.Amounts()
	for unit, n := range map[string]*uint64{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "cached_tokens": u.CachedTokens} {
		if _, named := out[unit]; !named && n != nil {
			out[unit] = float64(*n)
		}
	}
	return out
}

// recordUsageInTx accepts the usage record of what state adds to the stored
// attempt, in the transaction that stores it, so a result and its record
// commit together. It compares with the stored state, so a stale copy or a
// repeated write records nothing. An attempt already running when records
// began counts from what it had then. The Event names the Work in its data
// rather than addressing it, so it is never input to that Work. It is
// accepted whatever the causal limits say, since a record of what was used
// can't be refused; routing applies them (usageRoutableInTx).
func recordUsageInTx(tx storage.Tx, state *AttemptState, stored AttemptState, found bool) error {
	recorded := maps.Clone(stored.UsageRecorded)
	if recorded == nil {
		recorded = map[string]float64{}
		if found {
			recorded = meteredUnits(stored.Usage())
		}
	}
	state.UsageRecords = stored.UsageRecords
	delta := map[string]float64{}
	for unit, v := range meteredUnits(state.Usage()) {
		if v > recorded[unit] {
			delta[unit] = v - recorded[unit]
			recorded[unit] = v
		}
	}
	state.UsageRecorded = nil
	if len(recorded) > 0 {
		state.UsageRecorded = recorded
	}
	if len(delta) == 0 {
		return nil
	}
	scope := state.ScopeID
	if scope == "" {
		w, err := tx.Work(state.WorkID)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		scope = w.ScopeID
	}
	record := domain.UsageRecord{Work: string(state.WorkID), Execution: string(state.ExecutionID), Scope: string(scope), Binding: state.Binding.Name, Executor: state.Executor, Segment: state.SegmentNumber(), Amounts: delta}
	if state.Result != nil {
		record.Attribution = state.Result.Usage.Attribution
	}
	at := state.landedAt
	if n := len(state.Segments); n > 0 {
		g := state.Segments[n-1]
		started := g.StartedAt
		record.StartedAt = &started
		if at.IsZero() && g.EndedAt != nil {
			at = *g.EndedAt
		}
		if g.EndedAt != nil {
			ended := *g.EndedAt
			record.EndedAt = &ended
		} else if !at.IsZero() {
			ended := at
			record.EndedAt = &ended
		}
		if c := g.Container; c != nil {
			record.Container = &domain.UsageContainer{Provider: c.Provider, Image: c.Image, Digest: c.Digest, ID: c.ID}
		}
		if at.IsZero() {
			at = g.StartedAt
		}
	}
	if at.IsZero() {
		at = time.Unix(0, 0).UTC()
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	state.UsageRecords++
	delivery := fmt.Sprintf("usage/%s/%d", state.ExecutionID, state.UsageRecords)
	cause := state.CausalTriggerID
	if cause == "" {
		cause = state.CurrentTriggerID
	}
	e := domain.Event{ID: domain.ID("usage-event/" + identity(delivery)), Type: domain.TypeExecutionUsage, Source: "core.execution", DeliveryID: delivery, CausationID: cause, OccurredAt: at, ReceivedAt: at, Data: domain.Payload{Schema: domain.SchemaExecutionUsage, Data: data}}
	_, _, err = AcceptInTx(tx, scope, e)
	return err
}

// usageRoutableInTx says whether a usage record may reach routes. Work a
// route starts from one reports usage of its own, whose record descends from
// the first, so a route on every record would feed itself: past the causal
// depth limit the record stays in the feed and starts nothing.
func usageRoutableInTx(tx storage.Tx, id domain.ID, limits CausalLimits) (bool, error) {
	limits, err := normalizeCausalLimits(limits)
	if err != nil {
		return false, err
	}
	info, err := ancestry(tx, id, limits)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Depth < info.Limits.MaxDepth, nil
}
