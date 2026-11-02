package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
)

// scheduleDaemonConfig adds scheduled jobs to the GitHub webhook scope: an
// interval job, a push-triggered job and a webhook job, all bound to one
// shell command that records the trigger type.
func scheduleDaemonConfig(apiURL, dir string) []byte {
	var cfg map[string]any
	_ = json.Unmarshal(webhookConfig(apiURL), &cfg)
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	scope["bindings"] = append(scope["bindings"].([]any), map[string]any{"name": "job", "executor": "shell", "requires": map[string]any{}, "operations": []string{},
		"secret_names": []any{}, "config": map[string]any{"command": `echo "$OS_TRIGGER_TYPE" >> "$JOB_DIR/jobs"`, "env": map[string]string{"JOB_DIR": dir}}})
	scope["routes"] = append(scope["routes"].([]any), map[string]any{"name": "jobs", "handler": "schedule", "binding": "job", "active_policy": "queue", "config": map[string]any{}})
	scope["extensions"] = append(scope["extensions"].([]any), map[string]any{"name": "jobs", "type": "schedule", "secret_names": []string{"hook"},
		"config": map[string]any{"namespace": "schedule/eng", "scope": "eng", "webhook_secret": "hook", "jobs": []any{
			map[string]any{"name": "tick", "every_minutes": 60},
			map[string]any{"name": "deploy", "on": map[string]any{"events": []string{"push"}, "branches": []string{"main"}}},
			map[string]any{"name": "poke", "on": map[string]any{"webhook": true}},
		}}})
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonScheduledJobs drives the composed daemon (05-01): an interval
// job runs once at start on its durable timer and not again after a restart;
// a signed GitHub push delivery becomes a derived trigger for the job whose
// events and branches select it; a POST to a job's signed webhook URL runs
// it, while a wrong token is refused. Every run is a shell Execution of the
// job's standing Work.
func TestDaemonScheduledJobs(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	if err := os.WriteFile(d.config, scheduleDaemonConfig(fake.URL, d.root), 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test"}
	d.start("")
	ctx := context.Background()
	jobs := func() []string {
		b, _ := os.ReadFile(filepath.Join(d.root, "jobs"))
		return strings.Fields(string(b))
	}
	count := func(typ string) int {
		n := 0
		for _, j := range jobs() {
			if j == typ {
				n++
			}
		}
		return n
	}
	d.until("the interval job runs at start", 30*time.Second, func() bool { return count(sc.TypeSlot) == 1 })
	// The config view lists the jobs with when they run, and what the route takes.
	if cfg, err := d.c.Config(ctx); err != nil {
		t.Fatal(err)
	} else {
		var got []string
		for _, sc := range cfg.Scopes {
			for _, x := range sc.Extensions {
				for _, e := range x.Entries {
					got = append(got, e.Name+": "+e.Detail)
				}
			}
			for _, r := range sc.Routes {
				if r.Handler == "schedule" {
					got = append(got, "route: "+r.Detail)
				}
			}
		}
		if want := "tick: every hour|deploy: on push to main|poke: on its webhook|route: every job"; strings.Join(got, "|") != want {
			t.Fatalf("config entries: %q", got)
		}
	}
	var addr string
	d.until("webhook listener", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		if err != nil {
			return false
		}
		m := listenerAddr.FindStringSubmatch(check(r, "webhooks").Detail)
		if m == nil {
			return false
		}
		addr = m[1]
		items := strings.Join(check(r, "schedule").Items, "\n")
		return strings.Contains(items, "job tick: next ") && strings.Contains(items, "job deploy: events push") && strings.Contains(items, "job poke: webhook")
	})
	post := func(path string, h http.Header, body []byte) int {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
		for k, v := range h {
			req.Header[k] = v
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	push := []byte(`{"ref":"refs/heads/main","before":"` + strings.Repeat("a", 40) + `","after":"` + strings.Repeat("b", 40) + `","commits":[{"added":["a.txt"],"removed":[],"modified":[]}],"repository":{"full_name":"octo/widgets"}}`)
	if code := post("/github/eng/widgets", githubtest.Sign("hook-secret", "push", "guid-push", push), push); code != http.StatusAccepted {
		t.Fatalf("push delivery: %d", code)
	}
	d.until("the push job runs", 30*time.Second, func() bool { return count(sc.TypeEvent) == 1 })
	other := []byte(strings.Replace(string(push), "refs/heads/main", "refs/heads/feature", 1))
	if code := post("/github/eng/widgets", githubtest.Sign("hook-secret", "push", "guid-feature", other), other); code != http.StatusAccepted {
		t.Fatalf("feature push: %d", code)
	}
	if code := post("/schedule/eng/jobs/poke/wrong", nil, []byte(`{}`)); code != http.StatusNotFound {
		t.Fatalf("wrong token: %d", code)
	}
	token := sc.Token("hook-secret", "schedule/eng", "poke")
	if code := post("/schedule/eng/jobs/poke/"+token, http.Header{"Content-Type": {"application/json"}}, []byte(`{"hi":1}`)); code != http.StatusAccepted {
		t.Fatalf("job webhook: %d", code)
	}
	d.until("the webhook job runs", 30*time.Second, func() bool { return count(sc.TypeWebhook) == 1 })
	r, err := d.c.Doctor(ctx, true)
	if err != nil || !strings.Contains(strings.Join(check(r, "schedule_webhooks").Items, " "), "/schedule/eng/jobs/poke/"+token) {
		t.Fatalf("doctor --check-secrets: %+v %v", check(r, "schedule_webhooks"), err)
	}
	page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}, "kind": {sc.WorkKind}})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("job works: %+v %v", page.Items, err)
	}
	d.until("every job Work idles ready", 30*time.Second, func() bool {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}, "kind": {sc.WorkKind}})
		if err != nil {
			return false
		}
		for _, w := range page.Items {
			if w.State != "ready" {
				return false
			}
		}
		return true
	})
	// Restart: the interval job's timer is durable, so it does not run again
	// until its next slot; the feature-branch push ran nothing.
	d.kill()
	d.start("")
	d.until("doctor after restart", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		return err == nil && strings.Contains(strings.Join(check(r, "schedule").Items, "\n"), "job tick: next ")
	})
	d.settle()
	if got := jobs(); len(got) != 3 || count(sc.TypeSlot) != 1 || count(sc.TypeEvent) != 1 {
		t.Fatalf("runs after restart: %v", got)
	}
	// Reload through config apply: tick is removed (its timer goes) and a
	// cron job is added (its timer waits for the next slot).
	var cfg map[string]any
	_ = json.Unmarshal(scheduleDaemonConfig(fake.URL, d.root), &cfg)
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	for _, x := range scope["extensions"].([]any) {
		if xm := x.(map[string]any); xm["type"] == "schedule" {
			xc := xm["config"].(map[string]any)
			xc["jobs"] = []any{map[string]any{"name": "nightly", "cron": "0 3 * * *", "time_zone": "Europe/London"}, map[string]any{"name": "poke", "on": map[string]any{"webhook": true}}}
		}
	}
	next, _ := json.Marshal(cfg)
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if applied, err := d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "apply-schedule"); err != nil || applied.Generation != "2" {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	d.until("reloaded jobs", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		items := strings.Join(check(r, "schedule").Items, "\n")
		return err == nil && strings.Contains(items, "job nightly: next ") && !strings.Contains(items, "job tick")
	})
}

