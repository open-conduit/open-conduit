package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/extensions/github/githubtest"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
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
	mu        sync.Mutex
	tokens    map[domain.ID]string
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
			Routes: []domain.Route{{Name: "held", Handler: gh.HoldHandler, Binding: "hold", ActivePolicy: "queue"}, {Name: "issues", Handler: "github", Binding: "agent", ActivePolicy: "queue"}},
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
		Handlers: map[string]extension.RouteHandler{"github": gh.Route{}, gh.HoldHandler: gh.HoldRoute{}},
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
	if e.tools, err = gh.NewTools(gh.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []gh.Source {
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

	// Corrections and problems are durable notes for the stage 05 sweep.
	if out := e.must(a, gh.OpCorrection, "c1", map[string]any{"area": "code", "body": "Used magic numbers.\n\nName constants."}); out["logged"] == nil {
		t.Fatalf("correction: %v", out)
	}
	e.must(a, gh.OpCorrection, "c1", map[string]any{"area": "code", "body": "Used magic numbers.\n\nName constants."})
	_, err = e.call(a, gh.OpProblem, "", map[string]any{"kind": "bogus", "body": "x"})
	refused(t, err, "invalid_kind")
	e.must(a, gh.OpProblem, "p1", map[string]any{"kind": "tooling", "body": "pr list failed\n\nError: x\nWorkaround: none\nFix: open-conduit"})
	e.read(func(tx storage.Tx) error {
		c, _ := tx.List(gh.NotesNamespace, "correction/")
		p, _ := tx.List(gh.NotesNamespace, "problem/")
		if len(c) != 1 || len(p) != 1 {
			t.Fatalf("notes: %d corrections, %d problems", len(c), len(p))
		}
		return nil
	})

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
	return domain.Payload{Schema: gh.StatusSchema, Data: data}
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

// v1 TestHeldIssueStartsWhenAnotherAgentReports: issue:21:review waits on a
// turn Event the host derives from the other agent's durable outcome.
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
		if typ == gh.TypeAgentTurn {
			t.Fatal("turn event queued for its own Work")
		}
	}
	// Publishing again is a no-op.
	before := e.countType(gh.TypeAgentTurn)
	e.publishTurns()
	if e.countType(gh.TypeAgentTurn) != before {
		t.Fatal("turn published twice")
	}
}

// publishTurns does what bootstrap's projection pass does.
func (e *toolsEnv) publishTurns() {
	e.t.Helper()
	turns, err := e.projector.Turns(context.Background(), 50)
	if err != nil {
		e.t.Fatal(err)
	}
	causal, err := runtime.NewCausal(e.store, e.clock.Now, runtime.CausalLimits{})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, turn := range turns {
		if _, _, err := causal.Derive(context.Background(), turn.Scope, turn.Parent, turn.Event); err != nil {
			e.t.Fatalf("turn: %v", err)
		}
		if err := e.projector.MarkTurn(context.Background(), domain.ID(strings.TrimPrefix(turn.Event.DeliveryID, "turn/"))); err != nil {
			e.t.Fatal(err)
		}
	}
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
