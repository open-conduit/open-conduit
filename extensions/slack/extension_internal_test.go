package slack

import (
	"context"
	"errors"
	"strings"
	"testing"

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
