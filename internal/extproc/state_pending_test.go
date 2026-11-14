package extproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// pendingStore answers the reads state.pending makes: Work by ID, the
// routing queue's rows (the runtime's own KV namespace) and Events by ID.
type pendingStore struct {
	works  map[domain.ID]domain.Work
	queue  map[string][]byte
	events map[domain.ID]domain.AcceptedEvent
	// unrouted are accepted Events the router has yet to route.
	unrouted []domain.AcceptedEvent
}

func (s pendingStore) Transact(_ context.Context, _ storage.Guard, fn func(storage.Tx) error) error {
	return fn(pendingTx{s: s})
}

type pendingTx struct {
	storage.Tx
	s pendingStore
}

func (t pendingTx) Work(id domain.ID) (domain.Work, error) {
	w, ok := t.s.works[id]
	if !ok {
		return w, storage.ErrNotFound
	}
	return w, nil
}

func (t pendingTx) Event(id domain.ID) (domain.AcceptedEvent, error) {
	a, ok := t.s.events[id]
	if !ok {
		return a, storage.ErrNotFound
	}
	return a, nil
}

func (t pendingTx) UnroutedEvents(uint64, int) ([]domain.AcceptedEvent, error) {
	return t.s.unrouted, nil
}

func (t pendingTx) Get(ns, key string) ([]byte, error) {
	if v, ok := t.s.queue[key]; ns == "core.routing.queue" && ok {
		return v, nil
	}
	return nil, storage.ErrNotFound
}

func (t pendingTx) List(ns, prefix string) ([]storage.Entry, error) {
	var out []storage.Entry
	for k, v := range t.s.queue {
		if ns == "core.routing.queue" && strings.HasPrefix(k, prefix) {
			out = append(out, storage.Entry{Key: k, Value: v})
		}
	}
	return out, nil
}

// queueRow is a routing queue row as the runtime writes one: keyed by the
// Work's identity and the input's sequence.
func queueRow(work domain.ID, seq uint64, event domain.ID) (string, []byte) {
	h := sha256.New()
	fmt.Fprintf(h, "%d:", len(work))
	h.Write([]byte(work))
	v, _ := json.Marshal(map[string]any{"EventID": event, "Sequence": seq, "Binding": "builder"})
	return hex.EncodeToString(h.Sum(nil)) + fmt.Sprintf("/%020d", seq), v
}

// state.pending answers the input for a Work that no attempt has read,
// queued or accepted and not yet routed, so an extension can tell Work that
// has read everything from Work about to start, both ready (#608). Another
// scope's Work is not found.
func TestStatePendingReadsTheQueueInScope(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	k1, v1 := queueRow("busy", 7, "comment")
	k2, v2 := queueRow("foreign", 8, "other")
	store := pendingStore{
		works: map[domain.ID]domain.Work{
			"busy": {ID: "busy", ScopeID: "eng", State: domain.WorkReady}, "idle": {ID: "idle", ScopeID: "eng", State: domain.WorkReady},
			"foreign": {ID: "foreign", ScopeID: "other", State: domain.WorkReady},
			"late":    {ID: "late", ScopeID: "eng", State: domain.WorkReady},
		},
		// Accepted, not routed yet, and bound for late.
		unrouted: []domain.AcceptedEvent{{Sequence: 9, Event: domain.Event{ID: "fresh", Type: "github.comment.human", WorkID: "late"}}},
		queue:    map[string][]byte{k1: v1, k2: v2},
		events:   map[domain.ID]domain.AcceptedEvent{"comment": {Sequence: 7, Event: domain.Event{ID: "comment", Type: "github.comment.human"}}},
	}
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"}}
	s, err := NewService(m, Deps{Store: store, Now: func() time.Time { return now }, Scopes: func() []domain.ID { return []domain.ID{"eng"} }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(work string) ([]protocol.PendingInput, error) {
		raw, _ := json.Marshal(protocol.StateParams{ID: work})
		out, err := s.Handle(context.Background(), "state.pending", raw)
		if err != nil {
			return nil, err
		}
		return out.([]protocol.PendingInput), nil
	}
	busy, err := call("busy")
	if err != nil || len(busy) != 1 || busy[0] != (protocol.PendingInput{EventID: "comment", Sequence: 7, Type: "github.comment.human", Binding: "builder"}) {
		t.Fatalf("state.pending of Work with a comment queued: %#v %v", busy, err)
	}
	if idle, err := call("idle"); err != nil || len(idle) != 0 || idle == nil {
		t.Fatalf("state.pending of Work that has read everything: %#v %v, want an empty list", idle, err)
	}
	if late, err := call("late"); err != nil || len(late) != 1 || late[0] != (protocol.PendingInput{EventID: "fresh", Sequence: 9, Type: "github.comment.human", Unrouted: true}) {
		t.Fatalf("state.pending of Work with a comment not yet routed: %#v %v", late, err)
	}
	if _, err := call("foreign"); protocol.ErrorCode(err) != protocol.CodeNotFound {
		t.Fatalf("state.pending of another scope's Work: %v, want not_found", err)
	}
}
