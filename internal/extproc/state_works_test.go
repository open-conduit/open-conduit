package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// worksStore answers the reads state.works, state.execution and
// state.delivery make, only through View: a Transact fails the test, so
// every state read is shown not to take the writer. It counts the Work
// records its reads return.
type worksStore struct {
	t      *testing.T
	works  []domain.Work
	xs     []domain.Execution
	events []domain.AcceptedEvent
	read   *int
	// listed counts ListWorks calls, which a state read must never make.
	listed *int
}

func (s worksStore) Transact(context.Context, storage.Guard, func(storage.Tx) error) error {
	s.t.Error("a state read took the writer")
	return storage.ErrInvalid
}

func (s worksStore) View(_ context.Context, fn func(storage.Tx) error) error {
	return fn(storage.ReadOnly(worksTx{s: s}))
}

type worksTx struct {
	storage.Tx
	s worksStore
}

func (t worksTx) ListWorks() ([]domain.Work, error) {
	*t.s.listed++
	return slices.Clone(t.s.works), nil
}

func (t worksTx) QueryWorks(q storage.WorkQuery) ([]domain.Work, error) {
	if !q.Order.Valid() {
		return nil, storage.ErrInvalid
	}
	page, _ := storage.SelectWorks(t.s.works, q)
	*t.s.read += len(page)
	return page, nil
}

func (t worksTx) Work(id domain.ID) (domain.Work, error) {
	for _, w := range t.s.works {
		if w.ID == id {
			*t.s.read++
			return w, nil
		}
	}
	return domain.Work{}, storage.ErrNotFound
}

func (t worksTx) WorkByCorrelation(k domain.CorrelationKey) (domain.Work, error) {
	for _, w := range t.s.works {
		if slices.Contains(w.CorrelationKeys, k) {
			*t.s.read++
			return w, nil
		}
	}
	return domain.Work{}, storage.ErrNotFound
}

func (t worksTx) Execution(id domain.ID) (domain.Execution, error) {
	for _, x := range t.s.xs {
		if x.ID == id {
			return x, nil
		}
	}
	return domain.Execution{}, storage.ErrNotFound
}

func (t worksTx) EventByDelivery(source, delivery string) (domain.AcceptedEvent, error) {
	for _, a := range t.s.events {
		if a.Event.Source == source && a.Event.DeliveryID == delivery {
			return a, nil
		}
	}
	return domain.AcceptedEvent{}, storage.ErrNotFound
}

func (t worksTx) Get(string, string) ([]byte, error) { return nil, storage.ErrNotFound }

