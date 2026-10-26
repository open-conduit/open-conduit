package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	bb "github.com/open-conduit/open-conduit/extensions/bitbucket"
	"github.com/open-conduit/open-conduit/extensions/bitbucket/bitbuckettest"
	"github.com/open-conduit/open-conduit/extensions/jira"
	"github.com/open-conduit/open-conduit/extensions/jira/jiratest"
	"github.com/open-conduit/open-conduit/extensions/linear"
	"github.com/open-conduit/open-conduit/extensions/linear/lineartest"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// trackerFake is one tracker service fake behind a common test surface, so
// the same runtime cases run for Linear and Jira (06-01): the core projects
// the same state for every tracker without a label or transition branch.
type trackerFake struct {
	typ, ns, scope, key1, key2, key3, other string
	entry                                   string
	service                                 func(now func() time.Time) tracker.Service
	settings                                tracker.Settings
	addIssue                                func(key, title, status string, labels ...string)
	comment                                 func(key, user, body string)
	move                                    func(key, status string)
	snapshot                                func(key string) (labels []string, status string, bodies []string)
	loseComment                             func()
	open, review, done                      string
	close                                   func()
}

func linearFake(t *testing.T, clock *ghClock) *trackerFake {
	f := lineartest.New("k", "ENG")
	f.Now = clock.Now
	entry := `{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"transitions":{"claimed":"In Progress","review":"In Review","done":"Done"},"webhook_secret":"hook"}`
	s, err := linear.ParseExtension([]byte(entry), []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := linear.ParseConnection([]byte(`{"api_url":"` + f.URL() + `"}`))
	return &trackerFake{typ: linear.Type, ns: "linear/eng", scope: "eng", key1: "ENG-1", key2: "ENG-2", key3: "ENG-3", other: "ENG-4", entry: entry, settings: s.Settings,
		open: "Todo", review: "In Review", done: "Done",
		service: func(now func() time.Time) tracker.Service {
			c, err := linear.NewClient(conn, s, func(context.Context, string) (string, error) { return "k", nil }, linear.ClientOptions{Now: now})
			if err != nil {
				t.Fatal(err)
			}
			return c
		},
		addIssue: func(key, title, status string, labels ...string) {
			n, _ := strconv.Atoi(strings.TrimPrefix(key, "ENG-"))
			f.AddIssue("ENG", n, title, status, labels...)
		},
		comment: func(key, user, body string) { f.AddComment(key, user, body) },
		move:    func(key, status string) { f.Move(key, status) },
		snapshot: func(key string) ([]string, string, []string) {
			labels, state, comments := f.Snapshot(key)
			var bodies []string
			for _, c := range comments {
				bodies = append(bodies, c.Body)
			}
			return labels, state, bodies
		},
		loseComment: func() { f.LoseCommentResponse = 1 },
		close:       f.Close}
}

func jiraFake(t *testing.T, clock *ghClock) *trackerFake {
	f := jiratest.New("me@example.com", "tok")
	f.Now = clock.Now
	entry := `{"namespace":"jira/acme/proj","scope":"proj","projects":["PROJ"],"transitions":{"claimed":"In Progress","review":"In Review","done":"Done"},"webhook_secret":"hook"}`
	s, err := jira.ParseExtension([]byte(entry), []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := jira.ParseConnection([]byte(`{"base_url":"` + f.Server.URL + `","email":"me@example.com"}`))
	return &trackerFake{typ: jira.Type, ns: "jira/acme/proj", scope: "proj", key1: "PROJ-1", key2: "PROJ-2", key3: "PROJ-3", other: "PROJ-4", entry: entry, settings: s.Settings,
		open: "To Do", review: "In Review", done: "Done",
		service: func(now func() time.Time) tracker.Service {
			c, err := jira.NewClient(conn, s, func(context.Context, string) (string, error) { return "tok", nil }, jira.ClientOptions{Now: now})
			if err != nil {
				t.Fatal(err)
			}
			return c
		},
		addIssue: func(key, title, status string, labels ...string) { f.AddIssue(key, title, status, labels...) },
		comment:  func(key, user, body string) { f.AddComment(key, user, body) },
		move:     func(key, status string) { f.Move(key, status) },
		snapshot: func(key string) ([]string, string, []string) {
			labels, state, comments := f.Snapshot(key)
			var bodies []string
			for _, c := range comments {
				bodies = append(bodies, c.Body)
			}
			return labels, state, bodies
		},
		loseComment: func() { f.LoseCommentResponse = 1 },
		close:       f.Close}
}

type trEnv struct {
	t         *testing.T
	f         *trackerFake
	clock     *ghClock
	path      string
	store     *sqlite.Store
	ingress   *runtime.Ingress
	router    *runtime.Router
	service   tracker.Service
	adapter   *tracker.Ingress
	projector *tracker.Projector
	source    string
	bb        *bitbuckettest.Fake
	bbSource  bb.Source
	bbIngress *bb.Ingress
}

func newTrackerEnv(t *testing.T, make func(*testing.T, *ghClock) *trackerFake, withBitbucket bool) *trEnv {
	t.Helper()
	clock := &ghClock{t: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	f := make(t, clock)
	t.Cleanup(f.close)
	e := &trEnv{t: t, f: f, clock: clock, path: filepath.Join(t.TempDir(), "conduit.db"), source: f.typ + "/" + f.scope + "/entry"}
	extensions := []map[string]any{{"name": "entry", "type": f.typ, "connection": f.typ, "config": json.RawMessage(f.entry), "secret_names": []string{"hook"}}}
	routes := []domain.Route{{Name: "issues", Handler: f.typ, Binding: "agent", ActivePolicy: "queue"}}
	if withBitbucket {
		e.bb = bitbuckettest.New("acme/app", "me@example.com", "tok")
		e.bb.Now = clock.Now
		t.Cleanup(e.bb.Close)
		cfg := `{"namespace":"bitbucket/acme/app","scope":"` + f.scope + `","repo":"acme/app","tracker":{"namespace":"` + f.ns + `","keys":["` + strings.Split(f.key1, "-")[0] + `"]},"issue_ref":"{key}"}`
		extensions = append(extensions, map[string]any{"name": "app", "type": "bitbucket", "connection": "bitbucket", "config": json.RawMessage(cfg), "secret_names": []string{}})
		routes = append(routes, domain.Route{Name: "prs", Handler: "bitbucket", Binding: "agent", ActivePolicy: "queue"})
		s, err := bb.ParseExtension([]byte(cfg), nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, _ := bb.ParseConnection([]byte(`{"api_url":"` + e.bb.URL() + `","username":"me@example.com"}`))
		e.bbSource = bb.Source{Settings: s, Client: bb.NewClient(conn, func(context.Context, string) (string, error) { return "tok", nil }, bb.ClientOptions{Now: clock.Now})}
	}
	e.open()
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": extensions})
	if err := e.store.Transact(context.Background(), storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: domain.ID(f.scope), Name: f.scope, Bindings: []domain.Binding{{Name: "agent", Executor: "fixture"}},
			Routes: routes, Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	e.register()
	return e
}

func (e *trEnv) open() {
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
	handlers := map[string]extension.RouteHandler{e.f.typ: tracker.Route{Type: e.f.typ}, "bitbucket": bb.Route{}}
	if e.router, err = runtime.NewRouter(store, e.clock.Now, runtime.RouterOptions{Generation: "1", Handlers: handlers, Bindings: map[string]runtime.RouteBinding{"fixture": {}}}); err != nil {
		t.Fatal(err)
	}
	e.service = e.f.service(e.clock.Now)
	e.adapter = tracker.NewIngress(e.f.settings, e.service, e.clock.Now, tracker.IngressOptions{BatchUnits: 3})
	if e.projector, err = tracker.NewProjector(tracker.ProjectorOptions{Store: store, Service: e.service, Settings: e.f.settings, Now: e.clock.Now}); err != nil {
		t.Fatal(err)
	}
	if e.bb != nil {
		e.bbSource.Outbox = bb.NewOutbox(store, e.bbSource.Settings, e.clock.Now)
		e.bbIngress = bb.NewIngress(e.bbSource.Settings, e.bbSource.Client, e.clock.Now, bb.IngressOptions{
			Generations: func(ctx context.Context, key string, at time.Time) (string, error) {
				r, err := e.service.Reopens(ctx, key)
				return tracker.GenerationAt(r, at), err
			}})
	}
}

func (e *trEnv) register() {
	if err := e.ingress.Register(context.Background(), e.source, extension.Source{Namespace: e.f.ns, ScopeID: domain.ID(e.f.scope)}, e.adapter, 15*time.Minute); err != nil {
		e.t.Fatal(err)
	}
	if e.bb != nil {
		if err := e.ingress.Register(context.Background(), "bitbucket/"+e.f.scope+"/app", extension.Source{Namespace: "bitbucket/acme/app", ScopeID: domain.ID(e.f.scope)}, e.bbIngress, 15*time.Minute); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *trEnv) reopen() {
	_ = e.store.Close()
	e.open()
	e.register()
}

func (e *trEnv) reconcile(source string) int {
	e.t.Helper()
	accepted := 0
	for i := 0; i < 200; i++ {
		res, err := e.ingress.Reconcile(context.Background(), source)
		if err != nil {
			e.t.Fatalf("reconcile %s: %v (%s)", source, err, res.ErrorCode)
		}
		accepted += len(res.Accepted)
		if res.Complete {
			return accepted
		}
	}
	e.t.Fatal("reconcile did not complete")
	return 0
}

func (e *trEnv) route() {
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

func (e *trEnv) read(fn func(storage.Tx) error) {
	e.t.Helper()
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *trEnv) workFor(external string) (domain.Work, bool) {
	var w domain.Work
	found := false
	e.read(func(tx storage.Tx) error {
		var err error
		w, err = tx.WorkByCorrelation(domain.CorrelationKey{Source: e.f.ns, ExternalID: external})
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return w, found
}

func (e *trEnv) queued(work domain.ID) []string {
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

func (e *trEnv) startAttempt(work domain.ID) domain.ID {
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
		if _, err = tx.Claim(id, "test", time.Hour); err != nil {
			return err
		}
		w, err := tx.Work(work)
		if err != nil {
			return err
		}
		w.State = domain.WorkRunning
		return tx.PutWork(w, w.Revision)
	})
	return id
}

func (e *trEnv) finishAttempt(id domain.ID, state domain.WorkState, decl *tracker.StatusDeclaration) {
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
			x.Outcome.Data = domain.Payload{Schema: tracker.StatusSchema(e.f.typ), Data: data}
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

func (e *trEnv) converge() tracker.ProjectionResult {
	e.t.Helper()
	res, err := e.projector.Converge(context.Background(), 50)
	if err != nil {
		e.t.Fatalf("projection: %v %v", err, res.Codes)
	}
	return res
}

func trackerKinds() map[string]func(*testing.T, *ghClock) *trackerFake {
	return map[string]func(*testing.T, *ghClock) *trackerFake{"linear": linearFake, "jira": jiraFake}
}

// TestTrackerReconcileClaimProjectAndRecover runs the same runtime case for
// Linear and Jira: reconcile-only claim with the opt-in/skip rules, input
// selection, label + workflow-status convergence from durable Work state
// (no projection feedback into input), a marked comment published exactly
// once across a lost answer and a restart, and close/reopen generations.
func TestTrackerReconcileClaimProjectAndRecover(t *testing.T) {
	for name, make := range trackerKinds() {
		t.Run(name, func(t *testing.T) {
			e := newTrackerEnv(t, make, false)
			f := e.f
			f.addIssue(f.key1, "Header wobbles", f.open, "bug")
			f.addIssue(f.key2, "Later", f.open, "backlog")
			f.addIssue(f.key3, "Owned by v1", f.open, "agent-in-progress")
			f.comment(f.key1, "Ada", "please make it blue")
			if e.reconcile(e.source) == 0 {
				t.Fatal("reconcile accepted nothing")
			}
			e.route()
			w, ok := e.workFor("issue/" + f.key1)
			if !ok || w.Kind != f.typ+".issue" || w.State != domain.WorkReady {
				t.Fatalf("claimable issue produced no Work: %+v", w)
			}
			for _, other := range []string{f.key2, f.key3} {
				if _, ok := e.workFor("issue/" + other); ok {
					t.Fatalf("%s (skip label / another owner's claim) was claimed", other)
				}
			}
			if got := e.queued(w.ID); len(got) != 2 || got[0] != f.typ+".issue.open" || got[1] != f.typ+".comment.human" {
				t.Fatalf("queued inputs %v", got)
			}
			if again := e.reconcile(e.source); again != 0 {
				t.Fatalf("unchanged reconcile accepted %d", again)
			}
			// Running: claimed label and the claimed workflow status.
			x := e.startAttempt(w.ID)
			e.converge()
			labels, status, _ := f.snapshot(f.key1)
			if strings.Join(labels, ",") != "agent-in-progress,bug" || status != "In Progress" {
				t.Fatalf("while running: %v %s", labels, status)
			}
			// The attempt declares review with one comment; the answer is lost.
			f.loseComment()
			e.finishAttempt(x, domain.WorkReady, &tracker.StatusDeclaration{Status: "review", Comment: "Ready: see the PR"})
			res, _ := e.projector.Converge(context.Background(), 50)
			labels, status, bodies := f.snapshot(f.key1)
			if strings.Join(labels, ",") != "agent-in-progress,bug,ready-for-review" || status != f.review {
				t.Fatalf("after review: %v %s (%v)", labels, status, res.Codes)
			}
			if len(bodies) != 2 {
				t.Fatalf("the lost answer still created the comment once: %v", bodies)
			}
			// A restart, then the mark search finds the comment: never posted twice.
			e.reopen()
			e.clock.Add(time.Minute)
			if res := e.converge(); res.Recovered != 1 || res.Posted != 0 {
				t.Fatalf("recovery %+v", res)
			}
			e.converge()
			if _, _, bodies := f.snapshot(f.key1); len(bodies) != 2 {
				t.Fatalf("comment posted twice: %v", bodies)
			}
			// Projection writes (labels, status, marked comment) never become input.
			e.reconcile(e.source)
			e.route()
			if got := e.queued(w.ID); len(got) != 0 {
				t.Fatalf("projection fed back as input: %v", got)
			}
			// A human reply is input; then the issue closes.
			f.comment(f.key1, "Ada", "merged, thanks")
			f.move(f.key1, f.done)
			e.reconcile(e.source)
			e.route()
			if got := e.queued(w.ID); len(got) != 2 || !hasAllTypes(got, f.typ+".comment.human", f.typ+".issue.closed") {
				t.Fatalf("reply and close: %v", got)
			}
			x2 := e.startAttempt(w.ID)
			e.finishAttempt(x2, domain.WorkCompleted, &tracker.StatusDeclaration{Status: "finished"})
			e.converge()
			// Reopened: a successor generation claims it although it is labelled claimed.
			e.clock.Add(time.Hour)
			f.move(f.key1, f.open)
			e.reconcile(e.source)
			e.route()
			gen := "issue/" + f.key1 + "/generation/reopen-" + strconv.FormatInt(e.clock.Now().Unix(), 10)
			succ, ok := e.workFor(gen)
			if !ok || succ.ID == w.ID {
				t.Fatalf("reopened issue made no successor Work (%s)", gen)
			}
			if old, _ := e.workFor("issue/" + f.key1); old.ID != w.ID || old.State != domain.WorkCompleted {
				t.Fatal("the terminal owner keeps its key")
			}
		})
	}
}

// TestTrackerToolsAndBitbucket drives the tools through their registrations
// with validated authority: tracker issue view/comment (durable, idempotent
// per request) and the Bitbucket PR tools, with PR state, checks and comments
// correlated to the tracker issue's Work and routed as its input.
func TestTrackerToolsAndBitbucket(t *testing.T) {
	for name, make := range trackerKinds() {
		t.Run(name, func(t *testing.T) {
			e := newTrackerEnv(t, make, true)
			f := e.f
			f.addIssue(f.key1, "Header wobbles", f.open, "bug")
			e.reconcile(e.source)
			e.route()
			w, ok := e.workFor("issue/" + f.key1)
			if !ok {
				t.Fatal("no Work")
			}
			x := e.startAttempt(w.ID)
			auth := extension.Authority{ScopeID: w.ScopeID, WorkID: w.ID, ExecutionID: x}
			tools, err := tracker.NewTools(tracker.ToolsOptions{Type: f.typ, Store: e.store, Now: e.clock.Now,
				Sources: func() []tracker.Source {
					return []tracker.Source{{Settings: e.f.settings, Service: e.service, Projector: e.projector}}
				}})
			if err != nil {
				t.Fatal(err)
			}
			call := func(reg extension.Registration, op string, input any) (map[string]any, error) {
				data, _ := json.Marshal(input)
				for _, o := range reg.Operations {
					if o.Name == op {
						out, err := o.Handler(context.Background(), auth, domain.Payload{Data: data})
						if err != nil {
							return nil, err
						}
						var m map[string]any
						_ = json.Unmarshal(out.Data, &m)
						return m, nil
					}
				}
				t.Fatalf("no operation %s", op)
				return nil, nil
			}
			reg := tools.Registration()
			ctxOut, err := call(reg, f.typ+".context", map[string]any{})
			if err != nil || ctxOut["issue"] != f.key1 || ctxOut["branch"] != "agent/issue/"+f.key1 {
				t.Fatalf("context %v %v", ctxOut, err)
			}
			view, err := call(reg, f.typ+".issue.view", map[string]any{})
			if err != nil || view["title"] != "Header wobbles" {
				t.Fatalf("view %v %v", view, err)
			}
			for i := 0; i < 2; i++ {
				out, err := call(reg, f.typ+".issue.comment", map[string]any{"body": "Working on it"})
				if err != nil || out["state"] != "posted" {
					t.Fatalf("comment %v %v", out, err)
				}
			}
			if _, _, bodies := f.snapshot(f.key1); len(bodies) != 1 {
				t.Fatalf("a retried call posts once: %v", bodies)
			}
			if _, err := call(reg, f.typ+".issue.view", map[string]any{"key": "NOPE-1"}); err == nil {
				t.Fatal("another tracker's issue was readable")
			}
			// Bitbucket: open a PR from the issue's branch.
			bt, err := bb.NewTools(bb.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []bb.Source { return []bb.Source{e.bbSource} }})
			if err != nil {
				t.Fatal(err)
			}
			breg := bt.Registration()
			created, err := call(breg, bb.OpPRCreate, map[string]any{"title": "Fix header"})
			if err != nil || created["state"] != "created" {
				t.Fatalf("pr create %v %v", created, err)
			}
			again, err := call(breg, bb.OpPRCreate, map[string]any{"title": "Fix header"})
			if err != nil || again["state"] != "exists" {
				t.Fatalf("an open PR from the branch is the answer: %v %v", again, err)
			}
			pr := e.bb.PRs[1]
			if pr.Branch != "agent/issue/"+f.key1 || !strings.Contains(pr.Description, f.key1) {
				t.Fatalf("pr %+v", pr)
			}
			// A lost comment answer is confirmed by its mark, never posted twice.
			e.bb.LoseCommentResponse = 1
			first, _ := call(breg, bb.OpPRComment, map[string]any{"number": 1, "body": "pushed a fix"})
			if first["state"] != "pending" {
				t.Fatalf("lost answer %v", first)
			}
			e.clock.Add(time.Minute)
			second, err := call(breg, bb.OpPRComment, map[string]any{"number": 1, "body": "pushed a fix"})
			if err != nil || second["state"] != "posted" || len(e.bb.PRComments(1)) != 1 {
				t.Fatalf("recovered %v %v (%d comments)", second, err, len(e.bb.PRComments(1)))
			}
			if _, err := call(breg, bb.OpPRComment, map[string]any{"number": 99, "body": "x"}); err == nil {
				t.Fatal("a PR outside the issue was commented")
			}
			e.finishAttempt(x, domain.WorkReady, nil)
			// PR state reaches the issue's Work: failed checks, a human review comment, the merge.
			e.bb.SetStatus(pr.SHA, "build", "FAILED", "https://ci/1")
			e.bb.AddComment(1, "Ada", "rename this")
			e.reconcile("bitbucket/" + f.scope + "/app")
			e.route()
			got := e.queued(w.ID)
			if len(got) != 2 || !hasAllTypes(got, bb.TypeChecksFailed, bb.TypeCommentHuman) {
				t.Fatalf("PR input %v", got)
			}
			e.bb.SetState(1, "MERGED")
			e.reconcile("bitbucket/" + f.scope + "/app")
			e.route()
			if got := e.queued(w.ID); len(got) != 3 || got[2] != bb.TypePullMerged {
				t.Fatalf("merge input %v", got)
			}
			checks, err := call(breg, bb.OpChecks, map[string]any{"number": 1})
			if err != nil || checks["state"] != "failed" {
				t.Fatalf("checks %v %v", checks, err)
			}
		})
	}
}

func hasAllTypes(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
