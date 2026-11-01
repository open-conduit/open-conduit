package integration_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/storage"
)

// newReviewEnv is the tools environment with an entry that reviews pull
// requests and a scope that routes issues to a builder binding and each
// open PR's review to a reviewer binding.
func newReviewEnv(t *testing.T) *toolsEnv {
	t.Helper()
	fake := githubtest.New(ghRepo, "tok")
	t.Cleanup(fake.Close)
	ext := strings.TrimSuffix(ghExtensionConfig(), "}") + `,"review_pull_requests":true}`
	e := &toolsEnv{ghEnv: &ghEnv{t: t, fake: fake, clock: &ghClock{t: fake.Now()}, path: filepath.Join(t.TempDir(), "conduit.db"), extConfig: ext}, tokens: map[domain.ID]string{}}
	e.open()
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "widgets", "type": "github", "connection": "github", "config": json.RawMessage(ext), "secret_names": []string{"hook"}},
	}})
	agent := domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: ghScope, Name: ghScope,
			Bindings: []domain.Binding{{Name: "agent", Executor: "fixture", Requires: agent}, {Name: "reviewer", Executor: "fixture", Requires: agent}},
			Routes: []domain.Route{{Name: "issues", Handler: "github", Binding: "agent", ActivePolicy: "queue"},
				{Name: "reviews", Handler: "github", Binding: "reviewer", ActivePolicy: "queue", Config: domain.Payload{Data: json.RawMessage(`{"claim":"pull_requests"}`)}}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	e.register()
	e.compose()
	return e
}

func (e *toolsEnv) bindingOf(work domain.ID) string {
	var binding string
	e.read(func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(work)
		if err == nil && len(xs) > 0 {
			binding = xs[len(xs)-1].ExecutorBinding
		}
		return err
	})
	return binding
}

// The builder's issue Work opens a PR; review Work is claimed for it
// alongside, without disturbing the builder. The reviewer's own review
// wakes only the builder, a push wakes the parked reviewer, and the merge
// reaches both, each of which then completes.
func TestGitHubReviewWorkRunsAlongsideTheBuilder(t *testing.T) {
	e := newReviewEnv(t)
	n, builder := e.claim("Header wobbles", "It wobbles on scroll.", "bug")
	b := e.start(builder.ID)
	e.finish(b, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("building")}})

	// The builder opens its PR (on the issue's branch, so keyed to the issue).
	pr := e.fake.OpenPull("Steady the header", "Closes #"+itoa(n), "agent/issue-"+itoa(n), "sha1", ghAlice)
	e.sync()
	review, ok := e.workFor(gh.ReviewKey(pr))
	if !ok || review.Kind != gh.WorkKindReview || review.ID == builder.ID {
		t.Fatalf("no review Work for PR %d: %+v", pr, review)
	}
	if review.Display == nil || review.Display.Ref != "#"+itoa(pr) || review.Display.Title != "Steady the header" {
		t.Fatalf("review display: %+v", review.Display)
	}
	if owner, _ := e.workFor(gh.IssueKey(n, "")); owner.ID != builder.ID {
		t.Fatalf("the issue's key moved to %s", owner.ID)
	}
	if q := e.queued(builder.ID); len(q) != 0 {
		t.Fatalf("the builder heard the PR open: %v", q)
	}

	// The reviewer reviews as itself, then parks with no condition named.
	r := e.start(review.ID)
	if got := e.bindingOf(review.ID); got != "reviewer" {
		t.Fatalf("review ran on binding %q", got)
	}
	ctx := e.must(r, gh.OpContext, "context", map[string]any{})
	if ctx["pr"] != float64(pr) || ctx["repo"] != ghRepo {
		t.Fatalf("review context: %v", ctx)
	}
	e.must(r, gh.OpPRReview, "review-1", map[string]any{"number": pr, "event": "request_changes", "body": "Please name the constant.",
		"comments": []map[string]any{{"path": "header.go", "line": 3, "body": "This magic number."}}})
	e.must(r, gh.OpWait, "park-1", map[string]any{"reason": "Waiting for changes."})
	e.finish(r, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("review")}})
	e.sync()
	if got := e.work(review.ID); got.State != domain.WorkWaiting {
		t.Fatalf("the reviewer's own review woke it: %v (input %v)", got.State, e.queued(review.ID))
	}
	if q := e.queued(builder.ID); !contains(q, gh.TypeCommentAgent) {
		t.Fatalf("the builder did not hear the review: %v", q)
	}

	// The builder pushes a fix: the parked reviewer wakes with the push.
	e.fake.PushPull(pr, "sha2")
	e.sync()
	if got := e.work(review.ID); got.State != domain.WorkReady || !contains(e.queued(review.ID), gh.TypePullOpen) {
		t.Fatalf("the push did not wake the reviewer: %v %v", got.State, e.queued(review.ID))
	}
	r = e.start(review.ID)
	e.must(r, gh.OpWait, "park-2", map[string]any{"reason": "Approved; waiting for the merge."})
	e.finish(r, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.sync()
	if got := e.work(review.ID); got.State != domain.WorkWaiting {
		t.Fatalf("reviewer not parked again: %v", got.State)
	}

	// The merge reaches both, and each completes.
	e.fake.MergePull(pr)
	e.sync()
	if !contains(e.queued(review.ID), gh.TypePullMerged) || !contains(e.queued(builder.ID), gh.TypePullMerged) {
		t.Fatalf("merge input: reviewer %v, builder %v", e.queued(review.ID), e.queued(builder.ID))
	}
	for _, w := range []domain.ID{review.ID, builder.ID} {
		a := e.start(w)
		e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Data: statusData("finished")}})
		if got := e.work(w); got.State != domain.WorkCompleted {
			t.Fatalf("%s after the merge: %v", w, got.State)
		}
	}
}

// A PR opened while the daemon was down is claimed for review by the next
// reconcile, as is a draft once it is marked ready, and never before.
func TestGitHubReconcileClaimsMissedPullRequestsForReview(t *testing.T) {
	e := newReviewEnv(t)
	orphan := e.fake.OpenPull("Bump deps", "", "chore/deps", "s1", ghAlice)
	draft := e.fake.OpenPull("WIP", "", "wip", "s2", ghAlice)
	e.fake.SetDraft(draft, true)
	e.sync()
	if w, ok := e.workFor(gh.ReviewKey(orphan)); !ok || w.Kind != gh.WorkKindReview {
		t.Fatalf("missed PR not claimed: %+v", w)
	}
	if _, ok := e.workFor(gh.ReviewKey(draft)); ok {
		t.Fatal("a draft was claimed for review")
	}
	e.fake.SetDraft(draft, false)
	e.sync()
	if _, ok := e.workFor(gh.ReviewKey(draft)); !ok {
		t.Fatal("a draft marked ready was not claimed")
	}
}
