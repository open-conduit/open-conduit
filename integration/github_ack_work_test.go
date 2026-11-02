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
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

var ghMallory = githubtest.User{Login: "mallory", Type: "User"}

// newAckEnv is the tools environment with an entry that acknowledges
// issues and a scope that routes admitted issues to an agent and every
// other new issue to an acknowledgement binding (the recipe's shell script).
func newAckEnv(t *testing.T) *toolsEnv {
	t.Helper()
	fake := githubtest.New(ghRepo, "tok")
	t.Cleanup(fake.Close)
	ext := strings.TrimSuffix(ghExtensionConfig(), "}") + `,"acknowledge_issues":true}`
	e := &toolsEnv{ghEnv: &ghEnv{t: t, fake: fake, clock: &ghClock{t: fake.Now()}, path: filepath.Join(t.TempDir(), "conduit.db"), extConfig: ext}, tokens: map[domain.ID]string{}}
	e.open()
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "widgets", "type": "github", "connection": "github", "config": json.RawMessage(ext), "secret_names": []string{"hook"}},
	}})
	agent := domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: ghScope, Name: ghScope,
			Bindings: []domain.Binding{{Name: "agent", Executor: "fixture", Requires: agent}, {Name: "thanks", Executor: "fixture", Requires: agent}},
			Routes: []domain.Route{{Name: "issues", Handler: "github", Binding: "agent", ActivePolicy: "queue"},
				{Name: "acks", Handler: "github", Binding: "thanks", ActivePolicy: "queue", Config: domain.Payload{Data: json.RawMessage(`{"claim":"acknowledgements"}`)}}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	e.register()
	e.compose()
	return e
}

