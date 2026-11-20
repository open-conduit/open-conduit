package bootstrap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

const forwarderSecret = "forwarder-secret"

// forwarderDaemon is a daemon whose listener sits behind forwarder edge,
// with a fixed clock and its secret in the environment.
func forwarderDaemon(now time.Time) (*daemon, *config.Forwarder) {
	d := tunnelDaemon()
	d.now = func() time.Time { return now }
	d.resolver = &config.Resolver{LookupEnv: func(k string) (string, bool) {
		return forwarderSecret, k == "OC_FORWARDER"
	}}
	return d, &config.Forwarder{Name: "edge", Secret: "env:OC_FORWARDER", MaxAgeSeconds: 3600}
}

// forwarded is a request to path as forwarder edge sends it, with the
// headers given ("" leaves one out).
func forwarded(path, body, at, id, sig string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range map[string]string{extension.ForwardedAtHeader: at, extension.ForwardIDHeader: id, extension.ForwardSignatureHeader: sig} {
		if v != "" {
			r.Header.Set(k, v)
		}
	}
	return r
}

// A request the forwarder signed reaches the package with the receipt time
// and delivery ID it attested, its body intact and its headers removed;
// one with no MAC, a bad MAC, a tampered time or ID, a time older than the
// bound or in the future, never does.
func TestForwarderAuthenticatesEachRequest(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	d, f := forwarderDaemon(now)
	const path, body = "/slack/acme/team?x=1", `{"type":"event_callback"}`
	held := now.Add(-10 * time.Minute).Format(time.RFC3339)
	sign := func(at, id string) string {
		return extension.ForwardSignature(forwarderSecret, http.MethodPost, path, at, id, []byte(body))
	}
	serve := func(r *http.Request) (int, extension.WebhookReceipt, *http.Request) {
		w := httptest.NewRecorder()
		rc, ok := d.receive(w, r, f)
		if !ok {
			return w.Code, rc, nil
		}
		return http.StatusOK, rc, r
	}
	code, rc, r := serve(forwarded(path, body, held, "msg-1", sign(held, "msg-1")))
	if code != http.StatusOK || !rc.ReceivedAt.Equal(now.Add(-10*time.Minute)) || rc.ForwardID != "msg-1" {
		t.Fatalf("signed: %d %+v", code, rc)
	}
	if got, _ := io.ReadAll(r.Body); string(got) != body || r.Header.Get(extension.ForwardSignatureHeader) != "" || r.Header.Get(extension.ForwardedAtHeader) != "" || r.Header.Get(extension.ForwardIDHeader) != "" {
		t.Fatalf("passed on: body %q, header %v", got, r.Header)
	}
	stale := now.Add(-time.Hour - time.Second).Format(time.RFC3339)
	ahead := now.Add(6 * time.Minute).Format(time.RFC3339)
	later := now.Add(-time.Minute).Format(time.RFC3339)
	for name, c := range map[string]struct {
		r    *http.Request
		want int
		code forwardRefusal
	}{
		"no MAC":           {forwarded(path, body, held, "msg-1", ""), http.StatusUnauthorized, forwardUnsigned},
		"no time":          {forwarded(path, body, "", "msg-1", sign(held, "msg-1")), http.StatusUnauthorized, forwardUnsigned},
		"bad MAC":          {forwarded(path, body, held, "msg-1", extension.ForwardSignature("wrong", http.MethodPost, path, held, "msg-1", []byte(body))), http.StatusUnauthorized, forwardBadMAC},
		"tampered time":    {forwarded(path, body, later, "msg-1", sign(held, "msg-1")), http.StatusUnauthorized, forwardBadMAC},
		"tampered ID":      {forwarded(path, body, held, "msg-2", sign(held, "msg-1")), http.StatusUnauthorized, forwardBadMAC},
		"tampered body":    {forwarded(path, `{"type":"other"}`, held, "msg-1", sign(held, "msg-1")), http.StatusUnauthorized, forwardBadMAC},
		"another path":     {forwarded("/slack/acme/other?x=1", body, held, "msg-1", sign(held, "msg-1")), http.StatusUnauthorized, forwardBadMAC},
		"older than bound": {forwarded(path, body, stale, "msg-1", sign(stale, "msg-1")), http.StatusUnauthorized, forwardStale},
		"in the future":    {forwarded(path, body, ahead, "msg-1", sign(ahead, "msg-1")), http.StatusUnauthorized, forwardFuture},
		"not a time":       {forwarded(path, body, "yesterday", "msg-1", sign("yesterday", "msg-1")), http.StatusUnauthorized, forwardBadTime},
		"bad ID":           {forwarded(path, body, held, "msg 1", sign(held, "msg 1")), http.StatusUnauthorized, forwardBadID},
	} {
		if code, _, _ := serve(c.r); code != c.want || d.hooks.forwardLast != string(c.code) {
			t.Errorf("%s: %d (%s), want %d (%s)", name, code, d.hooks.forwardLast, c.want, c.code)
		}
	}
	d.resolver = &config.Resolver{LookupEnv: func(string) (string, bool) { return "", false }}
	if code, _, _ := serve(forwarded(path, body, held, "msg-1", sign(held, "msg-1"))); code != http.StatusServiceUnavailable {
		t.Fatalf("secret unavailable: %d, want 503 so the forwarder holds it", code)
	}
}

