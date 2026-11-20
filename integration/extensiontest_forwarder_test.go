package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestExtensiontestForwardedWebhook: behind a forwarder
// (settings.webhooks.forwarder) the listener serves only requests the
// forwarder signed, and a third-party package reads the receipt time and
// delivery ID the forwarder attested from its request, without the
// forwarder's headers. A request with no MAC, a bad MAC, a tampered receipt
// time or one older than max_age_seconds never reaches the package. With
// the probe off for a static public URL, doctor's webhooks check is ok
// without the daemon requesting it.
func TestExtensiontestForwardedWebhook(t *testing.T) {
	const secret = "forwarder-secret"
	d := extensiontest.Daemon(t, extensiontest.Package("testdata/relay"), extensiontest.Entry("relay", map[string]any{"scope": extensiontest.ScopeID}),
		extensiontest.Options{Env: []string{"OC_TEST_FORWARDER=" + secret}},
		extensiontest.Webhooks(map[string]any{
			"public_url": "https://hooks.example.test", "probe": false,
			"forwarder": map[string]any{"name": "edge", "secret": "env:OC_TEST_FORWARDER", "max_age_seconds": 3600},
		}))
	ctx := context.Background()
	d.Until("the listener behind its forwarder", 30*time.Second, func() bool {
		r, err := d.Client().Doctor(ctx, false)
		if err != nil {
			return false
		}
		for _, c := range r.Checks {
			if c.Name == "webhooks" {
				return c.Status == "ok" && strings.Contains(c.Detail, "not probed") && strings.Contains(c.Detail, "only forwarder edge is accepted")
			}
		}
		return false
	})
	path := "/relay/" + extensiontest.ScopeID
	body := []byte(`{"text":"held"}`)
	post := func(at, id, sig string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, d.WebhookURL()+path, bytes.NewReader(body))
		for k, v := range map[string]string{extension.ForwardedAtHeader: at, extension.ForwardIDHeader: id, extension.ForwardSignatureHeader: sig} {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		_, _ = out.ReadFrom(res.Body)
		return res.StatusCode, out.Bytes()
	}
	sign := func(at, id string) string {
		return extension.ForwardSignature(secret, http.MethodPost, path, at, id, body)
	}

	held := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	at := held.Format(time.RFC3339)
	code, out := post(at, "msg-1", sign(at, "msg-1"))
	if code != http.StatusOK {
		t.Fatalf("signed: %d %s", code, out)
	}
	var echo struct {
		ReceivedAt time.Time `json:"received_at"`
		ForwardID  string    `json:"forward_id"`
		Headers    []string  `json:"headers"`
	}
	if err := json.Unmarshal(out, &echo); err != nil {
		t.Fatal(err)
	}
	if !echo.ReceivedAt.Equal(held) || echo.ForwardID != "msg-1" {
		t.Fatalf("the package's receipt: %+v, want %s and msg-1", echo, held)
	}
	for _, h := range []string{extension.ForwardedAtHeader, extension.ForwardIDHeader, extension.ForwardSignatureHeader} {
		if slices.Contains(echo.Headers, h) {
			t.Fatalf("the package saw the forwarder's header %s: %v", h, echo.Headers)
		}
	}

	stale := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	later := held.Add(5 * time.Minute).Format(time.RFC3339)
	for name, c := range map[string][3]string{
		"no MAC":           {at, "msg-2", ""},
		"bad MAC":          {at, "msg-2", extension.ForwardSignature("wrong", http.MethodPost, path, at, "msg-2", body)},
		"tampered time":    {later, "msg-2", sign(at, "msg-2")},
		"older than bound": {stale, "msg-2", sign(stale, "msg-2")},
	} {
		if code, out := post(c[0], c[1], c[2]); code != http.StatusUnauthorized || bytes.Contains(out, []byte("received_at")) {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
}
