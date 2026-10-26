package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureFiles serves the synthetic web checkout from testdata for the
// project root ~/Code/web with Home /home/op.
func fixtureFiles(t *testing.T) func(string) ([]byte, bool) {
	return func(path string) ([]byte, bool) {
		rel, ok := strings.CutPrefix(path, "/home/op/Code/web/")
		if !ok {
			return nil, false
		}
		b, err := os.ReadFile(filepath.Join("testdata", "web", rel))
		return b, err == nil
	}
}

type doc struct {
	Settings    map[string]any   `json:"settings"`
	Connections []map[string]any `json:"connections"`
	Scopes      []map[string]any `json:"scopes"`
}

func convertFixture(t *testing.T, opts Options) (Result, doc) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "v1-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Convert(src, opts)
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := json.Unmarshal(res.Config, &d); err != nil {
		t.Fatal(err)
	}
	return res, d
}

func find(list any, key, value string) map[string]any {
	for _, x := range list.([]any) {
		if m := x.(map[string]any); m[key] == value {
			return m
		}
	}
	return nil
}

func scope(d doc, id string) map[string]any {
	for _, s := range d.Scopes {
		if s["id"] == id {
			return s
		}
	}
	return nil
}

func hasItem(res Result, level, path, contains string) bool {
	for _, it := range res.Items {
		if it.Level == level && strings.HasPrefix(it.Path, path) && strings.Contains(it.Message, contains) {
			return true
		}
	}
	return false
}

