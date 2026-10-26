package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// operatorCLI runs open-conduit commands in process against a drill's state
// directory, with the daemon's offline validator and a private config home.
type operatorCLI struct {
	t      *testing.T
	env    map[string]string
	cwd    string
	stdin  string
	attach func(argv []string, dir string) error
}

func newOperatorCLI(t *testing.T, d *drill) *operatorCLI {
	return &operatorCLI{t: t, cwd: d.root, env: map[string]string{
		"HOME": d.root, "XDG_CONFIG_HOME": filepath.Join(d.root, "xdg"), "OPEN_CONDUIT_DATA_DIR": d.state,
	}}
}

func validateOffline(data []byte) ([]client.Detail, error) {
	diags, err := bootstrap.ValidateConfig(data)
	out := make([]client.Detail, 0, len(diags))
	for _, d := range diags {
		out = append(out, client.Detail{Path: d.Path, Code: d.Code, Message: d.Message})
	}
	return out, err
}

func (o *operatorCLI) run(args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	env := cli.Env{Getenv: func(k string) string { return o.env[k] }, Cwd: o.cwd, Stdin: strings.NewReader(o.stdin), ValidateConfig: validateOffline, Attach: o.attach, Setups: bootstrap.Setups(), Adapters: bootstrap.Adapters,
		Migrate: bootstrap.Migrate, State: bootstrap.StateOps()}
	code := cli.RunOperator("test", "abc", args, env, &out, &errOut)
	return out.String(), errOut.String(), code
}

func (o *operatorCLI) must(v any, args ...string) string {
	o.t.Helper()
	out, errOut, code := o.run(args...)
	if code != 0 {
		o.t.Fatalf("%v: exit %d: %s", args, code, errOut)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(out), v); err != nil {
			o.t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	return out
}

func check(r client.Doctor, name string) client.Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return client.Check{}
}

