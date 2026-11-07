package github

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

func TestVerifyReportsAccess(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	c := &clock{t: time.Now()}
	client := tokenClient(t, fake, c, "tok")
	a, err := client.Verify(context.Background(), "octo/widgets")
	if err != nil || a.Known || a.DefaultBranch != "main" {
		t.Fatalf("verify without permissions: %+v %v", a, err)
	}
	fake.SetPermissions(map[string]bool{"pull": true, "push": true, "admin": false})
	if a, err = client.Verify(context.Background(), "octo/widgets"); err != nil || !a.Known || !a.Push || a.Admin {
		t.Fatalf("verify: %+v %v", a, err)
	}
	if a.Checks != "check_runs" {
		t.Fatalf("checks access %q", a.Checks)
	}
	fake.RefuseCheckRuns(true)
	if a, _ = client.Verify(context.Background(), "octo/widgets"); a.Checks != "workflow_runs" {
		t.Fatalf("fine-grained token with Actions read: %q", a.Checks)
	}
	fake.RefuseCheckRuns(false)
	if a, _ = client.Verify(context.Background(), "octo/widgets"); a.Checks != "unreadable" {
		t.Fatalf("fine-grained token without Actions read: %q", a.Checks)
	}
	fake.RevokeToken("tok")
	if _, err = client.Verify(context.Background(), "octo/widgets"); !IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("revoked token: %v", err)
	}
	other := tokenClient(t, fake, c, "nope")
	if _, err = other.Verify(context.Background(), "octo/missing"); err == nil {
		t.Fatal("unknown repository verified")
	}
}

func TestEnsureWebhookCreatesRepointsAndDryRuns(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	client := tokenClient(t, fake, &clock{t: time.Now()}, "tok")
	ctx := context.Background()
	const suffix = "/github/eng/widgets"
	id, wrote, err := client.EnsureWebhook(ctx, "octo/widgets", "https://a.example"+suffix, suffix, "s3cret", "", true)
	if err != nil || wrote || id != "" || len(fake.Hooks()) != 0 {
		t.Fatalf("dry run wrote: %q %v %v", id, wrote, err)
	}
	id, wrote, err = client.EnsureWebhook(ctx, "octo/widgets", "https://a.example"+suffix, suffix, "s3cret", "", false)
	hooks := fake.Hooks()
	if err != nil || !wrote || len(hooks) != 1 || hooks[0].URL != "https://a.example"+suffix || hooks[0].Secret != "s3cret" || len(hooks[0].Events) != len(HookEvents) {
		t.Fatalf("create: %q %v %v %+v", id, wrote, err, hooks)
	}
	// Unchanged: no write. A new tunnel URL re-points the same hook, found
	// by its path suffix after a restart (no remembered ID).
	if _, wrote, err = client.EnsureWebhook(ctx, "octo/widgets", "https://a.example"+suffix, suffix, "s3cret", "", false); err != nil || wrote {
		t.Fatalf("unchanged hook rewritten: %v %v", wrote, err)
	}
	again, wrote, err := client.EnsureWebhook(ctx, "octo/widgets", "https://b.example"+suffix, suffix, "s3cret", "", false)
	if err != nil || !wrote || again != id || len(fake.Hooks()) != 1 || fake.Hooks()[0].URL != "https://b.example"+suffix {
		t.Fatalf("re-point: %q %v %v %+v", again, wrote, err, fake.Hooks())
	}
}

func TestManageWebhookNeedsASecret(t *testing.T) {
	if _, err := ParseExtension([]byte(`{"namespace":"github/x","scope":"eng","repo":"o/r","manage_webhook":true}`), nil); err == nil {
		t.Fatal("manage_webhook without webhook_secret accepted")
	}
	s, err := ParseExtension([]byte(`{"namespace":"github/x","scope":"eng","repo":"o/r","manage_webhook":true,"webhook_secret":"hook"}`), []string{"hook"})
	if err != nil || !s.ManageWebhook {
		t.Fatalf("%+v %v", s, err)
	}
}

