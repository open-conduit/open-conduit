package githubext

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/protocol"
)

// The host refuses an observation whose schema the package manifest doesn't
// declare (protocol.Manifest.CheckWrite), so every payload the ingress
// writes, pushed or reconciled, must carry a declared schema. A schema left
// out of the manifest fails every push and reconcile page that carries it.
func TestManifestDeclaresEveryIngressSchema(t *testing.T) {
	fake := githubtest.New(FixtureRepo)
	defer fake.Close()
	fake.AddToken(FixtureToken)
	issue := fake.OpenIssue("The export is broken", "it fails on every run", alice)
	pr := fake.OpenPull("Fix the export", fmt.Sprintf("Closes #%d", issue), "agent/issue-1", "abc123", alice)
	comment := fake.AddComment(pr, alice, "looks good")
	review := fake.AddReview(pr, alice, "COMMENTED", "one question")
	inline := fake.AddReviewComment(pr, alice, "why this?", "main.go", 3)
	fake.SetCheck("abc123", "build", "completed", "success")

	conn, err := gh.ParseConnection(json.RawMessage(`{"api_url":"` + fake.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	client, err := gh.NewClient(conn, func(context.Context, string) (string, error) { return FixtureToken, nil }, gh.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entry := json.RawMessage(`{"namespace":"github/` + FixtureRepo + `","scope":"` + FixtureScope + `","repo":"` + FixtureRepo + `","track_pull_requests":true,"webhook_secret":"webhook_secret"}`)
	settings, err := gh.ParseExtension(entry, []string{"webhook_secret"})
	if err != nil {
		t.Fatal(err)
	}
	ingress := gh.NewIngress(settings, client, time.Now, gh.IngressOptions{})
	src := extension.Source{Namespace: settings.Namespace, ScopeID: FixtureScope}
	m := Manifest()
	seen := map[string]bool{}
	check := func(what string, obs []extension.Observation, events []extension.Observation) {
		t.Helper()
		for _, o := range append(obs, events...) {
			seen[o.Event.Data.Schema] = true
			if err := m.CheckWrite(o.Event.Data); err != nil {
				t.Errorf("%s: %s (%s): %v", what, o.ID, o.Event.Type, err)
			}
		}
	}

	hooks := []struct {
		event string
		body  []byte
	}{
		{"issues", fake.IssuesHook("opened", issue)},
		{"pull_request", fake.PullHook("opened", pr)},
		{"issue_comment", fake.CommentHook("created", comment)},
		{"pull_request_review", fake.ReviewHook("submitted", pr, review)},
		{"pull_request_review_comment", fake.ReviewCommentHook("created", inline)},
		{"check_run", fake.CheckRunHook("abc123", pr)},
	}
	for i, h := range hooks {
		id := fmt.Sprintf("delivery-%d", i)
		d, err := gh.VerifyDelivery(fixtureSecret, githubtest.Sign(fixtureSecret, h.event, id, h.body), h.body, time.Now().UTC())
		if err != nil {
			t.Fatalf("%s: %v", h.event, err)
		}
		res, err := ingress.Push(context.Background(), src, d)
		if err != nil {
			t.Fatalf("push %s: %v", h.event, err)
		}
		var events []extension.Observation
		for _, e := range res.Events {
			events = append(events, extension.Observation{ID: "event", Event: e})
		}
		check("push "+h.event, res.Observations, events)
	}

	scan := extension.Scan{Source: src}
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("reconcile did not complete")
		}
		snap, err := ingress.Reconcile(context.Background(), scan)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		check("reconcile", snap.Observations, nil)
		if snap.Complete || snap.NextCursor == "" {
			break
		}
		scan.ScanID, scan.Cursor = snap.ScanID, snap.NextCursor
	}

	for _, s := range []string{gh.SchemaIssue, gh.SchemaPull, gh.SchemaComment, gh.SchemaChecks, gh.SchemaDelivery} {
		if !seen[s] {
			t.Errorf("no observation carried %s; the test no longer covers it", s)
		}
	}
}

// Every schema the extension lists is declared at the version it writes.
func TestManifestDeclaresSchemasAtTheirVersion(t *testing.T) {
	m := Manifest()
	for _, s := range gh.Schemas() {
		if got := m.PayloadStatus(protocol.Payload{Schema: s}.Domain()); got != protocol.SchemaCurrent {
			t.Errorf("%s is %s in the manifest, not current", s, got)
		}
	}
}
