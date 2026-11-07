package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// manualProbes makes the probes fire only when the test sends on the
// returned channel, so no test waits on real time.
func manualProbes(t *testing.T) chan time.Time {
	t.Helper()
	tick := make(chan time.Time)
	old := probeTicker
	probeTicker = func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }
	t.Cleanup(func() { probeTicker = old })
	return tick
}

// syncBuffer is a log the daemon's goroutines write to while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type capturedEvents struct {
	mu    sync.Mutex
	names []string
}

func (c *capturedEvents) Capture(_ context.Context, _, name string, _ time.Time, _ map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
	return nil
}

func (c *capturedEvents) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.names, ",")
}

// The listener answers the probe with its token and nothing else; the
// probe fails on anything but that answer.
func TestProbePublicNeedsThisListenersAnswer(t *testing.T) {
	d := tunnelDaemon()
	d.hooks.probeToken = "tok"
	srv := httptest.NewServer(d.webhookHandler())
	defer srv.Close()
	if err := probePublic(context.Background(), srv.URL, "tok"); err != nil {
		t.Fatalf("own listener: %v", err)
	}
	if err := probePublic(context.Background(), srv.URL, "other"); err == nil || err.Error() != "answered 404" {
		t.Fatalf("wrong token: %v", err)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "failed to connect to host", http.StatusBadGateway)
	}))
	defer dead.Close()
	if err := probePublic(context.Background(), dead.URL, "tok"); err == nil || err.Error() != "answered 502" {
		t.Fatalf("dead tunnel: %v", err)
	}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }))
	defer elsewhere.Close()
	if err := probePublic(context.Background(), elsewhere.URL, "tok"); err == nil || !strings.Contains(err.Error(), "not from this daemon") {
		t.Fatalf("another server: %v", err)
	}
	d.hooks.probeToken = ""
	if err := probePublic(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("a listener with no token answered the probe")
	}
}

// A tunnel whose process keeps running but whose URL stops answering is
// ended after tunnelProbeFailures failed probes and started again; the new
// run's URL answers and becomes the public URL. The outage is in the log
// as a warning.
func TestSuperviseTunnelRestartsARunWhoseURLStopsAnswering(t *testing.T) {
	defer func(b time.Duration) { tunnelBackoff = b }(tunnelBackoff)
	tunnelBackoff = 0
	tick := manualProbes(t)
	log := &syncBuffer{}
	d := tunnelDaemon()
	d.log = slog.New(slog.NewTextHandler(log, nil))
	d.hooks.probe = func(_ context.Context, public, _ string) error {
		if public == "https://one.test" {
			return errors.New("answered 502")
		}
		return nil
	}
	f := &fakeTunnel{seen: make(chan extension.TunnelRun, 4), runs: []fakeRun{
		{urls: []string{"https://one.test"}, err: errHold},
		{urls: []string{"https://two.test"}, err: errHold},
	}}
	d.hooks.addTunnel("cloudflared", f)
	d.hooks.listening, d.hooks.tunnel = "127.0.0.1:1", "cloudflared"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.superviseTunnel(ctx, "cloudflared", extension.TunnelRun{Addr: "127.0.0.1:1"})
	}()
	<-f.seen
	deadline := time.After(10 * time.Second)
	for second := false; ; {
		select {
		case tick <- time.Time{}:
		case <-f.seen:
			second = true
		case <-deadline:
			t.Fatalf("no second run; log:\n%s", log)
		}
		d.hooks.mu.Lock()
		ok := second && !d.hooks.probed.IsZero()
		d.hooks.mu.Unlock()
		if ok {
			break
		}
	}
	c := d.webhookCheck(context.Background())
	cancel()
	<-done
	if c.Status != runtime.CheckOK || !strings.Contains(c.Detail, "public https://two.test") || !strings.Contains(c.Detail, "answered a probe at") {
		t.Fatalf("check %+v", c)
	}
	if d.hooks.failures != 1 || d.hooks.starts != 2 {
		t.Fatalf("starts %d failures %d", d.hooks.starts, d.hooks.failures)
	}
	for _, want := range []string{`level=WARN msg="webhook public URL unreachable" provider=cloudflared url=https://one.test probes=3`, `msg="tunnel stopped; restarting" provider=cloudflared code=tunnel_unreachable`} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// A static public URL that stops answering can't be restarted: doctor
// warns, a diagnostic and a telemetry event record it, and all of that
// clears once it answers again.
func TestWatchPublicReportsAStaticURLUntilItAnswers(t *testing.T) {
	tick := manualProbes(t)
	d := tunnelDaemon()
	events := &capturedEvents{}
	d.telemetry = telemetryFanout{primary: events}
	answering := false
	var mu sync.Mutex
	d.hooks.probe = func(context.Context, string, string) error {
		mu.Lock()
		defer mu.Unlock()
		if answering {
			return nil
		}
		return errors.New("no answer within 15s")
	}
	d.hooks.listening, d.hooks.tunnel, d.hooks.tunnelFrom = "127.0.0.1:1", staticTunnel, fromSettings
	d.publicURL(staticTunnel, "https://hooks.example.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deaths := make(chan string, 4)
	go d.watchPublic(ctx, staticTunnel, nil, func(cause string) { deaths <- cause })
	for range tunnelProbeFailures {
		tick <- time.Time{}
	}
	cause := <-deaths
	if !strings.Contains(cause, "https://hooks.example.test is not answering: 3 probes in a row failed (last: no answer within 15s)") {
		t.Fatalf("cause %q", cause)
	}
	if c := d.webhookCheck(context.Background()); c.Status != runtime.CheckWarn || c.Code != "tunnel_unreachable" || !strings.Contains(c.Detail, "check what routes it to the listener") {
		t.Fatalf("check %+v", c)
	}
	if code := d.reportedCode("tunnel", staticTunnel); code != "tunnel_unreachable" {
		t.Fatalf("diagnostic %q", code)
	}
	mu.Lock()
	answering = true
	mu.Unlock()
	tick <- time.Time{}
	tick <- time.Time{} // the first tick's probe has finished once this one is taken
	if c := d.webhookCheck(context.Background()); c.Status != runtime.CheckOK || !strings.Contains(c.Detail, "answered a probe at") {
		t.Fatalf("check after recovery %+v", c)
	}
	if code := d.reportedCode("tunnel", staticTunnel); code != "" {
		t.Fatalf("diagnostic after recovery %q", code)
	}
	if got := events.all(); got != "webhooks.public_url_unreachable,webhooks.public_url_answering" {
		t.Fatalf("telemetry %q", got)
	}
	if len(deaths) != 0 {
		t.Fatal("reported unreachable twice")
	}
}

// doctor says when a delivery last reached a package's route.
func TestWebhookCheckShowsTheLastDelivery(t *testing.T) {
	d := tunnelDaemon()
	d.hooks.listening = "127.0.0.1:1"
	if c := d.webhookCheck(context.Background()); !strings.Contains(c.Detail, "no deliveries since serve started") {
		t.Fatalf("check %+v", c)
	}
	d.hooks.delivered = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	if c := d.webhookCheck(context.Background()); !strings.Contains(c.Detail, "last delivery 2026-11-07T09:00:00Z") {
		t.Fatalf("check %+v", c)
	}
}