// TestDaemonFreshInstallGitHubIssue is the 03-05 acceptance path: from no
// configuration, the documented CLI steps (init, secret set, config set,
// config check) configure a workspace-free GitHub scope; the daemon starts
// on it; doctor validates the credential privately and passes; the issue is
// claimed and worked by Claude Code (a fake binary); and the operator
// controls (manual claim, attach with hold/release, scope reconcile) all go
// through the management API as causal core.manual Events. Revoking the
// credential then shows up as doctor's reconcile and auth failures.
func TestDaemonFreshInstallGitHubIssue(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	fake.SetPermissions(map[string]bool{"pull": true, "push": true, "admin": false})
	alice := githubtest.User{Login: "alice", Type: "User"}
	n := fake.OpenIssue("Header wobbles", "It wobbles.", alice, "bug")
	later := fake.OpenIssue("Someday", "", alice, "backlog")

	d := newDrill(t)
	o := newOperatorCLI(t, d)
	binary := filepath.Join(d.root, "claude")
	fakeClaude := strings.Replace(daemonClaude, "#!/bin/sh\n", "#!/bin/sh\n[ \"$1\" = --version ] && { echo '9.9.9 (Claude Code)'; exit 0; }\n", 1)
	if err := os.WriteFile(binary, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. init writes a workspace-free GitHub scope with the hold route.
	out := o.must(nil, "init", "github", "--repo", "octo/widgets", "--scope", "eng", "--no-workspace", "--token", "file:github-token", "--holds")
	config := filepath.Join(d.root, "xdg", "open-conduit", "config.json")
	if !strings.Contains(out, "Saved "+config) || !strings.Contains(out, "workspace-free") {
		t.Fatalf("init: %s", out)
	}
	// 2. The credential goes in privately (stdin, 0600 file), never as text in the config.
	o.stdin = "tok\n"
	var ref map[string]string
	o.must(&ref, "secret", "set", "github-token", "--store", "file")
	if ref["reference"] != "file:github-token" {
		t.Fatalf("secret: %v", ref)
	}
	o.stdin = ""
	// 3. Local edits (validated before saving): the fake API and fake claude.
	o.must(nil, "config", "set", "/connections/github/config", `{"api_url":"`+fake.URL+`"}`)
	agent := map[string]any{"binary": binary, "status_schema": "github.status.v1", "branch": "agent/issue-{number}", "handoff_share": 0,
		"env": map[string]string{"FAKE_DIR": d.root, "CLAUDE_CONFIG_DIR": filepath.Join(d.root, "claude-config")}}
	b, _ := json.Marshal(agent)
	o.must(nil, "config", "set", "/scopes/eng/bindings/agent/config", string(b))
	if got := strings.TrimSpace(o.must(nil, "config", "check")); got != "valid" {
		t.Fatalf("config check: %s", got)
	}
	raw, _ := os.ReadFile(config)
	if bytes.Contains(raw, []byte(`"tok"`)) {
		t.Fatal("the credential reached the config file")
	}

	// 4. Start the daemon on that config.
	d.config = config
	d.start("")
	ctx := context.Background()

	// 5. Doctor: the credential resolves privately and reaches the repo;
	//    Claude Code is installed; the scope can claim and hold.
	var report client.Doctor
	d.until("doctor passes", 30*time.Second, func() bool {
		var err error
		report, err = d.c.Doctor(ctx, true)
		return err == nil && check(report, "github").Code == "ok" && strings.Contains(strings.Join(check(report, "github").Items, " "), "reconciled")
	})
	for name, code := range map[string]string{"github_auth": "verified", "claude_code": "installed", "github_capability": "ok", "webhooks": "reconcile_only", "secrets": ""} {
		c := check(report, name)
		if code != "" && c.Code != code {
			t.Fatalf("doctor %s: %+v\n%+v", name, c, report.Checks)
		}
	}
	if s := strings.Join(check(report, "github_auth").Items, " "); !strings.Contains(s, "push=true") || strings.Contains(s, "tok\"") {
		t.Fatalf("auth items: %s", s)
	}
	if blob, _ := json.Marshal(report); bytes.Contains(blob, []byte("file:github-token")) || bytes.Contains(blob, []byte(`tok"`)) {
		t.Fatalf("doctor disclosed a reference or value: %s", blob)
	}
	// 5a. Required checks (08-01): doctor names them and their source, notes
	//     rulesets the plan lacks and warns when protection is refused.
	fake.RequireChecks("main", "test")
	fake.RefuseRequiredChecks(false, true)
	if r, err := d.c.Doctor(ctx, true); err != nil || check(r, "github_auth").Code != "verified" ||
		!strings.Contains(strings.Join(check(r, "github_auth").Items, " "), "main requires test (from branch_protection); required checks were read from branch_protection only: GitHub refused rulesets") {
		t.Fatalf("required checks: %+v %v", check(r, "github_auth"), err)
	}
	fake.RefuseRequiredChecks(true, false)
	if r, err := d.c.Doctor(ctx, true); err != nil || check(r, "github_auth").Status != "warn" || check(r, "github_auth").Code != "required_checks_unreadable" {
		t.Fatalf("refused protection: %+v %v", check(r, "github_auth"), err)
	}
	fake.RefuseRequiredChecks(false, false)
	fake.RequireChecks("main")

	// 6. The issue is claimed and worked by the agent.
	workFor := func(issue int) (client.Work, bool) {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		if err != nil {
			return client.Work{}, false
		}
		for _, w := range page.Items {
			for _, k := range w.CorrelationKeys {
				if k.ExternalID == "issue/"+itoa(issue) {
					return w, true
				}
			}
		}
		return client.Work{}, false
	}
	var w client.Work
	d.until("issue worked", 60*time.Second, func() bool {
		var ok bool
		if w, ok = workFor(n); !ok {
			return false
		}
		xs := d.executions(w.ID)
		return len(xs) == 1 && xs[0].Outcome != nil && xs[0].Outcome.Kind == "handoff"
	})
	d.until("claimed and review labels", 30*time.Second, func() bool {
		return equal(fake.Labels(n), "agent-in-progress", "bug", "ready-for-review")
	})
	detail, err := d.c.Work(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	ann := map[string]string{}
	for _, a := range detail.Annotations {
		ann[a.Name] = a.Value
	}
	if ann["issue"] != "octo/widgets#"+itoa(n) || !strings.HasSuffix(ann["url"], "/octo/widgets/issues/"+itoa(n)) {
		t.Fatalf("annotations: %+v", detail.Annotations)
	}

	// 7. Attach: held while the operator is in the session, released after.
	var attached []string
	o.attach = func(argv []string, dir string) error {
		cur, err := d.c.Work(ctx, w.ID)
		if err != nil || cur.Hold == nil {
			t.Errorf("not held during attach: %+v %v", cur, err)
		}
		attached = append(argv, "dir="+dir)
		return nil
	}
	o.must(nil, "work", "attach", w.ID)
	if len(attached) < 3 || attached[0] != binary || attached[1] != "--resume" || attached[2] == "" {
		t.Fatalf("attach argv: %v", attached)
	}
	if cur, _ := d.c.Work(ctx, w.ID); cur.Hold != nil {
		t.Fatal("attach left the Work held")
	}

	// 8. Manual claim overrides the skip label.
	var claim client.Event
	o.must(&claim, "claim", "eng#"+itoa(later))
	if claim.Type != "github.claim.requested" || claim.Source != "core.manual" {
		t.Fatalf("claim event: %+v", claim)
	}
	d.until("claimed by hand", 30*time.Second, func() bool {
		lw, ok := workFor(later)
		return ok && lw.Kind == "github.issue"
	})

	// 9. Scope reconcile on demand.
	var rec client.ReconcileResult
	o.must(&rec, "scopes", "reconcile", "eng")
	if len(rec.Sources) != 1 || rec.Sources[0] != "github/eng/eng" {
		t.Fatalf("reconcile: %+v", rec)
	}
	var controls []string
	page, err := d.c.Events(ctx, url.Values{"work_id": {w.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Items {
		if strings.HasPrefix(e.Type, "core.manual.") {
			controls = append(controls, e.Type)
		}
	}
	if strings.Join(controls, ",") != "core.manual.hold,core.manual.release" {
		t.Fatalf("attach controls in the Work's history: %v", controls)
	}

	// 10. A revoked credential: doctor diagnoses reconcile and auth failure.
	fake.RevokeToken("tok")
	o.must(nil, "scopes", "reconcile", "eng")
	d.until("reconcile failure diagnosed", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		return err == nil && check(r, "github").Code == "reconcile_failing"
	})
	r, err := d.c.Doctor(ctx, true)
	if err != nil || check(r, "github_auth").Status != "fail" || check(r, "github_auth").Code != "credential_rejected" {
		t.Fatalf("auth failure: %+v %v", check(r, "github_auth"), err)
	}
}

// webhookConfig is githubDaemonConfig with a webhook secret, managed hook
// and a long reconcile interval, so a prompt claim proves the push path.
func webhookConfig(apiURL string) []byte {
	var cfg map[string]any
	_ = json.Unmarshal(githubDaemonConfig(apiURL), &cfg)
	cfg["settings"] = map[string]any{"reconcile_seconds": 3600}
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	scope["secret_refs"] = map[string]any{"hook": "env:OC_TEST_HOOK_SECRET"}
	x := scope["extensions"].([]any)[0].(map[string]any)
	x["secret_names"] = []string{"hook"}
	xc := x["config"].(map[string]any)
	xc["webhook_secret"], xc["manage_webhook"] = "hook", true
	b, _ := json.Marshal(cfg)
	return b
}

var listenerAddr = regexp.MustCompile(`listener (127\.0\.0\.1:[0-9]+)`)

// TestDaemonGitHubWebhookListener: --webhook-addr serves only the
// registered entry route on its own TCP address; a static public URL
// re-points the repository webhook (manage_webhook); a signed delivery is
// accepted (202) and claims the issue long before the next reconcile, while
// a bad signature (401), an unknown route and the management API (404) are
// refused.
func TestDaemonGitHubWebhookListener(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	if err := os.WriteFile(d.config, webhookConfig(fake.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test"}
	d.start("")
	ctx := context.Background()
	var addr string
	d.until("webhooks listening with a managed hook", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		if err != nil {
			return false
		}
		c := check(r, "webhooks")
		m := listenerAddr.FindStringSubmatch(c.Detail)
		if m == nil || c.Code != "listening" || !strings.Contains(strings.Join(c.Items, " "), "hook ok") {
			return false
		}
		addr = m[1]
		return strings.Contains(strings.Join(check(r, "github").Items, " "), "reconciled")
	})
	hooks := fake.Hooks()
	if len(hooks) != 1 || hooks[0].URL != "https://hooks.example.test/github/eng/widgets" || hooks[0].Secret != "hook-secret" {
		t.Fatalf("managed hook: %+v", hooks)
	}
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
	n := fake.OpenIssue("Pushed", "", githubtest.User{Login: "alice", Type: "User"}, "bug")
	body := fake.IssuesHook("opened", n)
	if code := post("/github/eng/widgets", githubtest.Sign("wrong", "issues", "guid-1", body), body); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}
	if code := post("/v1/health", githubtest.Sign("hook-secret", "issues", "guid-1", body), body); code != http.StatusNotFound {
		t.Fatalf("management through the listener: %d", code)
	}
	if code := post("/github/eng/other", githubtest.Sign("hook-secret", "issues", "guid-1", body), body); code != http.StatusNotFound {
		t.Fatalf("unknown route: %d", code)
	}
	if code := post("/github/eng/widgets", githubtest.Sign("hook-secret", "issues", "guid-1", body), body); code != http.StatusAccepted {
		t.Fatalf("signed delivery: %d", code)
	}
	d.until("pushed issue worked", 30*time.Second, func() bool {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		return err == nil && len(page.Items) == 1 && page.Items[0].State == "completed"
	})
}

// TestDaemonStoredWebhookSettings: a bare serve starts the listener and
// tunnel the config stores in settings.webhooks, and doctor says so; on the
// next start a tunnel flag overrides the stored tunnel while the stored
// address still applies.
func TestDaemonStoredWebhookSettings(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	var cfg map[string]any
	_ = json.Unmarshal(webhookConfig(fake.URL), &cfg)
	cfg["settings"].(map[string]any)["webhooks"] = map[string]any{"addr": "127.0.0.1:0", "public_url": "https://stored.example.test"}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(d.config, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hooked := func(what, detail, url string) {
		t.Helper()
		d.until(what, 30*time.Second, func() bool {
			r, err := d.c.Doctor(ctx, false)
			if err != nil {
				return false
			}
			c := check(r, "webhooks")
			hooks := fake.Hooks()
			return c.Code == "listening" && strings.Contains(c.Detail, detail) && len(hooks) == 1 && hooks[0].URL == url
		})
	}
	d.start("")
	hooked("stored listener and tunnel", "(from settings.webhooks); public https://stored.example.test (static, from settings.webhooks)", "https://stored.example.test/github/eng/widgets")
	d.stop()
	d.extra = []string{"--public-url", "https://flag.example.test"}
	d.start("")
	hooked("flag tunnel over the stored address", "(from settings.webhooks); public https://flag.example.test (static, from serve flags)", "https://flag.example.test/github/eng/widgets")
}

// TestDaemonGitHubDryRun: serve --dry-run makes no GitHub writes (labels,
// comments, reactions, hooks) while issues are still claimed and worked, and
// doctor says so.
func TestDaemonGitHubDryRun(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	n := fake.OpenIssue("Header wobbles", "", githubtest.User{Login: "alice", Type: "User"}, "bug")
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	if err := os.WriteFile(d.config, webhookConfig(fake.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--dry-run", "--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test"}
	d.start("")
	ctx := context.Background()
	d.until("issue worked", 30*time.Second, func() bool {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		return err == nil && len(page.Items) == 1 && page.Items[0].State == "completed"
	})
	var r client.Doctor
	d.until("dry run reported", 30*time.Second, func() bool {
		var err error
		r, err = d.c.Doctor(ctx, false)
		return err == nil && strings.Contains(strings.Join(check(r, "webhooks").Items, " "), "hook dry_run")
	})
	if !strings.Contains(strings.Join(check(r, "github").Items, " "), "dry run") {
		t.Fatalf("doctor github: %+v", check(r, "github"))
	}
	time.Sleep(6 * time.Second) // at least one projection pass after completion
	for _, w := range fake.Writes() {
		t.Errorf("dry run wrote to GitHub: %s %s", w.Method, w.Path)
	}
	if !equal(fake.Labels(n), "bug") || len(fake.Comments(n)) != 0 || len(fake.Hooks()) != 0 {
		t.Fatalf("dry run changed GitHub: labels %v comments %d hooks %d", fake.Labels(n), len(fake.Comments(n)), len(fake.Hooks()))
	}
}
