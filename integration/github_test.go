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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	ghRepo   = "octo/widgets"
	ghNS     = "github/github.com/octo/widgets"
	ghScope  = "eng"
	ghSecret = "hook-secret"
	ghSource = "github/eng/widgets"
)

var ghAlice = githubtest.User{Login: "alice", Type: "User"}

type ghClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ghClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *ghClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// ghEnv composes the real runtime ingress and router over SQLite with the
// GitHub extension against the fake service, exactly as bootstrap wires them.
type ghEnv struct {
	t         *testing.T
	fake      *githubtest.Server
	clock     *ghClock
	path      string
	store     *sqlite.Store
	ingress   *runtime.Ingress
	router    *runtime.Router
	adapter   *gh.Ingress
	client    *gh.Client
	projector *gh.Projector
	settings  gh.Settings
	webhook   http.Handler
	// observed sees the adapter's observations, as bootstrap's wait
	// progress does; nil sees nothing.
	observed func(context.Context, []extension.Observation)
	// connections are the other `github` connections the host lets the
	// extension act as in ghScope (ones a binding names), by ID, each with
	// the token it signs in with on the fake.
	connMu      sync.Mutex
	connections map[string]string
	writers     *gh.Writers
}

func ghExtensionConfig() string {
	return `{"namespace":"` + ghNS + `","scope":"` + ghScope + `","repo":"` + ghRepo + `","webhook_secret":"hook","projection":{"verify_seconds":60}}`
}

func newGitHubEnv(t *testing.T) *ghEnv {
	t.Helper()
	fake := githubtest.New(ghRepo, "tok")
	t.Cleanup(fake.Close)
	e := &ghEnv{t: t, fake: fake, clock: &ghClock{t: fake.Now()}, path: filepath.Join(t.TempDir(), "conduit.db")}
	e.open()
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
		{"name": "widgets", "type": "github", "connection": "github", "config": json.RawMessage(ghExtensionConfig()), "secret_names": []string{"hook"}},
	}})
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: ghScope, Name: "engineering", Bindings: []domain.Binding{{Name: "agent", Executor: "fixture"}},
			Routes: []domain.Route{{Name: "issues", Handler: "github", Binding: "agent", ActivePolicy: "queue"}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	e.register()
	return e
}

