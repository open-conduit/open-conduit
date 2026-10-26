package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	slTeam    = "T0TEAM"
	slChannel = "C0SUPPORT"
	slNS      = "slack/T0TEAM"
	slScope   = "support"
	slSecret  = "signing-secret"
	slSource  = "slack/support/helpdesk"
	slAlice   = "UALICE"
	slBob     = "UBOB"
)

// slEnv composes the real runtime ingress and router over SQLite with the
// Slack extension against the fake service, exactly as bootstrap wires them.
// The scope has no workspace and no Git registration.
type slEnv struct {
	t         *testing.T
	fake      *slacktest.Server
	clock     *ghClock
	path      string
	extra     string
	opts      sl.IngressOptions
	store     *sqlite.Store
	ingress   *runtime.Ingress
	router    *runtime.Router
	adapter   *sl.Ingress
	client    *sl.Client
	projector *sl.Projector
	settings  sl.Settings
	webhook   http.Handler
	// binding is the router's view of the "fixture" executor (zero: none).
	binding runtime.RouteBinding
}

func slExtensionConfig(extra string) string {
	return `{"namespace":"` + slNS + `","scope":"` + slScope + `","team":"` + slTeam + `","channels":["` + slChannel + `"],"bot_user_id":"UBOT","signing_secret":"signing"` + extra + `}`
}

func newSlackEnv(t *testing.T, extra string, o sl.IngressOptions) *slEnv {
	t.Helper()
	fake := slacktest.New(slTeam, "xoxb-test")
	t.Cleanup(fake.Close)
	fake.Join(slChannel, slBob)
	e := &slEnv{t: t, fake: fake, clock: &ghClock{t: fake.Now()}, path: filepath.Join(t.TempDir(), "conduit.db"), extra: extra, opts: o}
	e.open()
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "helpdesk", "type": "slack", "connection": "slack", "config": json.RawMessage(slExtensionConfig(extra)), "secret_names": []string{"signing"}},
	}})
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: slScope, Name: "support", Bindings: []domain.Binding{{Name: "agent", Executor: "fixture"}},
			Routes: []domain.Route{{Name: "threads", Handler: "slack", Binding: "agent", ActivePolicy: "queue"}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	e.register()
	return e
}

