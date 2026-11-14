package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// github.pr.merge asks GitHub whether a PR can merge under the
// repository's own rules and relays its answer (#631), adding only what
// GitHub can't know: the merge is at the head the caller read, and no agent
// on the PR's issue is still working: mid-turn, with input it hasn't read,
// or with its last turn not handed back. A repository that doesn't require
// branches to be up to date merges a PR that is behind its base as it is.
func TestGitHubMergeAsksGitHubAndWaitsForAgents(t *testing.T) {
	e := newToolsEnv(t)
	e.fake.SetMergeRules("main", githubtest.MergeRules{Approvals: 1, Checks: []string{"ci"}})
	bob := githubtest.User{Login: "bob", Type: "User"}
	n, builder := e.claim("Header wobbles", "", "bug")
	pr := e.fake.OpenPull("fix(web): steady the header", "Closes #"+itoa(n), "agent/issue-"+itoa(n), "h1", ghAlice)
	e.fake.SetCheck("h1", "ci", "completed", "success")
	e.sync()
	merger := e.request("merge-bot")
	m := e.start(merger.ID)
	merge := func(head string) (map[string]any, error) {
		out, err := e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": head})
		if err != nil {
			return nil, err
		}
		return decodeMap(t, out), nil
	}
	turn := func(status string) {
		t.Helper()
		if len(e.queued(builder.ID)) == 0 {
			return
		}
		a := e.start(builder.ID)
		e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData(status)}})
	}
	turn("review")

	// GitHub's rules, relayed: a required review missing, then a change
	// request standing.
	_, err := merge("h1")
	refusedSaying(t, err, "blocked", "approving review")
	e.fake.AddReview(pr, bob, "CHANGES_REQUESTED", "The offset is wrong.")
	_, err = merge("h1")
	refusedSaying(t, err, "blocked", "changes requested")
	e.fake.AddReview(pr, bob, "APPROVED", "")
	e.sync()

	// GitHub would merge it now, but the builder has news it hasn't read
	// (the reviews), so it may yet act.
	_, err = merge("h1")
	refusedSaying(t, err, "work_active", "issue #"+itoa(n), "unread", `"issue:`+ghScope+`/`+itoa(n)+`:review"`, `"issue:`+ghScope+`/`+itoa(n)+`:done"`)
	// Mid-turn it is working.
	a := e.start(builder.ID)
	_, err = merge("h1")
	refusedSaying(t, err, "work_active", "working on a turn")
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("building")}})
	_, err = merge("h1")
	refusedSaying(t, err, "work_active", "ended its last turn in building")
	// A comment on the issue arrives after it handed back.
	e.fake.AddComment(n, ghAlice, "Can the logo move too?")
	e.sync()
	_, err = merge("h1")
	refusedSaying(t, err, "work_active", "unread")
	turn("review")

	_, err = merge("stale")
	refused(t, err, "head_moved")
	// Behind main, but main doesn't require branches to be up to date: it
	// merges as it is, without an update.
	e.fake.SetBehind(pr, 2)
	if e.fake.Pull(pr).MergedAt != nil {
		t.Fatal("merged before it was asked")
	}
	got, err := merge("h1")
	if err != nil || got["state"] != "merged" || got["method"] != "squash" {
		t.Fatalf("merge: %v %v", got, err)
	}
	if p := e.fake.Pull(pr); p.MergedAt == nil || p.HeadSHA != "h1" {
		t.Fatalf("the PR did not merge at h1: %+v", p)
	}
	// A retry answers merged and merges nothing again.
	if again, err := merge("h1"); err != nil || again["state"] != "merged" {
		t.Fatalf("retried merge: %v %v", again, err)
	}
}

// Where the repository requires branches to be up to date, GitHub refuses
// a PR that is behind, and the refusal says to call
// github.pr.update-branch, which brings it up to date from the head read.
func TestGitHubMergeOutOfDateWhereRequired(t *testing.T) {
	e := newToolsEnv(t)
	e.fake.SetMergeRules("main", githubtest.MergeRules{Checks: []string{"ci"}, UpToDate: true})
	pr := e.fake.OpenPull("feat: a thing", "", "feature", "f1", ghAlice)
	e.fake.SetCheck("f1", "ci", "completed", "success")
	e.fake.SetBehind(pr, 2)
	m := e.start(e.request("merge-bot").ID)
	merge := func(head string) (map[string]any, error) {
		out, err := e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": head})
		if err != nil {
			return nil, err
		}
		return decodeMap(t, out), nil
	}

	_, err := merge("f1")
	refusedSaying(t, err, "out_of_date", "out of date", gh.OpPRUpdateBranch)
	_, err = e.call(m, gh.OpPRUpdateBranch, "", map[string]any{"number": pr, "head": "stale"})
	refused(t, err, "head_moved")
	out, err := e.call(m, gh.OpPRUpdateBranch, "", map[string]any{"number": pr, "head": "f1"})
	if err != nil || decodeMap(t, out)["state"] != "updating" {
		t.Fatalf("update-branch: %v %v", out, err)
	}
	head := e.fake.Pull(pr).HeadSHA
	if head == "f1" {
		t.Fatal("the branch was not updated")
	}
	if out, err := e.call(m, gh.OpPRUpdateBranch, "", map[string]any{"number": pr, "head": head}); err != nil || decodeMap(t, out)["state"] != "up_to_date" {
		t.Fatalf("update of an up-to-date branch: %v %v", out, err)
	}
	_, err = merge("f1")
	refused(t, err, "head_moved")
	// The new head's required check hasn't reported: GitHub's reason.
	_, err = merge(head)
	refusedSaying(t, err, "blocked", `"ci"`)
	e.fake.SetCheck(head, "ci", "completed", "success")
	if got, err := merge(head); err != nil || got["state"] != "merged" {
		t.Fatalf("merge: %v %v", got, err)
	}
}

