package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// The engineering tools, waits and holds (03-04) through the production
// toolbelt and scheduler, over SQLite, against the GitHub fake: v1's
// `agent …` command fixtures and engine wait tests, ported to toolbelt
// operations, translated waits and the `github-hold` route.

// toolsEnv adds the toolbelt, scheduler, timers and the hold executor to
// the GitHub runtime environment, exactly as bootstrap composes them.
type toolsEnv struct {
	*ghEnv
	belt      *runtime.Toolbelt
	scheduler *runtime.Scheduler
	timers    *runtime.Timers
	agent     *schedulerExecutor
	hold      *gh.HoldExecutorImpl
	tools     *gh.Tools
	extra     []gh.Source
	aliasOK   bool
	// jobs resolves job: conditions (05-02); nil refuses them.
	jobs   func(context.Context, domain.ID, string) (domain.CorrelationKey, []string, bool)
	mu     sync.Mutex
	tokens map[domain.ID]string
}

var agentOps = append([]string{extension.ReadContext, extension.Emit, extension.Wait, extension.DeclareOutcome}, gh.Operations()...)

func newToolsEnv(t *testing.T) *toolsEnv {
	t.Helper()
	fake := githubtest.New(ghRepo, "tok")
	t.Cleanup(fake.Close)
	e := &toolsEnv{ghEnv: &ghEnv{t: t, fake: fake, clock: &ghClock{t: fake.Now()}, path: filepath.Join(t.TempDir(), "conduit.db")}, tokens: map[domain.ID]string{}}
	e.open()
	e.putScope(ghScope, ghExtensionConfig())
	e.register()
	e.compose()
	return e
}