func (e *slEnv) open() {
	t := e.t
	store, err := sqlite.Open(context.Background(), e.path)
	if err != nil {
		t.Fatal(err)
	}
	e.store = store
	t.Cleanup(func() { _ = store.Close() })
	if e.ingress, err = runtime.NewIngress(store, e.clock.Now, runtime.IngressOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.router, err = runtime.NewRouter(store, e.clock.Now, runtime.RouterOptions{Generation: "1", Handlers: map[string]extension.RouteHandler{"slack": sl.Route{}},
		Bindings: map[string]runtime.RouteBinding{"fixture": e.binding}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := sl.ParseConnection([]byte(`{"api_url":"` + e.fake.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.client, err = sl.NewClient(cfg, func(context.Context, string) (string, error) { return "xoxb-test", nil }, sl.ClientOptions{Now: e.clock.Now}); err != nil {
		t.Fatal(err)
	}
	if e.settings, err = sl.ParseExtension([]byte(slExtensionConfig(e.extra)), []string{"signing"}); err != nil {
		t.Fatal(err)
	}
	o := e.opts
	o.ActiveThreads = func(ctx context.Context) (map[string][]string, error) {
		out := map[string][]string{}
		err := store.Transact(ctx, storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
			works, err := tx.ListWorks()
			for _, w := range works {
				for _, k := range w.CorrelationKeys {
					if c, th, ok := sl.ParseThreadKey(k.ExternalID); ok && !w.State.Terminal() {
						out[c] = append(out[c], th)
					}
				}
			}
			return err
		})
		return out, err
	}
	e.adapter = sl.NewIngress(e.settings, e.client, e.clock.Now, o)
	if e.projector, err = sl.NewProjector(sl.ProjectorOptions{Store: store, Client: e.client, Settings: e.settings, Now: e.clock.Now, SearchDelay: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	e.webhook = sl.WebhookHandler(func(context.Context) (string, error) { return slSecret, nil }, func(ctx context.Context, d extension.Delivery) error {
		_, err := e.ingress.Push(ctx, slSource, d)
		return err
	}, e.clock.Now)
}

func (e *slEnv) register() {
	if err := e.ingress.Register(context.Background(), slSource, extension.Source{Namespace: slNS, ScopeID: slScope}, e.adapter, 15*time.Minute); err != nil {
		e.t.Fatal(err)
	}
}

func (e *slEnv) reopen() {
	_ = e.store.Close()
	e.open()
	e.register()
}

func (e *slEnv) sync() { e.clock.t = e.fake.Now() }

// deliver posts a signed Events API callback through the HTTP boundary.
func (e *slEnv) deliver(body []byte) int {
	req := httptest.NewRequest(http.MethodPost, "/slack/support/helpdesk", strings.NewReader(string(body)))
	req.Header = slacktest.Headers(slSecret, body, e.clock.Now())
	w := httptest.NewRecorder()
	e.webhook.ServeHTTP(w, req)
	return w.Code
}

func (e *slEnv) reconcile() runtime.ReconcileResult {
	e.t.Helper()
	var last runtime.ReconcileResult
	for i := 0; i < 500; i++ {
		res, err := e.ingress.Reconcile(context.Background(), slSource)
		if err != nil {
			e.t.Fatalf("reconcile: %v (%s)", err, res.ErrorCode)
		}
		last.Accepted = append(last.Accepted, res.Accepted...)
		if res.Complete {
			last.Complete = true
			return last
		}
	}
	e.t.Fatal("reconcile did not complete")
	return last
}

// route routes every pending Event, following the examined-row cursor as
// the daemon does (a page of 200 examined rows may hold no pending item).
func (e *slEnv) route() {
	e.t.Helper()
	var after uint64
	for {
		page, err := e.ingress.Pending(context.Background(), after, 200)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(page.Items) == 0 && !page.More {
			return
		}
		after = page.NextSequence
		if !page.More {
			after = 0
		}
		for _, p := range page.Items {
			if p.Diagnostic != "" {
				e.t.Fatalf("pending diagnostic %s for %s", p.Diagnostic, p.Event.Event.ID)
			}
			if err := e.router.Route(context.Background(), p.Event.Event.ID); err != nil {
				e.t.Fatalf("route %s: %v", p.Event.Event.Type, err)
			}
		}
	}
}

func (e *slEnv) read(fn func(storage.Tx) error) {
	e.t.Helper()
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *slEnv) events(prefix string) []domain.AcceptedEvent {
	var out []domain.AcceptedEvent
	e.read(func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if strings.HasPrefix(a.Event.Type, prefix) {
				out = append(out, a)
			}
		}
		return err
	})
	return out
}

func (e *slEnv) works() []domain.Work {
	var out []domain.Work
	e.read(func(tx storage.Tx) error { var err error; out, err = tx.ListWorks(); return err })
	return out
}

func (e *slEnv) workFor(thread string) (domain.Work, bool) {
	var w domain.Work
	found := false
	e.read(func(tx storage.Tx) error {
		var err error
		w, err = tx.WorkByCorrelation(domain.CorrelationKey{Source: slNS, ExternalID: sl.ThreadKey(slChannel, thread)})
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return w, found
}

// queued lists the pending input of Work as "type:ts".
func (e *slEnv) queued(work domain.ID) []string {
	inputs, err := e.router.PendingInputs(context.Background(), work)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	e.read(func(tx storage.Tx) error {
		for _, in := range inputs {
			a, err := tx.Event(in.EventID)
			if err != nil {
				return err
			}
			var p sl.MessagePayload
			_ = json.Unmarshal(a.Event.Data.Data, &p)
			out = append(out, a.Event.Type+":"+p.TS)
		}
		return nil
	})
	return out
}

func (e *slEnv) startAttempt(work domain.ID) domain.ID {
	e.t.Helper()
	var id domain.ID
	e.read(func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(work)
		if err != nil {
			return err
		}
		inputs, err := runtime.PendingInputsInTx(tx, work)
		if err != nil || len(inputs) == 0 {
			return errors.New("no input to start from")
		}
		id = domain.ID(string(work) + "/attempt/" + strconv.Itoa(len(xs)+1))
		if err := tx.PutExecution(domain.Execution{ID: id, WorkID: work, ExecutorBinding: "agent", Attempt: uint32(len(xs) + 1), State: domain.ExecutionPending, TriggerID: inputs[0].EventID}, 0); err != nil {
			return err
		}
		_, err = tx.Claim(id, "test", time.Hour)
		return err
	})
	return id
}

// finishAttempt ends the attempt (optionally declaring a reply) and moves
// Work to state.
func (e *slEnv) finishAttempt(id domain.ID, state domain.WorkState, reply string) {
	e.t.Helper()
	e.read(func(tx storage.Tx) error {
		x, err := tx.Execution(id)
		if err != nil {
			return err
		}
		x.State = domain.ExecutionCompleted
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "answered"}
		if reply != "" {
			data, _ := json.Marshal(sl.ReplyDeclaration{Text: reply})
			x.Outcome.Data = domain.Payload{Schema: sl.ReplySchema, Data: data}
		}
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		if _, err := runtime.ConsumeInputsInTx(tx, x.WorkID, id); err != nil {
			return err
		}
		w, err := tx.Work(x.WorkID)
		if err != nil {
			return err
		}
		w.State = state
		return tx.PutWork(w, w.Revision)
	})
}

func (e *slEnv) converge() sl.ProjectionResult {
	e.t.Helper()
	res, err := e.projector.Converge(context.Background(), 50)
	if err != nil {
		e.t.Fatalf("projection: %v %v", err, res.Codes)
	}
	return res
}

// TestSlackPushReconcileOverlapOneWork: hundreds of messages in one thread,
// half pushed (one redelivered by Slack, one forged) and half missed, then
// reconciled, produce one distinct Event per message and exactly one
// conversation Work holding every message as ordered input.
func TestSlackPushReconcileOverlapOneWork(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{PageSize: 50, BatchUnits: 3})
	root := e.fake.Post(slChannel, slAlice, "Exports time out for our team")
	e.sync()
	rootEvent := e.fake.MessageEvent(slChannel, root)
	if code := e.deliver(rootEvent); code != http.StatusOK {
		t.Fatalf("root delivery: %d", code)
	}
	// Slack retries the same callback (same event_id): one Event.
	if code := e.deliver(rootEvent); code != http.StatusOK {
		t.Fatalf("redelivery: %d", code)
	}
	forged := e.fake.MessageEvent(slChannel, root)
	req := httptest.NewRequest(http.MethodPost, "/slack", strings.NewReader(string(forged)))
	req.Header = slacktest.Headers("not-the-secret", forged, e.clock.Now())
	w := httptest.NewRecorder()
	e.webhook.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged delivery: %d", w.Code)
	}
	// The race-instrumented SQLite driver is ~30x slower per transaction, so
	// race runs use fewer replies; the plain run is the 300-reply evidence.
	n := 300
	if raceEnabled {
		n = 150
	}
	var replies []string
	for i := 0; i < n; i++ {
		ts := e.fake.Reply(slChannel, root, []string{slAlice, slBob}[i%2], "detail "+strconv.Itoa(i))
		replies = append(replies, ts)
		if i%2 == 0 {
			e.sync()
			if code := e.deliver(e.fake.MessageEvent(slChannel, ts)); code != http.StatusOK {
				t.Fatalf("reply delivery %d: %d", i, code)
			}
		}
	}
	e.route()
	e.sync()
	e.reconcile()
	e.route()
	if got := len(e.events("slack.message.")); got != n+1 {
		t.Fatalf("distinct message events = %d, want %d", got, n+1)
	}
	works := e.works()
	if len(works) != 1 || works[0].Kind != sl.WorkKind || works[0].State != domain.WorkReady {
		t.Fatalf("works: %+v", works)
	}
	q := e.queued(works[0].ID)
	if len(q) != n+1 || q[0] != sl.TypeMessageHuman+":"+root {
		t.Fatalf("queued %d first %v", len(q), q[:1])
	}
	seen := map[string]bool{}
	for _, item := range q {
		if seen[item] {
			t.Fatalf("input %s queued twice", item)
		}
		seen[item] = true
	}
	// Pushed replies precede reconciled ones; the per-Work order is the
	// acceptance order.
	if q[1] != sl.TypeMessageHuman+":"+replies[0] {
		t.Fatalf("second input %s", q[1])
	}
	// A second generation finds nothing new.
	if res := e.reconcile(); len(res.Accepted) != 0 {
		t.Fatalf("unchanged reconcile accepted %d", len(res.Accepted))
	}
}

// TestSlackReconcileOnlyRecoversMissedDeliveries: with push off, reconcile
// alone starts the conversation and queues its input; bots, joins and other
// threads behave per policy, and history older than start_window starts
// nothing.
func TestSlackReconcileOnlyRecoversMissedDeliveries(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	old := e.fake.Post(slChannel, slAlice, "ancient question")
	e.fake.Advance(48 * time.Hour)
	root := e.fake.Post(slChannel, slAlice, "Where is my invoice?")
	e.fake.BotReply(slChannel, root, "BOTHER", "Ticket #42 created")
	reply := e.fake.Reply(slChannel, root, slBob, "Seeing this too")
	e.fake.Join(slChannel, "UCAROL")
	e.sync()
	e.reconcile()
	e.route()
	if _, ok := e.workFor(old); ok {
		t.Fatal("history older than start_window started a conversation")
	}
	w, ok := e.workFor(root)
	if !ok || w.Kind != sl.WorkKind {
		t.Fatal("missed thread not recovered")
	}
	if q := e.queued(w.ID); len(q) != 2 || q[0] != sl.TypeMessageHuman+":"+root || q[1] != sl.TypeMessageHuman+":"+reply {
		t.Fatalf("queued %v", q)
	}
	if len(e.events(sl.TypeMessageBot)) != 1 {
		t.Fatal("bot message not recorded")
	}
	if len(e.works()) != 1 {
		t.Fatalf("works: %d", len(e.works()))
	}
}

// TestSlackThreadIdentitySurvivesRestart: the thread keeps one Work across a
// restart mid-generation and later messages, including an edit; a push-only
// deletion is recorded but is not input.
func TestSlackThreadIdentitySurvivesRestart(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{PageSize: 3, BatchUnits: 1})
	root := e.fake.Post(slChannel, slAlice, "VPN drops hourly")
	for i := 0; i < 8; i++ {
		e.fake.Post(slChannel, slBob, "unrelated "+strconv.Itoa(i))
	}
	e.sync()
	// One page, then the daemon dies mid-generation.
	if _, err := e.ingress.Reconcile(context.Background(), slSource); err != nil {
		t.Fatal(err)
	}
	e.reopen()
	e.reconcile()
	e.route()
	w, ok := e.workFor(root)
	if !ok {
		t.Fatal("no conversation")
	}
	attempt := e.startAttempt(w.ID)
	e.finishAttempt(attempt, domain.WorkReady, "")
	e.reopen()
	reply := e.fake.Reply(slChannel, root, slAlice, "still happening")
	e.fake.Edit(slChannel, root, "VPN drops hourly (on wifi)")
	gone := e.fake.Reply(slChannel, root, slAlice, "wrong thread, sorry")
	prev := e.fake.Delete(slChannel, gone)
	e.sync()
	if code := e.deliver(e.fake.DeletedEvent(slChannel, prev)); code != http.StatusOK {
		t.Fatalf("deletion delivery: %d", code)
	}
	e.reconcile()
	e.route()
	again, _ := e.workFor(root)
	if again.ID != w.ID || len(e.works()) != 9 {
		t.Fatalf("thread changed identity: %s vs %s (%d works)", again.ID, w.ID, len(e.works()))
	}
	q := e.queued(w.ID)
	if len(q) != 2 || !containsAll(q, sl.TypeMessageHuman+":"+reply, sl.TypeMessageHuman+":"+root) {
		t.Fatalf("queued after restart %v", q)
	}
	if len(e.events(sl.TypeMessageRemoved)) != 1 {
		t.Fatal("removal not recorded")
	}
}

func containsAll(got []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			return false
		}
	}
	return true
}