// state.works selects and pages Work through the store's QueryWorks, never
// ListWorks, so a page costs the Work it selects (#707). The extension
// reads its configured scopes and its own kinds anywhere, as before, and a
// correlation key reads the one Work owning it.
func TestStateWorksQueriesThroughTheIndex(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var works []domain.Work
	for i := range 40 {
		state := domain.WorkCompleted
		if i%10 == 0 {
			state = domain.WorkRunning
		}
		works = append(works, domain.Work{ID: domain.ID(fmt.Sprintf("eng-%02d", i)), ScopeID: "eng", Kind: "other.kind", State: state,
			CorrelationKeys: []domain.CorrelationKey{{Source: "acme", ExternalID: fmt.Sprint(i)}},
			CreatedAt:       now.Add(time.Duration(i) * time.Minute), UpdatedAt: now.Add(time.Duration(i) * time.Minute)})
	}
	works = append(works,
		domain.Work{ID: "left", ScopeID: "gone", Kind: "acme.job", State: domain.WorkWaiting, CreatedAt: now, UpdatedAt: now},
		domain.Work{ID: "foreign", ScopeID: "gone", Kind: "other.kind", State: domain.WorkRunning,
			CorrelationKeys: []domain.CorrelationKey{{Source: "acme", ExternalID: "foreign"}}, CreatedAt: now, UpdatedAt: now})
	read, listed := 0, 0
	store := worksStore{t: t, works: works, read: &read, listed: &listed,
		xs: []domain.Execution{{ID: "x-own", WorkID: "eng-03"}, {ID: "x-foreign", WorkID: "foreign"}},
		events: []domain.AcceptedEvent{
			{Event: domain.Event{ID: "e-own", Source: "acme", DeliveryID: "d1"}, Sequence: 1, ScopeID: "eng"},
			{Event: domain.Event{ID: "e-foreign", Source: "acme", DeliveryID: "d2"}, Sequence: 2, ScopeID: "gone"},
		}}
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"},
		Provides: protocol.Provides{Routes: []protocol.RouteDecl{{Handler: "acme"}}, WorkKinds: []protocol.WorkKindDecl{{Kind: "acme.job"}}}}
	s, err := NewService(m, Deps{Store: store, Now: func() time.Time { return now }, Scopes: func() []domain.ID { return []domain.ID{"eng"} }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method string, p any) (any, error) {
		raw, _ := json.Marshal(p)
		return s.Handle(context.Background(), method, raw)
	}
	ids := func(out any) []string {
		var got []string
		for _, w := range out.([]protocol.Work) {
			got = append(got, w.ID)
		}
		return got
	}

	// Live Work: the scope's four running, and its own kind elsewhere.
	out, err := call("state.works", protocol.WorksParams(storage.WorkQuery{States: storage.LiveWorkStates}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(out); !slices.Equal(got, []string{"eng-00", "left", "eng-10", "eng-20", "eng-30"}) {
		t.Fatalf("live Work: %v", got)
	}
	if read > 6 {
		t.Fatalf("the live read returned %d records, want the live set", read)
	}

	// Pages in updated order: changed since eng-25, two at a time.
	var pages [][]string
	q := storage.WorkQuery{ScopeID: "eng", UpdatedSince: now.Add(25 * time.Minute), Order: storage.WorkUpdated, Limit: 2}
	for {
		out, err := call("state.works", protocol.WorksParams(q, nil))
		if err != nil {
			t.Fatal(err)
		}
		page := out.([]protocol.Work)
		if len(page) == 0 {
			break
		}
		pages = append(pages, ids(out))
		last := page[len(page)-1]
		q.After = &storage.WorkKey{At: last.UpdatedAt, ID: domain.ID(last.ID)}
	}
	if len(pages) != 8 || !slices.Equal(pages[0], []string{"eng-25", "eng-26"}) || !slices.Equal(pages[7], []string{"eng-39"}) {
		t.Fatalf("pages: %v", pages)
	}

	// A correlation key reads the Work owning it; another scope's is empty.
	if out, err := call("state.works", protocol.WorksParams(storage.WorkQuery{}, &domain.CorrelationKey{Source: "acme", ExternalID: "7"})); err != nil || !slices.Equal(ids(out), []string{"eng-07"}) {
		t.Fatalf("by correlation: %v %v", out, err)
	}
	if out, err := call("state.works", protocol.WorksParams(storage.WorkQuery{}, &domain.CorrelationKey{Source: "acme", ExternalID: "foreign"})); err != nil || len(ids(out)) != 0 {
		t.Fatalf("another scope's Work by correlation: %v %v", out, err)
	}

	// An unknown state or order is refused; another scope is unauthorized.
	if _, err := call("state.works", protocol.StateParams{States: []string{"stuck"}}); protocol.ErrorCode(err) != protocol.CodeInvalid {
		t.Fatalf("unknown state: %v", err)
	}
	if _, err := call("state.works", protocol.StateParams{Order: "sideways"}); protocol.ErrorCode(err) != protocol.CodeInvalid {
		t.Fatalf("unknown order: %v", err)
	}
	if _, err := call("state.works", protocol.StateParams{ScopeID: "gone"}); protocol.ErrorCode(err) != protocol.CodeUnauthorized {
		t.Fatalf("another scope: %v", err)
	}

	// One attempt and one Event by their own identity, scoped the same way.
	if out, err := call("state.execution", protocol.StateParams{ID: "x-own"}); err != nil || out.(protocol.Execution).WorkID != "eng-03" {
		t.Fatalf("state.execution: %v %v", out, err)
	}
	if _, err := call("state.execution", protocol.StateParams{ID: "x-foreign"}); !errors.Is(err, storage.ErrNotFound) && protocol.ErrorCode(err) != protocol.CodeNotFound {
		t.Fatalf("another scope's attempt: %v, want not found", err)
	}
	if out, err := call("state.delivery", protocol.StateParams{Source: "acme", DeliveryID: "d1"}); err != nil || out.(protocol.AcceptedEvent).Event.ID != "e-own" {
		t.Fatalf("state.delivery: %v %v", out, err)
	}
	if _, err := call("state.delivery", protocol.StateParams{Source: "acme", DeliveryID: "d2"}); protocol.ErrorCode(err) != protocol.CodeNotFound {
		t.Fatalf("another scope's Event by delivery: %v, want not found", err)
	}
	if listed != 0 {
		t.Fatalf("state reads listed every Work %d times", listed)
	}
}
