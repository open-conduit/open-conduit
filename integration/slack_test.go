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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
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
	// actions and act stand in for the host's Actions and Act (prompts
	// and button presses); nil offers and takes nothing.
	actions  sl.ActionsFunc
	act      sl.ActFunc
	settings sl.Settings
	webhook  http.Handler
	// channels the entry watches (default: slChannel).
	channels []string
	// binding is the router's view of the "fixture" executor (zero: none).
	binding runtime.RouteBinding
}

func slExtensionConfig(extra string) string {
	return slExtensionConfigFor([]string{slChannel}, extra)
}

func slExtensionConfigFor(channels []string, extra string) string {
	list, _ := json.Marshal(channels)
	return `{"namespace":"` + slNS + `","scope":"` + slScope + `","team":"` + slTeam + `","channels":` + string(list) + `,"bot_user_id":"UBOT","signing_secret":"signing"` + extra + `}`
}

func newSlackEnv(t *testing.T, extra string, o sl.IngressOptions) *slEnv {
	t.Helper()
	return newSlackEnvWith(t, extra, o, []string{slChannel}, []domain.Binding{{Name: "agent", Executor: "fixture"}},
		[]domain.Route{{Name: "threads", Handler: "slack", Binding: "agent", ActivePolicy: "queue"}})
}