// GitHub's refusal of a draft, a conflict or a merge method the repository
// doesn't allow is relayed as one the caller can act on; with no rules of
// its own, a PR nobody is working on merges as GitHub allows.
func TestGitHubMergeRelaysDraftsConflictsAndMethods(t *testing.T) {
	e := newToolsEnv(t)
	e.fake.SetMergeRules("main", githubtest.MergeRules{Methods: []string{"squash"}})
	pr := e.fake.OpenPull("feat: a thing", "", "feature", "f1", ghAlice)
	m := e.start(e.request("merge-bot").ID)
	e.fake.SetDraft(pr, true)
	_, err := e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "f1"})
	refused(t, err, "draft")
	e.fake.SetDraft(pr, false)
	no := false
	e.fake.SetMergeable(pr, &no)
	_, err = e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "f1"})
	refused(t, err, "conflicts")
	_, err = e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr})
	refused(t, err, "missing_field")
	_, err = e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "f1", "method": "octopus"})
	refused(t, err, "invalid_input")
	e.fake.SetMergeable(pr, nil)
	_, err = e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "f1", "method": "merge"})
	refusedSaying(t, err, "method_not_allowed", "Merge commits are not allowed")
	// Nobody works on a PR without an issue, and main asks for no review
	// or check, so it merges.
	if _, err = e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "f1"}); err != nil {
		t.Fatal(err)
	}
}

// github.workflow.run starts only a workflow the repository's entry lists,
// once per request however often it is retried (#608); github.pr.list with
// open lists every open PR, whoever opened it.
func TestGitHubWorkflowRunIsAllowedAndOnce(t *testing.T) {
	e := newToolsEnv(t)
	e.fake.AddWorkflow("release.yml")
	e.fake.AddWorkflow("deploy.yml")
	job := e.start(e.request("release-job").ID)
	run := func(request string, in map[string]any) (map[string]any, error) {
		out, err := e.call(job, gh.OpWorkflowRun, request, in)
		if err != nil {
			return nil, err
		}
		return decodeMap(t, out), nil
	}
	_, err := run("", map[string]any{"workflow": "release.yml"})
	refusedSaying(t, err, "workflow_not_allowed", "allows no workflow")
	e.settings.Workflows = []string{"release.yml"}
	_, err = run("", map[string]any{"workflow": "deploy.yml", "ref": "main"})
	refusedSaying(t, err, "workflow_not_allowed", "only release.yml")
	_, err = run("", map[string]any{"workflow": "release.yml", "inputs": map[string]any{"x": map[string]any{}}})
	refused(t, err, "invalid_input")

	first, err := run("release-1", map[string]any{"workflow": "release.yml", "inputs": map[string]any{"dry_run": "false"}})
	if err != nil || first["state"] != "started" || first["ref"] != "main" {
		t.Fatalf("run: %v %v", first, err)
	}
	again, err := run("release-1", map[string]any{"workflow": "release.yml", "inputs": map[string]any{"dry_run": "false"}})
	if err != nil || again["state"] != "started" {
		t.Fatalf("retry: %v %v", again, err)
	}
	if d := e.fake.Dispatches(); len(d) != 1 || d[0].Workflow != "release.yml" || d[0].Ref != "main" || d[0].Inputs["dry_run"] != "false" {
		t.Fatalf("dispatches: %+v", d)
	}
	// A lost response leaves the run started once: the retry finds it.
	e.fake.LoseNextResponse("/repos/"+ghRepo+"/actions/workflows/release.yml/dispatches", 1)
	if _, err := run("release-2", map[string]any{"workflow": "release.yml"}); err == nil {
		t.Fatal("a lost dispatch answered as started")
	}
	if out, err := run("release-2", map[string]any{"workflow": "release.yml"}); err != nil || out["state"] != "started" || out["run_url"] == nil {
		t.Fatalf("retried lost dispatch: %v %v", out, err)
	}
	if d := e.fake.Dispatches(); len(d) != 2 {
		t.Fatalf("dispatches after a lost response: %d, want 2", len(d))
	}

	bob := githubtest.User{Login: "bob", Type: "User"}
	e.fake.OpenPull("one", "", "a", "s1", ghAlice)
	draft := e.fake.OpenPull("two", "", "b", "s2", bob)
	e.fake.SetDraft(draft, true)
	list := e.must(job, gh.OpPRList, "", map[string]any{"open": true})["prs"].([]any)
	if len(list) != 2 || list[1].(map[string]any)["draft"] != true || list[1].(map[string]any)["author"] != "bob" {
		t.Fatalf("open PRs: %v", list)
	}
}

