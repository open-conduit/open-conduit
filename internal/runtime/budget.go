package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Budgets (#361) cap what Work may consume, in named units, and are
// enforced where the host admits a turn (Scheduler.Start). Core counts two
// units itself: turns (every start, a resume included) and attempts (every
// new execution). Every other unit is what executors report on their
// results (extension.Usage.Amounts): core adds the amounts up and never
// decides what counts as a cost.
//
// A turn's usage is staged with its attempt state (putAttemptState) and
// charged to the ledgers of the budgets that apply, in the window of the
// time it is settled, at the next admission or settlement pass. A ledger
// counts from the time its budget is first configured.
//
// Admission refuses a turn when a ledger has reached its budget's max: the
// Work is held with the reason (an operator hold of kind "budget"), so it
// shows in Needs you and doctor. Releasing that hold lets one more turn
// start past the limit; raising the limit lets every turn start again.
// Crossing a budget's warning threshold or its max accepts a
// core.budget.warning or core.budget.exceeded Event in the Work's scope,
// once per ledger window and limit, which routes can act on.

const (
	budgetLedgerNamespace  = "core.budget.ledger"
	budgetPendingNamespace = "core.budget.pending"
	budgetPassNamespace    = "core.budget.pass"
	// HoldKindBudget marks a hold a budget placed.
	HoldKindBudget = "budget"
)

// Budget scopes: per Work, per scope, or across every scope.
const (
	BudgetPerWork  = "work"
	BudgetPerScope = "scope"
	BudgetPerAll   = "all"
)

// ErrBudgetHeld is a start refused because the Work is over a budget; the
// Work has been held with the reason. It matches ErrNotEligible.
var ErrBudgetHeld = fmt.Errorf("%w: over budget", ErrNotEligible)

// Budget is one usage limit: at most Max of Unit per Work, per scope or
// across every scope (Per), over the ledger's life ("" Period) or each UTC
// "day" or "week" (from Monday). ScopeID narrows it to Work in one scope;
// with Per scope and no ScopeID every scope has its own ledger. WarnAt is
// the fraction of Max that fires a warning (zero: none).
type Budget struct {
	Unit, Per, Period string
	ScopeID           domain.ID
	Max, WarnAt       float64
}

func (b Budget) applies(scope domain.ID) bool { return b.ScopeID == "" || b.ScopeID == scope }

// window names the ledger window a time falls in.
func (b Budget) window(at time.Time) string {
	at = at.UTC()
	switch b.Period {
	case "day":
		return "day-" + at.Format("2006-01-02")
	case "week":
		back := (int(at.Weekday()) + 6) % 7
		return "week-" + at.AddDate(0, 0, -back).Format("2006-01-02")
	}
	return "total"
}

// ledgerKey is the ledger a charge to work in scope at a time lands in.
func (b Budget) ledgerKey(work, scope domain.ID, at time.Time) string {
	switch b.Per {
	case BudgetPerWork:
		return "work/" + identity(string(work)) + "/" + b.window(at)
	case BudgetPerScope:
		return "scope/" + identity(string(scope)) + "/" + b.window(at)
	}
	return "all/" + b.window(at)
}

// Describe is the budget in words: "10 usd per Work per day".
func (b Budget) Describe() string {
	s := formatAmount(b.Max) + " " + b.Unit
	switch b.Per {
	case BudgetPerWork:
		s += " per Work"
	case BudgetPerScope:
		s += " per scope"
		if b.ScopeID != "" {
			s += " (" + string(b.ScopeID) + ")"
		}
	default:
		s += " across every scope"
	}
	if b.Period != "" {
		s += " per " + b.Period
	}
	return s
}

func formatAmount(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ledger is the units charged to one ledger window.
type ledger map[string]float64

func ledgerInTx(tx storage.Tx, key string) (ledger, error) {
	l := ledger{}
	err := getJSON(tx, budgetLedgerNamespace, key, &l)
	if errors.Is(err, storage.ErrNotFound) {
		return ledger{}, nil
	}
	return l, err
}

// pendingUsage is usage an attempt reported that is not yet charged.
type pendingUsage struct {
	WorkID  domain.ID          `json:"work_id"`
	ScopeID domain.ID          `json:"scope_id"`
	Amounts map[string]float64 `json:"amounts"`
}

// stageUsageInTx records, before the attempt state is written, the part of
// its usage not yet staged, so that every path that lands a result (finish,
// park, failure, timeout) charges it exactly once. It compares with the
// stored state, so writing a stale copy never charges twice.
func stageUsageInTx(tx storage.Tx, state *AttemptState) error {
	if state.ExecutionID == "" {
		return nil
	}
	charged := map[string]float64{}
	if stored, err := attemptState(tx, state.ExecutionID); err == nil {
		maps.Copy(charged, stored.Charged)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	delta := map[string]float64{}
	for unit, v := range state.Usage().Amounts() {
		if v > charged[unit] {
			delta[unit] = v - charged[unit]
			charged[unit] = v
		}
	}
	if len(charged) > 0 {
		state.Charged = charged
	}
	if len(delta) == 0 {
		return nil
	}
	key := identity(string(state.ExecutionID))
	p := pendingUsage{WorkID: state.WorkID, ScopeID: state.ScopeID, Amounts: map[string]float64{}}
	if err := getJSON(tx, budgetPendingNamespace, key, &p); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if p.Amounts == nil {
		p.Amounts = map[string]float64{}
	}
	for unit, v := range delta {
		p.Amounts[unit] += v
	}
	return putJSON(tx, budgetPendingNamespace, key, p)
}

// settleUsageInTx charges every staged usage to the ledgers of the budgets
// that apply, at now.
func settleUsageInTx(tx storage.Tx, now time.Time, budgets []Budget) error {
	rows, err := tx.List(budgetPendingNamespace, "")
	if err != nil {
		return err
	}
	for _, row := range rows {
		var p pendingUsage
		if err = json.Unmarshal(row.Value, &p); err != nil {
			return err
		}
		if err = chargeInTx(tx, now, budgets, p.WorkID, p.ScopeID, p.Amounts); err != nil {
			return err
		}
		if err = tx.Delete(budgetPendingNamespace, row.Key); err != nil {
			return err
		}
	}
	return nil
}

// chargeInTx adds amounts to every ledger an applicable budget keeps and
// accepts the events of the thresholds the charge crosses.
func chargeInTx(tx storage.Tx, now time.Time, budgets []Budget, work, scope domain.ID, amounts map[string]float64) error {
	if len(amounts) == 0 {
		return nil
	}
	before := map[string]ledger{}
	var keys []string
	for _, b := range budgets {
		if !b.applies(scope) {
			continue
		}
		key := b.ledgerKey(work, scope, now)
		if _, ok := before[key]; ok {
			continue
		}
		l, err := ledgerInTx(tx, key)
		if err != nil {
			return err
		}
		before[key] = l
		keys = append(keys, key)
	}
	for _, key := range keys {
		after := maps.Clone(before[key])
		for unit, v := range amounts {
			after[unit] += v
		}
		if err := putJSON(tx, budgetLedgerNamespace, key, after); err != nil {
			return err
		}
	}
	for _, b := range budgets {
		if !b.applies(scope) {
			continue
		}
		v, ok := amounts[b.Unit]
		if !ok || v <= 0 {
			continue
		}
		key := b.ledgerKey(work, scope, now)
		used := before[key][b.Unit]
		next := used + v
		if b.WarnAt > 0 && used < b.Max*b.WarnAt && next >= b.Max*b.WarnAt && next < b.Max {
			if err := budgetEventInTx(tx, now, "warning", b, key, next, work, scope); err != nil {
				return err
			}
		}
		if used < b.Max && next >= b.Max {
			if err := budgetEventInTx(tx, now, "exceeded", b, key, next, work, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// BudgetNotice is the data of a core.budget.warning or core.budget.exceeded
// Event (schema core.budget.v1).
type BudgetNotice struct {
	Unit    string    `json:"unit"`
	Per     string    `json:"per"`
	Period  string    `json:"period,omitempty"`
	Window  string    `json:"window"`
	Max     float64   `json:"max"`
	WarnAt  float64   `json:"warn_at,omitempty"`
	Used    float64   `json:"used"`
	WorkID  domain.ID `json:"work_id"`
	ScopeID domain.ID `json:"scope_id"`
	Budget  string    `json:"budget"`
}

// budgetEventInTx accepts a routable root Event in the Work's scope that a
// budget's threshold was crossed. It names the Work in its data rather than
// addressing it, so it never becomes the Work's own input. One per ledger
// window, kind and limit: raising the limit can fire it again.
func budgetEventInTx(tx storage.Tx, now time.Time, kind string, b Budget, key string, used float64, work, scope domain.ID) error {
	window := key[strings.LastIndex(key, "/")+1:]
	data, _ := json.Marshal(BudgetNotice{Unit: b.Unit, Per: b.Per, Period: b.Period, Window: window, Max: b.Max, WarnAt: b.WarnAt, Used: used, WorkID: work, ScopeID: scope, Budget: b.Describe()})
	delivery := kind + "/" + identity(key, b.Unit, formatAmount(b.Max))
	e := domain.Event{ID: domain.ID("budget-" + delivery), Type: "core.budget." + kind, Source: "core.budget", DeliveryID: delivery, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.budget.v1", Data: data}}
	_, _, err := AcceptInTx(tx, scope, e)
	return err
}

// overBudget is a budget whose ledger has reached its max.
type overBudget struct {
	Budget
	Used float64
}

// admitBudgetInTx is the budget check before a turn starts. It settles
// staged usage and, when an applicable ledger has reached its max, holds
// the Work with the reason and reports held (the caller commits and refuses
// the start). A pass an operator's release left lets this one turn through.
// An admitted turn is charged one turn, and one attempt when it starts a
// new execution.
func admitBudgetInTx(tx storage.Tx, now time.Time, budgets []Budget, w domain.Work, newAttempt bool) (held bool, err error) {
	if len(budgets) == 0 {
		return false, nil
	}
	if err = settleUsageInTx(tx, now, budgets); err != nil {
		return false, err
	}
	var over []overBudget
	for _, b := range budgets {
		if !b.applies(w.ScopeID) {
			continue
		}
		l, err := ledgerInTx(tx, b.ledgerKey(w.ID, w.ScopeID, now))
		if err != nil {
			return false, err
		}
		if used := l[b.Unit]; used >= b.Max {
			over = append(over, overBudget{b, used})
		}
	}
	if len(over) > 0 {
		passKey := identity(string(w.ID))
		var pass domain.ID
		switch err := getJSON(tx, budgetPassNamespace, passKey, &pass); {
		case err == nil:
			if err = tx.Delete(budgetPassNamespace, passKey); err != nil {
				return false, err
			}
		case errors.Is(err, storage.ErrNotFound):
			return true, holdOverBudgetInTx(tx, now, w, over)
		default:
			return false, err
		}
	}
	counted := map[string]float64{"turns": 1}
	if newAttempt {
		counted["attempts"] = 1
	}
	return false, chargeInTx(tx, now, budgets, w.ID, w.ScopeID, counted)
}

// holdOverBudgetInTx holds Work over its budgets: a core.budget.held control
// Event addressed to the Work records it in its history, the hold carries
// the reason, and each budget's exceeded notice is accepted if it has not
// been (a limit lowered below what was already used crosses nothing).
func holdOverBudgetInTx(tx storage.Tx, now time.Time, w domain.Work, over []overBudget) error {
	sort.Slice(over, func(i, j int) bool { return over[i].Describe() < over[j].Describe() })
	var parts []string
	for _, o := range over {
		parts = append(parts, fmt.Sprintf("%s %s used of %s", formatAmount(o.Used), o.Unit, o.Describe()))
		if err := budgetEventInTx(tx, now, "exceeded", o.Budget, o.ledgerKey(w.ID, w.ScopeID, now), o.Used, w.ID, w.ScopeID); err != nil {
			return err
		}
	}
	reason := "Over budget: " + strings.Join(parts, "; ") + ". Release the hold to let one more turn start, or raise the limit."
	data, _ := json.Marshal(struct {
		Reason string   `json:"reason"`
		Over   []string `json:"over"`
	}{reason, parts})
	delivery := "held/" + identity(string(w.ID), now.UTC().Format(time.RFC3339Nano))
	e := domain.Event{ID: domain.ID("budget-" + delivery), Type: "core.budget.held", Source: "core.budget", DeliveryID: delivery, WorkID: w.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.budget.held.v1", Data: data}}
	accepted, _, err := AcceptInTx(tx, w.ScopeID, e)
	if err != nil {
		return err
	}
	if err = tx.MarkRouted(accepted.Event.ID, "", "control"); err != nil {
		return err
	}
	if err = putJSON(tx, holdNamespace, identity(string(w.ID)), Hold{EventID: accepted.Event.ID, Reason: reason, At: now.UTC(), Kind: HoldKindBudget}); err != nil {
		return err
	}
	// Advance the revision, as an operator hold does, so views see it.
	return tx.PutWork(w, w.Revision)
}

// passBudgetInTx lets the next turn of Work an operator released from a
// budget hold start past the limits that held it.
func passBudgetInTx(tx storage.Tx, work, release domain.ID) error {
	return putJSON(tx, budgetPassNamespace, identity(string(work)), release)
}

// BudgetUsage is one budget's ledger for one Work at a time.
type BudgetUsage struct {
	Budget
	Window string
	Used   float64
}

// SettleBudgets charges staged usage to the ledgers, so warnings fire while
// no turn is starting. The daemon runs it with its other passes.
func (s *Scheduler) SettleBudgets(ctx context.Context) error {
	if len(s.options.Budgets) == 0 {
		// Without budgets nothing is charged; staged usage is dropped.
		return s.transact(ctx, nil, func(tx storage.Tx) error {
			rows, err := tx.List(budgetPendingNamespace, "")
			for _, row := range rows {
				if err == nil {
					err = tx.Delete(budgetPendingNamespace, row.Key)
				}
			}
			return err
		})
	}
	return s.transact(ctx, nil, func(tx storage.Tx) error { return settleUsageInTx(tx, s.now(), s.options.Budgets) })
}

// BudgetsFor reports every budget that applies to the Work and what its
// current ledger window has used.
func (s *Scheduler) BudgetsFor(ctx context.Context, work domain.ID) (out []BudgetUsage, err error) {
	now := s.now()
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		w, e := tx.Work(work)
		if e != nil {
			return e
		}
		out = nil
		for _, b := range s.options.Budgets {
			if !b.applies(w.ScopeID) {
				continue
			}
			key := b.ledgerKey(w.ID, w.ScopeID, now)
			l, e := ledgerInTx(tx, key)
			if e != nil {
				return e
			}
			out = append(out, BudgetUsage{Budget: b, Window: key[strings.LastIndex(key, "/")+1:], Used: l[b.Unit]})
		}
		return nil
	})
	return out, err
}