// A managed hook that is set but whose deliveries fail is not "hook ok":
// the hook's last delivery is read back from GitHub, doctor warns
// hook_delivery_failing with GitHub's own message, and a re-point pings the
// hook so the answer is the new URL's.
func TestManagedHookReportsFailingDeliveries(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	c := &clock{t: time.Now()}
	client := tokenClient(t, fake, c, "tok")
	x := New()
	var reported []string
	x.host = inproc.Host{Now: c.now, Report: func(code string) { reported = append(reported, code) },
		Secrets: func(context.Context, domain.ID, string, []string) (map[string]string, error) {
			return map[string]string{"hook": "hook-secret"}, nil
		}}
	s := Settings{Namespace: "github/eng/widgets", Repo: "octo/widgets", WebhookSecret: "hook", ManageWebhook: true}
	x.own.entries[s.Namespace] = &entry{name: "github/eng/widgets", scope: "eng", client: client, ingress: NewIngress(s, client, c.now, IngressOptions{})}
	x.own.public = "https://a.example"
	items := func() (string, []string) {
		var raised []string
		item := x.hookItems(func(_, code string) { raised = append(raised, code) })[s.Namespace]
		return item, raised
	}
	ctx := context.Background()

	if err := x.manageHooks(ctx); err != nil {
		t.Fatal(err)
	}
	hooks := fake.Hooks()
	if len(hooks) != 1 || hooks[0].Pings != 1 {
		t.Fatalf("created hook not pinged: %+v", hooks)
	}
	if item, raised := items(); item != "POST /github/eng/widgets; hook ok" || len(raised) != 0 {
		t.Fatalf("before the first read: %q %v", item, raised)
	}
	c.add(hookPingWait)
	_ = x.manageHooks(ctx)
	if item, raised := items(); item != "POST /github/eng/widgets; hook ok; no delivery yet" || len(raised) != 0 {
		t.Fatalf("no delivery yet: %q %v", item, raised)
	}

	fake.SetHookResponse(502, "failed to connect to host")
	_ = x.manageHooks(ctx)
	if item, _ := items(); strings.Contains(item, "502") {
		t.Fatalf("read again before hookCheckEvery: %q", item)
	}
	c.add(hookCheckEvery)
	_ = x.manageHooks(ctx)
	item, raised := items()
	if item != "POST /github/eng/widgets; hook ok; last delivery 502 failed to connect to host (hook_delivery_failing: GitHub's deliveries are not arriving)" || len(raised) != 1 || raised[0] != "hook_delivery_failing" {
		t.Fatalf("failing deliveries: %q %v", item, raised)
	}
	if reported[len(reported)-1] != "hook_delivery_failing" {
		t.Fatalf("reported %v", reported)
	}

	// The tunnel comes back at a new URL: the hook is re-pointed and
	// pinged, and the ping's answer replaces the failure.
	x.own.public = "https://b.example"
	fake.SetHookResponse(202, "OK")
	_ = x.manageHooks(ctx)
	if h := fake.Hooks()[0]; h.URL != "https://b.example/github/eng/widgets" || h.Pings != 2 {
		t.Fatalf("re-point: %+v", h)
	}
	c.add(hookPingWait)
	_ = x.manageHooks(ctx)
	if item, raised := items(); item != "POST /github/eng/widgets; hook ok; last delivery 202" || len(raised) != 0 {
		t.Fatalf("after re-point: %q %v", item, raised)
	}
	if reported[len(reported)-1] != "" {
		t.Fatalf("reported %v", reported)
	}
}

func TestDeliveryFailing(t *testing.T) {
	for d, want := range map[Delivery]bool{
		{Status: "unused"}:            false,
		{Code: 200, Status: "active"}: false,
		{Code: 202}:                   false,
		{Code: 401, Message: "bad"}:   true,
		{Code: 502}:                   true,
		{Status: "timeout"}:           true,
		{Status: "active"}:            false,
	} {
		if d.Failing() != want {
			t.Errorf("%+v: failing %v", d, d.Failing())
		}
	}
}