func (e *toolsEnv) putScope(id domain.ID, ext string) {
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "widgets", "type": "github", "connection": "github", "config": json.RawMessage(ext), "secret_names": []string{"hook"}},
	}})
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: id, Name: string(id), Bindings: []domain.Binding{{Name: "agent", Executor: "fixture", Requires: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}, {Name: "hold", Executor: gh.HoldExecutor, Requires: domain.Capabilities{Toolbelt: extension.ToolbeltSDK}}},
			Routes: []domain.Route{{Name: "held", Handler: gh.HoldHandler, Binding: "hold", ActivePolicy: "queue"}, {Name: "issues", Handler: "github", Binding: "agent", ActivePolicy: "queue"},
				{Name: "requests", Handler: "request", Binding: "agent", ActivePolicy: "queue"}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *toolsEnv) compose() {
	t := e.t
	e.agent = &schedulerExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}
	e.agent.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		e.mu.Lock()
		e.tokens[r.Execution.ID] = r.Context.ToolbeltToken
		e.mu.Unlock()
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff}}}, nil
	}
	e.hold = gh.NewHoldExecutor()
	e.hold.Now = e.clock.Now
	executors := map[string]extension.Executor{"fixture": e.agent, gh.HoldExecutor: e.hold}
	var err error
	e.router, err = runtime.NewRouter(e.store, e.clock.Now, runtime.RouterOptions{Generation: "1",
		Handlers: map[string]extension.RouteHandler{"github": gh.Route{}, gh.HoldHandler: gh.HoldRoute{}, "request": requestRoute},
		Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: e.agent}, gh.HoldExecutor: {Executor: e.hold}}})
	if err != nil {
		t.Fatal(err)
	}
	authorize := func(context.Context, runtime.AliasAuthorization) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.aliasOK {
			return errors.New("read_scopes does not grant it")
		}
		return nil
	}
	var belt *runtime.Toolbelt
	belt, err = runtime.NewToolbelt(e.store, e.clock.Now, e.router, runtime.ToolbeltOptions{
		Grant: func(_ context.Context, s runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			if s.Binding.Name == "hold" {
				return runtime.ToolbeltGrant{Operations: []string{extension.Emit, gh.OpHoldStatus, gh.OpHold}, EventTypes: belt.EventTypes()}, nil
			}
			return runtime.ToolbeltGrant{Operations: agentOps, EventTypes: belt.EventTypes()}, nil
		},
		AuthorizeAlias: authorize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.tools, err = gh.NewTools(gh.ToolsOptions{Store: e.store, Now: e.clock.Now, Jobs: e.jobs, Sources: func() []gh.Source {
		return append([]gh.Source{{Settings: e.settings, Client: e.client, Projector: e.projector}}, e.extra...)
	}}); err != nil {
		t.Fatal(err)
	}
	if err = belt.Register(e.tools.Registration()); err != nil {
		t.Fatal(err)
	}
	e.belt = belt
	e.hold.Dial = func(_ string, token string) (gh.ToolbeltClient, error) {
		return beltClient{belt: belt, token: token}, nil
	}
	if e.scheduler, err = runtime.NewScheduler(e.store, e.clock.Now, e.router, runtime.SchedulerOptions{Owner: "test", LeaseDuration: time.Hour, Generation: "1",
		Executors: executors, Toolbelt: belt, ToolbeltEndpoint: "unix:///fixture.sock", AuthorizeAlias: authorize, AliasFilter: belt.DependencyFilter}); err != nil {
		t.Fatal(err)
	}
	if e.timers, err = runtime.NewTimers(e.store, e.clock.Now, runtime.TimerOptions{}); err != nil {
		t.Fatal(err)
	}
}

// beltClient serves the hold executor's SDK calls in process, decoding the
// same wire inputs the management endpoint does.
type beltClient struct {
	belt  *runtime.Toolbelt
	token string
}

func (b beltClient) ToolbeltCall(ctx context.Context, c client.ToolCall) (client.ToolResult, error) {
	call := extension.ToolCall{Operation: c.Operation}
	if c.Operation == extension.Emit {
		var in struct {
			Type       string         `json:"type"`
			DeliveryID string         `json:"delivery_id"`
			Data       client.Payload `json:"data"`
			Key        client.Key     `json:"correlation_key"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return client.ToolResult{}, err
		}
		call.Event = &domain.Event{Type: in.Type, DeliveryID: in.DeliveryID, Data: domain.Payload{Schema: in.Data.Schema, Data: in.Data.Data}, CorrelationKey: &domain.CorrelationKey{Source: in.Key.Source, ExternalID: in.Key.ExternalID}}
	} else {
		var p client.Payload
		if err := json.Unmarshal(c.Input, &p); err != nil {
			return client.ToolResult{}, err
		}
		call.Input = domain.Payload{Schema: p.Schema, Data: p.Data}
	}
	out, err := b.belt.CallRequest(ctx, b.token, c.RequestID, call)
	if err != nil {
		return client.ToolResult{}, err
	}
	res := client.ToolResult{}
	if out.Output.Schema != "" || len(out.Output.Data) > 0 {
		res.Output = &client.Payload{Schema: out.Output.Schema, Data: out.Output.Data}
	}
	return res, nil
}

// sync reconciles, routes and installs subscriptions.
func (e *toolsEnv) sync() {
	e.t.Helper()
	e.reconcile()
	e.route()
	if err := e.scheduler.ReconcileSubscriptions(context.Background(), 50); err != nil {
		e.t.Fatal(err)
	}
	e.route()
}

func (e *toolsEnv) start(work domain.ID) runtime.Attempt {
	e.t.Helper()
	a, err := e.scheduler.Start(context.Background(), work)
	if err != nil {
		e.t.Fatalf("start %s: %v", work, err)
	}
	return a
}

// finish observes the attempt's handle and completes it, as the host loop does.
func (e *toolsEnv) finish(a runtime.Attempt, override *extension.Result) {
	e.t.Helper()
	res, err := a.Handle.Wait(context.Background())
	if err != nil {
		e.t.Fatalf("attempt %s: %v", a.Lease.ExecutionID, err)
	}
	if override != nil {
		res = *override
	}
	if err := e.scheduler.Complete(context.Background(), a.Lease, res); err != nil {
		e.t.Fatalf("complete %s: %v", a.Lease.ExecutionID, err)
	}
	e.route()
}

func (e *toolsEnv) call(a runtime.Attempt, op, request string, input any) (extension.ToolResult, error) {
	e.mu.Lock()
	token := e.tokens[a.Lease.ExecutionID]
	e.mu.Unlock()
	data, _ := json.Marshal(input)
	return e.belt.CallRequest(context.Background(), token, request, extension.ToolCall{Operation: op, Input: domain.Payload{Data: data}})
}

func (e *toolsEnv) must(a runtime.Attempt, op, request string, input any) map[string]any {
	e.t.Helper()
	out, err := e.call(a, op, request, input)
	if err != nil {
		e.t.Fatalf("%s: %v", op, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Output.Data, &m); err != nil {
		e.t.Fatalf("%s output: %s", op, out.Output.Data)
	}
	return m
}

func refused(t *testing.T, err error, code string) {
	t.Helper()
	var op *extension.OperationError
	if !errors.As(err, &op) || op.Code != code {
		t.Fatalf("want refusal %s, got %v", code, err)
	}
}

func (e *toolsEnv) claim(title, body string, labels ...string) (int, domain.Work) {
	e.t.Helper()
	n := e.fake.OpenIssue(title, body, ghAlice, labels...)
	e.sync()
	w, ok := e.workFor(gh.IssueKey(n, ""))
	if !ok {
		e.t.Fatalf("issue %d not claimed", n)
	}
	return n, w
}

func (e *toolsEnv) work(id domain.ID) domain.Work {
	var w domain.Work
	e.read(func(tx storage.Tx) error { var err error; w, err = tx.Work(id); return err })
	return w
}

func (e *toolsEnv) agentComments(n int) []githubtest.Comment {
	var out []githubtest.Comment
	for _, c := range e.fake.Comments(n) {
		if gh.ParseAgent(c.Body) != nil {
			out = append(out, c)
		}
	}
	return out
}

func TestGitHubToolsWorkAnIssue(t *testing.T) {
	e := newToolsEnv(t)
	n, w := e.claim("Header wobbles", "It wobbles on scroll.", "bug")
	e.fake.AddComment(n, ghAlice, "Also on mobile, please.")
	e.sync()
	a := e.start(w.ID)

	ctx := e.must(a, gh.OpContext, "", map[string]any{})
	if ctx["repo"] != ghRepo || ctx["issue"] != float64(n) || ctx["branch"] != "agent/issue-1" || ctx["base"] != "main" {
		t.Fatalf("context: %v", ctx)
	}
	view := e.must(a, gh.OpIssueView, "", map[string]any{})
	if view["title"] != "Header wobbles" || !strings.Contains(string(mustJSON(view["comments"])), "Also on mobile") {
		t.Fatalf("issue view: %v", view)
	}

	// Comments are marked and posted once per request, even when retried.
	for i := 0; i < 2; i++ {
		out := e.must(a, gh.OpIssueComment, "start", map[string]any{"body": "Working on this on branch `agent/issue-1`."})
		if out["state"] != "posted" || out["url"] == nil {
			t.Fatalf("comment: %v", out)
		}
	}
	if mine := e.agentComments(n); len(mine) != 1 || gh.ParseAgent(mine[0].Body).Work != string(w.ID) {
		t.Fatalf("marked comments: %+v", mine)
	}
	if _, err := e.call(a, gh.OpIssueComment, "start", map[string]any{"body": "something else"}); !errors.Is(err, runtime.ErrRequestConflict) {
		t.Fatalf("reused request with another body: %v", err)
	}
	// The agent's own note is never handed back to it as input.
	e.sync()
	for _, typ := range e.queued(w.ID) {
		if typ == gh.TypeCommentAgent {
			t.Fatal("own marked comment queued as input")
		}
	}

	// Opening the PR links it to the issue; a retried create finds it.
	pr := e.must(a, gh.OpPRCreate, "pr", map[string]any{"title": "Fix the wobble", "body": "Steadies the header."})
	number := int(pr["number"].(float64))
	if body := e.fake.Issue(number).Body; !strings.HasPrefix(body, "Closes #1\n\n") {
		t.Fatalf("PR body: %q", body)
	}
	again := e.must(a, gh.OpPRCreate, "pr-retry", map[string]any{"title": "Fix the wobble", "body": "Steadies the header."})
	if int(again["number"].(float64)) != number || again["note"] == nil {
		t.Fatalf("retried create: %v", again)
	}
	if list := e.must(a, gh.OpPRList, "", map[string]any{}); !strings.Contains(string(mustJSON(list["prs"])), `"number":2`) {
		t.Fatalf("pr list: %v", list)
	}

	// An inline review comment, viewed and answered in its thread.
	inline := e.fake.AddReviewComment(number, ghAlice, "Use a constant here.", "header.css", 12)
	prView := e.must(a, gh.OpPRView, "", map[string]any{"number": number})
	if s := string(mustJSON(prView["comments"])); !strings.Contains(s, "Use a constant here.") || !strings.Contains(s, gh.OpPRReply) {
		t.Fatalf("pr view: %s", s)
	}
	reply := e.must(a, gh.OpPRReply, "reply", map[string]any{"number": number, "comment_id": inline, "body": "Done in abc1234."})
	if reply["state"] != "posted" {
		t.Fatalf("reply: %v", reply)
	}
	var replies []githubtest.Comment
	for _, c := range e.fake.InlineComments(number) {
		if c.ReplyTo == inline {
			replies = append(replies, c)
		}
	}
	if len(replies) != 1 || !replies[0].Inline || gh.ParseAgent(replies[0].Body) == nil {
		t.Fatalf("inline reply: %+v", replies)
	}
	if out := e.must(a, gh.OpPRComment, "", map[string]any{"number": number, "body": "Pushed the constant."}); out["state"] != "posted" {
		t.Fatalf("pr comment: %v", out)
	}

	// Checks on the PR's head.
	head := e.fake.Pull(number).HeadSHA
	e.fake.SetCheck(head, "ci", "completed", "failure")
	if c := e.must(a, gh.OpChecks, "", map[string]any{}); c["state"] != "failed" || !strings.Contains(string(mustJSON(c["failed"])), "ci") {
		t.Fatalf("checks: %v", c)
	}

	// Attachments: a deterministic path keeps one file per request.
	png := []byte("\x89PNG\r\n\x1a\nfake")
	up := e.must(a, gh.OpUpload, "shot", map[string]any{"name": "screen shot.png", "content_base64": base64.StdEncoding.EncodeToString(png)})
	up2 := e.must(a, gh.OpUpload, "shot", map[string]any{"name": "screen shot.png", "content_base64": base64.StdEncoding.EncodeToString(png)})
	if up["url"] != up2["url"] || !strings.Contains(up["url"].(string), "/blob/"+gh.AttachBranch+"/1/") || !strings.HasPrefix(up["markdown"].(string), "![screen-shot.png](") {
		t.Fatalf("upload: %v %v", up, up2)
	}

	// Authenticated fetch: signed in on this host, the token never leaves it.
	e.fake.ServeFile("private.png", png, "")
	out, err := e.call(a, gh.OpFetch, "", map[string]any{"url": e.fake.URL + "/files/private.png", "save_to": ".local/private.png"})
	if err != nil || out.Output.Schema != gh.FileSchema {
		t.Fatalf("fetch: %v %+v", err, out)
	}
	var file struct {
		B64  string `json:"base64"`
		Save string `json:"save_to"`
	}
	if json.Unmarshal(out.Output.Data, &file) != nil || file.Save != ".local/private.png" {
		t.Fatalf("file result: %s", out.Output.Data)
	}
	if got, _ := base64.StdEncoding.DecodeString(file.B64); string(got) != string(png) {
		t.Fatal("fetched content differs")
	}
	var leaked string
	signed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		_, _ = w.Write(png)
	}))
	defer signed.Close()
	e.fake.ServeFile("redirected.png", nil, signed.URL+"/signed?sig=x")
	if _, err := e.call(a, gh.OpFetch, "", map[string]any{"url": e.fake.URL + "/files/redirected.png"}); err != nil || leaked != "" {
		t.Fatalf("redirected fetch: %v, authorization sent to storage: %q", err, leaked)
	}
	before := len(e.fake.Requests())
	_, err = e.call(a, gh.OpFetch, "", map[string]any{"url": signed.URL + "/elsewhere.png"})
	refused(t, err, "foreign_link")
	if len(e.fake.Requests()) != before || leaked != "" {
		t.Fatal("a foreign link was fetched with the token")
	}

	// Follow-up child work, held until a PR merges; opened once.
	for i := 0; i < 2; i++ {
		out := e.must(a, gh.OpIssueCreate, "wave2", map[string]any{"title": "Wave 2", "body": "After wave 1.", "wait": []string{"pr:2:merged"}})
		if out["number"] != float64(3) || !strings.Contains(out["held"].(string), "PR #2 to merge") {
			t.Fatalf("issue create: %v", out)
		}
	}
	created := e.fake.Issue(3)
	if !strings.Contains(created.Body, "<!-- waitFor: pr:2:merged -->") || !equal(created.Labels, "scheduled") || gh.ParseAgent(created.Body) == nil {
		t.Fatalf("held follow-up: %+v", created)
	}
	if list := e.must(a, gh.OpIssueList, "", map[string]any{}); !strings.Contains(string(mustJSON(list["issues"])), "Wave 2") {
		t.Fatalf("issue list: %v", list)
	}
	e.finish(a, nil)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestGitHubToolsRefuseOutsideTheirWork(t *testing.T) {
	e := newToolsEnv(t)
	_, w := e.claim("One", "", "bug")
	a := e.start(w.ID)
	if _, err := e.call(a, gh.OpPRView, "", map[string]any{"number": 99}); err == nil {
		t.Fatal("a missing PR was viewed")
	}
	_, err := e.call(a, gh.OpIssueComment, "", map[string]any{"body": " "})
	refused(t, err, "empty_body")
	_, err = e.call(a, gh.OpIssueComment, "", map[string]any{"body": "x", "extra": true})
	refused(t, err, "invalid_input")
	e.finish(a, nil)
	// Released authority: the tools refuse a finished attempt's token.
	if _, err := e.call(a, gh.OpContext, "", map[string]any{}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("finished attempt still acted: %v", err)
	}
}

// v1 TestWaitOnAnotherPRWakesTheAgentWhenItMerges, through a translated wait.
func TestGitHubWaitOnAnotherPRWakesTheAgentWhenItMerges(t *testing.T) {
	e := newToolsEnv(t)
	e.fake.OpenIssue("Twenty-nine", "", ghAlice, "backlog")
	n, w := e.claim("Thirty", "", "bug")
	pr := e.fake.OpenPull("Schema", "", "agent/issue-1", "s50", ghAlice)
	other := e.fake.OpenPull("Elsewhere", "", "elsewhere", "s51", ghAlice)
	e.sync()
	a := e.start(w.ID)
	out, err := e.call(a, gh.OpWait, "w1", map[string]any{"conditions": []string{"pr:" + itoa(pr) + ":merged"}, "reason": "Waiting for the schema PR."})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait: %v", err)
	}
	if mine := e.agentComments(n); len(mine) != 1 || !strings.Contains(mine[0].Body, "<!-- waitFor: pr:"+itoa(pr)+":merged -->") {
		t.Fatalf("reason comment: %+v", mine)
	}
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("not parked: %v", got.State)
	}
	e.converge()
	if labels := e.fake.Labels(n); !equal(labels, "agent-in-progress", "bug", "scheduled") {
		t.Fatalf("waiting labels: %v", labels)
	}
	// Some other PR merging does not wake it.
	e.fake.MergePull(other)
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("woke on another PR: %v", got.State)
	}
	e.fake.MergePull(pr)
	e.sync()
	got := e.work(w.ID)
	if got.State != domain.WorkReady {
		t.Fatalf("PR merge did not wake: %v", got.State)
	}
	var wake domain.Event
	e.read(func(tx storage.Tx) error {
		waits, err := tx.ListWaits(w.ID)
		for _, wt := range waits {
			if wt.ID == out.Wait.ID {
				a, er := tx.Event(wt.WakeEventID)
				wake = a.Event
				return er
			}
		}
		return err
	})
	text, ok := gh.RenderEvent(wake)
	if !ok || !strings.Contains(text, "You were waiting for PR #"+itoa(pr)+" to merge. That has happened: PR #"+itoa(pr)+" was merged.") {
		t.Fatalf("wake turn reads:\n%s", text)
	}
	e.converge()
	// Woken, the hold label comes off; the last turn's status shows until
	// the next turn runs.
	if labels := e.fake.Labels(n); !equal(labels, "agent-in-progress", "bug", "might-be-done") {
		t.Fatalf("labels after waking: %v", labels)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func statusData(state string) domain.Payload {
	data, _ := json.Marshal(map[string]string{"status": state})
	return domain.Payload{Schema: domain.TurnSchema, Data: data}
}

// v1 TestAlreadyMetWaitWakesAtOnce: the agent hears it already happened;
// a held issue whose condition already happened starts through the core path.
func TestGitHubAlreadyMetWaits(t *testing.T) {
	e := newToolsEnv(t)
	merged := e.fake.OpenPull("Done", "", "elsewhere", "s61", ghAlice)
	e.fake.MergePull(merged)
	_, w := e.claim("Agent", "", "bug")
	a := e.start(w.ID)
	_, err := e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{"pr:" + itoa(merged) + ":merged"}, "reason": "x"})
	refused(t, err, "already_met")
	_, err = e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{"job:backup"}, "reason": "x"})
	refused(t, err, "jobs_unavailable")
	_, err = e.call(a, gh.OpWait, "", map[string]any{"until": "2020-01-01T00:00Z", "reason": "x"})
	refused(t, err, "time_past")
	_, err = e.call(a, gh.OpWait, "", map[string]any{"reason": "x"})
	refused(t, err, "nothing_to_wait_for")
	// The refusals kept authority.
	e.must(a, gh.OpContext, "", map[string]any{})
	e.finish(a, nil)

	held, hw := e.heldClaim("Later", "<!-- waitFor: pr:"+itoa(merged)+":merged -->")
	why := e.runHold(hw.ID)
	if why != "" {
		t.Fatalf("first hold attempt released at once: %q", why)
	}
	if why = e.runHold(hw.ID); !strings.Contains(why, "This issue was held, waiting for PR #"+itoa(merged)+" to merge. That has happened: PR #"+itoa(merged)+" was merged.") {
		t.Fatalf("release: %q", why)
	}
	if q := e.queued(hw.ID); len(q) != 1 || q[0] != gh.TypeHoldReleased {
		t.Fatalf("agent input after release: %v", q)
	}
	_ = held
	// A hold nothing here can meet (a job before stage 05) and with no time
	// starts at once and says so, rather than being held for ever.
	_, jw := e.heldClaim("After the backup", "<!-- waitFor: job:backup -->")
	if why := e.runHold(jw.ID); !strings.Contains(why, "which nothing here can tell has happened") {
		t.Fatalf("unmeetable hold: %q", why)
	}
	// A scheduled issue with no marker is a scheduled task: it starts now.
	_, sw := e.heldClaim("Nightly routine", "Run the routine.")
	if why := e.runHold(sw.ID); why != "This issue was scheduled to start now." {
		t.Fatalf("scheduled task: %q", why)
	}
}

// heldClaim opens a scheduled issue and lets the hold route claim it.
func (e *toolsEnv) heldClaim(title, body string) (int, domain.Work) {
	e.t.Helper()
	n, w := e.claim(title, body, "scheduled")
	if w.Kind != gh.WorkKindHeld {
		e.t.Fatalf("scheduled issue claimed as %s", w.Kind)
	}
	return n, w
}

// runHold runs the held Work's next hold attempt; it returns the release
// reason, or "" when the attempt parked on its wait.
func (e *toolsEnv) runHold(work domain.ID) string {
	e.t.Helper()
	a := e.start(work)
	e.finish(a, nil)
	e.sync()
	var why string
	for _, ev := range e.events() {
		if ev.Event.Type == gh.TypeHoldReleased && ev.Event.ExecutionID == a.Lease.ExecutionID {
			var p gh.ReleasedPayload
			_ = json.Unmarshal(ev.Event.Data.Data, &p)
			why = p.Why
		}
	}
	return why
}

// v1 TestWaitTimesOutAndCancels (time half) and a scheduled start time: the
// hold parks on a core timeout timer and starts the agent when it fires.
func TestGitHubHeldIssueStartsAtItsTime(t *testing.T) {
	e := newToolsEnv(t)
	at := e.clock.Now().Add(2 * time.Hour).UTC().Format("2006-01-02T15:04Z")
	n, w := e.heldClaim("Match day", "Report on the match.\n\n<!-- runAt: "+at+" -->")
	if why := e.runHold(w.ID); why != "" {
		t.Fatalf("released before its time: %q", why)
	}
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("held Work: %v", got.State)
	}
	e.converge()
	if labels := e.fake.Labels(n); !equal(labels, "scheduled") {
		t.Fatalf("held labels (not claimed yet): %v", labels)
	}
	e.clock.Add(3 * time.Hour)
	if _, err := e.scheduler.Poll(context.Background(), e.timers, 50); err != nil {
		t.Fatal(err)
	}
	e.route()
	if got := e.work(w.ID); got.State != domain.WorkReady {
		t.Fatalf("timeout did not wake the hold: %v", got.State)
	}
	if why := e.runHold(w.ID); !strings.Contains(why, "This issue was scheduled to start at") {
		t.Fatalf("release: %q", why)
	}
	a := e.start(w.ID)
	e.converge()
	if labels := e.fake.Labels(n); !equal(labels, "agent-in-progress") {
		t.Fatalf("labels once the first agent turn runs: %v", labels)
	}
	e.finish(a, nil)
}

// v1 TestHeldIssueStartsWhenAnotherAgentReports: issue:21:review waits on
// the turn-ended Event the host publishes with the other agent's outcome.
func TestGitHubHeldIssueStartsWhenAnotherAgentReports(t *testing.T) {
	e := newToolsEnv(t)
	n21, w21 := e.claim("Wave 1", "", "bug")
	_, w20 := e.heldClaim("Wave 2", "after wave 1\n\n<!-- waitFor: issue:"+itoa(n21)+":review -->")
	if why := e.runHold(w20.ID); why != "" {
		t.Fatalf("released early: %q", why)
	}
	a := e.start(w21.ID)
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("building")}})
	e.publishTurns()
	if got := e.work(w20.ID); got.State != domain.WorkWaiting {
		t.Fatalf("a building turn met a review wait: %v", got.State)
	}
	e.fake.AddComment(n21, ghAlice, "Looks close")
	e.sync()
	b := e.start(w21.ID)
	e.finish(b, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("review")}})
	e.publishTurns()
	if got := e.work(w20.ID); got.State != domain.WorkReady {
		t.Fatalf("the review turn did not wake wave 2: %v", got.State)
	}
	if why := e.runHold(w20.ID); !strings.Contains(why, "This issue was held, waiting for the agent on #"+itoa(n21)+" to end a turn in review. That has happened") {
		t.Fatalf("release: %q", why)
	}
	// Turn Events are never input for the reporting issue's own Work.
	for _, typ := range e.queued(w21.ID) {
		if typ == domain.TypeTurnEnded {
			t.Fatal("turn event queued for its own Work")
		}
	}
	// One Event per turn on the issue's key.
	if got := e.countType(domain.TypeTurnEnded); got != 2 {
		t.Fatalf("turn events: %d", got)
	}
}

// publishTurns lets the host's turn-ended Events reach their waiters.
func (e *toolsEnv) publishTurns() {
	e.t.Helper()
	e.route()
	if err := e.scheduler.ReconcileSubscriptions(context.Background(), 50); err != nil {
		e.t.Fatal(err)
	}
	e.route()
}

// A wait on another scope's PR needs that scope in read_scopes (B04).
func TestGitHubCrossScopeWaitNeedsAuthorization(t *testing.T) {
	e := newToolsEnv(t)
	ops := githubtest.New("octo/ops", "tok")
	defer ops.Close()
	opsPR := ops.OpenPull("Deploy", "", "deploy", "d1", ghAlice)
	opsExt := `{"namespace":"github/github.com/octo/ops","scope":"ops","repo":"octo/ops"}`
	e.putScope("ops", opsExt)
	cfg, _ := gh.ParseConnection([]byte(`{"api_url":"` + ops.URL + `"}`))
	c, _ := gh.NewClient(cfg, func(context.Context, string) (string, error) { return "tok", nil }, gh.ClientOptions{Now: e.clock.Now})
	s, _ := gh.ParseExtension([]byte(opsExt), nil)
	p, _ := gh.NewProjector(gh.ProjectorOptions{Store: e.store, Client: c, Settings: s, Now: e.clock.Now})
	e.extra = []gh.Source{{Settings: s, Client: c, Projector: p}}
	in := gh.NewIngress(s, c, e.clock.Now, gh.IngressOptions{})
	if err := e.ingress.Register(context.Background(), "github/ops", extension.Source{Namespace: s.Namespace, ScopeID: "ops"}, in, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, w := e.claim("Needs the deploy", "", "bug")
	a := e.start(w.ID)
	cond := "pr:ops/" + itoa(opsPR) + ":merged"
	_, err := e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{cond}, "reason": "after the deploy"})
	if !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("unauthorized cross-scope wait: %v", err)
	}
	e.mu.Lock()
	e.aliasOK = true
	e.mu.Unlock()
	if _, err = e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{cond}, "reason": "after the deploy"}); err != nil {
		t.Fatalf("authorized cross-scope wait: %v", err)
	}
	e.finish(a, nil)
	ops.MergePull(opsPR)
	for i := 0; i < 50; i++ {
		res, err := e.ingress.Reconcile(context.Background(), "github/ops")
		if err != nil {
			t.Fatal(err)
		}
		if res.Complete {
			break
		}
	}
	e.route()
	if err := e.scheduler.ReconcileSubscriptions(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if got := e.work(w.ID); got.State != domain.WorkReady {
		t.Fatalf("cross-scope merge did not wake: %v", got.State)
	}
}

// A PR head with no checks at all reports "No checks ran" after
// no_checks_minutes (v1 noChecksMinutes), as an ordinary routed input.
func TestGitHubNoChecksRanIsObserved(t *testing.T) {
	e := newToolsEnv(t)
	_, w := e.claim("No CI", "", "bug")
	pr := e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
	e.sync()
	if e.countType(gh.TypeChecksNone) != 0 {
		t.Fatal("no-checks reported before the delay")
	}
	e.clock.Add(11 * time.Minute)
	e.sync()
	if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksNone {
		t.Fatalf("queued: %v", q)
	}
	var none domain.Event
	for _, ev := range e.events() {
		if ev.Event.Type == gh.TypeChecksNone {
			none = ev.Event
		}
	}
	if text, _ := gh.RenderEvent(none); !strings.Contains(text, "No checks ran on PR #"+itoa(pr)) {
		t.Fatalf("render: %s", text)
	}
	before := e.countType(gh.TypeChecksNone)
	e.sync()
	if e.countType(gh.TypeChecksNone) != before {
		t.Fatal("no-checks observed twice for one head")
	}
}

// A fine-grained personal access token cannot hold the Checks permission,
// so GitHub refuses check runs with an ordinary 403 (seen on the real sandbox
// in 03-06, where it failed every reconcile once a PR existed). Reconcile must
// keep working: Actions workflow runs stand in for check runs when readable;
// when they are refused too, nothing is observed for the head (never a false
// "No checks ran") and the checks tool says why.
func TestGitHubChecksWithoutTheChecksPermission(t *testing.T) {
	t.Run("workflow runs stand in", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("CI via Actions", "", "bug")
		e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
		e.fake.SetCheck("h1", "build", "completed", "success")
		e.fake.RefuseCheckRuns(true)
		e.sync()
		if q := e.queued(w.ID); len(q) == 0 || q[len(q)-1] != gh.TypeChecksPassed {
			t.Fatalf("queued: %v", q)
		}
	})
	t.Run("nothing readable", func(t *testing.T) {
		e := newToolsEnv(t)
		_, w := e.claim("No readable CI", "", "bug")
		pr := e.fake.OpenPull("Change", "", "agent/issue-1", "h1", ghAlice)
		e.fake.RefuseCheckRuns(false)
		e.sync()
		e.clock.Add(11 * time.Minute)
		e.sync()
		if n := e.countType(gh.TypeChecksNone); n != 0 {
			t.Fatalf("no-checks reported although check runs are unreadable (%d)", n)
		}
		a := e.start(w.ID)
		out := e.must(a, gh.OpChecks, "", map[string]any{"number": pr})
		if out["state"] != "unknown" || !strings.Contains(fmt.Sprint(out["note"]), "Checks permission") {
			t.Fatalf("checks tool: %v", out)
		}
		e.finish(a, nil)
	})
}

// A tool comment or issue whose response is lost is never written twice:
// the call reports it pending, and the projector confirms it by marker.
func TestGitHubToolWritesWithLostResponsesAreRecovered(t *testing.T) {
	e := newToolsEnv(t)
	n, w := e.claim("Lossy", "", "bug")
	a := e.start(w.ID)
	e.fake.LoseNextResponse("/repos/"+ghRepo+"/issues/"+itoa(n)+"/comments", 1)
	out := e.must(a, gh.OpIssueComment, "c", map[string]any{"body": "Started."})
	if out["state"] != "pending" {
		t.Fatalf("lost comment: %v", out)
	}
	// A retry inside the search delay does not post again.
	if again := e.must(a, gh.OpIssueComment, "c", map[string]any{"body": "Started."}); again["state"] != "pending" {
		t.Fatalf("retry: %v", again)
	}
	e.fake.LoseNextResponse("/repos/"+ghRepo+"/issues", 1)
	created := e.must(a, gh.OpIssueCreate, "i", map[string]any{"title": "Follow-up", "body": "Later."})
	if created["state"] != "pending" {
		t.Fatalf("lost issue: %v", created)
	}
	e.finish(a, nil)
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 {
		t.Fatalf("comment recovery: %+v", res)
	}
	e.converge()
	if mine := e.agentComments(n); len(mine) != 1 {
		t.Fatalf("%d agent comments", len(mine))
	}
	issues := 0
	for i := 1; i <= 3; i++ {
		func() {
			defer func() { _ = recover() }()
			if e.fake.Issue(i).Title == "Follow-up" {
				issues++
			}
		}()
	}
	if issues != 1 {
		t.Fatalf("%d follow-up issues", issues)
	}
}

// requestRoute stands in for a Slack route: a request Event starts Work keyed
// to its thread, which owns no GitHub issue.
var requestRoute = routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Type != "fixture.request" || e.CorrelationKey == nil {
		return extension.RouteDecision{}, nil
	}
	return extension.RouteDecision{Match: true, CreateID: domain.ID("work/" + e.CorrelationKey.ExternalID), Kind: "fixture.request"}, nil
})

// request starts Work from a Slack-like thread in the GitHub scope.
func (e *toolsEnv) request(thread string) domain.Work {
	e.t.Helper()
	key := domain.CorrelationKey{Source: "slack/T1", ExternalID: thread}
	at := e.clock.Now()
	if _, _, err := e.ingress.Accept(context.Background(), ghScope, domain.Event{ID: domain.ID("request-" + thread), Type: "fixture.request", Source: "slack/T1",
		DeliveryID: thread, OccurredAt: at, ReceivedAt: at, CorrelationKey: &key}); err != nil {
		e.t.Fatal(err)
	}
	e.route()
	return e.work(domain.ID("work/" + thread))
}

// Work that owns no issue (a Slack request to review a PR) reaches a PR in a
// repository its scope registers: it views it, comments and replies in a
// review thread through marked durable intents, recovered by marker after a
// lost response, without taking the PR from the Work that owns it.
func TestGitHubPROperationsWithoutAnIssue(t *testing.T) {
	e := newToolsEnv(t)
	n, owner := e.claim("Header wobbles", "", "bug")
	pr := e.fake.OpenPull("Fix the header", "Closes #"+itoa(n), "agent/issue-"+itoa(n), "h1", ghAlice)
	inline := e.fake.AddReviewComment(pr, ghAlice, "Why this offset?", "header.go", 12)
	e.fake.SetCheck("h1", "ci", "completed", "success")
	e.sync()
	w := e.request("thread-1")
	a := e.start(w.ID)

	// Issue-bound operations keep refusing Work without an issue.
	for _, op := range []string{gh.OpContext, gh.OpIssueComment, gh.OpPRCreate, gh.OpPRList} {
		_, err := e.call(a, op, "", map[string]any{"title": "t", "body": "b"})
		var refusal *extension.OperationError
		if !errors.As(err, &refusal) || (refusal.Code != "no_issue" && refusal.Code != "invalid_input") {
			t.Fatalf("%s without an issue: %v", op, err)
		}
	}
	view := e.must(a, gh.OpPRView, "", map[string]any{"number": pr})
	if view["repo"] != ghRepo || view["title"] != "Fix the header" || !strings.Contains(string(mustJSON(view["comments"])), `\"repo\": \"`+ghRepo+`\"`) {
		t.Fatalf("view: %v", view)
	}
	if c := e.must(a, gh.OpChecks, "", map[string]any{"number": pr, "repo": ghRepo}); c["state"] != "passed" {
		t.Fatalf("checks: %v", c)
	}
	_, err := e.call(a, gh.OpChecks, "", map[string]any{})
	refused(t, err, "missing_field")

	// A lost response leaves each post pending; the marker confirms it once.
	e.fake.LoseNextResponse("/repos/"+ghRepo+"/issues/"+itoa(pr)+"/comments", 1)
	if out := e.must(a, gh.OpPRComment, "review", map[string]any{"number": pr, "body": "Looks good apart from the offset."}); out["state"] != "pending" {
		t.Fatalf("lost PR comment: %v", out)
	}
	e.fake.LoseNextResponse("/repos/"+ghRepo+"/pulls/"+itoa(pr)+"/comments/", 1)
	if out := e.must(a, gh.OpPRReply, "reply", map[string]any{"number": pr, "comment_id": inline, "body": "Same question here."}); out["state"] != "pending" {
		t.Fatalf("lost reply: %v", out)
	}
	if again := e.must(a, gh.OpPRComment, "review", map[string]any{"number": pr, "body": "Looks good apart from the offset."}); again["state"] != "pending" {
		t.Fatalf("retry: %v", again)
	}
	e.finish(a, nil)
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 2 {
		t.Fatalf("recovery: %+v", res)
	}
	e.converge()
	if mine := e.agentComments(pr); len(mine) != 1 || gh.ParseAgent(mine[0].Body).Work != string(w.ID) {
		t.Fatalf("PR comments: %+v", mine)
	}
	replies := 0
	for _, c := range e.fake.InlineComments(pr) {
		if a := gh.ParseAgent(c.Body); a != nil && a.Work == string(w.ID) {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("%d replies", replies)
	}

	// The PR stays the issue Work's: it hears the review, the requester
	// neither takes the PR's key nor hears its own posts back.
	e.sync()
	if got := e.work(w.ID); len(got.CorrelationKeys) != 1 || got.CorrelationKeys[0].Source != "slack/T1" {
		t.Fatalf("requester keys: %+v", got.CorrelationKeys)
	}
	if q := e.queued(w.ID); len(q) != 0 {
		t.Fatalf("requester heard: %v", q)
	}
	if q := strings.Join(e.queued(owner.ID), " "); !strings.Contains(q, gh.TypeCommentAgent) {
		t.Fatalf("owner queued: %v", q)
	}
}

// The repository must be one the scope registers, and named when it
// registers several.
func TestGitHubPROperationsNeedARegisteredRepository(t *testing.T) {
	e := newToolsEnv(t)
	pr := e.fake.OpenPull("Change", "", "feature", "h1", ghAlice)
	s, err := gh.ParseExtension([]byte(`{"namespace":"github/github.com/octo/gadgets","scope":"`+ghScope+`","repo":"octo/gadgets"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	w := e.request("thread-2")
	a := e.start(w.ID)
	_, err = e.call(a, gh.OpPRComment, "", map[string]any{"number": pr, "body": "x", "repo": "octo/elsewhere"})
	refused(t, err, "unregistered_repo")
	e.mu.Lock()
	e.extra = []gh.Source{{Settings: s, Client: e.client, Projector: e.projector}}
	e.mu.Unlock()
	_, err = e.call(a, gh.OpPRView, "", map[string]any{"number": pr})
	refused(t, err, "repo_required")
	if v := e.must(a, gh.OpPRView, "", map[string]any{"number": pr, "repo": ghRepo}); v["title"] != "Change" {
		t.Fatalf("named repo: %v", v)
	}
	e.finish(a, nil)
}

// A review is one marked durable intent: requesting changes with inline
// comments posts one review across a retry and a lost response, recovered by
// marker, and neither the review nor its inline comments come back as a
// person's news. GitHub's refusals post nothing and leave nothing to redo.
func TestGitHubPRReviewIsSubmittedOnce(t *testing.T) {
	e := newToolsEnv(t)
	n, owner := e.claim("Header wobbles", "", "bug")
	pr := e.fake.OpenPull("Fix the header", "Closes #"+itoa(n), "agent/issue-"+itoa(n), "h1", ghAlice)
	own := e.fake.OpenPull("The bot's own change", "", "agent/elsewhere", "h2", githubtest.User{Login: "open-conduit[bot]", Type: "Bot"})
	e.sync()
	w := e.request("thread-review")
	a := e.start(w.ID)

	_, err := e.call(a, gh.OpPRReview, "", map[string]any{"number": pr, "event": "merge"})
	refused(t, err, "missing_field")
	_, err = e.call(a, gh.OpPRReview, "", map[string]any{"number": pr, "event": "request_changes"})
	refused(t, err, "missing_field")
	_, err = e.call(a, gh.OpPRReview, "", map[string]any{"number": pr, "event": "comment", "body": "x", "comments": []map[string]any{{"path": "a.go", "body": "no line"}}})
	refused(t, err, "invalid_comment")
	_, err = e.call(a, gh.OpPRReview, "bad-line", map[string]any{"number": pr, "event": "comment", "body": "x", "comments": []map[string]any{{"path": "a.go", "line": 5000, "body": "off the diff"}}})
	refused(t, err, "github_unprocessable")
	_, err = e.call(a, gh.OpPRReview, "own", map[string]any{"number": own, "event": "approve"})
	refused(t, err, "own_pull_request")
	// An approval needs no body and reports the review it posted.
	if out := e.must(a, gh.OpPRReview, "approve", map[string]any{"number": pr, "event": "approve"}); out["state"] != "posted" || out["review_id"] == nil {
		t.Fatalf("approve: %v", out)
	}

	review := map[string]any{"number": pr, "event": "request_changes", "body": "The offset needs a constant.",
		"comments": []map[string]any{{"path": "header.go", "line": 12, "body": "Name this."}, {"path": "header.go", "line": 30, "side": "left", "body": "Why removed?"}}}
	e.fake.LoseNextResponse("/repos/"+ghRepo+"/pulls/"+itoa(pr)+"/reviews", 1)
	if out := e.must(a, gh.OpPRReview, "review", review); out["state"] != "pending" {
		t.Fatalf("lost review: %v", out)
	}
	if again := e.must(a, gh.OpPRReview, "review", review); again["state"] != "pending" {
		t.Fatalf("retry: %v", again)
	}
	e.finish(a, nil)
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 {
		t.Fatalf("recovery: %+v", res)
	}
	e.converge()
	reviews := e.fake.Reviews(pr)
	if len(reviews) != 2 || reviews[0].State != "APPROVED" || reviews[1].State != "CHANGES_REQUESTED" || gh.ParseAgent(reviews[1].Body).Work != string(w.ID) {
		t.Fatalf("reviews: %+v", reviews)
	}
	inline := e.fake.InlineComments(pr)
	if len(inline) != 2 {
		t.Fatalf("inline: %+v", inline)
	}
	for _, c := range inline {
		if a := gh.ParseAgent(c.Body); a == nil || a.Work != string(w.ID) {
			t.Fatalf("unmarked inline comment: %+v", c)
		}
	}
	if len(e.fake.Reviews(own)) != 0 {
		t.Fatalf("refused review posted")
	}
	posts := 0
	for _, x := range e.fake.Writes() {
		if x.Method == "POST" && strings.HasSuffix(x.Path, "/reviews") {
			posts++
		}
	}
	if posts != 4 { // the bad line, the own PR, the approval and the lost review, each once
		t.Fatalf("%d review posts", posts)
	}

	e.sync()
	if q := e.queued(w.ID); len(q) != 0 {
		t.Fatalf("requester heard: %v", q)
	}
	if q := strings.Join(e.queued(owner.ID), " "); strings.Contains(q, gh.TypeCommentHuman) || !strings.Contains(q, gh.TypeCommentAgent) {
		t.Fatalf("owner queued: %v", q)
	}
}

// A reviewing agent waits for the next reply on a PR and, separately, the
// next push to it: each wakes it once, its own posts on the PR don't, and
// the wait survives a restart.
func TestGitHubWaitOnPRActivity(t *testing.T) {
	ghBob := githubtest.User{Login: "bob", Type: "User"}
	for _, tc := range []struct{ on, render string }{
		{"commented", "That has happened: someone replied on PR #"},
		{"pushed", "got a new push"},
	} {
		t.Run(tc.on, func(t *testing.T) {
			e := newToolsEnv(t)
			n, w := e.claim("Review", "", "bug")
			pr := e.fake.OpenPull("Feature", "", "feature", "s70", ghBob)
			e.fake.AddComment(pr, ghBob, "An earlier comment.")
			e.sync()
			a := e.start(w.ID)
			// The agent reviews the PR, then waits.
			e.must(a, gh.OpPRComment, "c1", map[string]any{"number": pr, "body": "Please rename this."})
			cond := "pr:" + itoa(pr) + ":" + tc.on
			out, err := e.call(a, gh.OpWait, "w1", map[string]any{"conditions": []string{cond}, "reason": "Waiting for the author."})
			if err != nil || out.Wait == nil {
				t.Fatalf("wait: %v", err)
			}
			if mine := e.agentComments(n); len(mine) != 1 || !strings.Contains(mine[0].Body, "<!-- waitFor: "+cond+" -->") {
				t.Fatalf("reason comment: %+v", mine)
			}
			e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
			e.sync()
			if got := e.work(w.ID); got.State != domain.WorkWaiting {
				t.Fatalf("woke on earlier activity or its own post: %v", got.State)
			}
			e.reopen()
			e.compose()
			// The other kind of activity does not wake it.
			if tc.on == "commented" {
				e.fake.PushPull(pr, "s71")
			} else {
				e.fake.AddComment(pr, ghBob, "Done, I think.")
			}
			e.sync()
			if got := e.work(w.ID); got.State != domain.WorkWaiting {
				t.Fatalf("woke on the wrong activity: %v", got.State)
			}
			if tc.on == "commented" {
				e.fake.AddReviewComment(pr, ghBob, "Renamed.", "main.go", 3)
			} else {
				e.fake.PushPull(pr, "s72")
			}
			e.sync()
			if got := e.work(w.ID); got.State != domain.WorkReady {
				t.Fatalf("%s did not wake: %v", tc.on, got.State)
			}
			var wake domain.Event
			e.read(func(tx storage.Tx) error {
				waits, err := tx.ListWaits(w.ID)
				for _, wt := range waits {
					if wt.ID == out.Wait.ID {
						a, er := tx.Event(wt.WakeEventID)
						wake = a.Event
						return er
					}
				}
				return err
			})
			if text, ok := gh.RenderEvent(wake); !ok || !strings.Contains(text, tc.render) {
				t.Fatalf("wake turn reads:\n%s", text)
			}
		})
	}
}

// A Slack-started Work (no GitHub issue) reviewing a PR waits for the next
// reply, then for the next push: each wakes it once, its own posts don't,
// the reason goes on no issue or PR, and the wait survives a restart.
func TestGitHubWaitOnPRActivityWithoutAnIssue(t *testing.T) {
	ghBob := githubtest.User{Login: "bob", Type: "User"}
	e := newToolsEnv(t)
	n, _ := e.claim("Header wobbles", "", "bug")
	pr := e.fake.OpenPull("Fix the header", "Closes #"+itoa(n), "agent/issue-"+itoa(n), "h1", ghBob)
	e.sync()
	w := e.request("thread-2")
	wakeText := func(wait *domain.Wait) string {
		var wake domain.Event
		e.read(func(tx storage.Tx) error {
			got, err := tx.Wait(wait.ID)
			if err != nil {
				return err
			}
			a, err := tx.Event(got.WakeEventID)
			wake = a.Event
			return err
		})
		text, _ := gh.RenderEvent(wake)
		return text
	}
	before := len(e.fake.Comments(n)) + len(e.fake.Comments(pr))

	a := e.start(w.ID)
	e.must(a, gh.OpPRComment, "c1", map[string]any{"number": pr, "body": "Please explain the offset."})
	out, err := e.call(a, gh.OpWait, "w1", map[string]any{"conditions": []string{"pr:" + itoa(pr) + ":commented"}, "reason": "Waiting for the author's answer."})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(out.Output.Data, &res)
	if res["waiting_for"] != "a reply or review on PR #"+itoa(pr) || res["comment"] != nil {
		t.Fatalf("wait output: %v", res)
	}
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("woke on its own post: %v", got.State)
	}
	if after := len(e.fake.Comments(n)) + len(e.fake.Comments(pr)); after != before+1 {
		t.Fatalf("the reason was posted on GitHub: %d comments, want %d", after, before+1)
	}
	e.reopen()
	e.compose()
	e.fake.AddComment(pr, ghBob, "It centres the logo.")
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkReady {
		t.Fatalf("reply did not wake: %v", got.State)
	}
	if text := wakeText(out.Wait); !strings.Contains(text, "someone replied on PR #"+itoa(pr)) {
		t.Fatalf("wake reads:\n%s", text)
	}

	a = e.start(w.ID)
	out, err = e.call(a, gh.OpWait, "w2", map[string]any{"conditions": []string{"pr:" + itoa(pr) + ":pushed"}, "repo": ghRepo, "reason": "Waiting for the fix."})
	if err != nil || out.Wait == nil {
		t.Fatalf("second wait: %v", err)
	}
	e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
	e.fake.AddComment(pr, ghBob, "Pushing shortly.")
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("a comment woke a push wait: %v", got.State)
	}
	e.fake.PushPull(pr, "h2")
	e.sync()
	if got := e.work(w.ID); got.State != domain.WorkReady {
		t.Fatalf("push did not wake: %v", got.State)
	}
	if text := wakeText(out.Wait); !strings.Contains(text, "got a new push") {
		t.Fatalf("wake reads:\n%s", text)
	}

	// Work without an issue still can't wait on a repository its scope
	// doesn't register.
	a = e.start(w.ID)
	_, err = e.call(a, gh.OpWait, "w3", map[string]any{"conditions": []string{"pr:" + itoa(pr) + ":merged"}, "repo": "elsewhere/repo", "reason": "x"})
	refused(t, err, "unregistered_repo")
}

// pr:N:checks waits for the checks on the PR's head as the wait begins:
// a result already in is met, a re-run's earlier result never wakes it, a
// new push or a merge first wakes it with what happened, and a closed PR
// is refused.
func TestGitHubWaitOnPRChecks(t *testing.T) {
	ghBob := githubtest.User{Login: "bob", Type: "User"}
	// waitOn parks an agent on pr:N:checks and returns its wait.
	waitOn := func(t *testing.T, e *toolsEnv, pr int) (domain.Work, extension.ToolResult) {
		t.Helper()
		_, w := e.claim("Review", "", "bug")
		a := e.start(w.ID)
		out, err := e.call(a, gh.OpWait, "w1", map[string]any{"conditions": []string{"pr:" + itoa(pr) + ":checks"}, "reason": "Waiting for CI."})
		if err != nil || out.Wait == nil {
			t.Fatalf("wait: %v", err)
		}
		e.finish(a, &extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Data: statusData("done")}})
		e.sync()
		if got := e.work(w.ID); got.State != domain.WorkWaiting {
			t.Fatalf("woke at once: %v", got.State)
		}
		e.read(func(tx storage.Tx) error {
			wt, err := tx.Wait(out.Wait.ID)
			if err != nil || wt.Description != "checks on PR #"+itoa(pr) {
				t.Fatalf("wait description %q %v", wt.Description, err)
			}
			return nil
		})
		return w, out
	}
	woke := func(t *testing.T, e *toolsEnv, w domain.Work, out extension.ToolResult, says string) {
		t.Helper()
		if got := e.work(w.ID); got.State != domain.WorkReady {
			t.Fatalf("did not wake: %v", got.State)
		}
		var wake domain.Event
		e.read(func(tx storage.Tx) error {
			wt, err := tx.Wait(out.Wait.ID)
			if err != nil {
				return err
			}
			a, err := tx.Event(wt.WakeEventID)
			wake = a.Event
			return err
		})
		if text, ok := gh.RenderEvent(wake); !ok || !strings.Contains(text, says) {
			t.Fatalf("wake turn reads:\n%s", text)
		}
	}

	t.Run("an earlier result", func(t *testing.T) {
		e := newToolsEnv(t)
		pr := e.fake.OpenPull("Feature", "", "feature", "c1", ghBob)
		e.fake.SetCheck("c1", "test", "completed", "failure")
		e.sync()
		_, w := e.claim("Review", "", "bug")
		a := e.start(w.ID)
		_, err := e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{"pr:" + itoa(pr) + ":checks"}, "reason": "x"})
		refused(t, err, "already_met")
	})
	t.Run("a re-run", func(t *testing.T) {
		e := newToolsEnv(t)
		pr := e.fake.OpenPull("Feature", "", "feature", "c1", ghBob)
		e.fake.SetCheck("c1", "test", "completed", "failure")
		e.sync()
		e.fake.RerunCheck("c1", "test")
		e.sync()
		w, out := waitOn(t, e, pr)
		e.reopen()
		e.compose()
		e.sync()
		if got := e.work(w.ID); got.State != domain.WorkWaiting {
			t.Fatalf("the earlier run's failure woke it: %v", got.State)
		}
		e.fake.SetCheck("c1", "test", "completed", "success")
		e.sync()
		woke(t, e, w, out, "checks on PR #"+itoa(pr)+" finished: passed")
	})
	t.Run("a push", func(t *testing.T) {
		e := newToolsEnv(t)
		pr := e.fake.OpenPull("Feature", "", "feature", "c1", ghBob)
		e.fake.SetCheck("c1", "test", "in_progress", "")
		e.sync()
		w, out := waitOn(t, e, pr)
		e.fake.PushPull(pr, "c2")
		e.sync()
		woke(t, e, w, out, "got a new push before its checks finished")
	})
	t.Run("a merge", func(t *testing.T) {
		e := newToolsEnv(t)
		pr := e.fake.OpenPull("Feature", "", "feature", "c1", ghBob)
		e.fake.SetCheck("c1", "test", "in_progress", "")
		e.sync()
		w, out := waitOn(t, e, pr)
		e.fake.MergePull(pr)
		e.sync()
		woke(t, e, w, out, "was merged before its checks finished")
	})
	t.Run("a closed PR", func(t *testing.T) {
		e := newToolsEnv(t)
		pr := e.fake.OpenPull("Feature", "", "feature", "c1", ghBob)
		e.fake.CloseIssue(pr, "")
		e.sync()
		_, w := e.claim("Review", "", "bug")
		a := e.start(w.ID)
		_, err := e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{"pr:" + itoa(pr) + ":checks"}, "reason": "x"})
		refused(t, err, "invalid_condition")
	})
}
