package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/protocol"
)

const (
	repo   = "octo/widgets"
	ns     = "github/github.com/octo/widgets"
	secret = "s3cret"
)

var (
	alice = githubtest.User{Login: "alice", Type: "User"}
	ci    = githubtest.User{Login: "ci[bot]", Type: "Bot"}
	src   = extension.Source{Namespace: ns, ScopeID: "eng"}
)

type fixture struct {
	fake *githubtest.Server
	in   *gh.Ingress
	now  time.Time
}

func newFixture(t *testing.T, extra string) *fixture {
	t.Helper()
	fake := githubtest.New(repo, "tok")
	t.Cleanup(fake.Close)
	// Half an hour after the fake's clock starts, inside the default horizon.
	f := &fixture{fake: fake, now: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)}
	cfg, err := gh.ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	client, err := gh.NewClient(cfg, func(context.Context, string) (string, error) { return "tok", nil }, gh.ClientOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	s, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"`+extra+`}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.in = gh.NewIngress(s, client, func() time.Time { return f.now }, gh.IngressOptions{BatchUnits: 1000, PageBudget: time.Minute})
	return f
}

func (f *fixture) push(t *testing.T, event, delivery string, body []byte) extension.PushResult {
	t.Helper()
	d, err := gh.VerifyDelivery(secret, githubtest.Sign(secret, event, delivery, body), body, f.now)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.in.Push(context.Background(), src, d)
	if err != nil {
		t.Fatalf("push %s: %v", event, err)
	}
	return res
}

// scan runs a whole generation and returns its observations by item.
func (f *fixture) scan(t *testing.T) map[string]extension.Observation {
	t.Helper()
	out := map[string]extension.Observation{}
	scan := extension.Scan{Source: src}
	for i := 0; i < 1000; i++ {
		page, err := f.in.Reconcile(context.Background(), scan)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		for _, o := range page.Observations {
			if prev, ok := out[o.ID]; ok && prev.Version != o.Version {
				t.Fatalf("item %s repeated with another version in one generation", o.ID)
			}
			out[o.ID] = o
		}
		if page.Complete {
			if page.NextCursor != "" {
				t.Fatal("complete page with a cursor")
			}
			return out
		}
		if page.NextCursor == "" {
			t.Fatalf("partial scan: %+v", page)
		}
		scan.ScanID, scan.Cursor, scan.Checkpoint = page.ScanID, page.NextCursor, page.Checkpoint
	}
	t.Fatal("scan did not finish")
	return nil
}

func key(o extension.Observation) string { return o.Event.CorrelationKey.ExternalID }