// TestSlackAmbiguousReplyIsRecoveredByMarker: a reply whose post answer was
// lost is found through its metadata marker, never posted twice; the
// observed agent reply carries its provenance and is not input for its own
// conversation, so it starts no loop.
func TestSlackAmbiguousReplyIsRecoveredByMarker(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Password reset email never arrives")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	attempt := e.startAttempt(w.ID)
	e.finishAttempt(attempt, domain.WorkReady, "Resent it; check spam for no-reply@example.com.")
	e.fake.FailNextPost("lost")
	if res, err := e.projector.Converge(context.Background(), 50); err != nil || res.Posted != 0 || len(res.Codes) != 1 {
		t.Fatalf("lost post: %+v %v", res, err)
	}
	if got := len(e.fake.AgentReplies(slChannel, root)); got != 1 {
		t.Fatalf("Slack holds %d replies after the lost answer", got)
	}
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.AgentReplies(slChannel, root)) != 1 {
		t.Fatal("reposted before the marker search delay")
	}
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 || len(e.fake.AgentReplies(slChannel, root)) != 1 {
		t.Fatalf("marker recovery: %+v", res)
	}
	e.clock.Add(time.Hour)
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.AgentReplies(slChannel, root)) != 1 {
		t.Fatal("reply posted twice")
	}
	// Reconcile sees the reply as the agent's own: recorded, never input.
	e.sync()
	e.reconcile()
	e.route()
	agent := e.events(sl.TypeMessageAgent)
	if len(agent) != 1 {
		t.Fatalf("agent reply events: %d", len(agent))
	}
	var p sl.MessagePayload
	_ = json.Unmarshal(agent[0].Event.Data.Data, &p)
	if p.Agent == nil || p.Agent.Work != string(w.ID) || p.Agent.Execution != string(attempt) || p.Agent.Intent != sl.MarkerID("outcome/"+string(attempt)) {
		t.Fatalf("agent provenance: %+v", p.Agent)
	}
	if q := e.queued(w.ID); len(q) != 0 {
		t.Fatalf("own reply became input: %v", q)
	}
	if !agent[0].Routed || agent[0].Disposition != "unmatched" || agent[0].RoutedWorkID != "" {
		t.Fatalf("own reply disposition %q work %q", agent[0].Disposition, agent[0].RoutedWorkID)
	}
}

