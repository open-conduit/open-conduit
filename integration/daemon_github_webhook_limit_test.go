package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/protocol"
)

// paddedDelivery is an `issues` delivery body grown to size bytes with a
// field GitHub doesn't send, the shape of a large pull_request or
// check_suite payload.
func paddedDelivery(t *testing.T, body []byte, size int) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	m["padding"] = ""
	short, _ := json.Marshal(m)
	m["padding"] = strings.Repeat("x", size-len(short))
	out, _ := json.Marshal(m)
	if len(out) != size {
		t.Fatalf("padded to %d, want %d", len(out), size)
	}
	return out
}

// TestDaemonGitHubLargeDelivery: the GitHub package declares the webhook
// body it accepts (webhook_max_bytes), so a 5 MiB signed delivery through
// the composed daemon's listener is accepted and produces its
// github.delivery Event, well over the 1 MiB a package that declares none
// gets (TestExternalSurfacesRoundTrip refuses the same delivery there). A
// body over the host's own bound is still refused with 413.
func TestDaemonGitHubLargeDelivery(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	var cfg map[string]any
	_ = json.Unmarshal(webhookConfig(fake.URL), &cfg)
	x := cfg["scopes"].([]any)[0].(map[string]any)["extensions"].([]any)[0].(map[string]any)
	x["config"].(map[string]any)["manage_webhook"] = false
	data, _ := json.Marshal(cfg)
	d := newDrill(t)
	if err := os.WriteFile(d.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0"}
	d.start("")
	ctx := context.Background()
	var addr string
	d.until("webhooks listening", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		if err != nil {
			return false
		}
		m := listenerAddr.FindStringSubmatch(check(r, "webhooks").Detail)
		if m == nil {
			return false
		}
		addr = m[1]
		return strings.Contains(strings.Join(check(r, "reconcile_lag").Items, " "), ": reconciled")
	})
	post := func(guid string, body []byte) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/github/eng/widgets", bytes.NewReader(body))
		req.Header = githubtest.Sign("hook-secret", "issues", guid, body)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	n := fake.OpenIssue("Large", "", githubtest.User{Login: "alice", Type: "User"}, "backlog")
	base := fake.IssuesHook("edited", n)

	over := paddedDelivery(t, base, protocol.MaxWebhookBodyBytes+1)
	if code, msg := post("guid-over", over); code != http.StatusRequestEntityTooLarge || !strings.Contains(msg, "at most") {
		t.Fatalf("over the host's bound: %d %q", code, msg)
	}

	big := paddedDelivery(t, base, 5<<20)
	if code, msg := post("guid-big", big); code != http.StatusAccepted {
		t.Fatalf("5 MiB delivery: %d %q", code, msg)
	}
	d.until("the delivery's Event", 30*time.Second, func() bool {
		for _, ev := range gateEvents(d.c, url.Values{}) {
			if ev.Type == "github.delivery" && strings.Contains(ev.DeliveryID, "guid-big") {
				return true
			}
		}
		return false
	})
	for _, ev := range gateEvents(d.c, url.Values{}) {
		if ev.Type == "github.delivery" && strings.Contains(ev.DeliveryID, "guid-over") {
			t.Fatalf("a refused delivery produced an Event: %+v", ev)
		}
	}
}
