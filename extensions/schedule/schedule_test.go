package schedule_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
)

// TestJobValidate ports v1 config/jobs_test.go TestJobValidate (run
// settings such as checkout and status belong to bindings and 05-02).
func TestJobValidate(t *testing.T) {
	for i, c := range []struct {
		job  sc.Job
		want string
	}{
		{sc.Job{Name: "j", Cron: "0 9,13,18 * * *", TimeZone: "Europe/London"}, ""},
		{sc.Job{Name: "j", Cron: "0 9 * * FRI", On: &sc.Trigger{Events: []string{"push"}, Paths: []string{"data/*.json", "!data/posted.json"}}}, ""},
		{sc.Job{Name: "j", At: []string{"05:17"}}, ""},
		{sc.Job{Name: "j"}, ""}, // manual only
		{sc.Job{Name: "j", Cron: "0 9 * *"}, "cron is not"},
		{sc.Job{Name: "j", Cron: "0 9 * * *", TimeZone: "Mars/Olympus"}, "time_zone is not a known zone"},
		{sc.Job{Name: "j", At: []string{"09:00"}, TimeZone: "Europe/London"}, "time_zone is for cron"},
		{sc.Job{Name: "j", At: []string{"09:00"}, Cron: "0 9 * * *"}, "not both"},
		{sc.Job{Name: "j", At: []string{"9am"}}, "isn't HH:MM"},
		{sc.Job{Name: "j", EveryMinutes: 5, Cron: "0 9 * * *"}, "every_minutes or at/cron"},
		{sc.Job{Name: "j", Cron: "CRON_TZ=Europe/London 0 9 * * *"}, "with time_zone"},
		{sc.Job{Name: "j", On: &sc.Trigger{Events: []string{"push"}, Paths: []string{"!"}}}, "empty path"},
		{sc.Job{Name: "j", On: &sc.Trigger{}}, "on needs events or webhook"},
		{sc.Job{Name: "j", On: &sc.Trigger{Webhook: true, Branches: []string{"main"}}}, "add events"},
		{sc.Job{Name: "Post reels"}, "name matching"},
	} {
		errs := strings.Join(c.job.Validate(), "; ")
		if c.want == "" && errs != "" || c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("case %d: %q; want %q", i, errs, c.want)
		}
	}
}

