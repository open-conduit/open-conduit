package github

import (
	"context"
	"net/http"
	"testing"
	"time"

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
