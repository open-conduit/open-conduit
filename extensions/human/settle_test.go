package human

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// liveTx reads core records as the protocol's Store does: each read sees the
// host as it is then, not one snapshot. The answer is accepted and wakes the
// wait straight after the first read of either, the one moment that can tell
// the two reads apart.
type liveTx struct {
	storage.Tx
	kv     map[string][]byte
	work   domain.Work
	wait   domain.Wait
	answer domain.AcceptedEvent
	reads  int
}

func (t *liveTx) landed() bool {
	t.reads++
	return t.reads > 1
}

func (t *liveTx) Get(_, key string) ([]byte, error) {
	if b, ok := t.kv[key]; ok {
		return b, nil
	}
	return nil, storage.ErrNotFound
}

func (t *liveTx) Put(_, key string, value []byte) error {
	t.kv[key] = value
	return nil
}

func (t *liveTx) Work(domain.ID) (domain.Work, error) { return t.work, nil }

func (t *liveTx) Events(uint64, int) ([]domain.AcceptedEvent, error) {
	if t.landed() {
		return []domain.AcceptedEvent{t.answer}, nil
	}
	return nil, nil
}

func (t *liveTx) ListWaits(domain.ID) ([]domain.Wait, error) {
	w := t.wait
	if t.landed() {
		w.State = domain.WaitWoken
	}
	return []domain.Wait{w}, nil
}

func newLiveTx(t *testing.T) (*liveTx, Ask) {
	t.Helper()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	rec := Ask{ID: "a1", Scope: "eng", Work: "work/1", Execution: "work/1/attempt/2", Provider: "decisions", Gate: "fixture.deploy",
		Question: "Allow fixture.deploy?", Options: []string{OptionApprove, OptionRefuse}, Deadline: at.Add(2 * time.Hour), AskedAt: at, Sequence: 6, State: StateOpen}
	data, err := json.Marshal(AnswerPayload{Ask: rec.ID, Answer: OptionApprove, Responder: "operator:local", Operation: rec.Gate})
	if err != nil {
		t.Fatal(err)
	}
	key := rec.Key()
	tx := &liveTx{
		kv:   map[string][]byte{"work/" + string(rec.Work): []byte(rec.ID)},
		work: domain.Work{ID: rec.Work, ScopeID: rec.Scope, State: domain.WorkWaiting},
		wait: domain.Wait{ID: "wait/1", WorkID: rec.Work, ExecutionID: rec.Execution, State: domain.WaitActive,
			Match: domain.EventMatch{Types: []string{TypeAnswered}}, CreatedAt: at.Add(time.Second)},
		answer: domain.AcceptedEvent{Sequence: 7, Event: domain.Event{ID: "action/1", Type: TypeAnswer, Source: extension.ActionSource,
			DeliveryID: rec.Provider + "/" + ActionAnswer + "/1", CorrelationKey: &key, ReceivedAt: at.Add(2 * time.Second),
			Data: domain.Payload{Schema: SchemaAnswer, Data: data}}},
	}
	if err := putAsk(tx, rec); err != nil {
		t.Fatal(err)
	}
	return tx, rec
}

// An approval that lands while an ask is being settled stays an approval:
// the wait it woke is not read as the asker having stopped waiting (#831).
func TestSettleKeepsAnAnswerThatLandsMidPass(t *testing.T) {
	tx, rec := newLiveTx(t)
	if err := settle(tx, rec.ID, rec.AskedAt.Add(3*time.Second), &SyncResult{}); err != nil {
		t.Fatal(err)
	}
	got, err := getAsk(tx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateAnswered || got.Answer == nil || got.Answer.Answer != OptionApprove {
		t.Fatalf("settled as %s with answer %+v, want the approval", got.State, got.Answer)
	}
}

// The requests a surface shows read the ask the same way.
func TestRequestKeepsAnAnswerThatLandsMidRead(t *testing.T) {
	tx, rec := newLiveTx(t)
	r, err := request(tx, rec, rec.AskedAt.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if r.State != extension.RequestAnswered {
		t.Fatalf("reported as %s, want answered", r.State)
	}
}

// Control: a wait woken with no answer at all is still a withdrawn ask.
func TestSettleWithdrawsAWaitWokenWithoutAnAnswer(t *testing.T) {
	tx, rec := newLiveTx(t)
	tx.wait.State = domain.WaitWoken
	tx.answer.Event.CorrelationKey = &domain.CorrelationKey{Source: Type, ExternalID: "ask/other"}
	if err := settle(tx, rec.ID, rec.AskedAt.Add(3*time.Second), &SyncResult{}); err != nil {
		t.Fatal(err)
	}
	got, err := getAsk(tx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateWithdrawn {
		t.Fatalf("settled as %s, want withdrawn", got.State)
	}
}
