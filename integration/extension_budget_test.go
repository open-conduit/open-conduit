package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
)

// The performance budget for the in-transaction RPCs (#428). Once every
// extension runs as a package, the four in-transaction methods
// (work_kind.trigger, recurrence.next, dependency.filter and emit.commit)
// are calls to another process made while the host holds its coordination
// transaction, on every routing pass, timer firing, Wait match and
// committed emission that reaches an extension. Each must stay a small fraction of
// the 250 ms in-transaction deadline, so a regression (a process started
// per call, a lock held across the wire, a per-call allocation storm) fails
// CI here rather than as a slow daemon.
//
// The budget is per call, measured round trip from the host's adapter
// (extproc.Remote) to the notes example process and back: the median and
// the 95th percentile over budgetCalls calls after a warm-up (the 99th and
// the slowest are logged, not gated: on a shared runner they are the
// scheduler's and the garbage collector's, not the protocol's). The race
// detector slows both sides several times, so the budget is scaled by
// budgetRaceFactor under -race, which is how `make check` runs it. A
// regression is slow every time and noise isn't, so a measurement over
// budget is taken again, up to budgetRounds times, before it fails.
// scripts/test-integration.sh runs these tests on their own, after the
// shards, so a busy shard's neighbours don't take the measurement.
const (
	budgetCalls      = 300
	budgetWarmup     = 20
	budgetRounds     = 3
	budgetP50        = 2 * time.Millisecond
	budgetP95        = 10 * time.Millisecond
	budgetRaceFactor = 5
)

// budgetLimits are the median and 95th-percentile limits, scaled under
// -race.
func budgetLimits() (time.Duration, time.Duration) {
	if raceEnabled {
		return budgetP50 * budgetRaceFactor, budgetP95 * budgetRaceFactor
	}
	return budgetP50, budgetP95
}

// measurement is one round's sorted call durations.
type measurement []time.Duration

func (m measurement) at(q int) time.Duration { return m[len(m)*q/100] }

func (m measurement) within() bool {
	limit50, limit95 := budgetLimits()
	return m.at(50) <= limit50 && m.at(95) <= limit95
}

func (m measurement) String() string {
	limit50, limit95 := budgetLimits()
	return fmt.Sprintf("p50 %v (budget %v), p95 %v (budget %v), p99 %v, max %v over %d calls", m.at(50), limit50, m.at(95), limit95, m.at(99), m[len(m)-1], len(m))
}

// measurePrepared times calls calls, each prepared afresh (an emission to
// commit): prepare runs untimed and returns the call to time, or nil for a
// wrong answer; wrong is the index of the first wrong answer, or -1.
func measurePrepared(calls int, prepare func() func() bool) (m measurement, wrong int) {
	m = make(measurement, calls)
	for i := range m {
		call := prepare()
		if call == nil {
			return nil, i
		}
		start := time.Now()
		ok := call()
		m[i] = time.Since(start)
		if !ok {
			return nil, i
		}
	}
	slices.Sort(m)
	return m, -1
}

// budget times call and fails when, in every one of budgetRounds rounds,
// the median or the 95th percentile is over budget. call reports whether
// its answer was right, so a fast wrong answer (a fallback after an error)
// fails at once.
func budget(t *testing.T, name string, call func() bool) {
	t.Helper()
	budgetPrepared(t, name, func() func() bool { return call })
}

// budgetPrepared is budget for a call with untimed setup (measurePrepared).
func budgetPrepared(t *testing.T, name string, prepare func() func() bool) {
	t.Helper()
	if _, wrong := measurePrepared(budgetWarmup, prepare); wrong >= 0 {
		t.Fatalf("%s: wrong answer during warm-up", name)
	}
	var last measurement
	for round := 1; round <= budgetRounds; round++ {
		m, wrong := measurePrepared(budgetCalls, prepare)
		if wrong >= 0 {
			t.Fatalf("%s: wrong answer on call %d", name, wrong)
		}
		t.Logf("%s round %d: %v", name, round, m)
		if m.within() {
			return
		}
		last = m
	}
	t.Errorf("%s is over its performance budget in all %d rounds; the last: %v", name, budgetRounds, last)
}