// An outsider's new issue starts one acknowledgement Work, which comments
// on the issue once and completes without the issue carrying any status
// label, comment or reaction of it. A redelivery or reconcile doesn't
// start another. The trail runs from the issue's Event through the Work and
// its Execution to the comment, and the issue can still be claimed by hand
// afterwards. A trusted author's issue starts no acknowledgement, and the
// route trace says why.
func TestGitHubAcknowledgesOutsideAuthorsWithoutClaiming(t *testing.T) {
	e := newAckEnv(t)
	n := e.fake.OpenIssue("Header wobbles", "On mobile.", ghMallory)
	e.fake.SetAssociation(n, "NONE")
	if code := e.deliver("issues", "d-1", e.fake.IssuesHook("opened", n)); code != 202 && code != 200 {
		t.Fatalf("delivery: %d", code)
	}
	e.route()
	ack, ok := e.workFor(gh.IssueAckKey(n))
	if !ok || ack.Kind != gh.WorkKindAck {
		t.Fatalf("no acknowledgement Work: %+v", ack)
	}
	if _, ok := e.workFor(gh.IssueKey(n, "")); ok {
		t.Fatal("the outsider's issue was claimed")
	}
	if ack.Display == nil || ack.Display.Ref != "#"+itoa(n) {
		t.Fatalf("ack display: %+v", ack.Display)
	}

	a := e.start(ack.ID)
	if got := e.bindingOf(ack.ID); got != "thanks" {
		t.Fatalf("the ack ran on binding %q", got)
	}
	if ctx := e.must(a, gh.OpContext, "context", map[string]any{}); ctx["issue"] != float64(n) || ctx["acknowledgement"] != true {
		t.Fatalf("ack context: %v", ctx)
	}
	const thanks = "Thanks for raising this issue, a maintainer will look at it soon."
	e.must(a, gh.OpIssueComment, "thanks", map[string]any{"body": thanks})
	e.must(a, gh.OpIssueComment, "thanks", map[string]any{"body": thanks}) // a retried call
	_, err := e.call(a, gh.OpPRCreate, "pr", map[string]any{"title": "x", "body": "y"})
	refused(t, err, "acknowledgement")
	e.converge()
	if labels := e.fake.Labels(n); len(labels) != 0 {
		t.Fatalf("the running ack projected labels: %v", labels)
	}
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "thanked"}})
	if got := e.work(ack.ID); got.State != domain.WorkCompleted {
		t.Fatalf("ack after its turn: %v", got.State)
	}
	e.converge()
	comments := e.agentComments(n)
	if len(comments) != 1 || !strings.Contains(comments[0].Body, thanks) {
		t.Fatalf("ack comments: %+v", comments)
	}
	if a := gh.ParseAgent(comments[0].Body); a == nil || a.Work != string(ack.ID) {
		t.Fatalf("the comment isn't marked as the ack Work's: %q", comments[0].Body)
	}
	if labels := e.fake.Labels(n); len(labels) != 0 {
		t.Fatalf("the completed ack projected labels: %v", labels)
	}
	if all := e.fake.Comments(n); len(all) != 1 {
		t.Fatalf("issue comments: %+v", all)
	}

	// The trail: the copy's Event created the Work, and triggered the
	// Execution that commented.
	var trigger domain.AcceptedEvent
	e.read(func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(ack.ID)
		if err != nil || len(xs) != 1 {
			t.Fatalf("ack executions: %v %+v", err, xs)
		}
		trigger, err = tx.Event(xs[0].TriggerID)
		return err
	})
	if k := trigger.Event.CorrelationKey; trigger.Event.Type != gh.TypeIssueOpen || k == nil || k.ExternalID != gh.IssueAckKey(n) {
		t.Fatalf("ack trigger: %+v", trigger.Event)
	}

	// Redelivery, an edit and reconcile don't acknowledge again.
	e.deliver("issues", "d-1", e.fake.IssuesHook("opened", n))
	e.fake.SetLabels(n, "bug")
	e.deliver("issues", "d-2", e.fake.IssuesHook("labeled", n))
	e.sync()
	if again, _ := e.workFor(gh.IssueAckKey(n)); again.ID != ack.ID || again.State != domain.WorkCompleted {
		t.Fatalf("acknowledged again: %+v", again)
	}
	if q := e.queued(ack.ID); len(q) != 0 {
		t.Fatalf("the completed ack heard %v", q)
	}

	// The issue's own key is free: a claim by hand starts its Work.
	e.claimByHand(n)
	if w, ok := e.workFor(gh.IssueKey(n, "")); !ok || w.Kind != gh.WorkKind {
		t.Fatalf("the acknowledged issue could not be claimed: %+v", w)
	}

	// A trusted author's issue starts no acknowledgement; the trace says why.
	trusted := e.fake.OpenIssue("Steady the footer", "", ghAlice)
	e.sync()
	if _, ok := e.workFor(gh.IssueAckKey(trusted)); ok {
		t.Fatal("a trusted author's issue was acknowledged")
	}
	if _, ok := e.workFor(gh.IssueKey(trusted, "")); !ok {
		t.Fatal("a trusted author's issue was not claimed")
	}
	var copied domain.Event
	for _, a := range e.events() {
		if k := a.Event.CorrelationKey; k != nil && k.ExternalID == gh.IssueAckKey(trusted) {
			copied = a.Event
		}
	}
	if copied.ID == "" {
		t.Fatal("no copy of the trusted author's issue")
	}
	tr := e.trace(copied)
	if verdicts(tr)["acks"] != runtime.VerdictSkip || !strings.Contains(reasons(tr)["acks"], "admit.authors admits the issue's author alice") {
		t.Fatalf("trusted trace: %+v", tr.Routes)
	}
}

// claimByHand accepts the claim action's Event for issue n and routes it.
func (e *toolsEnv) claimByHand(n int) {
	e.t.Helper()
	key := domain.CorrelationKey{Source: ghNS, ExternalID: gh.IssueKey(n, "")}
	data, _ := json.Marshal(map[string]int{"number": n})
	if _, _, err := e.ingress.Accept(context.Background(), ghScope, domain.Event{ID: domain.ID("claim-" + itoa(n)), Type: gh.TypeClaimRequested,
		Source: extension.ActionSource, DeliveryID: "claim/" + itoa(n), OccurredAt: e.clock.Now(), ReceivedAt: e.clock.Now(), CorrelationKey: &key,
		Data: domain.Payload{Schema: "github.claim.v1", Data: data}}); err != nil {
		e.t.Fatal(err)
	}
	e.route()
}