func (e *ghEnv) open() {
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
	if e.router, err = runtime.NewRouter(store, e.clock.Now, runtime.RouterOptions{Generation: "1", Handlers: map[string]extension.RouteHandler{"github": gh.Route{}},
		Bindings: map[string]runtime.RouteBinding{"fixture": {}}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := gh.ParseConnection([]byte(`{"api_url":"` + e.fake.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.client, err = gh.NewClient(cfg, func(context.Context, string) (string, error) { return "tok", nil }, gh.ClientOptions{Now: e.clock.Now}); err != nil {
		t.Fatal(err)
	}
	if e.settings, err = gh.ParseExtension([]byte(ghExtensionConfig()), []string{"hook"}); err != nil {
		t.Fatal(err)
	}
	e.adapter = gh.NewIngress(e.settings, e.client, e.clock.Now, gh.IngressOptions{Observed: func(ctx context.Context, obs []extension.Observation) {
		if e.observed != nil {
			e.observed(ctx, obs)
		}
	}})
	e.writers = gh.NewWriters(func(scope domain.ID, id string) (json.RawMessage, gh.SecretFunc, bool) {
		e.connMu.Lock()
		defer e.connMu.Unlock()
		token, ok := e.connections[id]
		if !ok || scope != ghScope {
			return nil, nil, false
		}
		return json.RawMessage(`{"api_url":"` + e.fake.URL + `"}`), func(context.Context, string) (string, error) { return token, nil }, true
	}, e.clock.Now)
	if e.projector, err = gh.NewProjector(gh.ProjectorOptions{Store: store, Client: e.client, Settings: e.settings, Now: e.clock.Now, SearchDelay: 30 * time.Second, Writers: e.writers, Scope: ghScope}); err != nil {
		t.Fatal(err)
	}
	e.webhook = gh.WebhookHandler(func(context.Context) (string, error) { return ghSecret, nil }, func(ctx context.Context, d extension.Delivery) error {
		_, err := e.ingress.Push(ctx, ghSource, d)
		return err
	}, e.clock.Now)
}

func (e *ghEnv) register() {
	if err := e.ingress.Register(context.Background(), ghSource, extension.Source{Namespace: ghNS, ScopeID: ghScope}, e.adapter, 15*time.Minute); err != nil {
		e.t.Fatal(err)
	}
}

// reopen simulates a daemon restart on the same database.
func (e *ghEnv) reopen() {
	_ = e.store.Close()
	e.open()
	e.register()
}

// deliver posts a signed webhook through the extension's HTTP boundary.
func (e *ghEnv) deliver(event, id string, body []byte) int {
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(string(body)))
	req.Header = githubtest.Sign(ghSecret, event, id, body)
	w := httptest.NewRecorder()
	e.webhook.ServeHTTP(w, req)
	return w.Code
}

// reconcile runs a full generation through the runtime's due scheduling.
func (e *ghEnv) reconcile() runtime.ReconcileResult {
	e.t.Helper()
	var last runtime.ReconcileResult
	for i := 0; i < 200; i++ {
		res, err := e.ingress.Reconcile(context.Background(), ghSource)
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

func (e *ghEnv) route() {
	e.t.Helper()
	for {
		page, err := e.ingress.Pending(context.Background(), 0, 200)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(page.Items) == 0 {
			return
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

func (e *ghEnv) read(fn func(storage.Tx) error) {
	e.t.Helper()
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *ghEnv) events() []domain.AcceptedEvent {
	var out []domain.AcceptedEvent
	e.read(func(tx storage.Tx) error { var err error; out, err = tx.Events(0, 0); return err })
	return out
}

func (e *ghEnv) countType(prefix string) int {
	n := 0
	for _, a := range e.events() {
		if strings.HasPrefix(a.Event.Type, prefix) {
			n++
		}
	}
	return n
}

func (e *ghEnv) workFor(external string) (domain.Work, bool) {
	var w domain.Work
	found := false
	e.read(func(tx storage.Tx) error {
		var err error
		w, err = tx.WorkByCorrelation(domain.CorrelationKey{Source: ghNS, ExternalID: external})
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return w, found
}

func (e *ghEnv) queued(work domain.ID) []string {
	inputs, err := e.router.PendingInputs(context.Background(), work)
	if err != nil {
		e.t.Fatal(err)
	}
	var types []string
	e.read(func(tx storage.Tx) error {
		for _, in := range inputs {
			a, err := tx.Event(in.EventID)
			if err != nil {
				return err
			}
			types = append(types, a.Event.Type)
		}
		return nil
	})
	return types
}

// startAttempt records a running attempt for Work (durable fixture state).
func (e *ghEnv) startAttempt(work domain.ID) domain.ID {
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

// finishAttempt ends the attempt and moves Work to state.
func (e *ghEnv) finishAttempt(id domain.ID, state domain.WorkState, decl *domain.TurnDeclaration) {
	e.t.Helper()
	e.read(func(tx storage.Tx) error {
		x, err := tx.Execution(id)
		if err != nil {
			return err
		}
		x.State = domain.ExecutionCompleted
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "done"}
		if decl != nil {
			data, _ := json.Marshal(decl)
			x.Outcome.Data = domain.Payload{Schema: domain.TurnSchema, Data: data}
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

func (e *ghEnv) converge() gh.ProjectionResult {
	e.t.Helper()
	res, err := e.projector.Converge(context.Background(), 50)
	if err != nil {
		e.t.Fatalf("projection: %v %v", err, res.Codes)
	}
	return res
}

func TestGitHubReconcileOnlyRecoversMissedWebhooks(t *testing.T) {
	e := newGitHubEnv(t)
	n := e.fake.OpenIssue("Header wobbles", "", ghAlice, "bug")
	skipped := e.fake.OpenIssue("Later", "", ghAlice, "backlog")
	theirs := e.fake.OpenIssue("Owned by v1", "", ghAlice, "agent-in-progress")
	comment := e.fake.AddComment(n, ghAlice, "please make it blue")
	// Webhooks are off: nothing is pushed. Reconcile alone converges.
	res := e.reconcile()
	if len(res.Accepted) == 0 {
		t.Fatal("reconcile accepted nothing")
	}
	e.route()
	w, ok := e.workFor("issue/" + strconv.Itoa(n))
	if !ok || w.Kind != gh.WorkKind || w.State != domain.WorkReady {
		t.Fatalf("claimable issue produced no Work: %+v", w)
	}
	for _, other := range []int{skipped, theirs} {
		if _, ok := e.workFor("issue/" + strconv.Itoa(other)); ok {
			t.Fatalf("issue %d (skip label / another owner's claim) was claimed", other)
		}
	}
	// The issue observation creates Work and is its first input; the comment
	// joins in acceptance order.
	if got := e.queued(w.ID); len(got) != 2 || got[0] != gh.TypeIssueOpen || got[1] != gh.TypeCommentHuman {
		t.Fatalf("queued inputs %v", got)
	}
	// A quiet reconcile accepts nothing new.
	count := len(e.events())
	if again := e.reconcile(); len(again.Accepted) != 0 || len(e.events()) != count {
		t.Fatalf("unchanged reconcile accepted %d events", len(again.Accepted))
	}
	_ = comment
}

func TestGitHubWebhookAndReconcileOverlapDeduplicates(t *testing.T) {
	e := newGitHubEnv(t)
	fk := e.fake
	n := fk.OpenIssue("Header", "", ghAlice)
	c := fk.AddComment(n, ghAlice, "first")
	if code := e.deliver("issues", "d-1", fk.IssuesHook("opened", n)); code != http.StatusAccepted {
		t.Fatalf("issues delivery: %d", code)
	}
	if code := e.deliver("issue_comment", "d-2", fk.CommentHook("created", c)); code != http.StatusAccepted {
		t.Fatalf("comment delivery: %d", code)
	}
	// GitHub redelivers: the same delivery is accepted once.
	if code := e.deliver("issue_comment", "d-2", fk.CommentHook("created", c)); code != http.StatusAccepted {
		t.Fatalf("redelivery: %d", code)
	}
	if code := e.deliver("issues", "forged", []byte(`{"repository":{"full_name":"`+ghRepo+`"}}`)); code != http.StatusAccepted {
		// forged signature path is covered below
		_ = code
	}
	before := e.events()
	// The reconcile covering the same state emits no new events: its
	// observations deduplicate against the pushed deliveries.
	e.reconcile()
	if got := e.events(); len(got) != len(before) {
		var types []string
		for _, a := range got[len(before):] {
			types = append(types, a.Event.Type+" "+a.Event.DeliveryID)
		}
		t.Fatalf("overlapping reconcile added %v", types)
	}
	// A change made while push was down arrives through reconcile only.
	fk.CloseIssue(n, "completed")
	e.reconcile()
	if got := e.events(); len(got) != len(before)+1 || got[len(got)-1].Event.Type != gh.TypeIssueClosed {
		t.Fatalf("missed close not recovered: %d events", len(got))
	}
	// The late webhook for that close is then a duplicate.
	e.deliver("issues", "d-3", fk.IssuesHook("closed", n))
	after := e.events()
	if len(after) != len(before)+2 { // the reconciled close + d-3's raw delivery
		t.Fatalf("events %d -> %d", len(before), len(after))
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(`{}`))
	req.Header = githubtest.Sign("wrong", "issues", "d-4", []byte(`{}`))
	w := httptest.NewRecorder()
	e.webhook.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || len(e.events()) != len(after) {
		t.Fatalf("forged delivery: %d", w.Code)
	}
}

func TestGitHubRateLimitedScanInfersNoAbsence(t *testing.T) {
	e := newGitHubEnv(t)
	for i := 0; i < 3; i++ {
		e.fake.OpenIssue("issue "+strconv.Itoa(i), "", ghAlice)
	}
	e.reconcile()
	reset := e.clock.Now().Add(20 * time.Minute)
	e.fake.RateLimitPrimary(reset)
	res, err := e.ingress.Reconcile(context.Background(), ghSource)
	if err == nil || res.Complete || res.ErrorCode != "provider_failed" {
		t.Fatalf("rate-limited scan: %+v %v", res, err)
	}
	// The paused credential fails locally on later attempts; still no absence.
	for i := 0; i < 3; i++ {
		e.clock.Add(time.Minute)
		_, _ = e.ingress.ReconcileDue(context.Background(), 10)
	}
	if n := e.countType("core.ingress.absent"); n != 0 {
		t.Fatalf("absence inferred from a rate-limited scan: %d", n)
	}
	statuses, err := e.ingress.Inspect(context.Background())
	if err != nil || len(statuses) != 1 || statuses[0].LastError == "" {
		t.Fatalf("inspection: %+v %v", statuses, err)
	}
	e.clock.Add(20 * time.Minute)
	count := len(e.events())
	if e.reconcile(); len(e.events()) != count {
		t.Fatalf("recovered scan accepted %d", len(e.events())-count)
	}
	if n := e.countType("core.ingress.absent"); n != 0 {
		t.Fatalf("absence after recovery: %d", n)
	}
}

func TestGitHubProjectionConvergesAndRepairsDrift(t *testing.T) {
	e := newGitHubEnv(t)
	n := e.fake.OpenIssue("Header", "", ghAlice)
	e.reconcile()
	e.route()
	w, _ := e.workFor("issue/" + strconv.Itoa(n))
	attempt := e.startAttempt(w.ID)
	if res := e.converge(); res.Labeled != 1 || !equal(e.fake.Labels(n), "agent-in-progress") {
		t.Fatalf("claim projection: %+v labels %v", res, e.fake.Labels(n))
	}
	e.finishAttempt(attempt, domain.WorkReady, &domain.TurnDeclaration{Status: "review"})
	e.converge()
	if !equal(e.fake.Labels(n), "agent-in-progress", "ready-for-review") {
		t.Fatalf("review projection: %v", e.fake.Labels(n))
	}
	// Steady state makes no writes.
	writes := len(e.fake.Writes())
	if res := e.converge(); res.Labeled != 0 || len(e.fake.Writes()) != writes {
		t.Fatalf("converged projection wrote again: %+v", res)
	}
	// Drift: someone removes our labels. The next verification repairs it.
	e.fake.SetLabels(n, "bug")
	e.converge()
	if !equal(e.fake.Labels(n), "bug") {
		t.Fatal("repaired before the verify interval")
	}
	e.clock.Add(61 * time.Second)
	e.converge()
	if !equal(e.fake.Labels(n), "agent-in-progress", "bug", "ready-for-review") {
		t.Fatalf("drift not repaired: %v", e.fake.Labels(n))
	}
	// The projection writes did not produce issue input (status labels are
	// excluded; the claim label was observed once when first applied).
	e.reconcile()
	e.route()
	for _, typ := range e.queued(w.ID) {
		if typ == gh.TypeIssueOpen {
			t.Fatal("our own label projection was queued as input")
		}
	}
	// A new attempt takes the status off while it runs (v1 parity).
	e.fake.AddComment(n, ghAlice, "one more thing")
	e.reconcile()
	e.route()
	second := e.startAttempt(w.ID)
	e.converge()
	if !equal(e.fake.Labels(n), "agent-in-progress", "bug") {
		t.Fatalf("status not removed while running: %v", e.fake.Labels(n))
	}
	e.finishAttempt(second, domain.WorkCompleted, nil)
	e.converge()
	if !equal(e.fake.Labels(n), "agent-in-progress", "bug", "might-be-done") {
		t.Fatalf("completion projection: %v", e.fake.Labels(n))
	}
}

// TestGitHubCompletedClosedIssueDropsTheClaim covers the label set a
// Work's end projects: a completed Work's closed issue no longer reads as
// claimed, and is still never claimed again.
func TestGitHubCompletedClosedIssueDropsTheClaim(t *testing.T) {
	e := newGitHubEnv(t)
	fk := e.fake
	done, finished, failed := fk.OpenIssue("Done", "", ghAlice), fk.OpenIssue("Finished", "", ghAlice), fk.OpenIssue("Failed", "", ghAlice)
	e.reconcile()
	e.route()
	end := func(n int, state domain.WorkState, status string) {
		w, _ := e.workFor("issue/" + strconv.Itoa(n))
		e.finishAttempt(e.startAttempt(w.ID), state, &domain.TurnDeclaration{Status: status})
	}
	end(done, domain.WorkReady, "done")
	end(finished, domain.WorkCompleted, "finished")
	end(failed, domain.WorkFailed, "")
	e.converge()
	if !equal(fk.Labels(done), "agent-in-progress", "might-be-done") {
		t.Fatalf("done projection: %v", fk.Labels(done))
	}
	// Finished while the issue is open (its PR merged, the close not yet
	// landed): still claimed, so nobody else picks it up.
	if !equal(fk.Labels(finished), "agent-in-progress") {
		t.Fatalf("finished open projection: %v", fk.Labels(finished))
	}
	if !equal(fk.Labels(failed), "agent-in-progress", "agent-needs-help") {
		t.Fatalf("failed projection: %v", fk.Labels(failed))
	}
	for _, n := range []int{finished, failed} {
		fk.CloseIssue(n, "completed")
		e.deliver("issues", "d-close-"+strconv.Itoa(n), fk.IssuesHook("closed", n))
	}
	e.route()
	e.clock.Add(61 * time.Second)
	e.converge()
	if labels := fk.Labels(finished); len(labels) != 0 {
		t.Fatalf("closed finished issue still labelled: %v", labels)
	}
	// Failed work still asks for help, closed or not.
	if !equal(fk.Labels(failed), "agent-in-progress", "agent-needs-help") {
		t.Fatalf("closed failed projection: %v", fk.Labels(failed))
	}
	// Settled: no more reads or writes for the closed issues.
	writes := len(fk.Writes())
	e.clock.Add(time.Hour)
	if e.converge(); len(fk.Writes()) != writes {
		t.Fatal("settled projection wrote again")
	}
	// Without the claimed label the closed issue is still not claimed again.
	e.reconcile()
	e.route()
	var works int
	e.read(func(tx storage.Tx) error { ws, err := tx.ListWorks(); works = len(ws); return err })
	if works != 3 {
		t.Fatalf("completed issue claimed again: %d Works", works)
	}
}

func equal(got []string, want ...string) bool {
	return strings.Join(got, ",") == strings.Join(want, ",")
}

func TestGitHubAmbiguousCommentIsRecoveredByMarker(t *testing.T) {
	e := newGitHubEnv(t)
	n := e.fake.OpenIssue("Header", "", ghAlice)
	e.reconcile()
	e.route()
	w, _ := e.workFor("issue/" + strconv.Itoa(n))
	attempt := e.startAttempt(w.ID)
	// The comment is created but its response is lost in transit.
	e.fake.LoseNextResponse("/repos/"+ghRepo+"/issues/"+strconv.Itoa(n)+"/comments", 1)
	e.finishAttempt(attempt, domain.WorkReady, &domain.TurnDeclaration{Status: "help", Comment: "I need the design tokens."})
	res, _ := e.projector.Converge(context.Background(), 50)
	if len(res.Codes) == 0 || len(e.fake.Comments(n)) != 1 {
		t.Fatalf("ambiguous post: %+v comments %d", res, len(e.fake.Comments(n)))
	}
	// Before the search delay nothing is posted again.
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.Comments(n)) != 1 {
		t.Fatalf("retried before the marker search: %+v", res)
	}
	e.clock.Add(31 * time.Second)
	if res := e.converge(); res.Recovered != 1 || len(e.fake.Comments(n)) != 1 {
		t.Fatalf("marker recovery: %+v comments %d", res, len(e.fake.Comments(n)))
	}
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.Comments(n)) != 1 {
		t.Fatal("posted after recovery")
	}
	// The posted comment is observed as this Work's own agent note: it is not
	// handed back as input.
	e.reconcile()
	e.route()
	for _, typ := range e.queued(w.ID) {
		if typ == gh.TypeCommentAgent {
			t.Fatal("own marked comment queued as input")
		}
	}
}

func TestGitHubCommentCrashAfterPostIsRecoveredOnRestart(t *testing.T) {
	e := newGitHubEnv(t)
	n := e.fake.OpenIssue("Header", "", ghAlice)
	e.reconcile()
	e.route()
	w, _ := e.workFor("issue/" + strconv.Itoa(n))
	attempt := e.startAttempt(w.ID)
	e.finishAttempt(attempt, domain.WorkReady, &domain.TurnDeclaration{Status: "review", Comment: "PR is ready: #12"})
	_ = e.store.Close()
	// A separate process converges and is SIGKILLed right after GitHub created
	// the comment, before it records local completion.
	cmd := exec.Command(os.Args[0], "-test.run=^TestGitHubCommentCrashHelper$")
	cmd.Env = append(os.Environ(), "CONDUIT_GITHUB_HELPER=1", "CONDUIT_GITHUB_DB="+e.path, "CONDUIT_GITHUB_URL="+e.fake.URL,
		"CONDUIT_GITHUB_NOW="+e.clock.Now().Format(time.RFC3339Nano))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("helper was not killed at the post boundary: %v\n%s", err, out)
	}
	if got := len(e.fake.Comments(n)); got != 1 {
		t.Fatalf("comments after crash = %d", got)
	}
	// Restart on the same database: recovery finds the marker and does not
	// post twice.
	e.open()
	e.register()
	e.clock.Add(31 * time.Second)
	res := e.converge()
	if res.Recovered != 1 || res.Posted != 0 || len(e.fake.Comments(n)) != 1 {
		t.Fatalf("restart recovery: %+v comments %d", res, len(e.fake.Comments(n)))
	}
	body := e.fake.Comments(n)[0].Body
	if !strings.Contains(body, "PR is ready: #12") || !strings.Contains(body, "open-conduit:agent work="+string(w.ID)) {
		t.Fatalf("marked comment body: %q", body)
	}
	e.clock.Add(time.Hour)
	if res := e.converge(); res.Posted+res.Recovered != 0 || len(e.fake.Comments(n)) != 1 {
		t.Fatal("comment posted twice")
	}
}

func TestGitHubCommentCrashHelper(t *testing.T) {
	if os.Getenv("CONDUIT_GITHUB_HELPER") == "" {
		return
	}
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("CONDUIT_GITHUB_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), os.Getenv("CONDUIT_GITHUB_DB"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := gh.ParseConnection([]byte(`{"api_url":"` + os.Getenv("CONDUIT_GITHUB_URL") + `"}`))
	client, err := gh.NewClient(cfg, func(context.Context, string) (string, error) { return "tok", nil }, gh.ClientOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := gh.ParseExtension([]byte(ghExtensionConfig()), []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := gh.NewProjector(gh.ProjectorOptions{Store: store, Client: client, Settings: settings, Now: func() time.Time { return now },
		AfterPost: func(string) { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL); select {} }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Converge(context.Background(), 50)
	t.Fatalf("helper survived the post: %v", err)
}

func TestGitHubReopenedIssueCreatesASuccessorGeneration(t *testing.T) {
	e := newGitHubEnv(t)
	fk := e.fake
	n := fk.OpenIssue("Header", "", ghAlice)
	e.reconcile()
	e.route()
	first, _ := e.workFor("issue/" + strconv.Itoa(n))
	attempt := e.startAttempt(first.ID)
	e.converge()
	e.finishAttempt(attempt, domain.WorkCompleted, nil)
	fk.CloseIssue(n, "completed")
	e.deliver("issues", "d-close", fk.IssuesHook("closed", n))
	e.route()
	e.converge()
	if !equal(fk.Labels(n), "might-be-done") {
		t.Fatalf("closed completed projection: %v", fk.Labels(n))
	}
	// Late input for the terminal Work is audited, not reopened.
	reopen := fk.ReopenIssue(n)
	e.deliver("issues", "d-reopen", fk.IssuesHook("reopened", n))
	e.route()
	gen := "issue/" + strconv.Itoa(n) + "/generation/reopen-" + strconv.FormatInt(reopen, 10)
	second, ok := e.workFor(gen)
	if !ok || second.ID == first.ID || second.State != domain.WorkReady {
		t.Fatalf("no successor generation Work: %+v", second)
	}
	old, _ := e.workFor("issue/" + strconv.Itoa(n))
	if old.ID != first.ID || old.State != domain.WorkCompleted {
		t.Fatalf("terminal owner changed: %+v", old)
	}
	// Comments on the reopened issue reach the successor.
	c := fk.AddComment(n, ghAlice, "still broken")
	e.deliver("issue_comment", "d-c", fk.CommentHook("created", c))
	e.route()
	if got := e.queued(second.ID); len(got) != 2 || got[1] != gh.TypeCommentHuman {
		t.Fatalf("successor inputs %v", got)
	}
	// Reconcile agrees with push about the new generation.
	count := len(e.events())
	if e.reconcile(); len(e.events()) != count {
		t.Fatalf("reconcile disagreed with push: %d new", len(e.events())-count)
	}
	// The projection follows the live generation, not the completed one.
	e.clock.Add(time.Hour)
	e.converge()
	if !equal(fk.Labels(n), "agent-in-progress") {
		t.Fatalf("successor projection: %v", fk.Labels(n))
	}
}

func TestGitHubAcknowledgesDeliveredHumanComments(t *testing.T) {
	e := newGitHubEnv(t)
	fk := e.fake
	n := fk.OpenIssue("Header", "", ghAlice)
	e.reconcile()
	e.route()
	human := fk.AddComment(n, ghAlice, "please")
	bot := fk.AddComment(n, githubtest.User{Login: "ci[bot]", Type: "Bot"}, "beep")
	e.deliver("issue_comment", "d-1", fk.CommentHook("created", human))
	e.deliver("issue_comment", "d-2", fk.CommentHook("created", bot))
	e.route()
	res := e.converge()
	if res.Reacted != 1 || fk.Reactions(human)["+1"] != 1 || len(fk.Reactions(bot)) != 0 {
		t.Fatalf("acknowledgements: %+v human %v bot %v", res, fk.Reactions(human), fk.Reactions(bot))
	}
	// Reactions are idempotent; a replayed cursor repeats nothing visible.
	if res := e.converge(); res.Reacted != 0 {
		t.Fatalf("reacted again: %+v", res)
	}
	// Unrouted human comments are never skipped by the acknowledgement cursor.
	late := fk.AddComment(n, ghAlice, "and another")
	e.deliver("issue_comment", "d-3", fk.CommentHook("created", late))
	e.converge()
	if len(fk.Reactions(late)) != 0 {
		t.Fatal("acknowledged before routing")
	}
	e.route()
	e.converge()
	if fk.Reactions(late)["+1"] != 1 {
		t.Fatal("routed comment not acknowledged after the cursor waited")
	}
}

func TestGitHubIngressSurvivesRestartMidGeneration(t *testing.T) {
	e := newGitHubEnv(t)
	for i := 0; i < 150; i++ {
		e.fake.OpenIssue("issue "+strconv.Itoa(i), "", ghAlice)
	}
	// One page, then a restart: the generation resumes from its staged cursor.
	if res, err := e.ingress.Reconcile(context.Background(), ghSource); err != nil || res.Complete {
		t.Fatalf("first page: %+v %v", res, err)
	}
	e.reopen()
	res := e.reconcile()
	if len(res.Accepted) != 150 {
		t.Fatalf("accepted %d issue observations across the restart, want 150", len(res.Accepted))
	}
	if n := e.countType("core.ingress.absent"); n != 0 {
		t.Fatalf("absence after resumed generation: %d", n)
	}
}