// TestInTransactionRPCBudgetCatchesASlowCall is the budget's negative
// control, on synthetic durations (tests don't wait on real time): calls
// just over the median limit, and a tenth of calls over the
// 95th-percentile limit, are both over budget; calls well inside both are
// within it.
func TestInTransactionRPCBudgetCatchesASlowCall(t *testing.T) {
	limit50, limit95 := budgetLimits()
	durations := func(n int, at func(i int) time.Duration) measurement {
		m := make(measurement, n)
		for i := range m {
			m[i] = at(i)
		}
		slices.Sort(m)
		return m
	}
	if slow := durations(40, func(int) time.Duration { return limit50 + time.Millisecond }); slow.within() {
		t.Fatalf("calls over the median limit are within budget: %v", slow)
	}
	tail := durations(40, func(i int) time.Duration {
		if i%10 == 9 {
			return limit95 + time.Millisecond
		}
		return limit50 / 10
	})
	if tail.within() {
		t.Fatalf("a tenth of calls over the 95th-percentile limit is within budget: %v", tail)
	}
	if fast := durations(40, func(int) time.Duration { return limit50 / 10 }); !fast.within() {
		t.Fatalf("fast calls are over budget: %v", fast)
	}
}

func TestInTransactionRPCBudget(t *testing.T) {
	notesPath, _ := extensionBinaries(t)
	e := newExtEnv(t)
	r := e.load(e.install(notes.Manifest(notes.Options{Release: 2}), notesPath), e.deps(), e.options(notes.Name))

	t.Run("work_kind.trigger", func(t *testing.T) {
		policy, ok := r.WorkKind(notes.Kind)
		if !ok || policy.Trigger == nil {
			t.Fatal("notes declares no trigger")
		}
		// A routing pass's pending batch: a few inputs of about 1 KiB.
		body, _ := json.Marshal(map[string]string{"text": strings.Repeat("a note line of ordinary text. ", 35)})
		var inputs []domain.Event
		for i := range 4 {
			inputs = append(inputs, domain.Event{ID: domain.ID("in-" + string(rune('a'+i))), Type: notes.Changed, Source: notes.Name, Data: domain.Payload{Data: body}})
		}
		// The notes trigger picks the newest input.
		budget(t, "work_kind.trigger", func() bool { return policy.Trigger(inputs) == len(inputs)-1 })
	})

	t.Run("recurrence.next", func(t *testing.T) {
		next := r.Recurrence(notes.Recurrence)
		fired := domain.Timer{ID: "budget", FireAt: extensionNoteTime}
		budget(t, "recurrence.next", func() bool {
			occ, nt, err := next(nil, fired, extensionNoteTime)
			return err == nil && occ != nil && nt != nil
		})
	})

	t.Run("dependency.filter", func(t *testing.T) {
		filter := fixtureRemote(t).Registration().DependencyFilter
		if filter == nil {
			t.Fatal("the fixture declares no dependency filter")
		}
		data := json.RawMessage(`{"pr":42}`)
		ev := domain.Event{ID: "dep", Type: "fixture.changed.v1", Source: "fixture", Data: domain.Payload{Data: data}}
		// The fixture's filter matches a payload equal to the filter.
		budget(t, "dependency.filter", func() bool {
			ok, err := filter(domain.Payload{Data: data}, ev)
			return ok && err == nil
		})
	})

	t.Run("emit.commit", func(t *testing.T) {
		var annotate extension.Operation
		for _, op := range r.Registration().Operations {
			if op.Name == "notes.annotate" {
				annotate = op
			}
		}
		if annotate.Emit == nil {
			t.Fatal("notes declares no committed emission")
		}
		authority := extension.Authority{ScopeID: extensionScope}
		input := domain.Payload{Data: json.RawMessage(`{"note":"` + strings.Repeat("an annotation. ", 16) + `"}`)}
		n := 0
		// Each commit ID is used once, so every call plans a fresh emission
		// (operation.emit, outside the transaction, untimed) and times only
		// its commit, which carries the accepted Event back.
		budgetPrepared(t, "emit.commit", func() func() bool {
			n++
			plan, err := annotate.Emit(extension.WithCallRequestID(context.Background(), fmt.Sprintf("budget-%d", n)), authority, input)
			if err != nil || plan.Commit == nil {
				return nil
			}
			em := extension.Emitted{Event: domain.AcceptedEvent{Event: domain.Event{ID: domain.ID(fmt.Sprintf("emitted-%d", n)), Type: notes.Annotated, Source: notes.Name, DeliveryID: plan.Event.DeliveryID}, Sequence: uint64(n)}, Added: true, Depth: 1}
			return func() bool {
				c, err := plan.Commit(em)
				return err == nil && len(c.Records) == 1
			}
		})
	})
}