// TestDaemonJobConcurrency (#13): jobs have their own limit,
// settings.job_concurrency. With it at one and a job running, a second due
// job waits, and doctor and the Work's detail say which limit holds it back;
// it starts once the first finishes.
func TestDaemonJobConcurrency(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	var cfg map[string]any
	_ = json.Unmarshal(scheduleDaemonConfig(fake.URL, d.root), &cfg)
	cfg["settings"] = map[string]any{"concurrency": 1, "job_concurrency": 1}
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	for _, b := range scope["bindings"].([]any) {
		if bm := b.(map[string]any); bm["name"] == "job" {
			bm["config"].(map[string]any)["command"] = `echo "$OS_TRIGGER_TYPE" >> "$JOB_DIR/jobs"; while [ ! -f "$JOB_DIR/release" ]; do sleep 0.1; done`
		}
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(d.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test"}
	d.start("")
	ctx := context.Background()
	jobs := func() string { b, _ := os.ReadFile(filepath.Join(d.root, "jobs")); return string(b) }
	d.until("the interval job runs and blocks", 30*time.Second, func() bool { return strings.Contains(jobs(), sc.TypeSlot) })
	var addr string
	d.until("webhook listener", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		if m := listenerAddr.FindStringSubmatch(check(r, "webhooks").Detail); err == nil && m != nil {
			addr = m[1]
		}
		return addr != ""
	})
	token := sc.Token("hook-secret", "schedule/eng", "poke")
	res, err := http.Post("http://"+addr+"/schedule/eng/jobs/poke/"+token, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var waiting string
	d.until("doctor names the job limit", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		c := check(r, "capacity")
		if err != nil || c.Code != "waiting" || len(c.Items) != 1 || !strings.Contains(c.Detail, "jobs (settings.job_concurrency) 1/1 running") ||
			!strings.Contains(c.Detail, "agent turns (settings.concurrency) 0/1 running") || !strings.Contains(c.Items[0], "waiting for jobs (settings.job_concurrency) 1/1 running") {
			return false
		}
		waiting, _, _ = strings.Cut(c.Items[0], ":")
		return true
	})
	// GET /v1/capacity reports the same accounting as doctor: the job
	// holding the one job slot, the poked job waiting for it, agents idle.
	pools := func() map[string]client.CapacityPool {
		c, err := d.c.Capacity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]client.CapacityPool{}
		for _, p := range c.Pools {
			out[p.Name] = p
		}
		return out
	}
	if p := pools(); len(p) != 2 || p["agents"] != (client.CapacityPool{Name: "agents", Setting: "settings.concurrency", Limit: 1}) ||
		p["jobs"] != (client.CapacityPool{Name: "jobs", Setting: "settings.job_concurrency", Limit: 1, InUse: 1, Waiting: 1}) {
		t.Fatalf("capacity while a job waits: %+v", p)
	}
	// Doctor's capacity check reads the same accounting: each pool's usage
	// as the endpoint reports it, and the one Work waiting.
	label := map[string]string{"agents": "agent turns (settings.concurrency)", "jobs": "jobs (settings.job_concurrency)"}
	r, err := d.c.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	waitingTotal := 0
	for name, p := range pools() {
		if want := fmt.Sprintf("%s %d/%d running", label[name], p.InUse, p.Limit); !strings.Contains(check(r, "capacity").Detail, want) {
			t.Fatalf("doctor %q lacks %q", check(r, "capacity").Detail, want)
		}
		waitingTotal += p.Waiting
	}
	if n := len(check(r, "capacity").Items); n != waitingTotal {
		t.Fatalf("doctor lists %d waiting, capacity %d", n, waitingTotal)
	}
	w, err := d.c.Work(ctx, waiting)
	if err != nil || len(w.Annotations) == 0 || w.Annotations[0].Name != "waiting" || !strings.Contains(w.Annotations[0].Value, "settings.job_concurrency") {
		t.Fatalf("waiting Work detail: %+v %v", w, err)
	}
	if err := os.WriteFile(filepath.Join(d.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.until("the waiting job runs", 30*time.Second, func() bool { return strings.Contains(jobs(), sc.TypeWebhook) })
	d.until("doctor clears", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		return err == nil && check(r, "capacity").Status == "ok"
	})
	d.until("capacity frees the job slot", 30*time.Second, func() bool { p := pools(); return p["jobs"].InUse == 0 && p["jobs"].Waiting == 0 })
}
