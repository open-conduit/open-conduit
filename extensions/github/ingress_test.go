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
	f := &fixture{fake: fake, now: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
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
	// Everything on the PR correlates with the issue it closes (v1 attachment).
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
// waiting agent after each of its own replies, a wake loop. v1 kept only
// reviews with a body.
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