// newSlackEnvWith is newSlackEnv for one entry watching channels, with the
// scope's own bindings and routes.
func newSlackEnvWith(t *testing.T, extra string, o sl.IngressOptions, channels []string, bindings []domain.Binding, routes []domain.Route) *slEnv {
	t.Helper()
	fake := slacktest.New(slTeam, "xoxb-test")
	t.Cleanup(fake.Close)
	for _, c := range channels {
		fake.Join(c, slBob)
	}
	e := &slEnv{t: t, fake: fake, clock: &ghClock{t: fake.Now()}, path: filepath.Join(t.TempDir(), "conduit.db"), extra: extra, opts: o, channels: channels}
	e.open()
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "helpdesk", "type": "slack", "connection": "slack", "config": json.RawMessage(slExtensionConfigFor(channels, extra)), "secret_names": []string{"signing"}},
	}})
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: slScope, Name: "support", Bindings: bindings, Routes: routes,
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
	if e.router, err = runtime.NewRouter(store, e.clock.Now, runtime.RouterOptions{Generation: "1", Handlers: map[string]extension.RouteHandler{"slack": sl.Route{}, "jobs": slJobRoute{}},
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
	if e.settings, err = sl.ParseExtension([]byte(slExtensionConfigFor(e.channels, e.extra)), []string{"signing"}); err != nil {
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
	if e.projector, err = sl.NewProjector(sl.ProjectorOptions{Store: store, Client: e.client, Settings: e.settings, Now: e.clock.Now, SearchDelay: 30 * time.Second,
		Actions: func(ctx context.Context, scope, work domain.ID) ([]extension.ActionOffer, error) {
			if e.actions == nil {
				return nil, nil
			}
			return e.actions(ctx, scope, work)
		},
		Act: func(ctx context.Context, r extension.ActRequest) (extension.ActResult, error) {
			if e.act == nil {
				return extension.ActResult{}, &extension.OperationError{Code: extension.CodeNoAction, Message: "nothing here"}
			}
			return e.act(ctx, r)
		},
		// As the host accepts an extension's own Event in its transaction.
		Open: func(tx storage.Tx, scope, parent domain.ID, ev domain.Event) error {
			ev.ID = domain.ID(protocol.EmittedEventID(ev.Source, ev.DeliveryID))
			var err error
			if parent != "" {
				_, _, err = runtime.AcceptDerivedInTx(tx, e.clock.Now(), runtime.CausalLimits{MaxDepth: 32, PerMinute: 128}, scope, parent, ev)
			} else {
				_, _, err = runtime.AcceptInTx(tx, scope, ev)
			}
			return err
		}}); err != nil {
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
	id, _ := e.startAttemptOn(work, "agent")
	return id
}

// startAttemptOn claims a new attempt of work on binding from its first
// pending input, returning the attempt and its lease.
func (e *slEnv) startAttemptOn(work domain.ID, binding string) (domain.ID, domain.LeaseRef) {
	e.t.Helper()
	var id domain.ID
	var ref domain.LeaseRef
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
		if err := tx.PutExecution(domain.Execution{ID: id, WorkID: work, ExecutorBinding: binding, Attempt: uint32(len(xs) + 1), State: domain.ExecutionPending, TriggerID: inputs[0].EventID}, 0); err != nil {
			return err
		}
		ref, err = tx.Claim(id, "test", time.Hour)
		return err
	})
	return id, ref
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

// TestSlackRoutesSelectBindingByChannel: one entry watching two channels
// serves two routes, each narrowed by its config's channels to its own
// binding (and so its own playbook). First match wins: the catch-all route
// declared last takes no thread a narrower route claimed first.
func TestSlackRoutesSelectBindingByChannel(t *testing.T) {
	const reviews = "C0REVIEWS"
	bindings := []domain.Binding{
		{Name: "review", Executor: "fixture", Config: domain.Payload{Schema: "core.config.binding.v1", Data: json.RawMessage(`{"playbook":"review-pr","args":"{key}"}`)}},
		{Name: "support", Executor: "fixture", Config: domain.Payload{Schema: "core.config.binding.v1", Data: json.RawMessage(`{"playbook":"slack-thread","args":"{key}"}`)}},
	}
	routes := []domain.Route{
		{Name: "reviews", Handler: "slack", Binding: "review", ActivePolicy: "queue", Config: domain.Payload{Data: json.RawMessage(`{"channels":["` + reviews + `"]}`)}},
		{Name: "support", Handler: "slack", Binding: "support", ActivePolicy: "queue"},
	}
	e := newSlackEnvWith(t, "", sl.IngressOptions{}, []string{slChannel, reviews}, bindings, routes)
	review := e.fake.Post(reviews, slAlice, "Please review #12")
	help := e.fake.Post(slChannel, slAlice, "Exports time out")
	e.sync()
	for _, m := range []struct{ channel, ts string }{{reviews, review}, {slChannel, help}} {
		if code := e.deliver(e.fake.MessageEvent(m.channel, m.ts)); code != http.StatusOK {
			t.Fatalf("delivery %s: %d", m.channel, code)
		}
	}
	reply := e.fake.Reply(reviews, review, slBob, "on it")
	e.sync()
	if code := e.deliver(e.fake.MessageEvent(reviews, reply)); code != http.StatusOK {
		t.Fatalf("reply delivery: %d", code)
	}
	e.route()
	if works := e.works(); len(works) != 2 {
		t.Fatalf("works: %+v", works)
	}
	want := map[string]string{sl.ThreadKey(reviews, review): "review", sl.ThreadKey(slChannel, help): "support"}
	got := map[string][]string{}
	for _, a := range e.events("slack.message.human") {
		d, err := e.router.Decision(context.Background(), a.Event.ID)
		if err != nil {
			t.Fatal(err)
		}
		got[a.Event.CorrelationKey.ExternalID] = append(got[a.Event.CorrelationKey.ExternalID], d.Route+"/"+d.Binding)
	}
	for key, binding := range want {
		if len(got[key]) == 0 {
			t.Fatalf("no decision for %s: %v", key, got)
		}
		for _, d := range got[key] {
			if !strings.HasSuffix(d, "/"+binding) {
				t.Fatalf("thread %s routed %v, want binding %s", key, got[key], binding)
			}
		}
	}
	if n := len(got[sl.ThreadKey(reviews, review)]); n != 2 {
		t.Fatalf("review thread decisions = %d, want root and reply", n)
	}
}

// TestSlackRouteChannelsValidateAtLoad: the documented per-channel config
// validates offline, and a route naming a channel that is not a Slack
// channel ID is rejected at its config path.
func TestSlackRouteChannelsValidateAtLoad(t *testing.T) {
	releaseLayout(t)
	var doc string
	for _, block := range fencedBlocks(t, "../site/src/content/docs/workflows/slack.md", "json") {
		if strings.Contains(block, `"C0REVIEWS01"]}}`) {
			doc = block
		}
	}
	if doc == "" {
		t.Fatal("the Slack docs lost the per-channel example")
	}
	if diags, err := bootstrap.ValidateConfig([]byte(doc)); err != nil || len(diags) != 0 {
		t.Fatalf("documented config: %v %v", diags, err)
	}
	bad := strings.Replace(doc, `"config": {"channels": ["C0REVIEWS01"]}`, `"config": {"channels": ["#reviews"]}`, 1)
	if bad == doc {
		t.Fatal("route config not found in the example")
	}
	diags, err := bootstrap.ValidateConfig([]byte(bad))
	if err != nil || len(diags) != 1 || diags[0].Path != "scopes/0/routes/0/config" || diags[0].Code != "invalid_config" {
		t.Fatalf("diagnostics %+v %v", diags, err)
	}
}

// park puts work into a waiting state on an active Wait, as a gated call
// or a question does.
func (e *slEnv) park(work domain.ID, wait domain.ID) {
	e.t.Helper()
	attempt := e.startAttempt(work)
	e.read(func(tx storage.Tx) error {
		x, err := tx.Execution(attempt)
		if err != nil {
			return err
		}
		if x.State == domain.ExecutionPending {
			x.State = domain.ExecutionRunning
			if err := tx.PutExecution(x, x.Revision); err != nil {
				return err
			}
			if x, err = tx.Execution(attempt); err != nil {
				return err
			}
		}
		x.State = domain.ExecutionParked
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "gated"}
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		if _, err := runtime.ConsumeInputsInTx(tx, work, attempt); err != nil {
			return err
		}
		w, err := tx.Work(work)
		if err != nil {
			return err
		}
		if w.State == domain.WorkReady || w.State == domain.WorkWaiting {
			w.State = domain.WorkRunning
			if err := tx.PutWork(w, w.Revision); err != nil {
				return err
			}
			if w, err = tx.Work(work); err != nil {
				return err
			}
		}
		w.State = domain.WorkWaiting
		if err := tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		return tx.PutWait(domain.Wait{ID: wait, WorkID: work, ExecutionID: attempt, State: domain.WaitActive,
			Match: domain.EventMatch{Keys: w.CorrelationKeys, Types: []string{"human.answered"}}, CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}, 0)
	})
}

// While a conversation's Work waits, each action with choices offered on it
// is posted once in its thread with a button per choice, and a press is
// handed to Act as the person who pressed it, who is told the outcome. An
// action without choices, or Work that isn't waiting, is never prompted,
// and a press from elsewhere or of another app's button is ignored.
func TestSlackPromptsAndTakesButtonAnswers(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Please deploy 1.2")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	// Like a real provider, it offers the answer only once there is a
	// question: here, once the Work parks.
	offering := false
	e.actions = func(_ context.Context, scope, work domain.ID) ([]extension.ActionOffer, error) {
		if scope != slScope || work != w.ID {
			return nil, errors.New("wrong target")
		}
		if !offering {
			return nil, nil
		}
		return []extension.ActionOffer{
			{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Approve or refuse fixture.deploy", Prompt: "an option", Choices: []string{"approve", "refuse"}}},
			{Provider: "notes", Action: extension.Action{ID: "note", Label: "Add a note", Prompt: "your note"}},
		}, nil
	}
	if res := e.converge(); res.Posted != 0 {
		t.Fatalf("prompted with nothing offered: %+v", res)
	}
	offering = true
	e.park(w.ID, "wait/1")
	if res := e.converge(); res.Posted != 1 {
		t.Fatalf("prompt: %+v", res)
	}
	if res := e.converge(); res.Posted != 0 {
		t.Fatalf("prompted twice: %+v", res)
	}
	var prompt sl.Message
	for _, m := range e.fake.Thread(slChannel, root) {
		if len(m.Blocks) > 0 {
			prompt = m
		}
	}
	if prompt.Text != "Approve or refuse fixture.deploy" || len(prompt.Blocks) != 2 {
		t.Fatalf("prompt message: %+v", prompt)
	}
	buttons, _ := prompt.Blocks[1]["elements"].([]any)
	if len(buttons) != 2 {
		t.Fatalf("buttons: %+v", prompt.Blocks[1])
	}
	approve := buttons[0].(map[string]any)
	if approve["text"].(map[string]any)["text"] != "approve" {
		t.Fatalf("first button: %+v", approve)
	}

	var got []extension.ActRequest
	e.act = func(_ context.Context, r extension.ActRequest) (extension.ActResult, error) {
		got = append(got, r)
		if r.Caller != "U0ALICE" {
			return extension.ActResult{}, &extension.OperationError{Code: "not_allowed", Message: "you are not one of the people this question is for"}
		}
		return extension.ActResult{Added: true, Message: "answered: " + r.Input}, nil
	}
	ctx := context.Background()
	press := func(user string) {
		t.Helper()
		if err := e.projector.Interact(ctx, e.fake.Press(slChannel, root, prompt.TS, user, approve["action_id"].(string), approve["value"].(string))); err != nil {
			t.Fatal(err)
		}
	}
	press("U0MALLORY")
	press("U0ALICE")
	if len(got) != 2 || got[1].Work != w.ID || got[1].Provider != "decisions" || got[1].Action != "answer" || got[1].Input != "approve" || got[1].Scope != slScope || got[0].Key == got[1].Key {
		t.Fatalf("act requests: %+v", got)
	}
	eph := e.fake.Ephemerals()
	if len(eph) != 2 || eph[0].User != "U0MALLORY" || !strings.Contains(eph[0].Text, "not one of the people") || eph[1].User != "U0ALICE" || eph[1].Text != "answered: approve" || eph[1].ThreadTS != root {
		t.Fatalf("ephemerals: %+v", eph)
	}
	// A press from another workspace, another channel or of another button
	// is ignored.
	other := e.fake.Press("C0ELSEWHERE", root, prompt.TS, "U0ALICE", approve["action_id"].(string), approve["value"].(string))
	if err := e.projector.Interact(ctx, other); err != nil || len(got) != 2 {
		t.Fatalf("a press in an unwatched channel was taken: %v %d", err, len(got))
	}
	if err := e.projector.Interact(ctx, e.fake.Press(slChannel, root, prompt.TS, "U0ALICE", "someone_else", approve["value"].(string))); err != nil || len(got) != 2 {
		t.Fatalf("another app's button was taken: %v %d", err, len(got))
	}
}

// What a person in the Slack thread sees comes from the real `human`
// provider: the question it was asked (not only "Answer the agent's
// question") and its options as buttons. A new question on the same Work is
// prompted afresh, and the same one isn't reposted.
func TestSlackPromptShowsTheHumanQuestion(t *testing.T) {
	h := newHumanHarness(t, "persistent", decisions)
	h.ask(t, `{"question":"Ship the migration tonight?","options":["yes","no"],"context":"The backup finished at 21:00."}`)
	acts, err := h.actions.Actions(context.Background(), h.target(t))
	if err != nil || len(acts) != 1 {
		t.Fatalf("human offers: %+v %v", acts, err)
	}

	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Can we migrate?")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	offer := extension.ActionOffer{Provider: "decisions", Action: acts[0]}
	e.actions = func(context.Context, domain.ID, domain.ID) ([]extension.ActionOffer, error) {
		return []extension.ActionOffer{offer}, nil
	}
	e.park(w.ID, "wait/1")
	if res := e.converge(); res.Posted != 1 {
		t.Fatalf("prompt: %+v", res)
	}
	prompts := func() []sl.Message {
		var out []sl.Message
		for _, m := range e.fake.Thread(slChannel, root) {
			if len(m.Blocks) > 0 {
				out = append(out, m)
			}
		}
		return out
	}
	shown := prompts()[0]
	text, _ := shown.Blocks[0]["text"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Ship the migration tonight?") || !strings.Contains(text, "The backup finished at 21:00.") {
		t.Fatalf("the prompt doesn't show the question: %q", text)
	}
	buttons, _ := shown.Blocks[1]["elements"].([]any)
	if len(buttons) != 2 || buttons[0].(map[string]any)["text"].(map[string]any)["text"] != "yes" {
		t.Fatalf("buttons: %+v", shown.Blocks[1])
	}
	if res := e.converge(); res.Posted != 0 || len(prompts()) != 1 {
		t.Fatalf("the same question was posted again: %+v", res)
	}
	offer.Detail = "Ship the migration tomorrow instead?"
	if res := e.converge(); res.Posted != 1 || len(prompts()) != 2 {
		t.Fatalf("a new question wasn't prompted: %+v", res)
	}
}

// A conversation's Work that is ready but hasn't started (held until a
// person approves its start, say) is prompted for what is offered on it,
// as waiting Work is.
func TestSlackPromptsReadyWork(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Please deploy 1.2")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	if w.State != domain.WorkReady {
		t.Fatalf("Work %s, want ready", w.State)
	}
	e.actions = func(_ context.Context, _, work domain.ID) ([]extension.ActionOffer, error) {
		return []extension.ActionOffer{{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Approve or refuse the slack.thread Work", Choices: []string{"approve", "refuse"}, Detail: "Allow it to start?"}}}, nil
	}
	if res := e.converge(); res.Posted != 1 {
		t.Fatalf("ready Work not prompted: %+v", res)
	}
}

// slJobRoute starts a job's Work for a fixture.tick, standing in for a
// schedule: Work that did not start in Slack.
type slJobRoute struct{}

func (slJobRoute) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	if in.Event.Event.Type != "fixture.tick" {
		return extension.Skip("not a tick"), nil
	}
	return extension.RouteDecision{Match: true, CreateID: "job/nightly", Kind: "fixture.job"}, nil
}

// TestSlackPostOpensAThreadTheJobOwns: Work that did not start in Slack
// posts its report to a channel its binding may post in, through the real
// toolbelt; the thread becomes the job's without giving it another turn, a
// retried call posts once, and a person's reply in the thread is the job's
// input on its own binding, not the conversation route's. A channel the
// binding may not post in is refused, and the builtin emit cannot forge
// the thread's attachment.
func TestSlackPostOpensAThreadTheJobOwns(t *testing.T) {
	ctx := context.Background()
	e := newSlackEnvWith(t, `,"post":[{"binding":"nightly","channels":["`+slChannel+`"],"follow_up":{}}]`, sl.IngressOptions{}, []string{slChannel},
		[]domain.Binding{{Name: "agent", Executor: "fixture"}, {Name: "nightly", Executor: "fixture"}},
		[]domain.Route{{Name: "jobs", Handler: "jobs", Binding: "nightly"}, {Name: "threads", Handler: "slack", Binding: "agent", ActivePolicy: "queue"}})
	tick := domain.Event{ID: "tick-1", Type: "fixture.tick", Source: "fixture/jobs", DeliveryID: "tick-1", OccurredAt: e.clock.Now(), ReceivedAt: e.clock.Now(),
		CorrelationKey: &domain.CorrelationKey{Source: "fixture/jobs", ExternalID: "nightly"}}
	if _, _, err := e.ingress.Accept(ctx, slScope, tick); err != nil {
		t.Fatal(err)
	}
	e.route()

	// As the daemon grants it: every registered family is in every
	// execution's authority, so only the seal keeps the builtin emit off it.
	var belt *runtime.Toolbelt
	belt, err := runtime.NewToolbelt(e.store, e.clock.Now, e.router, runtime.ToolbeltOptions{
		Grant: func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			return runtime.ToolbeltGrant{Operations: []string{sl.OpPost, sl.OpReply, sl.OpThread, extension.Emit}, EventTypes: belt.EventTypes()}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := sl.NewTools(sl.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []sl.Source {
		return []sl.Source{{Settings: e.settings, Client: e.client, Projector: e.projector}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := belt.Register(tools.Registration()); err != nil {
		t.Fatal(err)
	}
	var token string
	var bindings []string
	outcome := domain.OutcomeHandoff
	executor := &schedulerExecutor{Caps: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}
	executor.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		token = r.Context.ToolbeltToken
		bindings = append(bindings, r.Execution.ExecutorBinding)
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: outcome, Summary: "reported"}}}, nil
	}
	scheduler, err := runtime.NewScheduler(e.store, e.clock.Now, e.router, runtime.SchedulerOptions{Owner: "test", LeaseDuration: time.Hour, Generation: "1",
		Executors: map[string]extension.Executor{"fixture": executor}, Toolbelt: belt, ToolbeltEndpoints: fixtureEndpoint("unix:///fixture.sock")})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := scheduler.Start(ctx, "job/nightly")
	if err != nil || token == "" {
		t.Fatalf("start: %v (token %q)", err, token)
	}
	post := func(request, input string) (map[string]any, error) {
		out, err := belt.CallRequest(ctx, token, request, extension.ToolCall{Operation: sl.OpPost, Input: domain.Payload{Schema: "json", Data: json.RawMessage(input)}})
		if err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(out.Output.Data, &m)
		return m, nil
	}
	var op *extension.OperationError
	if _, err := post("elsewhere", `{"channel":"C0OTHER","text":"Nightly report"}`); !errors.As(err, &op) || op.Code != "channel_not_allowed" {
		t.Fatalf("a channel the binding may not post in: %v", err)
	}
	first, err := post("report", `{"channel":"`+slChannel+`","text":"Nightly report: 3 failures"}`)
	if err != nil || first["state"] != sl.IntentPosted || first["ts"] == "" {
		t.Fatalf("post: %v %v", first, err)
	}
	ts, _ := first["ts"].(string)
	if m, ok := e.fake.Message(slChannel, ts); !ok || m.ThreadTS != "" && m.ThreadTS != ts || m.Text != "Nightly report: 3 failures" {
		t.Fatalf("the report is not a top-level message: %+v %v", m, ok)
	}
	again, err := post("report", `{"channel":"`+slChannel+`","text":"Nightly report: 3 failures"}`)
	if err != nil || again["ts"] != ts || len(e.events(sl.TypeThreadOpened)) != 1 {
		t.Fatalf("a retried post wrote again: %v %v", again, err)
	}
	forged := domain.Event{Type: sl.TypeThreadOpened, DeliveryID: "forged", Data: domain.Payload{Schema: sl.SchemaThreadOpened, Data: json.RawMessage(`{"namespace":"` + slNS + `","channel":"` + slChannel + `","ts":"1.000001"}`)}}
	if _, err := belt.Call(ctx, token, extension.ToolCall{Operation: extension.Emit, Event: &forged}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("the builtin emit forged a posted thread: %v", err)
	}

	e.sync()
	e.reconcile()
	e.route()
	job, ok := e.workFor(ts)
	if !ok || job.ID != "job/nightly" {
		t.Fatalf("the posted thread is not the job's: %+v %v", job, ok)
	}
	if !slices.ContainsFunc(job.CorrelationKeys, func(k domain.CorrelationKey) bool { return k.ExternalID == sl.PostedKey(slChannel, ts, "nightly") }) {
		t.Fatalf("the job's thread does not say nightly posted it: %+v", job.CorrelationKeys)
	}
	if q := e.queued(job.ID); len(q) != 0 {
		t.Fatalf("the attachment or the post itself became input: %v", q)
	}
	if opened := e.events(sl.TypeThreadOpened); len(opened) != 1 || !opened[0].Routed || opened[0].Disposition != extension.DispositionAttach || opened[0].RoutedWorkID != job.ID {
		t.Fatalf("the posted thread's attachment was not recorded on the job: %+v", opened)
	}
	// A post whose answer is lost answers pending and needs no second call:
	// the projector finds it after the turn has ended, and its thread is
	// still the job's.
	e.fake.FailNextPost("lost")
	lost, err := post("follow-up", `{"channel":"`+slChannel+`","text":"Also: one flaky test"}`)
	if err != nil || lost["state"] != "pending" {
		t.Fatalf("a lost post: %v %v", lost, err)
	}
	if err := scheduler.Observe(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 {
		t.Fatalf("the lost post was not recovered: %+v", res)
	}
	if opened := e.events(sl.TypeThreadOpened); len(opened) != 2 {
		t.Fatalf("the recovered post's thread was not opened: %+v", opened)
	}
	e.converge()
	if opened := e.events(sl.TypeThreadOpened); len(opened) != 2 {
		t.Fatalf("a second pass opened the thread again: %+v", opened)
	}

	reply := e.fake.Reply(slChannel, ts, slAlice, "Which tests failed?")
	e.sync()
	e.reconcile()
	e.route()
	var followUp string
	e.read(func(tx storage.Tx) error {
		w, err := tx.Work(job.ID)
		for _, k := range w.CorrelationKeys {
			if _, th, ok := sl.ParseThreadKey(k.ExternalID); ok && th != ts {
				followUp = th
			}
		}
		return err
	})
	if followUp == "" {
		t.Fatal("the recovered post's thread is not the job's")
	}
	second := e.fake.Reply(slChannel, followUp, slBob, "Which one?")
	e.sync()
	e.reconcile()
	e.route()
	if q := e.queued(job.ID); len(q) != 2 || q[0] != sl.TypeMessageHuman+":"+reply || q[1] != sl.TypeMessageHuman+":"+second {
		t.Fatalf("the replies did not reach the job: %v", q)
	}
	inputs, err := e.router.PendingInputs(ctx, job.ID)
	if err != nil || len(inputs) != 2 || inputs[0].Binding != "nightly" || inputs[1].Binding != "nightly" {
		t.Fatalf("the replies are not on the job's binding: %+v %v", inputs, err)
	}
	for _, w := range e.works() {
		if w.Kind == sl.WorkKind {
			t.Fatalf("the reply started a conversation: %+v", w)
		}
	}

	// The post config opts nightly's posts into follow-ups. The job answers
	// and finishes. A question after that opens its successor on the job's
	// binding, which reads the post in the thread and answers there; once
	// it has finished too, the next question opens the generation after it.
	outcome = domain.OutcomeCompleted
	finish := func(work domain.ID) {
		t.Helper()
		a, err := scheduler.Start(ctx, work)
		if err != nil {
			t.Fatalf("start %s: %v", work, err)
		}
		if err := scheduler.Observe(ctx, a); err != nil {
			t.Fatal(err)
		}
		e.read(func(tx storage.Tx) error {
			w, err := tx.Work(work)
			if err == nil && w.State != domain.WorkCompleted {
				t.Fatalf("%s is %s, want completed", work, w.State)
			}
			return err
		})
	}
	finish(job.ID)
	ask := func(user, text string) string {
		t.Helper()
		ts := e.fake.Reply(slChannel, ts, user, text)
		e.sync()
		e.reconcile()
		e.route()
		return ts
	}
	late := ask(slAlice, "And the flaky one?")
	g2 := job.ID + "/g2"
	if q := e.queued(g2); len(q) != 1 || q[0] != sl.TypeMessageHuman+":"+late {
		t.Fatalf("the question after the job finished did not open its successor: %v", q)
	}
	if inputs, err := e.router.PendingInputs(ctx, g2); err != nil || len(inputs) != 1 || inputs[0].Binding != "nightly" {
		t.Fatalf("the successor is not on the job's binding: %+v %v", inputs, err)
	}
	e.read(func(tx storage.Tx) error {
		w, err := tx.Work(g2)
		if err != nil {
			return err
		}
		if w.Kind != "fixture.job" || len(w.CorrelationKeys) != 1 || w.CorrelationKeys[0].ExternalID != sl.GenerationKey(slChannel, ts, "nightly", 2) || len(w.Grants) != 0 {
			t.Fatalf("the successor is not the job's next generation: %+v", w)
		}
		old, err := tx.Work(job.ID)
		if err == nil && old.State != domain.WorkCompleted {
			t.Fatalf("the finished job changed: %+v", old)
		}
		return err
	})
	outcome = domain.OutcomeHandoff
	a, err := scheduler.Start(ctx, g2)
	if err != nil {
		t.Fatal(err)
	}
	if bindings[len(bindings)-1] != "nightly" {
		t.Fatalf("the successor ran on %s", bindings[len(bindings)-1])
	}
	read, err := belt.CallRequest(ctx, token, "read", extension.ToolCall{Operation: sl.OpThread, Input: domain.Payload{Schema: "json", Data: json.RawMessage(`{}`)}})
	if err != nil || !strings.Contains(string(read.Output.Data), "Nightly report: 3 failures") || !strings.Contains(string(read.Output.Data), "agent on "+string(job.ID)) {
		t.Fatalf("the successor does not read the job's post in its thread: %s %v", read.Output.Data, err)
	}
	answer, err := belt.CallRequest(ctx, token, "answer", extension.ToolCall{Operation: sl.OpReply, Input: domain.Payload{Schema: "json", Data: json.RawMessage(`{"text":"testFlaky"}`)}})
	if err != nil || !strings.Contains(string(answer.Output.Data), sl.IntentPosted) {
		t.Fatalf("the successor's answer: %s %v", answer.Output.Data, err)
	}
	if err := scheduler.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}
	e.sync()
	e.reconcile()
	e.route()
	if q := e.queued(g2); len(q) != 0 {
		t.Fatalf("the successor's own answer became its input: %v", q)
	}
	outcome = domain.OutcomeCompleted
	e.read(func(tx storage.Tx) error {
		w, err := tx.Work(g2)
		if err == nil && w.State != domain.WorkReady {
			t.Fatalf("the successor is %s, want ready", w.State)
		}
		return err
	})
	// A handoff with nothing queued needs a fresh input to run again; the
	// next question finds it live, so it is its input, not a successor's.
	fixed := ask(slBob, "Is it fixed?")
	if q := e.queued(g2); len(q) != 1 || q[0] != sl.TypeMessageHuman+":"+fixed {
		t.Fatalf("a question for the live successor did not reach it: %v", q)
	}
	finish(g2)
	last := ask(slAlice, "Thanks")
	if q := e.queued(job.ID + "/g3"); len(q) != 1 || q[0] != sl.TypeMessageHuman+":"+last {
		t.Fatalf("a question after the successor finished did not open the next generation: %v", q)
	}
}

// slIdentities gives two bindings their own name and icon; "agent" has none.
const slIdentities = `,"identities":[{"binding":"engineer","username":"Steve","icon_emoji":":wrench:"},{"binding":"designer","username":"Dana","icon_url":"https://example.com/dana.png"}]`

// slIdentityEnv is a Slack env whose scope has the engineer, designer and
// agent bindings, each threads route on agent.
func slIdentityEnv(t *testing.T) *slEnv {
	return newSlackEnvWith(t, slIdentities, sl.IngressOptions{}, []string{slChannel},
		[]domain.Binding{{Name: "agent", Executor: "fixture"}, {Name: "engineer", Executor: "fixture"}, {Name: "designer", Executor: "fixture"}},
		[]domain.Route{{Name: "threads", Handler: "slack", Binding: "agent", ActivePolicy: "queue"}})
}

// slReplier calls slack.reply as attempt of work.
func (e *slEnv) slReplier(work, attempt domain.ID) func(request, text string) (map[string]any, error) {
	tools, err := sl.NewTools(sl.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []sl.Source {
		return []sl.Source{{Settings: e.settings, Client: e.client, Projector: e.projector}}
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	var reply extension.Operation
	for _, op := range tools.Registration().Operations {
		if op.Name == sl.OpReply {
			reply = op
		}
	}
	auth := extension.Authority{ScopeID: slScope, WorkID: work, ExecutionID: attempt}
	return func(request, text string) (map[string]any, error) {
		in, _ := json.Marshal(map[string]string{"text": text})
		out, err := reply.Handler(extension.WithCallRequestID(context.Background(), request), auth, domain.Payload{Schema: "json", Data: in})
		if err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(out.Data, &m)
		return m, nil
	}
}

// TestSlackBindingsPostUnderTheirOwnIdentity: two bindings reply in one
// channel under their own names, read from each attempt's stored
// Execution; a binding without an identity posts as the app. A reply with
// a custom name is the app's own by its marker, or by its bot ID when
// Slack withholds metadata, never by its name: it wakes nothing, reconcile
// takes it for no person's message, and a lost post is found again rather
// than repeated. A waiting Work's prompt posts under its latest attempt's
// binding.
func TestSlackBindingsPostUnderTheirOwnIdentity(t *testing.T) {
	e := slIdentityEnv(t)
	roots := map[string]string{}
	for _, b := range []string{"engineer", "designer", "agent"} {
		roots[b] = e.fake.Post(slChannel, slAlice, "a question for "+b)
	}
	e.sync()
	e.reconcile()
	e.route()
	works := map[string]domain.Work{}
	for b, root := range roots {
		w, ok := e.workFor(root)
		if !ok {
			t.Fatalf("no Work for %s", b)
		}
		works[b] = w
		attempt, _ := e.startAttemptOn(w.ID, b)
		if out, err := e.slReplier(w.ID, attempt)("r1", "answer from "+b); err != nil || out["state"] != sl.IntentPosted {
			t.Fatalf("%s reply: %v %v", b, out, err)
		}
	}
	for b, want := range map[string]string{"engineer": "Steve", "designer": "Dana", "agent": ""} {
		got := e.fake.AgentReplies(slChannel, roots[b])
		if len(got) != 1 || got[0].Username != want {
			t.Fatalf("%s posted %+v, want name %q", b, got, want)
		}
		if want != "" && (got[0].User != "" || got[0].BotID == "" || got[0].Subtype != "bot_message") {
			t.Fatalf("%s's reply is not shown as Slack shows a custom name: %+v", b, got[0])
		}
		if want == "" && got[0].User != e.fake.BotUser() {
			t.Fatalf("agent's reply is not the app's: %+v", got[0])
		}
	}
	// The replies come back by push and by reconcile: each is the app's own
	// and wakes no Work.
	steve := e.fake.AgentReplies(slChannel, roots["engineer"])[0]
	if code := e.deliver(e.fake.MessageEvent(slChannel, steve.TS)); code != http.StatusOK {
		t.Fatalf("delivery: %d", code)
	}
	e.sync()
	e.reconcile()
	e.route()
	if got := e.events(sl.TypeMessageAgent); len(got) != 3 {
		t.Fatalf("agent events: %d, want the three replies", len(got))
	}
	if got := e.events(sl.TypeMessageHuman); len(got) != 3 {
		t.Fatalf("human events: %d, want only the three questions", len(got))
	}
	for b, w := range works {
		for _, q := range e.queued(w.ID) {
			if !strings.HasSuffix(q, ":"+roots[b]) {
				t.Fatalf("%s's Work woke for %s", b, q)
			}
		}
	}

	// Slack withholds metadata and the answer to a post is lost: the reply
	// is found again by the app's bot ID, not posted twice, and reconcile
	// takes it for no person's message.
	e.fake.WithholdMetadata(true)
	w := works["engineer"]
	xs := []domain.Execution{}
	e.read(func(tx storage.Tx) error { var err error; xs, err = tx.ListExecutions(w.ID); return err })
	e.fake.FailNextPost("lost")
	if out, err := e.slReplier(w.ID, xs[0].ID)("r2", "one more thing"); err != nil || out["state"] != "pending" {
		t.Fatalf("lost reply: %v %v", out, err)
	}
	e.clock.Add(31 * time.Second)
	own := func() int {
		n := 0
		for _, m := range e.fake.Thread(slChannel, roots["engineer"]) {
			if m.BotID != "" {
				n++
			}
		}
		return n
	}
	if res := e.converge(); res.Recovered != 1 || own() != 2 {
		t.Fatalf("recovery by bot ID: %+v, replies %d", res, own())
	}
	e.sync()
	e.reconcile()
	e.route()
	if got := e.events(sl.TypeMessageHuman); len(got) != 3 {
		t.Fatalf("an unmarked custom-named reply was taken for a person's: %d human events", len(got))
	}
	e.fake.WithholdMetadata(false)

	// A ready Work's prompt posts under its latest attempt's binding.
	d := works["designer"]
	e.read(func(tx storage.Tx) error { var err error; xs, err = tx.ListExecutions(d.ID); return err })
	e.finishAttempt(xs[0].ID, domain.WorkReady, "")
	e.actions = func(_ context.Context, _, work domain.ID) ([]extension.ActionOffer, error) {
		if work != d.ID {
			return nil, nil
		}
		return []extension.ActionOffer{{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Ship it?", Choices: []string{"yes", "no"}}}}, nil
	}
	if res := e.converge(); res.Posted != 1 {
		t.Fatalf("prompt: %+v", res)
	}
	got := e.fake.AgentReplies(slChannel, roots["designer"])
	if len(got) != 2 || got[1].Username != "Dana" {
		t.Fatalf("prompt not posted as Dana: %+v", got)
	}
}

// TestSlackIdentityWithoutScopeIsRefused: a token without
// chat:write.customize posts nothing for a binding with its own identity,
// where Slack would post under the app's name: the tool gives a clear
// refusal, a declared reply fails rather than retrying, and a binding with
// no identity still posts as the app.
func TestSlackIdentityWithoutScopeIsRefused(t *testing.T) {
	e := slIdentityEnv(t)
	var scopes []string
	for _, s := range sl.BotScopes {
		if s != sl.CustomizeScope {
			scopes = append(scopes, s)
		}
	}
	e.fake.SetScopes(scopes)
	eng := e.fake.Post(slChannel, slAlice, "for the engineer")
	plain := e.fake.Post(slChannel, slAlice, "for the agent")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(eng)
	attempt, _ := e.startAttemptOn(w.ID, "engineer")
	var op *extension.OperationError
	if _, err := e.slReplier(w.ID, attempt)("r1", "hello"); !errors.As(err, &op) || op.Code != "identity_scope_missing" || !strings.Contains(op.Message, sl.CustomizeScope) {
		t.Fatalf("refusal: %v", err)
	}
	if got := e.fake.AgentReplies(slChannel, eng); len(got) != 0 {
		t.Fatalf("posted without the scope: %+v", got)
	}
	e.finishAttempt(attempt, domain.WorkReady, "declared answer")
	if res := e.converge(); res.Failed != 1 || res.Posted != 0 || len(e.fake.AgentReplies(slChannel, eng)) != 0 {
		t.Fatalf("declared reply without the scope: %+v", res)
	}
	if res := e.converge(); res.Failed != 0 || res.Posted != 0 {
		t.Fatalf("a refused reply was retried: %+v", res)
	}
	p, _ := e.workFor(plain)
	pa := e.startAttempt(p.ID)
	if out, err := e.slReplier(p.ID, pa)("r1", "hello"); err != nil || out["state"] != sl.IntentPosted {
		t.Fatalf("a binding without an identity: %v %v", out, err)
	}
}