func TestConvertFixture(t *testing.T) {
	res, d := convertFixture(t, Options{ReadFile: fixtureFiles(t), Home: "/home/op", TimeZone: "Europe/London"})
	if strings.Join(res.Scopes, ",") != "web,api,ops" {
		t.Fatalf("scopes: %v", res.Scopes)
	}
	all, _ := json.Marshal(res)
	if strings.Contains(string(all), "ghp_not_a_real_token_literal") {
		t.Fatal("a literal secret reached the result")
	}

	// Settings: only non-default values, retries from maxAttempts.
	if d.Settings["concurrency"] != 6.0 || d.Settings["reconcile_seconds"] != 600.0 || len(d.Settings["retry_delays_seconds"].([]any)) != 3 {
		t.Fatalf("settings: %v", d.Settings)
	}

	// Connections: claude-code and the tunnel are not connections; references map.
	ids := map[string]map[string]any{}
	for _, c := range d.Connections {
		ids[c["id"].(string)] = c
	}
	for _, id := range []string{"claude", "tun"} {
		if ids[id] != nil {
			t.Fatalf("%s became a connection", id)
		}
	}
	gh := ids["gh"]
	if gh["config"].(map[string]any)["auth"] != "app" || gh["secret_refs"].(map[string]any)["app_private_key"] != "keyring:gh-app-private-key" || gh["secret_refs"].(map[string]any)["bot_private_key"] != "file:gh-bot-private-key" {
		t.Fatalf("github connection: %v", gh)
	}
	if ids["lin"]["secret_refs"].(map[string]any)["token"] != "env:LINEAR_TOKEN" || ids["ghtok"]["secret_refs"].(map[string]any)["token"] != "keyring:ghtok-token" {
		t.Fatalf("references: %v %v", ids["lin"], ids["ghtok"])
	}
	schemes := map[string]string{}
	for _, s := range res.Secrets {
		schemes[s.From] = s.Scheme
		if s.Scheme == "keyring" && !strings.Contains(s.Action, "open-conduit secret set") {
			t.Fatalf("keyring mapping without a copy action: %+v", s)
		}
	}
	for from, scheme := range map[string]string{"connections.ghtok.token": "literal", "connections.jira.token": "cmd", "connections.gh.botPrivateKey": "file", "projects.web.env.POST_TOKEN": "keyring", "projects.web (job webhooks)": "new"} {
		if schemes[from] != scheme {
			t.Fatalf("secret %s: %q (all %v)", from, schemes[from], schemes)
		}
	}

	// The GitHub scope: agent, hold, jobs, issue route, introspection.
	web := scope(d, "web")
	agent := find(web["bindings"], "name", "agent")
	cfg := agent["config"].(map[string]any)
	if cfg["branch"] != "claude/issue-{number}" || cfg["model"] != "sonnet" || cfg["binary"] != "/home/op/bin/claude" || cfg["handoff_minutes"] != 40.0 {
		t.Fatalf("agent config: %v", cfg)
	}
	if cfg["env"].(map[string]any)["API_URL"] != "https://api.example.test" || cfg["secret_env"].(map[string]any)["env-post_token"] != "POST_TOKEN" {
		t.Fatalf("agent env: %v", cfg)
	}
	git := agent["resource"].(map[string]any)["config"].(map[string]any)
	if git["checkout"] != "/home/op/Code/web" || git["mode"] != "work" || git["dotenv"] != true || git["setup"] != "pnpm install" || git["base_branch"] != "main" {
		t.Fatalf("agent resource: %v", git)
	}
	entry := find(web["extensions"], "type", "github")["config"].(map[string]any)
	labels := entry["labels"].(map[string]any)
	if entry["namespace"] != "github/github.com/octo/web" || labels["claimed"] != "claude-in-progress" || labels["review"] != nil || entry["webhook_secret"] != "github-webhook" || entry["no_checks_minutes"] != 15.0 {
		t.Fatalf("github entry: %v", entry)
	}
	var routes []string
	for _, r := range web["routes"].([]any) {
		routes = append(routes, r.(map[string]any)["name"].(string))
	}
	if strings.Join(routes, ",") != "held,job-nightly-report,job-refresh-data-slots,job-refresh-data,job-deploy,job-poke,job-from-repo,issues,sweep,triage,fixes" {
		t.Fatalf("routes: %v", routes)
	}

	// Jobs (05-01/05-02 targets).
	jobs := find(web["extensions"], "type", "schedule")["config"].(map[string]any)
	if jobs["namespace"] != "schedule/web" || jobs["webhook_secret"] != "job-webhook" {
		t.Fatalf("schedule entry: %v", jobs)
	}
	nightly := find(jobs["jobs"], "name", "nightly-report")
	if nightly["cron"] != "0 9 * * *" || nightly["time_zone"] != "Europe/London" {
		t.Fatalf("nightly: %v", nightly)
	}
	if find(jobs["jobs"], "name", "from-repo")["every_minutes"] != 60.0 || find(jobs["jobs"], "name", "broken") != nil {
		t.Fatalf("repository file jobs: %v", jobs["jobs"])
	}
	shellOf := func(name string) (map[string]any, map[string]any) {
		b := find(web["bindings"], "name", name)
		if b["executor"] != "legacy-job" {
			t.Fatalf("%s executor %v", name, b["executor"])
		}
		return b, b["config"].(map[string]any)["shell"].(map[string]any)
	}
	if _, sh := shellOf("job-nightly-report"); sh["timeout_seconds"] != 300.0 || sh["argv"].([]any)[2] != "./scripts/report.sh" {
		t.Fatalf("nightly binding: %v", sh)
	}
	if b, sh := shellOf("job-deploy"); sh["timeout_seconds"] != 1200.0 || b["resource"].(map[string]any)["config"].(map[string]any)["mode"] != "fresh" {
		t.Fatalf("deploy binding: %v", b)
	}
	b, sh := shellOf("job-refresh-data")
	if sh["timeout_seconds"] != nil || b["timeout_seconds"] != 21600.0 || b["config"].(map[string]any)["status"] != ".status/refresh" || b["resource"].(map[string]any)["config"].(map[string]any)["mode"] != "shared" {
		t.Fatalf("status binding: %v", b)
	}
	slots := find(web["routes"], "name", "job-refresh-data-slots")
	drop := find(web["routes"], "name", "job-refresh-data")
	if slots["active_policy"] != "queue" || drop["active_policy"] != "drop" {
		t.Fatalf("status routes: %v %v", slots, drop)
	}

	// Introspection: local times in the given zone, the host watch once.
	intro := find(web["extensions"], "type", "introspection")["config"].(map[string]any)
	sweep := intro["sweep"].(map[string]any)
	if sweep["cron"] != "41 8 * * *" || sweep["time_zone"] != "Europe/London" || sweep["land"] != "pr" || intro["host_watch"] == nil || intro["telemetry"] != true {
		t.Fatalf("introspection: %v", intro)
	}
	if find(scope(d, "api")["extensions"], "type", "introspection")["config"].(map[string]any)["host_watch"] != nil {
		t.Fatal("host watch converted twice")
	}

	// A shared Linear source: one entry per scope with a route match and a Bitbucket code host.
	api := scope(d, "api")
	lin := find(api["extensions"], "type", "linear")["config"].(map[string]any)
	if lin["namespace"] != "linear/api" || len(lin["teams"].([]any)) != 2 || lin["transitions"].(map[string]any)["claimed"] != "In Progress" || lin["states"] != nil {
		t.Fatalf("linear entry: %v", lin)
	}
	issues := find(api["routes"], "name", "issues")["config"].(map[string]any)["match"].(map[string]any)
	if issues["team"].([]any)[0] != "ENG" {
		t.Fatalf("route match: %v", issues)
	}
	code := find(api["extensions"], "type", "bitbucket")["config"].(map[string]any)
	if code["tracker"].(map[string]any)["namespace"] != "linear/api" || code["preview_pattern"] != "preview" {
		t.Fatalf("code host: %v", code)
	}
	apiAgent := find(api["bindings"], "name", "agent")["config"].(map[string]any)
	if apiAgent["playbook"] != "work-tracker-issue" || apiAgent["args"] != "{key}" || apiAgent["status_schema"] != "linear.status.v1" {
		t.Fatalf("tracker agent: %v", apiAgent)
	}

	// Jira: spaces in labels become dashes; a disabled v1 project stays disabled.
	ops := scope(d, "ops")
	if ops["enabled"] != false {
		t.Fatal("disabled project converted enabled")
	}
	jira := find(ops["extensions"], "type", "jira")["config"].(map[string]any)
	if jira["labels"].(map[string]any)["help"] != "claude-needs-help" || jira["transitions"].(map[string]any)["done"] != "Done" {
		t.Fatalf("jira entry: %v", jira)
	}
	if find(scope(d, "web")["extensions"], "type", "github")["config"].(map[string]any)["labels"].(map[string]any)["help"] != "claude needs help" {
		t.Fatal("the Jira label rewrite leaked into another scope")
	}

	// The report.
	for _, want := range []struct{ level, path, text string }{
		{Unsupported, "projects.legacy", "not converted"},
		{Unsupported, "sources.linear-src.routes.api", "any/all/not"},
		{Unsupported, "sources.linear-src.unrouted", "unrouted label"},
		{Action, "settings.tunnel", "--tunnel ngrok"},
		{Action, "projects.web.jobs.poke.on.webhook", "/schedule/web/jobs/poke/<token>"},
		{Action, "projects.web .open-supervisor/playbooks/triage.md", "triage-reports"},
		{Info, "projects.web .open-supervisor/playbooks/work-issue.md", "fallback"},
		{Warn, "projects.web.jobs.deploy.keepRuns", "kept_executions"},
		{Warn, "projects.web .open-supervisor.yaml", "left out"},
		{Changed, "projects.web .open-supervisor.yaml", "never reads"},
		{Changed, "projects.web.jobs.Nightly report.timeoutMinutes", "300"},
		{Changed, "projects.web.jobs.Refresh data.status", "21600"},
		{Warn, "connections.ghtok.token", "literal"},
		{Warn, "projects.api.branch", "issue/KEY"},
	} {
		if !hasItem(res, want.level, want.path, want.text) {
			t.Errorf("report lacks %s %s %q", want.level, want.path, want.text)
		}
	}
}