// The listener refuses a request its forwarder didn't sign before it looks
// for a route, so no package sees one; a signed request to no route is the
// usual 404.
func TestForwarderRefusesBeforeAnyRoute(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	d, f := forwarderDaemon(now)
	d.hooks.forwarder = f
	h := d.webhookHandler()
	at := now.Format(time.RFC3339)
	for name, c := range map[string]struct {
		r    *http.Request
		want int
	}{
		"unsigned": {forwarded("/nowhere/x", "{}", "", "", ""), http.StatusUnauthorized},
		"signed":   {forwarded("/nowhere/x", "{}", at, "m-1", extension.ForwardSignature(forwarderSecret, http.MethodPost, "/nowhere/x", at, "m-1", []byte("{}"))), http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, c.r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", name, w.Code, c.want)
		}
	}
	if !strings.Contains(d.webhookCheckDetail(), "only forwarder edge is accepted (1 forwarded, 1 refused, last for unsigned)") {
		t.Fatalf("doctor: %s", d.webhookCheckDetail())
	}
}

// With no forwarder configured nothing changes: the receipt is the
// listener's own clock, and a forwarder's headers, which nothing verified,
// are ignored and removed.
func TestNoForwarderIgnoresForwarderClaims(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	d, _ := forwarderDaemon(now)
	held := now.Add(-time.Hour).Format(time.RFC3339)
	r := forwarded("/slack/acme/team", "{}", held, "msg-1", "v1=00")
	rc, ok := d.receive(httptest.NewRecorder(), r, nil)
	if !ok || !rc.ReceivedAt.Equal(now) || rc.ForwardID != "" || r.Header.Get(extension.ForwardedAtHeader) != "" {
		t.Fatalf("receipt %+v, header %v", rc, r.Header)
	}
}

// With the probe off for a static public URL the daemon starts no probe,
// so it makes no request through the URL, and doctor's webhooks check is
// ok and says so; with it on, the probe runs.
func TestStaticURLWithTheProbeOff(t *testing.T) {
	for _, off := range []bool{true, false} {
		tick := manualProbes(t)
		d := tunnelDaemon()
		probed := make(chan struct{}, 1)
		d.hooks.probe = func(context.Context, string, string) error {
			probed <- struct{}{}
			return nil
		}
		stop, err := d.startWebhooks(context.Background(), config.Webhooks{Addr: "127.0.0.1:0", PublicURL: "https://hooks.example.test", NoProbe: off})
		if err != nil {
			t.Fatal(err)
		}
		if off {
			select {
			case tick <- time.Now():
				t.Fatal("a probe is running with probe off")
			default:
			}
			check := d.webhookCheck(context.Background())
			if check.Status != runtime.CheckOK || !strings.Contains(check.Detail, "public https://hooks.example.test (static, from settings.webhooks); not probed (settings.webhooks.probe is false)") {
				t.Fatalf("doctor: %+v", check)
			}
		} else {
			tick <- time.Now()
			<-probed
		}
		stop()
	}
}

func (d *daemon) webhookCheckDetail() string {
	d.hooks.mu.Lock()
	d.hooks.listening = "127.0.0.1:1"
	d.hooks.mu.Unlock()
	return d.webhookCheck(context.Background()).Detail
}
