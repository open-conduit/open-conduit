package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// retirementTx answers the reads the guard makes.
type retirementTx struct {
	storage.Tx
	works      []domain.Work
	executions map[domain.ID][]domain.Execution
	waits      map[domain.ID][]domain.Wait
	kv         map[string]map[string][]byte
}

func (t retirementTx) LiveWorks() ([]domain.Work, error) { return t.works, nil }
func (t retirementTx) ListExecutions(id domain.ID) ([]domain.Execution, error) {
	return t.executions[id], nil
}
func (t retirementTx) ListWaits(id domain.ID) ([]domain.Wait, error) { return t.waits[id], nil }
func (t retirementTx) Get(ns, key string) ([]byte, error) {
	if b, ok := t.kv[ns][key]; ok {
		return b, nil
	}
	return nil, storage.ErrNotFound
}
func (t retirementTx) List(ns, prefix string) ([]storage.Entry, error) {
	var out []storage.Entry
	for k, v := range t.kv[ns] {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.Entry{Key: k, Value: v})
		}
	}
	return out, nil
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func retirable(id domain.ID, sequence uint64) domain.AcceptedEvent {
	return domain.AcceptedEvent{Event: domain.Event{ID: id, Type: "test.old", Source: "test", DeliveryID: string(id)}, Sequence: sequence, Routed: true, Disposition: "unmatched"}
}

func TestRetirementGuardKeepsWhatTheLiveSystemDecidesWith(t *testing.T) {
	tx := retirementTx{
		works:      []domain.Work{{ID: "live", State: domain.WorkWaiting}},
		executions: map[domain.ID][]domain.Execution{"live": {{ID: "attempt", WorkID: "live", TriggerID: "trigger"}}},
		waits:      map[domain.ID][]domain.Wait{"live": {{ID: "wait", WorkID: "live", State: domain.WaitActive, AfterSequence: 50}, {ID: "over", WorkID: "live", State: domain.WaitWoken, AfterSequence: 1}}},
		kv: map[string]map[string][]byte{
			attemptNamespace:      {identity("attempt"): mustJSON(t, AttemptState{InputEventIDs: []domain.ID{"input"}, WakeEventID: "wake"})},
			waitNamespace:         {identity("wait"): mustJSON(t, waitMetadata{Scanned: 60})},
			subscriptionNamespace: {"sub": mustJSON(t, subscription{Checkpoint: 70, Active: true}), "ended": mustJSON(t, subscription{Checkpoint: 2})},
			onceNamespace:         {"first": []byte("once")},
		},
	}
	g, err := NewRetirementGuardInTx(tx)
	if err != nil {
		t.Fatal(err)
	}
	with := func(id domain.ID, sequence uint64, change func(*domain.AcceptedEvent)) domain.AcceptedEvent {
		a := retirable(id, sequence)
		if change != nil {
			change(&a)
		}
		return a
	}
	for _, c := range []struct {
		name  string
		event domain.AcceptedEvent
		want  string
	}{
		{"finished history at the scan", with("old", 60, nil), ""},
		{"finished Work's Event", with("done", 10, func(a *domain.AcceptedEvent) { a.Event.WorkID, a.RoutedWorkID = "finished", "finished" }), ""},
		{"a child's end, which no replay compares", with("ended", 10, func(a *domain.AcceptedEvent) {
			a.Event.Source, a.Event.DeliveryID = domain.LineageSource, "child-ended/x"
		}), ""},
		{"already retired", with("gone", 10, func(a *domain.AcceptedEvent) { a.Retired = true }), RetireBlockedRetired},
		{"unrouted", with("new", 10, func(a *domain.AcceptedEvent) { a.Routed = false }), RetireBlockedUnrouted},
		{"dead-lettered", with("failed", 10, func(a *domain.AcceptedEvent) { a.Disposition = DispositionFailed }), RetireBlockedReroutable},
		{"skipped", with("skipped", 10, func(a *domain.AcceptedEvent) { a.Disposition = DispositionSkipped }), RetireBlockedReroutable},
		{"operator control", with("manual", 10, func(a *domain.AcceptedEvent) { a.Event.Source = ManualSource }), RetireBlockedControl},
		{"action", with("action", 10, func(a *domain.AcceptedEvent) { a.Event.Source = extension.ActionSource }), RetireBlockedControl},
		{"Work creation", with("created", 10, func(a *domain.AcceptedEvent) {
			a.Event.Source, a.Event.DeliveryID = domain.LineageSource, "created/x"
		}), RetireBlockedCreation},
		{"once-only emission", with("once", 10, nil), RetireBlockedOnce},
		{"names unfinished Work", with("named", 10, func(a *domain.AcceptedEvent) { a.Event.WorkID = "live" }), RetireBlockedLiveWork},
		{"routed to unfinished Work", with("routed", 10, func(a *domain.AcceptedEvent) { a.RoutedWorkID = "live" }), RetireBlockedLiveWork},
		{"an attempt's trigger", with("trigger", 10, nil), RetireBlockedAttempt},
		{"an attempt's input", with("input", 10, nil), RetireBlockedAttempt},
		{"an attempt's wake", with("wake", 10, nil), RetireBlockedAttempt},
		{"past the oldest scan", with("unscanned", 61, nil), RetireBlockedWaitScan},
	} {
		if got := g.Blocked(c.event); got != c.want {
			t.Errorf("%s: blocked %q, want %q", c.name, got, c.want)
		}
	}
}

// With no active Wait or dependency, nothing bounds the scan: an ended
// subscription's old checkpoint must not hold history back.
func TestRetirementGuardWithNothingWaiting(t *testing.T) {
	g, err := NewRetirementGuardInTx(retirementTx{kv: map[string]map[string][]byte{subscriptionNamespace: {"ended": mustJSON(t, subscription{Checkpoint: 2})}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Blocked(retirable("old", 1000)); got != "" {
		t.Fatalf("blocked %q", got)
	}
}
