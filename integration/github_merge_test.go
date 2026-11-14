package integration_test

import (
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

// github.pr.merge merges only when every rule holds, checked where the call
// is served (#608): approved with no change request outstanding, green on
// the head the caller read, holding every commit of its base, and no agent
// on the PR's issue still working: mid-turn, with input it hasn't read, or
// with its last turn not handed back. github.pr.update-branch brings a PR
// that is behind up to date from the head read, never under a working agent.
func TestGitHubMergeWaitsForEveryRule(t *testing.T) {
	e := newToolsEnv(t)
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

	_, err := merge("h1")
	refused(t, err, "not_approved")
	e.fake.AddReview(pr, bob, "CHANGES_REQUESTED", "The offset is wrong.")
	_, err = merge("h1")
	refusedSaying(t, err, "changes_requested", "bob")
	e.fake.AddReview(pr, bob, "COMMENTED", "Still looking.")
	_, err = merge("h1")
	refused(t, err, "changes_requested")
	e.fake.AddReview(pr, bob, "APPROVED", "")
	e.sync()

	// The builder has news it hasn't read (the reviews), so it may yet act.
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

	// Green and handed back, but behind main: its checks ran on an older main.
	e.fake.SetBehind(pr, 2)
	_, err = merge("h1")
	refusedSaying(t, err, "behind_base", "lacks 2", gh.OpPRUpdateBranch)
	_, err = e.call(m, gh.OpPRUpdateBranch, "", map[string]any{"number": pr, "head": "stale"})
	refused(t, err, "head_moved")
	out, err := e.call(m, gh.OpPRUpdateBranch, "", map[string]any{"number": pr, "head": "h1"})
	if err != nil || decodeMap(t, out)["state"] != "updating" {
		t.Fatalf("update-branch: %v %v", out, err)
	}
	head := e.fake.Pull(pr).HeadSHA
	if head == "h1" {
		t.Fatal("the branch was not updated")
	}
	if out, err := e.call(m, gh.OpPRUpdateBranch, "", map[string]any{"number": pr, "head": head}); err != nil || decodeMap(t, out)["state"] != "up_to_date" {
		t.Fatalf("update of an up-to-date branch: %v %v", out, err)
	}
	_, err = merge("h1")
	refused(t, err, "head_moved")
	e.sync()
	turn("review")
	_, err = merge(head)
	refused(t, err, "checks_pending")
	e.fake.SetCheck(head, "ci", "completed", "failure")
	_, err = merge(head)
	refusedSaying(t, err, "checks_failed", "ci")
	e.fake.SetCheck(head, "ci", "completed", "success")
	e.sync()
	turn("review")
	if e.fake.Pull(pr).MergedAt != nil {
		t.Fatal("merged before it was asked")
	}
	got, err := merge(head)
	if err != nil || got["state"] != "merged" || got["method"] != "squash" {
		t.Fatalf("merge: %v %v", got, err)
	}
	if e.fake.Pull(pr).MergedAt == nil {
		t.Fatal("the PR did not merge")
	}
	// A retry answers merged and merges nothing again.
	if again, err := merge(head); err != nil || again["state"] != "merged" {
		t.Fatalf("retried merge: %v %v", again, err)
	}
}

// A draft, a conflict or an agent's PR that is still waiting on its owner
// never merges, whatever else holds.
func TestGitHubMergeRefusesDraftsAndConflicts(t *testing.T) {
	e := newToolsEnv(t)
	bob := githubtest.User{Login: "bob", Type: "User"}
	pr := e.fake.OpenPull("feat: a thing", "", "feature", "f1", ghAlice)
	e.fake.SetCheck("f1", "ci", "completed", "success")
	e.fake.AddReview(pr, bob, "APPROVED", "")
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
	// Nobody works on a PR without an issue, so it merges.
	if _, err = e.call(m, gh.OpPRMerge, "", map[string]any{"number": pr, "head": "f1", "method": "merge"}); err != nil {
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