func TestParseExtension(t *testing.T) {
	ok := `{"namespace":"schedule/demo","scope":"demo","webhook_secret":"hook","jobs":[{"name":"a","every_minutes":5},{"name":"b","on":{"webhook":true}}]}`
	if _, err := sc.ParseExtension([]byte(ok), []string{"hook"}); err != nil {
		t.Fatal(err)
	}
	for i, bad := range []string{
		`{"namespace":"schedule/demo","scope":"demo","jobs":[{"name":"b","on":{"webhook":true}}]}`,                            // webhook without secret
		`{"namespace":"schedule/demo","scope":"demo","webhook_secret":"other","jobs":[]}`,                                     // secret not selected
		`{"namespace":"core.x","scope":"demo","jobs":[]}`,                                                                     // reserved namespace
		`{"namespace":"schedule/demo","scope":"demo","jobs":[{"name":"a","every_minutes":5},{"name":"a","every_minutes":6}]}`, // duplicate
		`{"namespace":"schedule/demo","scope":"demo","jobs":[{"name":"a","command":["true"]}]}`,                               // run settings are the binding's
	} {
		if _, err := sc.ParseExtension([]byte(bad), []string{"hook"}); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := sc.ValidateRoute(json.RawMessage(`{"jobs":["a"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := sc.ValidateRoute(json.RawMessage(`{"jobs":["a"],"triggers":["timer","manual"]}`)); err != nil {
		t.Fatal(err)
	}
	if sc.ValidateRoute(json.RawMessage(`{"job":"a"}`)) == nil || sc.ValidateRoute(json.RawMessage(`{"jobs":["A b"]}`)) == nil || sc.ValidateRoute(json.RawMessage(`{"triggers":["slot"]}`)) == nil {
		t.Fatal("bad route config accepted")
	}
}

// TestJobMatches ports v1 TestJobMatches onto code-host deliveries.
func TestJobMatches(t *testing.T) {
	job := func(on sc.Trigger) sc.Job { return sc.Job{Name: "j", On: &on} }
	push := sc.Delivery{Event: "push", Ref: "refs/heads/main"}
	for i, c := range []struct {
		job  sc.Job
		d    sc.Delivery
		want bool
	}{
		{job(sc.Trigger{Events: []string{"push"}}), push, true},
		{job(sc.Trigger{Events: []string{"release"}}), push, false},
		{job(sc.Trigger{Events: []string{"release.published"}}), sc.Delivery{Event: "release", Action: "published"}, true},
		{job(sc.Trigger{Events: []string{"release.published"}}), sc.Delivery{Event: "release", Action: "created"}, false},
		{job(sc.Trigger{Events: []string{"push"}, Branches: []string{"main"}}), push, true},
		{job(sc.Trigger{Events: []string{"push"}, Branches: []string{"release/*"}}), sc.Delivery{Event: "push", Ref: "refs/heads/release/1.2"}, true},
		{job(sc.Trigger{Events: []string{"push"}, Branches: []string{"main"}}), sc.Delivery{Event: "push", Ref: "refs/tags/v1"}, false},
		{job(sc.Trigger{Webhook: true}), push, false},
		{sc.Job{Name: "j"}, push, false},
	} {
		if got := c.job.Matches(c.d); got != c.want {
			t.Errorf("case %d: matches = %v", i, got)
		}
	}
}

// TestPathsMatch ports v1 TestPathsMatch.
func TestPathsMatch(t *testing.T) {
	reels := []string{"data/reels/*.json", "!data/reels/posted.json"}
	for i, c := range []struct {
		patterns, files []string
		want            bool
	}{
		{reels, []string{"data/reels/new.json"}, true},
		{reels, []string{"data/reels/posted.json"}, false},
		{reels, []string{"data/reels/posted.json", "data/reels/b.json"}, true},
		{reels, []string{"data/reels/deep/x.json"}, false},
		{reels, []string{"README.md"}, false},
		{[]string{"src/**"}, []string{"src/a/b/c.ts"}, true},
		{[]string{"**/*.md"}, []string{"README.md"}, true},
		{[]string{"**/*.md"}, []string{"docs/x/README.md"}, true},
		{[]string{"**.json", "!**/posted.json"}, []string{"a/posted.json"}, false},
		{[]string{"data/[ab].json"}, []string{"data/b.json"}, true},
		{[]string{"data/?.json"}, []string{"data/ab.json"}, false},
		{[]string{"!a.txt"}, []string{"a.txt", "b.txt"}, false},
	} {
		if got := sc.PathsMatch(c.patterns, c.files); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

// TestJobPathsFilterPushes ports v1 TestJobPathsFilterPushes onto Matches.
func TestJobPathsFilterPushes(t *testing.T) {
	j := sc.Job{Name: "post", On: &sc.Trigger{Events: []string{"push"}, Branches: []string{"main"}, Paths: []string{"data/reels/*.json", "!data/reels/posted.json"}}}
	if j.Matches(sc.Delivery{Event: "push", Ref: "refs/heads/main", SHA: "bbb", Files: []string{"data/reels/posted.json"}}) {
		t.Error("ran on a push changing only a taken-back file")
	}
	if !j.Matches(sc.Delivery{Event: "push", Ref: "refs/heads/main", SHA: "ccc", Files: []string{"data/reels/new.json"}}) {
		t.Error("did not run on a matching push")
	}
	if !j.Matches(sc.Delivery{Event: "push", Ref: "refs/heads/main", SHA: "ddd"}) {
		t.Error("unknown changed files must run anyway (v1)")
	}
	if j.Matches(sc.Delivery{Event: "push", Ref: "refs/heads/main", SHA: strings.Repeat("0", 40)}) {
		t.Error("a deleted branch changes nothing")
	}
	// 05-02: a possibly truncated list is unknown, so it never skips wrongly.
	if !j.Matches(sc.Delivery{Event: "push", Ref: "refs/heads/main", SHA: "eee", Files: []string{"README.md"}, Truncated: true}) {
		t.Error("a truncated file list must run anyway")
	}
}

func scopeWith(entries ...string) domain.Scope {
	var xs []map[string]any
	for _, e := range entries {
		xs = append(xs, map[string]any{"name": "jobs", "type": "schedule", "config": json.RawMessage(e), "secret_names": []string{"hook"}})
	}
	meta, _ := json.Marshal(map[string]any{"extensions": xs})
	return domain.Scope{ID: "demo", Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}
}

const demoEntry = `{"namespace":"schedule/demo","scope":"demo","webhook_secret":"hook","jobs":[{"name":"report","cron":"0 9 * * *"},{"name":"deploy","on":{"webhook":true}}]}`

func trigger(typ, source, job string) domain.AcceptedEvent {
	return domain.AcceptedEvent{Event: domain.Event{ID: "e", Type: typ, Source: source, DeliveryID: "d", CorrelationKey: &domain.CorrelationKey{Source: "schedule/demo", ExternalID: sc.JobKey(job)}}}
}

func TestRouteMatch(t *testing.T) {
	ctx := context.Background()
	scope := scopeWith(demoEntry)
	route := domain.Route{Name: "jobs", Handler: "schedule"}
	for i, c := range []struct {
		event domain.AcceptedEvent
		route domain.Route
		work  *domain.Work
		match bool
	}{
		{trigger(sc.TypeSlot, "schedule/demo", "report"), route, nil, true},
		{trigger(sc.TypeWebhook, "schedule/demo", "deploy"), route, nil, true},
		{trigger("schedule.run", sc.ManualSource, "report"), route, nil, true},
		{trigger("core.manual.retry", sc.ManualSource, "report"), route, nil, false},
		{trigger(sc.TypeSlot, "schedule/other", "report"), route, nil, false},        // foreign source
		{trigger(sc.TypeSlot, "schedule/demo", "removed"), route, nil, false},        // undeclared job
		{trigger("github.issue.open", "schedule/demo", "report"), route, nil, false}, // not a trigger
		{trigger(sc.TypeSlot, "schedule/demo", "report"), domain.Route{Config: domain.Payload{Data: []byte(`{"jobs":["deploy"]}`)}}, nil, false},
		{trigger(sc.TypeSlot, "schedule/demo", "report"), domain.Route{Config: domain.Payload{Data: []byte(`{"jobs":["report"]}`)}}, nil, true},
		// 05-02: triggers narrows a route to trigger kinds (detached jobs:
		// timer slots queue, deliveries and manual runs drop).
		{trigger(sc.TypeSlot, "schedule/demo", "report"), domain.Route{Config: domain.Payload{Data: []byte(`{"triggers":["timer"]}`)}}, nil, true},
		{trigger(sc.TypeWebhook, "schedule/demo", "deploy"), domain.Route{Config: domain.Payload{Data: []byte(`{"triggers":["timer"]}`)}}, nil, false},
		{trigger("schedule.run", sc.ManualSource, "report"), domain.Route{Config: domain.Payload{Data: []byte(`{"triggers":["webhook","event","manual"]}`)}}, nil, true},
		{trigger(sc.TypeSlot, "schedule/demo", "report"), domain.Route{Config: domain.Payload{Data: []byte(`{"jobs":["report"],"triggers":["manual"]}`)}}, nil, false},
		{trigger(sc.TypeSlot, "schedule/demo", "report"), route, &domain.Work{ID: "w", Kind: sc.WorkKind}, true},
		{trigger(sc.TypeSlot, "schedule/demo", "report"), route, &domain.Work{ID: "w", Kind: "manual.work"}, false},
	} {
		d, err := sc.Route{}.Match(ctx, extension.RouteInput{Event: c.event, Scope: scope, Work: c.work, Route: c.route})
		if err != nil || d.Match != c.match {
			t.Errorf("case %d: %+v %v", i, d, err)
		}
		if d.Match && c.work == nil && (d.Kind != sc.WorkKind || d.CreateID != domain.ID(sc.WorkID("schedule/demo", "report")) && d.CreateID != domain.ID(sc.WorkID("schedule/demo", "deploy"))) {
			t.Errorf("case %d: create %+v", i, d)
		}
		// A job's Work is shown by the job's name.
		if d.Match && c.work == nil && (d.Display == nil || d.Display.Title != string(d.CreateID)[strings.LastIndex(string(d.CreateID), "/")+1:]) {
			t.Errorf("case %d: display %+v for %s", i, d.Display, d.CreateID)
		}
	}
}

// TestSelectTrigger is the one-pending-run contract (B09, v1
// TestJobDeliveryWhileRunningRunsAgain and TestJobTimerRunWaitsForTheOneGoing).
func TestSelectTrigger(t *testing.T) {
	ev := func(typ, id string) domain.Event { return domain.Event{ID: domain.ID(id), Type: typ} }
	for i, c := range []struct {
		batch []domain.Event
		want  string
	}{
		{[]domain.Event{ev(sc.TypeSlot, "cron2"), ev(sc.TypeSlot, "cron3")}, "cron2"},                                                    // a timer does not displace a pending timer
		{[]domain.Event{ev(sc.TypeWebhook, "w2"), ev(sc.TypeWebhook, "w3")}, "w3"},                                                       // the newest delivery wins
		{[]domain.Event{ev(sc.TypeSlot, "cron5"), ev(sc.TypeWebhook, "webhook"), ev(sc.TypeSlot, "cron6")}, "webhook"},                   // delivery replaces a timer; a timer does not replace a delivery
		{[]domain.Event{ev("schedule.run", "hand"), ev(sc.TypeEvent, "push"), ev(sc.TypeWebhook, "post"), ev(sc.TypeSlot, "x")}, "post"}, // newest of either delivery kind
		{[]domain.Event{ev("schedule.run", "hand")}, "hand"},
	} {
		if got := c.batch[sc.SelectTrigger(c.batch)].ID; string(got) != c.want {
			t.Errorf("case %d: %s; want %s", i, got, c.want)
		}
	}
}

func TestWebhookIngress(t *testing.T) {
	settings, err := sc.ParseExtension([]byte(demoEntry), []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	in := sc.NewIngress("schedule/demo", func() (sc.Settings, bool) { return settings, true }, func() time.Time { return now })
	fams := in.Spec().Families
	if len(fams) != 1 || fams[0].Type != sc.TypeWebhook || fams[0].ReDerivable || fams[0].Loss == "" {
		t.Fatalf("families %+v", fams)
	}
	src := extension.Source{Namespace: "schedule/demo", ScopeID: "demo"}
	snap, err := in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil || !snap.Complete || len(snap.Observations) != 0 {
		t.Fatalf("reconcile %+v %v", snap, err)
	}
	var pushed []extension.Delivery
	h := sc.WebhookHandler("schedule/demo", func(context.Context) (string, error) { return "s3cret", nil }, func() (sc.Settings, bool) { return settings, true },
		func(ctx context.Context, d extension.Delivery) error {
			if string(d.Body) == "" {
				return errors.New("empty")
			}
			pushed = append(pushed, d)
			_, err := in.Push(ctx, src, d)
			return err
		}, func() time.Time { return now })
	post := func(path string, header map[string]string) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"hi":1}`))
		r.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	token := sc.Token("s3cret", "schedule/demo", "deploy")
	if code := post("/deploy/wrong", nil); code != http.StatusNotFound {
		t.Errorf("a bad token: %d", code)
	}
	if code := post("/report/"+sc.Token("s3cret", "schedule/demo", "report"), nil); code != http.StatusNotFound {
		t.Errorf("a job without on.webhook: %d", code)
	}
	if code := post("/deploy/"+token, map[string]string{"Idempotency-Key": "abc"}); code != http.StatusAccepted {
		t.Errorf("the job's url: %d", code)
	}
	if len(pushed) != 1 || pushed[0].ID != "abc" {
		t.Fatalf("pushed %+v", pushed)
	}
	res, err := in.Push(context.Background(), src, pushed[0])
	if err != nil || len(res.Events) != 1 || len(res.Observations) != 0 {
		t.Fatalf("push %+v %v", res, err)
	}
	e := res.Events[0]
	p, _ := sc.Payload(e)
	if e.Type != sc.TypeWebhook || e.CorrelationKey.ExternalID != "job/deploy" || e.DeliveryID != "webhook/deploy/abc" || string(p.Body) != `{"hi":1}` || p.Kind != sc.KindWebhook {
		t.Fatalf("event %+v %+v", e, p)
	}
	if sc.Token("s3cret", "schedule/demo", "deploy") == sc.Token("other", "schedule/demo", "deploy") {
		t.Fatal("token ignores the secret")
	}
}

func TestJobDescribe(t *testing.T) {
	for _, c := range []struct {
		job  sc.Job
		want string
	}{
		{sc.Job{EveryMinutes: 30}, "every 30 minutes"},
		{sc.Job{EveryMinutes: 120}, "every 2 hours"},
		{sc.Job{EveryMinutes: 1440}, "every day"},
		{sc.Job{At: []string{"06:00", "18:00"}}, "daily at 06:00, 18:00 UTC"},
		{sc.Job{Cron: "0 9 * * 1-5", TimeZone: "Europe/London"}, "cron 0 9 * * 1-5 (Europe/London)"},
		{sc.Job{EveryMinutes: 60, On: &sc.Trigger{Events: []string{"push"}, Branches: []string{"main"}, Paths: []string{"docs/**"}, Webhook: true}}, "every hour; on push to main changing docs/**; on its webhook"},
		{sc.Job{}, "by hand only"},
	} {
		if got := c.job.Describe(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.job, got, c.want)
		}
	}
	if got := sc.DescribeRoute([]byte(`{"jobs":["a","b"],"triggers":["timer"]}`)); got != "a, b (timer)" {
		t.Errorf("route: %q", got)
	}
}

func TestSamplesTraceAgainstTheScope(t *testing.T) {
	scope := scopeWith(demoEntry)
	samples := sc.Route{}.Samples(context.Background(), extension.SampleInput{Scope: scope})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
	}
	d, err := sc.Route{}.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: samples[0].Event}, Scope: scope})
	if err != nil || !d.Match || d.Kind != sc.WorkKind {
		t.Fatalf("the first sample must start the job: %+v, %v", d, err)
	}
}