func payload[T any](t *testing.T, o extension.Observation) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(o.Event.Data.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIngressConformance(t *testing.T) {
	conformance.RunIngress(t, func() conformance.IngressFixture {
		f := newFixture(t, "")
		n := f.fake.OpenIssue("Fix the header", "it wobbles", alice)
		for i := 0; i < 3; i++ {
			f.fake.OpenIssue("filler "+strconv.Itoa(i), "", alice)
		}
		body := f.fake.IssuesHook("opened", n)
		d, _ := gh.VerifyDelivery(secret, githubtest.Sign(secret, "issues", "d-1", body), body, f.now)
		res, err := f.in.Push(context.Background(), src, d)
		if err != nil || len(res.Observations) != 1 {
			t.Fatalf("push: %v %+v", err, res)
		}
		return conformance.IngressFixture{Ingress: f.in, Source: src, Delivery: d, Canonical: res.Observations[0], OneShotType: gh.TypeDelivery}
	})
}

// sameIdentity requires push and reconcile to agree on everything except the
// actual receipt time.
func sameIdentity(t *testing.T, label string, pushed, scanned extension.Observation) {
	t.Helper()
	pushed.Event.ReceivedAt, scanned.Event.ReceivedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(pushed, scanned) {
		t.Fatalf("%s: push and reconcile differ\npush: %+v\n%s\nscan: %+v\n%s", label, pushed, pushed.Event.Data.Data, scanned, scanned.Event.Data.Data)
	}
}

func onlyObservation(t *testing.T, res extension.PushResult) extension.Observation {
	t.Helper()
	if len(res.Observations) != 1 {
		t.Fatalf("observations = %d", len(res.Observations))
	}
	if len(res.Events) != 1 || res.Events[0].Type != gh.TypeDelivery {
		t.Fatalf("raw delivery missing: %+v", res.Events)
	}
	return res.Observations[0]
}

func TestPushAndReconcileAgreeForEveryFamily(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	issue := fk.OpenIssue("Header", "wobbles", alice, "bug")
	human := fk.AddComment(issue, alice, "please make it blue")
	bot := fk.AddComment(issue, ci, "coverage 80%")
	agent := fk.AddComment(issue, ci, gh.WithMarker("on it", "work/x", "i1"))
	pr := fk.OpenPull("Blue header", "Closes #1", "feature/blue", "sha1", alice)
	review := fk.AddReview(pr, alice, "CHANGES_REQUESTED", "rename it")
	inline := fk.AddReviewComment(pr, alice, "nit", "header.go", 12)
	prComment := fk.AddComment(pr, alice, "looks close")
	fk.SetCheck("sha1", "build", "completed", "failure")
	fk.SetStatus("sha1", "deploy-preview", "success", "https://preview.example/1")
	closed := fk.OpenIssue("Old", "", alice)
	fk.CloseIssue(closed, "completed")

	cases := []struct {
		label, event string
		body         []byte
		item, typ    string
	}{
		{"issue", "issues", fk.IssuesHook("labeled", issue), "issue/1", gh.TypeIssueOpen},
		{"closed issue", "issues", fk.IssuesHook("closed", closed), "issue/" + strconv.Itoa(closed), gh.TypeIssueClosed},
		{"human comment", "issue_comment", fk.CommentHook("created", human), "issue/1/comment/" + strconv.FormatInt(human, 10), gh.TypeCommentHuman},
		{"bot comment", "issue_comment", fk.CommentHook("created", bot), "issue/1/comment/" + strconv.FormatInt(bot, 10), gh.TypeCommentBot},
		{"agent comment", "issue_comment", fk.CommentHook("created", agent), "issue/1/comment/" + strconv.FormatInt(agent, 10), gh.TypeCommentAgent},
		{"pr", "pull_request", fk.PullHook("opened", pr), "pr/" + strconv.Itoa(pr), gh.TypePullOpen},
		{"review", "pull_request_review", fk.ReviewHook("submitted", pr, review), "pr/" + strconv.Itoa(pr) + "/review/" + strconv.FormatInt(review, 10), gh.TypeCommentHuman},
		{"inline", "pull_request_review_comment", fk.ReviewCommentHook("created", inline), "pr/" + strconv.Itoa(pr) + "/review_comment/" + strconv.FormatInt(inline, 10), gh.TypeCommentHuman},
		{"pr conversation", "issue_comment", fk.CommentHook("created", prComment), "pr/" + strconv.Itoa(pr) + "/comment/" + strconv.FormatInt(prComment, 10), gh.TypeCommentHuman},
		{"checks", "check_run", fk.CheckRunHook("sha1", pr), "pr/" + strconv.Itoa(pr) + "/checks", gh.TypeChecksFailed},
	}
	scanned := f.scan(t)
	for i, tc := range cases {
		o := onlyObservation(t, f.push(t, tc.event, "d-"+strconv.Itoa(i), tc.body))
		if o.ID != tc.item || o.Event.Type != tc.typ {
			t.Fatalf("%s: item %s type %s", tc.label, o.ID, o.Event.Type)
		}
		s, ok := scanned[tc.item]
		if !ok {
			t.Fatalf("%s: reconcile did not observe %s", tc.label, tc.item)
		}
		sameIdentity(t, tc.label, o, s)
	}
	// Everything on the PR correlates with the issue it closes.
	for _, item := range []string{"pr/" + strconv.Itoa(pr), "pr/" + strconv.Itoa(pr) + "/review/" + strconv.FormatInt(review, 10), "pr/" + strconv.Itoa(pr) + "/review_comment/" + strconv.FormatInt(inline, 10), "pr/" + strconv.Itoa(pr) + "/checks", "pr/" + strconv.Itoa(pr) + "/comment/" + strconv.FormatInt(prComment, 10)} {
		if k := key(scanned[item]); k != "issue/1" {
			t.Fatalf("%s correlates with %s, want issue/1", item, k)
		}
	}
	checks := payload[gh.ChecksPayload](t, scanned["pr/"+strconv.Itoa(pr)+"/checks"])
	if !checks.Complete || !reflect.DeepEqual(checks.Failed, []string{"build"}) || checks.Preview != "https://preview.example/1" {
		t.Fatalf("checks: %+v", checks)
	}
	agentPayload := payload[gh.CommentPayload](t, scanned["issue/1/comment/"+strconv.FormatInt(agent, 10)])
	if agentPayload.Agent == nil || agentPayload.Agent.Work != "work/x" {
		t.Fatalf("agent marker: %+v", agentPayload)
	}
	// A status event on the same head yields the same aggregate.
	o := onlyObservation(t, f.push(t, "status", "d-status", fk.StatusHook("sha1", "success")))
	sameIdentity(t, "status", o, scanned["pr/"+strconv.Itoa(pr)+"/checks"])
}

// A review with no body (GitHub wraps every inline comment and reply in one,
// the agent's own replies included) is observed by neither push nor
// reconcile: on the real sandbox (03-06) it was classified human and woke a
// waiting agent after each of its own replies, a wake loop. Only reviews
// with a body are observed.
func TestEmptyReviewIsNotObserved(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	fk.OpenIssue("Header", "wobbles", alice, "bug")
	pr := fk.OpenPull("Blue header", "Closes #1", "feature/blue", "sha1", alice)
	empty := fk.AddReview(pr, alice, "COMMENTED", "")
	approved := fk.AddReview(pr, alice, "APPROVED", "  ")
	scanned := f.scan(t)
	for _, id := range []int64{empty, approved} {
		item := "pr/" + strconv.Itoa(pr) + "/review/" + strconv.FormatInt(id, 10)
		if _, ok := scanned[item]; ok {
			t.Fatalf("reconcile observed the bodiless review %s", item)
		}
		if res := f.push(t, "pull_request_review", "d-empty-"+strconv.FormatInt(id, 10), fk.ReviewHook("submitted", pr, id)); len(res.Observations) != 0 {
			t.Fatalf("push observed the bodiless review %s: %+v", item, res.Observations)
		}
	}
}

func TestRawDeliveryIsDeclaredOneShot(t *testing.T) {
	f := newFixture(t, "")
	res := f.push(t, "push", "d-push", f.fake.PushHook("refs/heads/main", "a", "b", "README.md"))
	if len(res.Observations) != 0 || len(res.Events) != 1 {
		t.Fatalf("push delivery: %+v", res)
	}
	e := res.Events[0]
	var p gh.DeliveryPayload
	_ = json.Unmarshal(e.Data.Data, &p)
	if e.DeliveryID != "delivery/d-push" || p.Event != "push" || p.Ref != "refs/heads/main" || !reflect.DeepEqual(p.Files, []string{"README.md"}) {
		t.Fatalf("raw delivery: %+v %+v", e, p)
	}
	for _, fam := range f.in.Spec().Families {
		if fam.Type == gh.TypeDelivery && (fam.ReDerivable || fam.Loss == "") {
			t.Fatal("raw delivery must be declared non-re-derivable with a loss limitation")
		}
	}
	// Another repository's delivery and pings are ignored.
	other := []byte(`{"repository":{"full_name":"someone/else"},"action":"opened"}`)
	if res := f.push(t, "issues", "d-other", other); len(res.Observations)+len(res.Events) != 0 {
		t.Fatal("foreign repository delivery accepted")
	}
	if res := f.push(t, "ping", "d-ping", []byte(`{"repository":{"full_name":"`+repo+`"}}`)); len(res.Observations)+len(res.Events) != 0 {
		t.Fatal("ping produced events")
	}
}

// A finished check suite or run names its branch, commit and conclusion in
// the raw delivery, so a job can run when the default branch goes red.
func TestCheckDeliveryNamesBranchAndConclusion(t *testing.T) {
	f := newFixture(t, "")
	raw := func(event, id string, body []byte) gh.DeliveryPayload {
		t.Helper()
		res := f.push(t, event, id, body)
		if len(res.Events) != 1 {
			t.Fatalf("%s: %+v", event, res)
		}
		var p gh.DeliveryPayload
		if err := json.Unmarshal(res.Events[0].Data.Data, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f.fake.CommitOnBranch("main", "abc123")
	f.fake.CommitOnBranch("main", "def456")
	suite := raw("check_suite", "d-suite", f.fake.CheckSuiteHook("main", "abc123", "failure"))
	if suite.Ref != "refs/heads/main" || suite.After != "abc123" || suite.Conclusion != "failure" || suite.Action != "completed" {
		t.Fatalf("check_suite delivery: %+v", suite)
	}
	run := raw("check_run", "d-run", []byte(`{"action":"completed","repository":{"full_name":"`+repo+`"},`+
		`"check_run":{"head_sha":"def456","status":"completed","conclusion":"timed_out","check_suite":{"head_branch":"main"},"pull_requests":[]}}`))
	if run.Ref != "refs/heads/main" || run.After != "def456" || run.Conclusion != "timed_out" {
		t.Fatalf("check_run delivery: %+v", run)
	}
	// A fork PR from the fork's own main: GitHub names head_branch main for
	// a commit this repository's main doesn't have, so no branch is named.
	fork := raw("check_suite", "d-fork", f.fake.CheckSuiteHook("main", "f0rk", "failure"))
	if fork.Ref != "" || fork.After != "f0rk" || fork.Conclusion != "failure" {
		t.Fatalf("fork check_suite delivery: %+v", fork)
	}
	// Nor for a branch this repository doesn't have.
	if gone := raw("check_suite", "d-gone", f.fake.CheckSuiteHook("feature", "abc123", "failure")); gone.Ref != "" {
		t.Fatalf("unknown branch check_suite delivery: %+v", gone)
	}
	// A push keeps its own fields and names no conclusion.
	push := raw("push", "d-push", f.fake.PushHook("refs/heads/main", "a", "b"))
	if push.Ref != "refs/heads/main" || push.After != "b" || push.Conclusion != "" {
		t.Fatalf("push delivery: %+v", push)
	}
}

func TestReopenedIssueIsANewGeneration(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	n := fk.OpenIssue("Header", "", alice)
	before := fk.AddComment(n, alice, "first")
	fk.CloseIssue(n, "completed")
	closedPush := onlyObservation(t, f.push(t, "issues", "d-close", fk.IssuesHook("closed", n)))
	if key(closedPush) != "issue/1" || closedPush.Event.Type != gh.TypeIssueClosed {
		t.Fatalf("close: %s %s", key(closedPush), closedPush.Event.Type)
	}
	reopen := fk.ReopenIssue(n)
	gen := "reopen-" + strconv.FormatInt(reopen, 10)
	after := fk.AddComment(n, alice, "again")
	reopened := onlyObservation(t, f.push(t, "issues", "d-reopen", fk.IssuesHook("reopened", n)))
	p := payload[gh.IssuePayload](t, reopened)
	if key(reopened) != "issue/1/generation/"+gen || p.Generation != gen || p.Predecessor != "issue/1" || reopened.Event.Type != gh.TypeIssueOpen {
		t.Fatalf("reopen: key %s payload %+v", key(reopened), p)
	}
	scanned := f.scan(t)
	sameIdentity(t, "reopened", reopened, scanned["issue/1"])
	if k := key(scanned["issue/1/comment/"+strconv.FormatInt(before, 10)]); k != "issue/1" {
		t.Fatalf("comment before reopening correlates with %s", k)
	}
	if k := key(scanned["issue/1/comment/"+strconv.FormatInt(after, 10)]); k != "issue/1/generation/"+gen {
		t.Fatalf("comment after reopening correlates with %s", k)
	}
	// Closing the reopened generation addresses that generation's Work.
	fk.CloseIssue(n, "not_planned")
	closedAgain := onlyObservation(t, f.push(t, "issues", "d-close-2", fk.IssuesHook("closed", n)))
	if key(closedAgain) != "issue/1/generation/"+gen || closedAgain.Version == closedPush.Version {
		t.Fatalf("second close: %s", key(closedAgain))
	}
	// A second reopening moves on again and names the first as predecessor.
	second := fk.ReopenIssue(n)
	o := onlyObservation(t, f.push(t, "issues", "d-reopen-2", fk.IssuesHook("reopened", n)))
	p = payload[gh.IssuePayload](t, o)
	if p.Generation != "reopen-"+strconv.FormatInt(second, 10) || p.Predecessor != "issue/1/generation/"+gen {
		t.Fatalf("second reopen: %+v", p)
	}
}

func TestProjectedLabelsDoNotChangeIssueIdentity(t *testing.T) {
	f := newFixture(t, "")
	n := f.fake.OpenIssue("Header", "", alice, "bug")
	base := f.scan(t)["issue/1"]
	// Status labels are excluded: projecting them never produces input.
	f.fake.SetLabels(n, "bug", "ready-for-review")
	if again := f.scan(t)["issue/1"]; again.Version != base.Version {
		t.Fatal("a status label changed the issue version")
	}
	// A comment bumps updated_at only.
	f.fake.AddComment(n, alice, "hello")
	if again := f.scan(t)["issue/1"]; again.Version != base.Version {
		t.Fatal("a comment changed the issue version")
	}
	// Labels routes can see (including the claim) are observed.
	f.fake.SetLabels(n, "bug", "agent-in-progress")
	claimed := f.scan(t)["issue/1"]
	if claimed.Version == base.Version || !reflect.DeepEqual(payload[gh.IssuePayload](t, claimed).Labels, []string{"agent-in-progress", "bug"}) {
		t.Fatal("claim label not observed")
	}
}

func TestPullRequestCorrelation(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	fk.OpenIssue("One", "", alice)
	fk.OpenIssue("Two", "", alice)
	byBranch := fk.OpenPull("Two", "no refs here", "agent/issue-2", "s2", alice)
	byBody := fk.OpenPull("One", "Fixes #1 and refs #2", "x", "s1", alice)
	orphan := fk.OpenPull("Chore", "Bump deps", "chore", "s3", alice)
	foreign := fk.OpenPull("Elsewhere", "Closes other/repo#2", "y", "s4", alice)
	missing := fk.OpenPull("Typo", "Closes #999", "z", "s5", alice)
	scanned := f.scan(t)
	want := map[int]string{byBranch: "issue/2", byBody: "issue/1", orphan: "pr/" + strconv.Itoa(orphan), foreign: "pr/" + strconv.Itoa(foreign), missing: "pr/" + strconv.Itoa(missing)}
	for n, k := range want {
		if got := key(scanned["pr/"+strconv.Itoa(n)]); got != k {
			t.Fatalf("PR %d correlates with %s, want %s", n, got, k)
		}
	}
	fk.MergePull(byBody)
	merged := onlyObservation(t, f.push(t, "pull_request", "d-merge", fk.PullHook("closed", byBody)))
	if merged.Event.Type != gh.TypePullMerged || key(merged) != "issue/1" {
		t.Fatalf("merge: %s %s", merged.Event.Type, key(merged))
	}
	sameIdentity(t, "merged", merged, f.scan(t)["pr/"+strconv.Itoa(byBody)])
}

// Issues and PRs carry their author's association, and a PR whether its
// head is a fork, the same from push and reconcile, for admission.
func TestAuthorAssociationAndForkAreObserved(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	issue := fk.OpenIssue("Stranger", "", alice)
	fk.SetAssociation(issue, "NONE")
	own := fk.OpenPull("Own", "", "own", "s1", alice)
	fork := fk.OpenPull("Fork", "", "patch-1", "s2", alice)
	fk.SetHeadRepo(fork, "mallory/repo")
	gone := fk.OpenPull("Gone", "", "patch-2", "s3", alice)
	fk.SetHeadRepo(gone, "-")
	fk.SetAssociation(gone, "FIRST_TIME_CONTRIBUTOR")
	scanned := f.scan(t)
	var ip gh.IssuePayload
	if err := json.Unmarshal(scanned["issue/"+strconv.Itoa(issue)].Event.Data.Data, &ip); err != nil || ip.AuthorAssociation != "NONE" {
		t.Fatalf("issue association %q (%v)", ip.AuthorAssociation, err)
	}
	for n, want := range map[int]struct {
		fork  bool
		assoc string
	}{own: {false, "OWNER"}, fork: {true, "OWNER"}, gone: {true, "FIRST_TIME_CONTRIBUTOR"}} {
		var p gh.PullPayload
		if err := json.Unmarshal(scanned["pr/"+strconv.Itoa(n)].Event.Data.Data, &p); err != nil || p.Fork != want.fork || p.AuthorAssociation != want.assoc {
			t.Fatalf("PR %d: fork=%v association=%q, want %+v (%v)", n, p.Fork, p.AuthorAssociation, want, err)
		}
	}
	pushed := onlyObservation(t, f.push(t, "pull_request", "d-fork", fk.PullHook("opened", fork)))
	sameIdentity(t, "fork", pushed, scanned["pr/"+strconv.Itoa(fork)])
}

// GitHub sends no webhook when a PR picks up conflicts (#209), so reconcile
// reads each open issue PR's mergeability. One conflicting head is observed
// once however often main moves after it; a new head is new news.
func TestConflictingPullIsObservedOncePerHead(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	fk.OpenIssue("Header", "", alice)
	pr := fk.OpenPull("Blue header", "Closes #1", "agent/issue-1", "sha1", alice)
	orphan := fk.OpenPull("Chore", "Bump deps", "chore", "sha2", alice)
	item := "pr/" + strconv.Itoa(pr) + "/conflicts"
	no, yes := false, true

	for _, m := range []*bool{nil, &yes} {
		fk.SetMergeable(pr, m)
		if _, ok := f.scan(t)[item]; ok {
			t.Fatalf("mergeable=%v observed as conflicts", m)
		}
	}
	fk.SetMergeable(pr, &no)
	fk.SetMergeable(orphan, &no)
	scanned := f.scan(t)
	o, ok := scanned[item]
	if !ok || o.Event.Type != gh.TypePullConflicts || key(o) != "issue/1" {
		t.Fatalf("conflicts not observed for the issue: %+v", o)
	}
	if p := payload[gh.ConflictsPayload](t, o); p.HeadSHA != "sha1" || p.BaseRef != "main" || p.Number != pr {
		t.Fatalf("payload: %+v", p)
	}
	if _, ok := scanned["pr/"+strconv.Itoa(orphan)+"/conflicts"]; ok {
		t.Fatal("a PR with no issue observed as conflicts")
	}
	// The pull_request delivery carries mergeable too, with the same identity.
	res := f.push(t, "pull_request", "d-sync", fk.PullHook("edited", pr))
	var pushed *extension.Observation
	for i := range res.Observations {
		if res.Observations[i].ID == item {
			pushed = &res.Observations[i]
		}
	}
	if pushed == nil {
		t.Fatalf("push did not observe the conflicts: %+v", res.Observations)
	}
	sameIdentity(t, "conflicts", *pushed, o)
	// main moving again changes nothing about this head.
	f.now = f.now.Add(time.Hour)
	if again := f.scan(t)[item]; again.Version != o.Version {
		t.Fatal("a later scan of the same conflicting head is a new version")
	}
	// The agent pushes, and the new head conflicts again: new news.
	fk.PushPull(pr, "sha3")
	if again := f.scan(t)[item]; again.Version == o.Version {
		t.Fatal("a new conflicting head kept the old version")
	}
}

func TestSharedTrackerReferences(t *testing.T) {
	f := newFixture(t, `,"track_issues":false,"tracker":{"repo":"octo/tracker","namespace":"github/github.com/octo/tracker"}`)
	fk := f.fake
	local := fk.OpenIssue("Local issue", "", alice)
	bare := fk.OpenPull("Bare", "Closes #1", "x", "s1", alice)
	scanned := f.scan(t)
	if _, ok := scanned["issue/"+strconv.Itoa(local)]; ok {
		t.Fatal("track_issues=false still observed the repository's issues")
	}
	// A bare #1 names this repository, not the shared tracker.
	if o := scanned["pr/"+strconv.Itoa(bare)]; key(o) != "pr/"+strconv.Itoa(bare) || o.Event.CorrelationKey.Source != ns {
		t.Fatalf("bare reference in a shared-tracker repo: %+v", o.Event.CorrelationKey)
	}
}

func TestClosedItemsAgeOutOfTheHorizon(t *testing.T) {
	f := newFixture(t, `,"closed_horizon_hours":1`)
	fk := f.fake
	open := fk.OpenIssue("Open", "", alice)
	old := fk.OpenIssue("Old", "", alice)
	fk.CloseIssue(old, "completed")
	f.now = fk.Now().Add(30 * time.Minute)
	scanned := f.scan(t)
	if _, ok := scanned["issue/"+strconv.Itoa(old)]; !ok {
		t.Fatal("recently closed issue not observed")
	}
	f.now = fk.Now().Add(3 * time.Hour)
	scanned = f.scan(t)
	if _, ok := scanned["issue/"+strconv.Itoa(old)]; ok {
		t.Fatal("closed issue beyond the horizon still observed")
	}
	if _, ok := scanned["issue/"+strconv.Itoa(open)]; !ok {
		t.Fatal("old open issue must stay observed regardless of age")
	}
	// The horizon runs from the close, so news on a long-closed issue
	// doesn't bring it back.
	fk.Advance(3 * time.Hour)
	fk.AddComment(old, alice, "thanks")
	f.now = fk.Now().Add(time.Minute)
	if _, ok := f.scan(t)["issue/"+strconv.Itoa(old)]; ok {
		t.Fatal("closed issue observed again because of a later comment")
	}
}

// scanFrom runs a whole generation after the one checkpoint names, as the
// runtime does once a generation has completed.
func (f *fixture) scanFrom(t *testing.T, checkpoint string) (map[string]extension.Observation, string) {
	t.Helper()
	out := map[string]extension.Observation{}
	scan := extension.Scan{Source: src, Checkpoint: checkpoint}
	for i := 0; i < 1000; i++ {
		page, err := f.in.Reconcile(context.Background(), scan)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		for _, o := range page.Observations {
			out[o.ID] = o
		}
		if page.Complete {
			return out, page.Checkpoint
		}
		scan.ScanID, scan.Cursor, scan.Checkpoint = page.ScanID, page.NextCursor, page.Checkpoint
	}
	t.Fatal("scan did not finish")
	return nil, ""
}

func TestScanListsOnlyOpenAndRecentlyClosedItems(t *testing.T) {
	f := newFixture(t, `,"track_pull_requests":true`)
	fk := f.fake
	var old []int
	for i := 0; i < 150; i++ {
		n := fk.OpenPull("old "+strconv.Itoa(i), "", "old/"+strconv.Itoa(i), "sha-old-"+strconv.Itoa(i), alice)
		fk.MergePull(n)
		old = append(old, n)
	}
	fk.Advance(2 * time.Hour)
	open := fk.OpenPull("Open", "", "feature/open", "sha-open", alice)
	recent := fk.OpenPull("Recent", "", "feature/recent", "sha-recent", alice)
	fk.MergePull(recent)
	f.now = fk.Now().Add(time.Minute)
	before := len(fk.Requests())
	scanned := f.scan(t)
	for _, n := range []int{open, recent} {
		if _, ok := scanned["pr/"+strconv.Itoa(n)]; !ok {
			t.Fatalf("PR %d not observed", n)
		}
	}
	for _, n := range old {
		if _, ok := scanned["pr/"+strconv.Itoa(n)]; ok {
			t.Fatalf("PR %d closed before the horizon was observed", n)
		}
	}
	// The closed listing stops at the horizon, so history costs one page
	// and no PR's details are read.
	for _, r := range fk.Requests()[before:] {
		for _, n := range old {
			if strings.Contains(r, "/pulls/"+strconv.Itoa(n)+"/") || strings.HasSuffix(r, "/pulls/"+strconv.Itoa(n)) {
				t.Fatalf("read the details of PR %d, closed long ago: %s", n, r)
			}
		}
		if strings.Contains(r, "/pulls?") && strings.Contains(r, "page=2") {
			t.Fatalf("listed past the horizon: %s", r)
		}
	}
}

func TestScanAfterAnOutageLooksBackToTheLastCompletedGeneration(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	n := fk.OpenIssue("Header", "", alice)
	_, checkpoint := f.scanFrom(t, "")
	// Scans fail for three hours, and the issue closes in the meantime.
	fk.Advance(time.Hour)
	fk.CloseIssue(n, "completed")
	f.now = fk.Now().Add(2 * time.Hour)
	scanned, _ := f.scanFrom(t, checkpoint)
	if o, ok := scanned["issue/"+strconv.Itoa(n)]; !ok || o.Event.Type != gh.TypeIssueClosed {
		t.Fatalf("close made during the outage was not observed: %+v", o)
	}
	// Without the earlier generation, the horizon alone applies.
	if _, ok := f.scan(t)["issue/"+strconv.Itoa(n)]; ok {
		t.Fatal("closed issue beyond the horizon observed without a checkpoint")
	}
}

func TestItemClosedDuringAScanIsObservedOnce(t *testing.T) {
	f := newFixture(t, `,"track_pull_requests":true`)
	fk := f.fake
	first := fk.OpenPull("First", "", "feature/first", "sha-first", alice)
	fk.OpenPull("Second", "", "feature/second", "sha-second", alice)
	f.in = gh.NewIngress(f.in.Settings(), clientFor(t, f), func() time.Time { return f.now }, gh.IngressOptions{BatchUnits: 1})
	seen := map[string]string{}
	var page extension.Snapshot
	var err error
	scan := extension.Scan{Source: src}
	for seen["pr/"+strconv.Itoa(first)] == "" {
		if page, err = f.in.Reconcile(context.Background(), scan); err != nil || page.Complete {
			t.Fatalf("PR %d not observed open: %+v %v", first, page, err)
		}
		for _, o := range page.Observations {
			seen[o.ID] = o.Version
		}
		scan.ScanID, scan.Cursor = page.ScanID, page.NextCursor
	}
	fk.Advance(time.Hour) // the merge comes after the generation began
	fk.MergePull(first)
	for !page.Complete {
		if page, err = f.in.Reconcile(context.Background(), scan); err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Observations {
			if v, ok := seen[o.ID]; ok && v != o.Version {
				t.Fatalf("%s observed twice with different versions in one generation", o.ID)
			}
			seen[o.ID] = o.Version
		}
		scan.Cursor = page.NextCursor
	}
}

func TestEarlierGenerationsRestart(t *testing.T) {
	f := newFixture(t, "")
	f.fake.OpenIssue("One", "", alice)
	old := "gen-" + strconv.FormatInt(f.now.Add(-time.Minute).UnixNano(), 10)
	for _, cursor := range []string{"", "issues:1:0", "pulls:2:0"} {
		p, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src, ScanID: old, Cursor: cursor})
		if err != nil || p.Complete || p.NextCursor != "" || len(p.Observations) != 0 {
			t.Fatalf("cursor %q of an earlier version's generation must restart: %+v %v", cursor, p, err)
		}
	}
	page, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src, ScanID: page.ScanID, Cursor: "pulls:1:0"})
	if err != nil || p.Complete || p.NextCursor != "" {
		t.Fatalf("an unknown phase must restart: %+v %v", p, err)
	}
}

