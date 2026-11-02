package extproc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// waitsStore answers the reads state.waits makes: Work by ID and a Work's
// Waits.
type waitsStore struct {
	works map[domain.ID]domain.Work
	waits map[domain.ID][]domain.Wait
}

func (s waitsStore) Transact(_ context.Context, _ storage.Guard, fn func(storage.Tx) error) error {
	return fn(waitsTx{s: s})
}

type waitsTx struct {
	storage.Tx
	s waitsStore
}

func (t waitsTx) Work(id domain.ID) (domain.Work, error) {
	w, ok := t.s.works[id]
	if !ok {
		return w, storage.ErrNotFound
	}
	return w, nil
}

func (t waitsTx) ListWaits(work domain.ID) ([]domain.Wait, error) { return t.s.waits[work], nil }

func (t waitsTx) LatestSequence() (uint64, error) { return 42, nil }

// state.waits answers a Work's Waits only for a Work in one of the
// extension's scopes; another scope's Work is not found, so its Waits
// (descriptions, progress, match keys) never cross (#428).
func TestStateWaitsIsScoped(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	wait := func(work domain.ID) domain.Wait {
		return domain.Wait{ID: "wait-" + work, WorkID: work, ExecutionID: "x-" + work, State: domain.WaitActive, Description: "the answer on " + string(work),
			Match: domain.EventMatch{Keys: []domain.CorrelationKey{{Source: "acme", ExternalID: "q"}}, Types: []string{"acme.answered"}}, Revision: 2, CreatedAt: now, UpdatedAt: now}
	}
	store := waitsStore{
		works: map[domain.ID]domain.Work{"own": {ID: "own", ScopeID: "eng"}, "foreign": {ID: "foreign", ScopeID: "other"}},
		waits: map[domain.ID][]domain.Wait{"own": {wait("own")}, "foreign": {wait("foreign")}},
	}
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"}}
	s, err := NewService(m, Deps{Store: store, Now: func() time.Time { return now }, Scopes: func() []domain.ID { return []domain.ID{"eng"} }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(work string) (any, error) {
		raw, _ := json.Marshal(protocol.StateParams{ID: work})
		return s.Handle(context.Background(), "state.waits", raw)
	}
	out, err := call("own")
	if err != nil {
		t.Fatal(err)
	}
	list, ok := out.([]protocol.Wait)
	if !ok || len(list) != 1 {
		t.Fatalf("state.waits of an own Work: %#v", out)
	}
	if got, want := list[0].Domain(), wait("own"); got.ID != want.ID || got.Description != want.Description || got.Match.Keys[0] != want.Match.Keys[0] || got.Match.Types[0] != want.Match.Types[0] || got.Revision != want.Revision || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("the Wait read back: %+v, want %+v", got, want)
	}
	if _, err := call("foreign"); protocol.ErrorCode(err) != protocol.CodeNotFound {
		t.Fatalf("state.waits of another scope's Work: %v, want not_found", err)
	}
	if _, err := call("missing"); err == nil {
		t.Fatal("state.waits of no Work answered")
	}
}

// state.sequence answers the log's latest sequence, a cursor for
// state.events, in one call rather than a read of the whole log (#428).
func TestStateSequenceIsTheLatest(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"}}
	s, err := NewService(m, Deps{Store: waitsStore{}, Now: time.Now, Scopes: func() []domain.ID { return []domain.ID{"eng"} }})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Handle(context.Background(), "state.sequence", json.RawMessage(`{}`))
	if r, ok := out.(protocol.StateSequenceResult); err != nil || !ok || r.Sequence != 42 {
		t.Fatalf("state.sequence: %#v %v", out, err)
	}
}

func (t waitsTx) ListWorks() ([]domain.Work, error) {
	var out []domain.Work
	for _, w := range t.s.works {
		out = append(out, w)
	}
	return out, nil
}

// An extension reads and cancels Work of its own declared kinds in any
// scope, so it can retire Work left where its entry was removed (#428);
// another kind's Work outside its scopes stays not found and uncancellable.
func TestOwnWorkKindsReachEveryScope(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := waitsStore{works: map[domain.ID]domain.Work{
		"here":    {ID: "here", ScopeID: "eng", Kind: "other.kind"},
		"left":    {ID: "left", ScopeID: "gone", Kind: "acme.job"},
		"foreign": {ID: "foreign", ScopeID: "gone", Kind: "other.kind"},
	}}
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"},
		Provides: protocol.Provides{Routes: []protocol.RouteDecl{{Handler: "acme"}}, WorkKinds: []protocol.WorkKindDecl{{Kind: "acme.job"}}}}
	var cancelled []domain.ID
	s, err := NewService(m, Deps{Store: store, Now: func() time.Time { return now }, Scopes: func() []domain.ID { return []domain.ID{"eng"} },
		CancelWork: func(_ context.Context, w domain.ID, _ domain.Revision, _, _ string) (domain.ID, error) {
			cancelled = append(cancelled, w)
			return "cancel-" + w, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method string, p any) (any, error) {
		raw, _ := json.Marshal(p)
		return s.Handle(context.Background(), method, raw)
	}
	out, err := call("state.works", protocol.StateParams{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, w := range out.([]protocol.Work) {
		got[w.ID] = true
	}
	if !got["here"] || !got["left"] || got["foreign"] {
		t.Fatalf("state.works: %v, want here and left", got)
	}
	if _, err := call("state.work", protocol.StateParams{ID: "left"}); err != nil {
		t.Fatalf("state.work of its own kind elsewhere: %v", err)
	}
	if _, err := call("state.work", protocol.StateParams{ID: "foreign"}); protocol.ErrorCode(err) != protocol.CodeNotFound {
		t.Fatalf("state.work of another kind elsewhere: %v, want not_found", err)
	}
	if _, err := call("work.cancel", protocol.WorkCancelParams{WorkID: "left", Reason: "removed", Key: "k"}); err != nil {
		t.Fatalf("work.cancel of its own kind elsewhere: %v", err)
	}
	for _, id := range []string{"foreign", "here"} {
		if _, err := call("work.cancel", protocol.WorkCancelParams{WorkID: id, Reason: "removed", Key: "k"}); protocol.ErrorCode(err) != protocol.CodeUnauthorized {
			t.Fatalf("work.cancel of %s: %v, want unauthorized", id, err)
		}
	}
	if len(cancelled) != 1 || cancelled[0] != "left" {
		t.Fatalf("cancelled %v, want left", cancelled)
	}
}
