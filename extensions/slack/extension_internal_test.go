package slack

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// A Socket Mode receiver is not started by configure but by the sockets
// task, which runs once the host has registered the entry's source; and a
// delivery the host refuses (an unregistered source) fails the entry's
// doctor check instead of leaving it ok.
func TestSocketsStartAfterRegistrationAndRefusalsShow(t *testing.T) {
	refused := errors.New("no such source")
	x := New()
	x.h = inproc.Host{Deliver: func(context.Context, string, extension.Delivery) error { return refused }}
	client, err := NewClient(ConnectionConfig{APIURL: "http://127.0.0.1:1"}, func(context.Context, string) (string, error) { return "", ErrCredential }, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{name: "slack/eng/e", settings: Settings{Namespace: "slack/T1", Receiver: ReceiverSocket}, client: client, socket: NewSocket(client, SocketOptions{})}
	x.sources[s.settings.Namespace] = s
	if s.started {
		t.Fatal("a receiver started before the sockets task")
	}
	if err := x.push(s)(context.Background(), extension.Delivery{}); !errors.Is(err, refused) {
		t.Fatalf("push: %v", err)
	}
	if c := x.check(context.Background(), protocol.CheckConfig{}); c.Status != inproc.CheckFail || c.Code != "deliver_failed" || !strings.Contains(strings.Join(c.Items, " "), "deliveries refused") {
		t.Fatalf("check after a refused delivery: %+v", c)
	}
	x.h.Deliver = func(context.Context, string, extension.Delivery) error { return nil }
	_ = x.push(s)(context.Background(), extension.Delivery{})
	if c := x.check(context.Background(), protocol.CheckConfig{}); c.Code == "deliver_failed" {
		t.Fatalf("a refusal outlived a delivery that succeeded: %+v", c)
	}
	x.Stop()
	if err := x.startSockets(context.Background()); err != nil || s.started {
		t.Fatalf("a receiver started after Stop: %v", err)
	}
}

// claimTx is the slice of a storage.Tx the claim records use.
type claimTx struct {
	storage.Tx
	kv    map[string][]byte
	works map[domain.ID]bool
}

func (t *claimTx) Get(ns, key string) ([]byte, error) {
	if v, ok := t.kv[ns+"|"+key]; ok {
		return v, nil
	}
	return nil, storage.ErrNotFound
}
func (t *claimTx) Put(ns, key string, v []byte) error { t.kv[ns+"|"+key] = v; return nil }
func (t *claimTx) Work(id domain.ID) (domain.Work, error) {
	if t.works[id] {
		return domain.Work{ID: id}, nil
	}
	return domain.Work{}, storage.ErrNotFound
}

type claimStore struct{ tx *claimTx }

func (s claimStore) Transact(_ context.Context, _ storage.Guard, fn func(storage.Tx) error) error {
	return fn(s.tx)
}

// A thread is recorded as an earlier daemon's only when a marker in it
// names Work this daemon has no record of: a reply of another entry's
// Work that exists here (two entries sharing a channel) records nothing.
func TestForeignRecordsOnlyWorkUnknownHere(t *testing.T) {
	tx := &claimTx{kv: map[string][]byte{}, works: map[domain.ID]bool{"work/other-entry": true}}
	x := New()
	x.h = inproc.Host{Store: claimStore{tx}}
	s := Settings{Namespace: "slack/T1"}
	got, err := x.foreign(s)(context.Background(), "C1", map[string][]string{
		"1700000000.000001": {"work/other-entry"},
		"1700000000.000002": {"work/gone"},
		"1700000000.000003": {"work/other-entry", "work/gone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "1700000000.000002,1700000000.000003" {
		t.Fatalf("recorded %v", got)
	}
	if x.book.Foreign(s.Namespace, "C1", "1700000000.000001") || !x.book.Foreign(s.Namespace, "C1", "1700000000.000002") {
		t.Fatal("the book disagrees with the records")
	}
	if _, err := tx.Get(ClaimNamespace, foreignKey(s.Namespace, "C1", "1700000000.000001")); err == nil {
		t.Fatal("another entry's thread was recorded durably")
	}
}