func TestScanPagesAreStableAndBounded(t *testing.T) {
	f := newFixture(t, "")
	for i := 0; i < 230; i++ {
		f.fake.OpenIssue("issue "+strconv.Itoa(i), "", alice)
	}
	f.in = gh.NewIngress(f.in.Settings(), clientFor(t, f), func() time.Time { return f.now }, gh.IngressOptions{BatchUnits: 1})
	scanned := f.scan(t)
	if len(scanned) != 230 {
		t.Fatalf("observed %d issues, want 230", len(scanned))
	}
	// Unchanged pages are revalidated conditionally on the next generation.
	before := f.fake.NotModified()
	f.scan(t)
	if f.fake.NotModified() <= before {
		t.Fatal("second generation made no conditional requests")
	}
}

func clientFor(t *testing.T, f *fixture) *gh.Client {
	cfg, _ := gh.ParseConnection([]byte(`{"api_url":"` + f.fake.URL + `"}`))
	c, err := gh.NewClient(cfg, func(context.Context, string) (string, error) { return "tok", nil }, gh.ClientOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRateLimitedScanIsAnErrorAndResumes(t *testing.T) {
	f := newFixture(t, "")
	f.fake.OpenIssue("One", "", alice)
	page, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	f.fake.RateLimitPrimary(f.now.Add(time.Hour))
	next := extension.Scan{Source: src, ScanID: page.ScanID, Cursor: page.NextCursor}
	var limit *gh.RateLimitError
	if _, err := f.in.Reconcile(context.Background(), next); !errors.As(err, &limit) {
		t.Fatalf("rate limited page must fail, not complete: %v", err)
	}
	if _, err := f.in.Reconcile(context.Background(), next); !errors.As(err, &limit) {
		t.Fatalf("paused page must fail locally: %v", err)
	}
	f.now = f.now.Add(time.Hour)
	if _, err := f.in.Reconcile(context.Background(), next); err != nil {
		t.Fatalf("after reset: %v", err)
	}
}

func TestRepeatedlyRejectedPageRestartsTheGeneration(t *testing.T) {
	f := newFixture(t, "")
	f.fake.OpenIssue("One", "", alice)
	page, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	next := extension.Scan{Source: src, ScanID: page.ScanID, Cursor: page.NextCursor}
	for i := 0; i < 3; i++ { // the runtime keeps asking for the same page again
		if p, err := f.in.Reconcile(context.Background(), next); err != nil || p.NextCursor == "" && !p.Complete {
			t.Fatalf("attempt %d: %+v %v", i, p, err)
		}
	}
	p, err := f.in.Reconcile(context.Background(), next)
	if err != nil || p.Complete || p.NextCursor != "" || len(p.Observations) != 0 {
		t.Fatalf("expected an exhausted partial page to restart: %+v %v", p, err)
	}
	// So does a generation older than MaxScanAge.
	f.now = f.now.Add(7 * time.Hour)
	p, err = f.in.Reconcile(context.Background(), extension.Scan{Source: src, ScanID: page.ScanID, Cursor: page.NextCursor})
	if err != nil || p.Complete || p.NextCursor != "" {
		t.Fatalf("stale generation: %+v %v", p, err)
	}
}

func TestPushRejectsUnverifiedShapes(t *testing.T) {
	f := newFixture(t, "")
	for name, d := range map[string]extension.Delivery{
		"raw body":     {ID: "x", ContentType: "application/json", Body: []byte(`{}`)},
		"id mismatch":  {ID: "x", ContentType: gh.DeliveryContentType, Body: []byte(`{"event":"issues","delivery":"y","payload":{}}`)},
		"wrong source": {ID: "x", ContentType: gh.DeliveryContentType, Body: []byte(`{"event":"issues","delivery":"x","payload":{}}`)},
	} {
		s := src
		if name == "wrong source" {
			s.Namespace = "other"
		}
		if _, err := f.in.Push(context.Background(), s, d); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestEditedCommentIsANewVersion(t *testing.T) {
	f := newFixture(t, "")
	n := f.fake.OpenIssue("One", "", alice)
	id := f.fake.AddComment(n, alice, "first")
	created := onlyObservation(t, f.push(t, "issue_comment", "d-1", f.fake.CommentHook("created", id)))
	f.fake.EditComment(id, "edited")
	edited := onlyObservation(t, f.push(t, "issue_comment", "d-2", f.fake.CommentHook("edited", id)))
	if created.Version == edited.Version || !payload[gh.CommentPayload](t, edited).Edited || payload[gh.CommentPayload](t, created).Edited {
		t.Fatal("edit not distinguished")
	}
	if !strings.HasPrefix(created.Event.DeliveryID, "issue/1/comment/") || created.Event.Source != ns {
		t.Fatalf("delivery identity: %+v", created.Event)
	}
	_ = domain.ID("")
}

// With track_pull_requests, each PR state and each comment, inline comment,
// review and check on a PR is also observed under the PR's own key, the
// same way by push and reconcile. Nothing of an issue's is, and an entry
// without the option observes no copies.
func TestCopiesOfPullRequestsForClaiming(t *testing.T) {
	f := newFixture(t, `,"track_pull_requests":true`)
	fk := f.fake
	issue := fk.OpenIssue("Header", "wobbles", alice, "bug")
	fk.AddComment(issue, alice, "please make it blue")
	pr := fk.OpenPull("Blue header", "Closes #1", "agent/issue-1", "sha1", alice)
	review := fk.AddReview(pr, alice, "CHANGES_REQUESTED", "rename it")
	inline := fk.AddReviewComment(pr, alice, "nit", "header.go", 12)
	prComment := fk.AddComment(pr, alice, "looks close")
	fk.SetCheck("sha1", "build", "completed", "success")
	p := strconv.Itoa(pr)
	scanned := f.scan(t)
	cases := []struct {
		label, event, item string
		body               []byte
	}{
		{"pr", "pull_request", "pr/" + p, fk.PullHook("opened", pr)},
		{"review", "pull_request_review", "pr/" + p + "/review/" + strconv.FormatInt(review, 10), fk.ReviewHook("submitted", pr, review)},
		{"inline", "pull_request_review_comment", "pr/" + p + "/review_comment/" + strconv.FormatInt(inline, 10), fk.ReviewCommentHook("created", inline)},
		{"pr conversation", "issue_comment", "pr/" + p + "/comment/" + strconv.FormatInt(prComment, 10), fk.CommentHook("created", prComment)},
	}
	for i, tc := range cases {
		orig, copied := scanned[tc.item], scanned["pull_request/"+strings.TrimPrefix(tc.item, "pr/")]
		if key(orig) != "issue/1" || key(copied) != gh.PullRequestKey(pr) {
			t.Fatalf("%s: original keyed %s, copy keyed %s", tc.label, key(orig), key(copied))
		}
		if copied.Event.Type != orig.Event.Type || string(copied.Event.Data.Data) != string(orig.Event.Data.Data) || copied.Event.ID == orig.Event.ID {
			t.Fatalf("%s: the copy is not the original under its own identity: %+v", tc.label, copied.Event)
		}
		res := f.push(t, tc.event, "d-"+strconv.Itoa(i), tc.body)
		if len(res.Observations) != 2 {
			t.Fatalf("%s: push observed %d, want the original and its copy", tc.label, len(res.Observations))
		}
		sameIdentity(t, tc.label, res.Observations[0], orig)
		sameIdentity(t, tc.label+" copy", res.Observations[1], copied)
	}
	// Every PR family is copied alike, checks included; nothing of an issue's.
	if c := scanned["pull_request/"+p+"/checks"]; key(c) != gh.PullRequestKey(pr) || c.Event.Type != gh.TypeChecksPassed {
		t.Fatalf("checks copy: %+v", c.Event)
	}
	for item := range scanned {
		if strings.HasPrefix(item, "pull_request/issue") {
			t.Fatalf("%s copied", item)
		}
	}
	plain := newFixture(t, "")
	plain.fake.OpenPull("Chore", "", "chore", "sha2", alice)
	for item := range plain.scan(t) {
		if strings.HasPrefix(item, "pull_request/") {
			t.Fatalf("%s observed without track_pull_requests", item)
		}
	}
}

// A tracking entry observes conflicts on any open PR, not only one that
// belongs to an issue, and copies them under the PR's own key.
func TestTrackedPullConflictsWithoutAnIssue(t *testing.T) {
	f := newFixture(t, `,"track_pull_requests":true`)
	pr := f.fake.OpenPull("Bump deps", "", "chore/deps", "sha9", alice)
	no := false
	f.fake.SetMergeable(pr, &no)
	scanned := f.scan(t)
	p := strconv.Itoa(pr)
	if o := scanned["pr/"+p+"/conflicts"]; key(o) != "pr/"+p || o.Event.Type != gh.TypePullConflicts {
		t.Fatalf("conflicts: %+v", o.Event)
	}
	if o := scanned["pull_request/"+p+"/conflicts"]; key(o) != gh.PullRequestKey(pr) {
		t.Fatalf("conflicts copy: %+v", o.Event)
	}
	plain := newFixture(t, "")
	pr = plain.fake.OpenPull("Bump deps", "", "chore/deps", "sha9", alice)
	plain.fake.SetMergeable(pr, &no)
	if _, ok := plain.scan(t)["pr/"+strconv.Itoa(pr)+"/conflicts"]; ok {
		t.Fatal("conflicts observed on a PR with no issue and no tracking")
	}
}

// An author whose association wouldn't be admitted carries their
// permission on the repository, read once per login per scan: GitHub
// reports a member whose org membership is private as CONTRIBUTOR (#432).
// An admitted association, a bot and a failed read carry none.
func TestUnadmittedAuthorCarriesRepositoryPermission(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	member := githubtest.User{Login: "Priya", Type: "User"}
	outsider := githubtest.User{Login: "mallory", Type: "User"}
	first := fk.OpenIssue("Private member", "", member)
	second := fk.OpenIssue("Private member again", "", member)
	stranger := fk.OpenIssue("Outside contributor", "", outsider)
	owner := fk.OpenIssue("Owner", "", alice)
	bot := fk.OpenIssue("Bot", "", githubtest.User{Login: "dependabot[bot]", Type: "Bot"})
	closed := fk.OpenIssue("Closed", "", githubtest.User{Login: "eve", Type: "User"})
	fk.CloseIssue(closed, "completed")
	pr := fk.OpenPull("Private member's PR", "", "fix", "s1", member)
	for _, n := range []int{first, second, stranger, bot, closed, pr} {
		fk.SetAssociation(n, "CONTRIBUTOR")
	}
	fk.SetCollaborator("priya", "write")
	fk.SetCollaborator("mallory", "read")
	permission := func(o extension.Observation) string {
		var p struct {
			Permission string `json:"author_permission"`
		}
		if err := json.Unmarshal(o.Event.Data.Data, &p); err != nil {
			t.Fatal(err)
		}
		return p.Permission
	}
	scanned := f.scan(t)
	for item, want := range map[string]string{
		"issue/" + strconv.Itoa(first): "write", "issue/" + strconv.Itoa(second): "write", "issue/" + strconv.Itoa(stranger): "read",
		"issue/" + strconv.Itoa(owner): "", "issue/" + strconv.Itoa(bot): "", "issue/" + strconv.Itoa(closed): "", "pr/" + strconv.Itoa(pr): "write",
	} {
		if got := permission(scanned[item]); got != want {
			t.Fatalf("%s: author_permission %q, want %q", item, got, want)
		}
	}
	reads := map[string]int{}
	for _, r := range fk.Requests() {
		if strings.Contains(r, "/collaborators/") {
			reads[r]++
		}
	}
	if len(reads) != 2 || reads["GET /repos/"+repo+"/collaborators/Priya/permission"] != 1 {
		t.Fatalf("permission reads %v, want one per unadmitted person", reads)
	}
	// A webhook delivery reads it too; a failed read leaves it out, so
	// admission fails closed.
	if got := permission(onlyObservation(t, f.push(t, "issues", "d-member", fk.IssuesHook("opened", first)))); got != "write" {
		t.Fatalf("pushed author_permission %q", got)
	}
	fk.FailNext("/repos/"+repo+"/collaborators/", 100)
	if got := permission(onlyObservation(t, f.push(t, "issues", "d-fail", fk.IssuesHook("opened", first)))); got != "" {
		t.Fatalf("a failed read carried author_permission %q", got)
	}
}

// A permission read is spent from a scan page's budget, so a page of
// issues by many unadmitted authors ends sooner than one by the owner.
func TestPermissionReadsSpendTheScanBudget(t *testing.T) {
	firstPage := func(association string) int {
		f := newFixture(t, "")
		for i := 0; i < 3; i++ {
			n := f.fake.OpenIssue("issue "+strconv.Itoa(i), "", githubtest.User{Login: "user" + strconv.Itoa(i), Type: "User"})
			f.fake.SetAssociation(n, association)
		}
		f.in = gh.NewIngress(f.in.Settings(), clientFor(t, f), func() time.Time { return f.now }, gh.IngressOptions{BatchUnits: 2, PageBudget: time.Minute})
		page, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Observations)
	}
	if owners, contributors := firstPage("OWNER"), firstPage("CONTRIBUTOR"); owners != 3 || contributors != 2 {
		t.Fatalf("first page held %d issues by owners and %d by contributors, want 3 and 2", owners, contributors)
	}
}

// Only this connection's own App bot is marked by_connection, which admits
// what its agents file: another App's bot isn't, nor is anyone on a token
// connection, which has no App bot of its own (#346).
func TestOwnBotIsMarkedByConnection(t *testing.T) {
	fake := githubtest.New(repo, "tok")
	t.Cleanup(fake.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake.AddApp("42", &key.PublicKey)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	_, own, _ := githubtest.AppBot("42")
	marker := "Follow-up\n\n<!-- open-conduit:agent work=work/1 -->"
	mine := fake.OpenIssue("Mine", marker, githubtest.User{Login: own, Type: "Bot"})
	other := fake.OpenIssue("Other", marker, githubtest.User{Login: "dependabot[bot]", Type: "Bot"})
	person := fake.OpenIssue("Person", marker, alice)
	for _, n := range []int{mine, other, person} {
		fake.SetAssociation(n, "NONE")
	}
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	scan := func(conn string) map[string]bool {
		cfg, err := gh.ParseConnection([]byte(`{"api_url":"` + fake.URL + `"` + conn + `}`))
		if err != nil {
			t.Fatal(err)
		}
		client, err := gh.NewClient(cfg, func(_ context.Context, name string) (string, error) {
			if name == gh.SecretToken {
				return "tok", nil
			}
			return pemText, nil
		}, gh.ClientOptions{Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		s, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		f := &fixture{fake: fake, now: now, in: gh.NewIngress(s, client, func() time.Time { return now }, gh.IngressOptions{BatchUnits: 1000, PageBudget: time.Minute})}
		out := map[string]bool{}
		for item, o := range f.scan(t) {
			var p gh.IssuePayload
			if strings.Count(item, "/") == 1 && json.Unmarshal(o.Event.Data.Data, &p) == nil {
				out[p.Author] = p.ByConnection
			}
		}
		return out
	}
	if got := scan(`,"bot_app_id":"42"`); !got[own] || got["dependabot[bot]"] || got["alice"] {
		t.Fatalf("bot App connection: %v", got)
	}
	if got := scan(""); got[own] || got["dependabot[bot]"] || got["alice"] {
		t.Fatalf("token connection: %v", got)
	}
}

// An inline comment and a review stay one observation when a push moves
// the comment's anchor and when someone replies in its thread: only the
// reply is news (#402).
func TestReviewCommentsKeepTheirVersionAcrossPushesAndReplies(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	fk.OpenIssue("Header", "wobbles", alice)
	pr := fk.OpenPull("Blue header", "Closes #1", "feature/blue", "sha1", alice)
	review := fk.AddReview(pr, alice, "APPROVED", "ship it")
	inline := fk.AddReviewComment(pr, alice, "nit", "header.go", 12)
	reviewItem := "pr/" + strconv.Itoa(pr) + "/review/" + strconv.FormatInt(review, 10)
	inlineItem := "pr/" + strconv.Itoa(pr) + "/review_comment/" + strconv.FormatInt(inline, 10)
	before := f.scan(t)

	fk.PushPull(pr, "sha2")
	reply := fk.AddReviewReply(pr, alice, inline, "thanks, fixed")
	after := f.scan(t)
	for _, item := range []string{reviewItem, inlineItem} {
		if before[item].Version != after[item].Version {
			t.Fatalf("%s changed version after a push and a reply:\n%s\n%s", item, before[item].Event.Data.Data, after[item].Event.Data.Data)
		}
	}
	c := payload[gh.CommentPayload](t, after[inlineItem])
	if c.CommitID != "sha1" || c.Line != 12 {
		t.Fatalf("inline comment observed at %s:%d, want where it was made (sha1:12)", c.CommitID, c.Line)
	}
	if _, ok := before["pr/"+strconv.Itoa(pr)+"/review_comment/"+strconv.FormatInt(reply, 10)]; ok {
		t.Fatal("reply observed before it was made")
	}
	if _, ok := after["pr/"+strconv.Itoa(pr)+"/review_comment/"+strconv.FormatInt(reply, 10)]; !ok {
		t.Fatal("reply not observed")
	}
	// Push and reconcile still agree on the moved comment and the review.
	sameIdentity(t, "inline after push", onlyObservation(t, f.push(t, "pull_request_review_comment", "d-inline", fk.ReviewCommentHook("edited", inline))), after[inlineItem])
	sameIdentity(t, "review", onlyObservation(t, f.push(t, "pull_request_review", "d-review", fk.ReviewHook("submitted", pr, review))), after[reviewItem])
}

// A review stays one observation when a force-push makes GitHub move its
// commit_id to the new head: only a new review is news (#408).
func TestReviewKeepsItsVersionAcrossForcePush(t *testing.T) {
	f := newFixture(t, "")
	fk := f.fake
	fk.OpenIssue("Header", "wobbles", alice)
	pr := fk.OpenPull("Blue header", "Closes #1", "feature/blue", "sha1", alice)
	review := fk.AddReview(pr, alice, "APPROVED", "ship it")
	item := "pr/" + strconv.Itoa(pr) + "/review/" + strconv.FormatInt(review, 10)
	hook := fk.ReviewHook("submitted", pr, review)
	before := f.scan(t)

	fk.ForcePushPull(pr, "sha2")
	after := f.scan(t)
	if before[item].Version != after[item].Version {
		t.Fatalf("review changed version after a force-push:\n%s\n%s", before[item].Event.Data.Data, after[item].Event.Data.Data)
	}
	// The webhook delivered before the force-push and the scan after it
	// still agree.
	sameIdentity(t, "review", onlyObservation(t, f.push(t, "pull_request_review", "d-review", hook)), after[item])
}

// With acknowledge_issues, each newly opened issue's first generation is
// also observed under its acknowledgement key, the same way by push and
// reconcile. The copy carries only what the opening fixed, so a later
// label or body edit doesn't re-observe it, and a reopening isn't copied.
// An old issue, or one whose author's permission couldn't be read, isn't
// copied; an entry without the option observes no copies.
func TestCopiesOfIssueOpeningsForAcknowledgement(t *testing.T) {
	f := newFixture(t, `,"acknowledge_issues":true`)
	fk := f.fake
	outsider := githubtest.User{Login: "mallory", Type: "User"}
	n := fk.OpenIssue("Header wobbles", "on mobile", outsider)
	fk.SetAssociation(n, "NONE")
	item, ack := "issue/"+strconv.Itoa(n), "issue_ack/"+strconv.Itoa(n)
	scanned := f.scan(t)
	copied := scanned[ack]
	if key(scanned[item]) != gh.IssueKey(n, "") || key(copied) != gh.IssueAckKey(n) || copied.Event.Type != gh.TypeIssueOpen {
		t.Fatalf("original keyed %s, copy %+v", key(scanned[item]), copied.Event)
	}
	if p := payload[gh.IssuePayload](t, copied); p.Number != n || p.Author != "mallory" || p.AuthorAssociation != "NONE" || p.Body != "" {
		t.Fatalf("copy payload %+v", p)
	}
	res := f.push(t, "issues", "d-1", fk.IssuesHook("opened", n))
	if len(res.Observations) != 2 {
		t.Fatalf("push observed %d, want the issue and its copy", len(res.Observations))
	}
	sameIdentity(t, "issue", res.Observations[0], scanned[item])
	sameIdentity(t, "copy", res.Observations[1], copied)

	fk.SetLabels(n, "bug")
	res = f.push(t, "issues", "d-2", fk.IssuesHook("labeled", n))
	if len(res.Observations) != 2 || res.Observations[0].Version == scanned[item].Version || res.Observations[1].Version != copied.Version {
		t.Fatalf("a label edit re-observed the copy: %+v", res.Observations)
	}
	fk.CloseIssue(n, "completed")
	fk.ReopenIssue(n)
	if res := f.push(t, "issues", "d-3", fk.IssuesHook("reopened", n)); len(res.Observations) != 1 {
		t.Fatalf("a reopening was copied: %+v", res.Observations)
	}

	// A member whose permission couldn't be read is not taken for an
	// outsider: no copy until a read succeeds.
	member := fk.OpenIssue("Private member", "", githubtest.User{Login: "priya", Type: "User"})
	fk.SetAssociation(member, "CONTRIBUTOR")
	fk.SetCollaborator("priya", "write")
	fk.FailNext("/repos/"+repo+"/collaborators/", 1)
	if res := f.push(t, "issues", "d-4", fk.IssuesHook("opened", member)); len(res.Observations) != 1 {
		t.Fatalf("an unread permission was copied: %+v", res.Observations)
	}
	if res := f.push(t, "issues", "d-5", fk.IssuesHook("opened", member)); len(res.Observations) != 2 || payload[gh.IssuePayload](t, res.Observations[1]).AuthorPermission != "write" {
		t.Fatalf("a read permission was not copied: %+v", res.Observations)
	}

	// An issue opened longer than AckWindow ago is not acknowledged.
	f.now = fk.Now().Add(gh.AckWindow + time.Hour)
	if _, ok := f.scan(t)[ack]; ok {
		t.Fatal("an old issue's opening was copied")
	}

	plain := newFixture(t, "")
	plain.fake.OpenIssue("Header", "", outsider)
	for item := range plain.scan(t) {
		if strings.HasPrefix(item, "issue_ack/") {
			t.Fatalf("%s observed without acknowledge_issues", item)
		}
	}
	if _, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","track_issues":false,"acknowledge_issues":true}`), nil); err == nil {
		t.Fatal("acknowledge_issues without track_issues validated")
	}
}

// A failed page authors GitHub's answer, so the host can record it rather
// than an unauthored failure (#517).
func TestFailedScanAuthorsGitHubsAnswer(t *testing.T) {
	f := newFixture(t, "")
	f.fake.OpenIssue("One", "", alice)
	page, err := f.in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	f.fake.RateLimitPrimary(f.now.Add(time.Hour))
	_, err = f.in.Reconcile(context.Background(), extension.Scan{Source: src, ScanID: page.ScanID, Cursor: page.NextCursor})
	if e := protocol.Encode(err); e.Code != "rate_limited" || !strings.Contains(e.Message, "primary rate limit until") {
		t.Fatalf("rate limited page crossed as %+v (%v)", e, err)
	}
}