func TestConvertDefaults(t *testing.T) {
	src := []byte(`
settings: {agent: claude, sweepAt: "", triageAt: ""}
connections:
  gh: {type: github}
  claude: {type: claude-code}
projects:
  - name: Widgets
    root: /srv/widgets
    repo: {connection: gh, repo: octo/widgets}
    tracker: {connection: gh, repo: octo/widgets}
    jobs:
      - {name: tick, command: [true], everyMinutes: 5}
  - name: lin
    root: /srv/lin
    repo: {connection: gh, repo: octo/lin}
    tracker: {connection: nope, team: ENG}
`)
	res, err := Convert(src, Options{ShellJobs: true})
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := json.Unmarshal(res.Config, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Settings) != 0 || len(d.Scopes) != 1 || d.Scopes[0]["id"] != "widgets" || d.Scopes[0]["name"] != "Widgets" {
		t.Fatalf("defaults: %s", res.Config)
	}
	w := d.Scopes[0]
	gh := d.Connections[0]
	if gh["secret_refs"].(map[string]any)["token"] != "cmd:gh auth token" {
		t.Fatalf("gh login: %v", gh)
	}
	entry := find(w["extensions"], "type", "github")["config"].(map[string]any)
	if entry["labels"] != nil || entry["webhook_secret"] != nil || entry["manage_webhook"] != nil {
		t.Fatalf("default entry: %v", entry)
	}
	if find(w["extensions"], "type", "introspection") != nil {
		t.Fatal("sweep and triage off, but an introspection entry was written")
	}
	tick := find(w["bindings"], "name", "job-tick")
	if tick["executor"] != "shell" || tick["config"].(map[string]any)["timeout_seconds"] != 300.0 {
		t.Fatalf("--shell-jobs binding: %v", tick)
	}
	if find(w["bindings"], "name", "agent")["config"].(map[string]any)["branch"] != "agent/issue-{number}" {
		t.Fatal("default GitHub branch")
	}
	if !hasItem(res, Unsupported, "projects.lin", "tracker connection type") || !hasItem(res, Info, "settings.tunnel", "reconcile-only") {
		t.Fatalf("report: %+v", res.Items)
	}

	off := []byte(`
settings: {jobs: false}
connections: {gh: {type: github, auth: token, token: "env:GH_TOKEN"}}
projects:
  - {name: w, root: /srv/w, repo: {connection: gh, repo: o/w}, jobs: [{name: tick, command: [true], everyMinutes: 5}]}
`)
	res, err = Convert(off, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Config), `"schedule"`) || !hasItem(res, Unsupported, "settings.jobs", "not converted") || !hasItem(res, Warn, "settings.sweepAt, settings.triageAt", "UTC") {
		t.Fatalf("jobs off: %s %+v", res.Config, res.Items)
	}
}

func TestConvertRefuses(t *testing.T) {
	for name, src := range map[string]string{
		"yaml":        "projects: [",
		"version":     "version: 2\nprojects: []",
		"no projects": "connections: {}",
	} {
		if _, err := Convert([]byte(src), Options{}); err == nil {
			t.Errorf("%s: converted", name)
		}
	}
}

func TestIDsAndTimes(t *testing.T) {
	for in, want := range map[string]string{"Nightly report": "nightly-report", "9lives": "job-9lives", "web": "web", "A.B_c": "a-b_c", "--": "job", strings.Repeat("x", 80): strings.Repeat("x", 63)} {
		if got := ID(in, "job"); got != want || !idPattern.MatchString(got) {
			t.Errorf("ID(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"08:41": "41 8 * * *", "00:05": "5 0 * * *", "23:00": "0 23 * * *"} {
		if got, ok := cronAt(in); !ok || got != want {
			t.Errorf("cronAt(%q) = %q", in, got)
		}
	}
	if _, ok := cronAt("8.41"); ok {
		t.Error("cronAt accepted a bad time")
	}
}