func decodeMap(t *testing.T, out extension.ToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out.Output.Data, &m); err != nil {
		t.Fatalf("output %s: %v", out.Output.Data, err)
	}
	return m
}

// The builder's newest turn decides whether it has handed back, however it
// ended: a turn cancelled after it read a comment leaves that comment
// unanswered, and an earlier turn's review doesn't stand in for it. A
// comment accepted but not yet routed is unread too.
func TestGitHubMergeWaitsOnACancelledTurnAndUnroutedInput(t *testing.T) {
	e := newToolsEnv(t)
	bob := githubtest.User{Login: "bob", Type: "User"}
	n, builder := e.claim("Header wobbles", "", "bug")
	pr := e.fake.OpenPull("fix(web): steady the header", "Closes #"+itoa(n), "agent/issue-"+itoa(n), "h1", ghAlice)
	e.fake.SetCheck("h1", "ci", "completed", "success")
	e.fake.AddReview(pr, bob, "APPROVED", "")
	e.sync()
	a := e.start(builder.ID)
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("review")}})
	m := e.start(e.request("merge-bot").ID)
	merge := func() error {
		_, err := e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "h1"})
		return err
	}

	// Accepted, not yet routed: the router hasn't queued it, but it is the
	// builder's.
	e.fake.AddComment(n, ghAlice, "Can the logo move too?")
	e.reconcile()
	refusedSaying(t, merge(), "work_active", "unread")
	e.route()

	// The builder reads it, and its turn is cancelled before it ends.
	b := e.start(builder.ID)
	var x domain.Execution
	e.read(func(tx storage.Tx) error { var err error; x, err = tx.Execution(b.Lease.ExecutionID); return err })
	ctl, err := runtime.NewControl(e.store, e.clock.Now, e.router, e.scheduler, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ctl.CancelExecution(context.Background(), x.ID, x.Revision, "stop", "cancel-1"); err != nil {
		t.Fatal(err)
	}
	e.route()
	refusedSaying(t, merge(), "work_active", "cancelled")

	// Retried by the operator, it has the retry to read before it hands
	// back again.
	if _, err := ctl.RetryWork(context.Background(), builder.ID, e.work(builder.ID).Revision, "go on", "retry-1"); err != nil {
		t.Fatal(err)
	}
	e.route()
	refusedSaying(t, merge(), "work_active", "unread")
}

// A merge refused because the PR's own Work (its reviewer) is mid-turn
// names conditions on that Work's handing back, and they wake the merge
// bot when the reviewer ends its turn done without posting anything, with
// no further GitHub event (#627).
func TestGitHubMergeWakesWhenThePullsReviewerHandsBack(t *testing.T) {
	e := newPullEnv(t)
	bob := githubtest.User{Login: "bob", Type: "User"}
	n, builder := e.claim("Header wobbles", "", "bug")
	pr := e.fake.OpenPull("fix(web): steady the header", "Closes #"+itoa(n), "agent/issue-"+itoa(n), "h1", ghAlice)
	e.fake.SetCheck("h1", "ci", "completed", "success")
	e.fake.AddReview(pr, bob, "APPROVED", "")
	e.sync()
	b := e.start(builder.ID)
	e.finish(b, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("review")}})
	reviewer, ok := e.workFor(gh.PullRequestKey(pr))
	if !ok {
		t.Fatal("no Work for the PR")
	}
	r := e.start(reviewer.ID)

	merger := e.request("merge-bot")
	m := e.start(merger.ID)
	_, err := e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "h1"})
	review, done := "pr:"+string(ghScope)+"/"+itoa(pr)+":review", "pr:"+string(ghScope)+"/"+itoa(pr)+":done"
	refusedSaying(t, err, "work_active", "PR #"+itoa(pr), "working on a turn", `"`+review+`"`, `"`+done+`"`)
	e.must(m, gh.OpWait, "park", map[string]any{"conditions": []string{review, done}, "reason": "the reviewer is mid-turn"})
	e.finish(m, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.publishTurns()
	if got := e.work(merger.ID); got.State != domain.WorkWaiting {
		t.Fatalf("the merge bot's own turn woke it: %v", got.State)
	}

	// The reviewer approves without posting and hands back.
	e.finish(r, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.publishTurns()
	if got := e.work(merger.ID); got.State != domain.WorkReady {
		t.Fatalf("the reviewer's handing back did not wake the merge bot: %v", got.State)
	}
	m = e.start(merger.ID)
	out, err := e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "h1"})
	if err != nil || decodeMap(t, out)["state"] != "merged" {
		t.Fatalf("merge after the reviewer handed back: %v %v", out, err)
	}
}