func TestSlackReplyCrashAfterPostIsRecoveredOnRestart(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Need a new laptop charger")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	attempt := e.startAttempt(w.ID)
	e.finishAttempt(attempt, domain.WorkReady, "Ordered; it arrives Tuesday.")
	_ = e.store.Close()
	// A separate process converges and is SIGKILLed right after Slack created
	// the reply, before it records local completion.
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlackReplyCrashHelper$")
	cmd.Env = append(os.Environ(), "CONDUIT_SLACK_HELPER=1", "CONDUIT_SLACK_DB="+e.path, "CONDUIT_SLACK_URL="+e.fake.URL,
		"CONDUIT_SLACK_NOW="+e.clock.Now().Format(time.RFC3339Nano))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("helper was not killed at the post boundary: %v\n%s", err, out)
	}
	if got := len(e.fake.AgentReplies(slChannel, root)); got != 1 {
		t.Fatalf("replies after crash = %d", got)
	}
	e.open()
	e.register()
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 || res.Posted != 0 || len(e.fake.AgentReplies(slChannel, root)) != 1 {
		t.Fatalf("restart recovery: %+v replies %d", res, len(e.fake.AgentReplies(slChannel, root)))
	}
	if got := e.fake.AgentReplies(slChannel, root)[0]; got.Text != "Ordered; it arrives Tuesday." {
		t.Fatalf("reply text %q", got.Text)
	}
	e.clock.Add(time.Hour)
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.AgentReplies(slChannel, root)) != 1 {
		t.Fatal("reply posted twice")
	}
}

