package slack_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSocketModeAcknowledgesOnlyAcceptedDeliveries: Socket Mode connects
// with the app token, hands each events_api envelope to push and
// acknowledges it only after push accepted it; it answers pings and
// reconnects after a refresh request or a dropped connection.
func TestSocketModeAcknowledgesOnlyAcceptedDeliveries(t *testing.T) {
	fake := slacktest.New(team, "xoxb-test")
	defer fake.Close()
	fake.AddAppToken("xapp-test")
	fake.Join(chanA, bob)
	cfg, _ := slack.ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	client, err := slack.NewClient(cfg, func(_ context.Context, name string) (string, error) {
		if name == slack.SecretAppToken {
			return "xapp-test", nil
		}
		return "xoxb-test", nil
	}, slack.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	in := slack.NewIngress(settings(t, ""), client, nil, slack.IngressOptions{})
	var mu sync.Mutex
	var accepted []extension.PushResult
	refuse := false
	push := func(ctx context.Context, d extension.Delivery) error {
		mu.Lock()
		defer mu.Unlock()
		if refuse {
			return errors.New("not accepted")
		}
		res, err := in.Push(ctx, src, d)
		if err != nil {
			return err
		}
		accepted = append(accepted, res)
		return nil
	}
	sock := slack.NewSocket(client, slack.SocketOptions{MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sock.Run(ctx, push); close(done) }()
	until(t, "connected", func() bool { return sock.Status().Connected && fake.SocketsOpen() == 1 })

	ts := fake.Post(chanA, alice, "printer down")
	fake.SocketPing()
	env := fake.SocketSend(fake.MessageEvent(chanA, ts))
	until(t, "acknowledged", func() bool { return fake.Acked(env) })
	mu.Lock()
	if len(accepted) != 1 || len(accepted[0].Observations) != 1 || accepted[0].Observations[0].Event.Type != slack.TypeMessageHuman {
		t.Fatalf("accepted: %+v", accepted)
	}
	refuse = true
	mu.Unlock()
	unacked := fake.SocketSend(fake.MessageEvent(chanA, ts))
	until(t, "refused delivery counted", func() bool { return sock.Status().Unacked == 1 })
	if fake.Acked(unacked) {
		t.Fatal("an unaccepted envelope was acknowledged")
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	// Slack's refresh and a dropped connection both reconnect.
	fake.SocketDisconnect()
	until(t, "reconnected after refresh", func() bool { return fake.SocketConnects() == 2 && sock.Status().Connected })
	fake.SocketDrop()
	until(t, "reconnected after drop", func() bool { return fake.SocketConnects() == 3 && sock.Status().Connected })
	env = fake.SocketSend(fake.MessageEvent(chanA, ts))
	until(t, "acknowledged after reconnect", func() bool { return fake.Acked(env) })
	cancel()
	<-done
	if sock.Status().Connected {
		t.Fatal("still connected after stop")
	}
	if st := sock.Status(); st.Acked != 2 || st.Connects != 3 {
		t.Fatalf("status %+v", st)
	}
}

func TestSocketModeBadAppTokenBacksOff(t *testing.T) {
	fake := slacktest.New(team, "xoxb-test")
	defer fake.Close()
	cfg, _ := slack.ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	client, _ := slack.NewClient(cfg, func(context.Context, string) (string, error) { return "xapp-wrong", nil }, slack.ClientOptions{})
	sock := slack.NewSocket(client, slack.SocketOptions{MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sock.Run(ctx, func(context.Context, extension.Delivery) error { return nil }); close(done) }()
	until(t, "credential failure reported", func() bool { return sock.Status().LastError == "credential" })
	cancel()
	<-done
	if fake.SocketConnects() != 0 {
		t.Fatal("connected with a bad token")
	}
}