func TestSlackReplyCrashHelper(t *testing.T) {
	if os.Getenv("CONDUIT_SLACK_HELPER") == "" {
		return
	}
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("CONDUIT_SLACK_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), os.Getenv("CONDUIT_SLACK_DB"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := sl.ParseConnection([]byte(`{"api_url":"` + os.Getenv("CONDUIT_SLACK_URL") + `"}`))
	client, err := sl.NewClient(cfg, func(context.Context, string) (string, error) { return "xoxb-test", nil }, sl.ClientOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := sl.ParseExtension([]byte(slExtensionConfig("")), []string{"signing"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := sl.NewProjector(sl.ProjectorOptions{Store: store, Client: client, Settings: settings, Now: func() time.Time { return now },
		AfterPost: func(string) { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL); select {} }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Converge(context.Background(), 50)
	t.Fatalf("helper survived the post: %v", err)
}

// TestSlackReplyToolIsDurableAndOnce drives the slack.reply operation with
// validated authority: a retried call writes once, a lost answer is finished
// by the projector through the marker, and a refusal is reported, not retried.
func TestSlackReplyToolIsDurableAndOnce(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Printer on 3 is jammed")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	attempt := e.startAttempt(w.ID)
	tools, err := sl.NewTools(sl.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []sl.Source {
		return []sl.Source{{Settings: e.settings, Client: e.client, Projector: e.projector}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]extension.Operation{}
	for _, op := range tools.Registration().Operations {
		ops[op.Name] = op
	}
	auth := extension.Authority{ScopeID: slScope, WorkID: w.ID, ExecutionID: attempt}
	call := func(op, request, input string) (map[string]any, error) {
		ctx := extension.WithCallRequestID(context.Background(), request)
		out, err := ops[op].Handler(ctx, auth, domain.Payload{Schema: "json", Data: json.RawMessage(input)})
		if err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(out.Data, &m)
		return m, nil
	}
	ctxOut, err := call(sl.OpContext, "", `{}`)
	if err != nil || ctxOut["thread_ts"] != root || ctxOut["channel"] != slChannel {
		t.Fatalf("context: %v %v", ctxOut, err)
	}
	first, err := call(sl.OpReply, "r1", `{"text":"Facilities are on their way."}`)
	if err != nil || first["state"] != sl.IntentPosted {
		t.Fatalf("reply: %v %v", first, err)
	}
	again, err := call(sl.OpReply, "r1", `{"text":"Facilities are on their way."}`)
	if err != nil || again["ts"] != first["ts"] || len(e.fake.AgentReplies(slChannel, root)) != 1 {
		t.Fatalf("retried call wrote again: %v %v", again, err)
	}
	if _, err := call(sl.OpReply, "r1", `{"text":"Different text"}`); err == nil {
		t.Fatal("a reused request ID with new text was accepted")
	}
	e.fake.FailNextPost("lost")
	lost, err := call(sl.OpReply, "r2", `{"text":"Update: fixed."}`)
	if err != nil || lost["state"] != "pending" {
		t.Fatalf("lost answer: %v %v", lost, err)
	}
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 || len(e.fake.AgentReplies(slChannel, root)) != 2 {
		t.Fatalf("tool intent recovery: %+v", res)
	}
	e.fake.FailNextPost("refuse:is_archived")
	var op *extension.OperationError
	if _, err := call(sl.OpReply, "r3", `{"text":"Anything else?"}`); !errors.As(err, &op) || op.Code != "slack_refused" {
		t.Fatalf("refusal: %v", err)
	}
	if res := e.converge(); res.Posted != 0 || len(e.fake.AgentReplies(slChannel, root)) != 2 {
		t.Fatal("a refused reply was retried")
	}
	thread, err := call(sl.OpThread, "", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := thread["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("thread view: %v", thread)
	}
	// Another Work's authority cannot reach this thread.
	other := extension.Authority{ScopeID: slScope, WorkID: "missing", ExecutionID: "x"}
	if _, err := ops[sl.OpReply].Handler(context.Background(), other, domain.Payload{Data: json.RawMessage(`{"text":"hi"}`)}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("foreign authority: %v", err)
	}
}

// TestSlackRateLimitedScanInfersNoAbsence: a rate-limited generation infers
// no absence; once the limit lifts, deletions surface as generic absence that
// reaches no Work as input.
func TestSlackRateLimitedScanInfersNoAbsence(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Calendar sync broken")
	reply := e.fake.Reply(slChannel, root, slBob, "me too")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	e.fake.Delete(slChannel, reply)
	e.fake.Reply(slChannel, root, slAlice, "now it works")
	e.sync()
	e.fake.RateLimit("conversations.replies", 20, 1)
	res, err := e.ingress.Reconcile(context.Background(), slSource)
	if err == nil && res.Complete {
		t.Fatal("rate-limited scan completed")
	}
	if len(e.events("core.ingress.absent")) != 0 {
		t.Fatal("absence inferred from a failed scan")
	}
	e.clock.Add(21 * time.Second)
	e.reconcile()
	e.route()
	if got := len(e.events("core.ingress.absent")); got != 1 {
		t.Fatalf("absence events %d", got)
	}
	q := e.queued(w.ID)
	if len(q) != 3 || q[2] != sl.TypeMessageHuman+":"+e.fake.Thread(slChannel, root)[1].TS {
		t.Fatalf("queued %v", q)
	}
}

// TestSlackLostReplyRecoveredWithoutMetadata: where Slack returns no message
// metadata, a lost reply is still found (the app's bot user posting exactly
// this text) and the reply, recognized as the app's own, is never input.
func TestSlackLostReplyRecoveredWithoutMetadata(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	e.fake.WithholdMetadata(true)
	root := e.fake.Post(slChannel, slAlice, "Shared drive is read-only")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	attempt := e.startAttempt(w.ID)
	e.finishAttempt(attempt, domain.WorkReady, "Fixed the permissions; try again.")
	e.fake.FailNextPost("lost")
	_, _ = e.projector.Converge(context.Background(), 50)
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 || len(e.fake.Thread(slChannel, root)) != 2 {
		t.Fatalf("fallback recovery: %+v, thread %d", res, len(e.fake.Thread(slChannel, root)))
	}
	e.sync()
	e.reconcile()
	e.route()
	if got := e.events(sl.TypeMessageAgent); len(got) != 1 {
		t.Fatalf("own unmarked reply class: %d agent events", len(got))
	}
	if q := e.queued(w.ID); len(q) != 0 {
		t.Fatalf("own reply became input: %v", q)
	}
}
