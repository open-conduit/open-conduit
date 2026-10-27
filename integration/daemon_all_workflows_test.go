package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	is "github.com/open-conduit/open-conduit/extensions/introspection"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// The stage 05 gate (05-04): workflows A–D together on one composed
// `open-conduit serve` daemon, with fakes only (GitHub and Slack fakes, a
// fake Claude Code CLI, shell and legacy-job executors calling the real `os`
// toolbelt). Three scopes share one core:
//
//   - eng (A + D): the GitHub route claims an issue for a shell agent, which
//     reports a defect through the introspection extension and parks on the
//     issue; a report-about-a-report loop runs on the user's report binding;
//     the extension's own timer starts triage, which requests fix Work; the
//     fix parks on a timeout and then resolves the reports.
//   - support (B): the Slack route starts a workspace-free conversation on
//     claude-code (fake), which replies and parks on the thread.
//   - ops (C): an interval job (shell) reads its context and parks on a
//     timeout; a manual legacy job runs beside it.
//
// It checks a SIGKILL at a timer-fire boundary at startup and an external
// SIGKILL mid-wait, webhook-off (reconcile-only) wakes, the one wake path,
// loop rejection, namespace isolation, one run history and retention model,
// operator explainability and secret redaction under concurrent activity.

const (
	gateGitHubToken = "ghp_gateSecretValue0001"
	gateSlackBot    = "xoxb-gate-secret-0002"
	gateSlackApp    = "xapp-gate-secret-0003"
	gateDepth       = 5
	gateNS          = "schedule/ops"
)

// gateAgentScript: the engineering agent (workflow A) reports a defect (the
// origin of workflow D) and parks on the issue; the human comment that wakes
// it starts the next attempt, which finishes the issue.
const gateAgentScript = `set -e
case "$OS_TRIGGER_TYPE" in
github.comment.human)
  echo "$OS_EXECUTION_ID woke $OS_TRIGGER_TYPE" >> "$W_DIR/agent"
  os outcome completed --summary fixed --schema core.turn.v1 --data '{"status":"done","comment":"Fixed after the reply."}' --request-id done >/dev/null ;;
*)
  echo "$OS_EXECUTION_ID $OS_TRIGGER_TYPE" >> "$W_DIR/agent"
  os call introspection.report-issue --request-id defect --data '{"kind":"runtime","title":"os call failed","body":"Tried os context; got 503."}' >> "$W_DIR/agent.out"
  os wait --type github.comment.human --timeout 3600 --request-id wait >/dev/null ;;
esac
`

// gateProcessorScript reports about every report until the core stops it.
const gateProcessorScript = `set -e
if os call introspection.report-issue --request-id loop --data "{\"kind\":\"other\",\"title\":\"about $OS_TRIGGER_ID\",\"body\":\"a report about a report\"}" >> "$W_DIR/loop.out" 2>> "$W_DIR/loop.err"; then
  echo looped
else
  echo refused >> "$W_DIR/refused"
fi
`

// gateTriageScript requests one fix for every open report; the fix cites the
// earliest.
const gateTriageScript = `set -e
echo "$OS_TRIGGER_TYPE" >> "$W_DIR/triage"
os call introspection.evidence --data '{"for":"triage"}' > "$W_DIR/evidence.json"
os call introspection.reports > "$W_DIR/open.json"
ids=$(grep -o '"id": *"[^"]*"' "$W_DIR/open.json" | sed 's/.*"\([^"]*\)"$/"\1"/' | paste -sd, -)
if [ -n "$ids" ]; then
  os call introspection.fix --request-id fix --data "{\"reports\":[$ids],\"title\":\"os call fails with 503\",\"body\":\"Fix the endpoint wiring.\"}" > "$W_DIR/fix.json"
fi
`

// gateFixScript parks on a timeout first (a timer wake), then resolves.
const gateFixScript = `set -e
case "$OS_TRIGGER_TYPE" in
core.wait.timeout)
  os call introspection.reports --data '{"state":"fixing"}' > "$W_DIR/fixing.json"
  for id in $(grep -o '"id": *"[^"]*"' "$W_DIR/fixing.json" | sed 's/.*"\([^"]*\)"$/\1/'); do
    os call introspection.report.resolve --data "{\"id\":\"$id\",\"summary\":\"fixed\",\"link\":\"https://example.test/pull/1\"}" >> "$W_DIR/resolved.json"
  done ;;
*)
  echo "$OS_TRIGGER_ID" > "$W_DIR/fix-trigger"
  os wait --type gate.never --timeout 2 --request-id wait >/dev/null ;;
esac
`

// gateTickScript: the interval job (workflow C) reads its context and parks
// on a timeout; the timeout's attempt completes.
const gateTickScript = `set -e
case "$OS_TRIGGER_TYPE" in
core.wait.timeout)
  echo "$OS_EXECUTION_ID woke" >> "$W_DIR/tick"
  os outcome completed --summary "woken by its timer" --request-id done >/dev/null ;;
*)
  echo "$OS_EXECUTION_ID $OS_TRIGGER_TYPE" >> "$W_DIR/tick"
  os context --request-id ctx > "$W_DIR/tick.context"
  os wait --type gate.never --timeout 2 --request-id wait >/dev/null ;;
esac
`

func gateConfig(dir, slackDir, githubURL, slackURL, claude string, triage map[string]any) []byte {
	shell := func(name, script string, ops []string) map[string]any {
		return map[string]any{"name": name, "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
			"config": map[string]any{"command": script, "env": map[string]string{"W_DIR": dir}}}
	}
	introRoute := func(name, binding, work string) map[string]any {
		return map[string]any{"name": name, "handler": is.Type, "binding": binding, "active_policy": "queue", "config": map[string]any{"work": work}}
	}
	engOps := []string{"read_context", "declare_outcome", "wait", is.OpReport, is.OpList, is.OpResolve, is.OpEvidence, is.OpNotesResolve, is.OpFix}
	supportOps := []string{"read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"}
	opsOps := []string{"read_context", "declare_outcome", "wait"}
	cfg := map[string]any{
		"version": 1,
		"settings": map[string]any{"retry_delays_seconds": []int{}, "reconcile_seconds": 3600, "kept_executions": 1,
			"causal_max_depth": gateDepth},
		"connections": []any{
			map[string]any{"id": "github", "type": "github", "config": map[string]any{"api_url": githubURL},
				"secret_refs": map[string]any{"token": "env:OC_TEST_GITHUB_TOKEN"}},
			map[string]any{"id": "slack", "type": "slack", "config": map[string]any{"api_url": slackURL},
				"secret_refs": map[string]any{"bot_token": "env:OC_TEST_SLACK_BOT_TOKEN", "app_token": "env:OC_TEST_SLACK_APP_TOKEN"}},
		},
		"scopes": []any{
			map[string]any{
				"id": "eng", "name": "Engineering",
				"authorization": map[string]any{"connections": []string{"github"}, "operations": engOps, "read_scopes": []any{}},
				"bindings": []any{
					shell("agent", gateAgentScript, []string{"read_context", "declare_outcome", "wait", is.OpReport}),
					shell("processor", gateProcessorScript, []string{is.OpReport}),
					shell("triager", gateTriageScript, []string{is.OpEvidence, is.OpList, is.OpFix}),
					shell("fixer", gateFixScript, []string{"wait", is.OpList, is.OpResolve}),
				},
				"routes": []any{
					map[string]any{"name": "issues", "handler": "github", "binding": "agent", "active_policy": "queue", "config": map[string]any{}},
					introRoute("reports", "processor", is.WorkReport),
					introRoute("triage", "triager", is.WorkTriage),
					introRoute("fixes", "fixer", is.WorkFix),
				},
				"extensions": []any{
					map[string]any{"name": "widgets", "type": "github", "connection": "github", "secret_names": []any{},
						"config": map[string]any{"namespace": "github/github.com/octo/widgets", "scope": "eng", "repo": "octo/widgets"}},
					map[string]any{"name": "introspection", "type": is.Type, "secret_names": []any{}, "config": map[string]any{"triage": triage}},
				},
				"secret_refs": map[string]any{},
			},
			map[string]any{
				"id": "support", "name": "Slack support",
				"authorization": map[string]any{"connections": []string{"slack"}, "operations": supportOps, "read_scopes": []any{}},
				"bindings": []any{map[string]any{"name": "support", "executor": "claude-code", "connection": "slack",
					"requires":   map[string]any{"toolbelt": "mcp", "inject": true, "resume": true, "workspace": false},
					"operations": supportOps, "secret_names": []any{},
					"config": map[string]any{"binary": claude, "playbook": "support-conversation", "args": "{key}", "cold_turns": "resume",
						"env": map[string]string{"FAKE_DIR": slackDir, "CLAUDE_CONFIG_DIR": filepath.Join(slackDir, "claude-config")}}}},
				"routes": []any{map[string]any{"name": "conversations", "handler": "slack", "binding": "support", "active_policy": "inject", "config": map[string]any{}}},
				"extensions": []any{map[string]any{"name": "support", "type": "slack", "connection": "slack", "secret_names": []any{},
					"config": map[string]any{"namespace": "slack/T0TEAM", "scope": "support", "team": "T0TEAM", "channels": []string{"C0SUPPORT"}, "bot_user_id": "UBOT", "receiver": "socket"}}},
				"secret_refs": map[string]any{},
			},
			map[string]any{
				"id": "ops", "name": "Ops",
				"authorization": map[string]any{"connections": []any{}, "operations": opsOps, "read_scopes": []any{}},
				"bindings": []any{
					shell("tick-cmd", gateTickScript, opsOps),
					map[string]any{"name": "nightly-cmd", "executor": sc.LegacyExecutorName, "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"}, "secret_names": []any{},
						"config": map[string]any{"shell": map[string]any{"command": `echo "$OPEN_SUPERVISOR_JOB_TRIGGER" >> "$DIR/nightly"`, "env": map[string]string{"DIR": dir}}}},
				},
				"routes": []any{
					map[string]any{"name": "jobs-tick", "handler": "schedule", "binding": "tick-cmd", "active_policy": "queue", "config": map[string]any{"jobs": []string{"tick"}}},
					map[string]any{"name": "jobs-nightly", "handler": "schedule", "binding": "nightly-cmd", "active_policy": "queue", "config": map[string]any{"jobs": []string{"nightly"}}},
				},
				"extensions": []any{map[string]any{"name": "jobs", "type": "schedule", "secret_names": []any{}, "config": map[string]any{"namespace": gateNS, "scope": "ops", "jobs": []any{
					map[string]any{"name": "tick", "every_minutes": 60},
					map[string]any{"name": "nightly"},
				}}}},
				"secret_refs": map[string]any{},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// gateEvents pages through every Event matching q.
func gateEvents(c *client.Client, q url.Values) []client.Event {
	var out []client.Event
	q.Set("limit", "200")
	for {
		page, err := c.Events(context.Background(), q)
		if err != nil {
			return out
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out
		}
		q.Set("cursor", page.NextCursor)
	}
}

func TestDaemonAllWorkflows(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	ctx := context.Background()
	gh := githubtest.New("octo/widgets", gateGitHubToken)
	defer gh.Close()
	issue := gh.OpenIssue("Header wobbles", "", githubtest.User{Login: "alice", Type: "User"}, "bug")
	sl := slacktest.New("T0TEAM", gateSlackBot)
	defer sl.Close()
	sl.AddAppToken(gateSlackApp)
	sl.SetClock(time.Now().Add(-time.Minute))
	thread := sl.Post("C0SUPPORT", "UALICE", "The VPN drops every hour")
	t.Setenv("OC_TEST_GITHUB_TOKEN", gateGitHubToken)
	t.Setenv("OC_TEST_SLACK_BOT_TOKEN", gateSlackBot)
	t.Setenv("OC_TEST_SLACK_APP_TOKEN", gateSlackApp)

	d := newDrill(t)
	d.extra = []string{"--retention-interval", "1s"}
	dir, slackDir := filepath.Join(d.root, "w"), filepath.Join(d.root, "slack")
	for _, p := range []string{dir, slackDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	claude := filepath.Join(d.root, "claude")
	if err := os.WriteFile(claude, []byte(supportClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	config := func(triage map[string]any) []byte {
		return gateConfig(dir, slackDir, gh.URL, sl.URL, claude, triage)
	}
	if err := os.WriteFile(d.config, config(map[string]any{"cron": "17 4 * * 0"}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, p := range []string{dir, slackDir} {
			files, _ := filepath.Glob(filepath.Join(p, "*"))
			for _, f := range files {
				if b, err := os.ReadFile(f); err == nil {
					t.Logf("%s:\n%s", filepath.Base(f), b)
				}
			}
		}
		if d.c != nil {
			page, _ := d.c.Works(ctx, url.Values{"limit": {"200"}})
			for _, w := range page.Items {
				t.Logf("work %s %s %s %s", w.ScopeID, w.ID, w.Kind, w.State)
			}
			if doc, err := d.c.Doctor(ctx, false); err == nil {
				for _, c := range doc.Checks {
					t.Logf("doctor %s %s %s %s %v", c.Name, c.Status, c.Code, c.Detail, c.Items)
				}
			}
			for _, r := range d.records() {
				if strings.HasPrefix(r.Msg, "ingress.") {
					t.Logf("record %s %s %v", r.At.Format("15:04:05.000"), r.Msg, r.Attributes)
				}
			}
			for _, ev := range gateEvents(d.c, url.Values{}) {
				t.Logf("event %s %s %s %s routed=%v %s -> %s", ev.Sequence, ev.ScopeID, ev.Type, ev.Source, ev.Routed, ev.Disposition, ev.RoutedWorkID)
			}
		}
	})

	works := func(scope, kind string) []client.Work {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {scope}, "limit": {"200"}})
		if err != nil {
			return nil
		}
		var out []client.Work
		for _, w := range page.Items {
			if kind == "" || w.Kind == kind {
				out = append(out, w)
			}
		}
		return out
	}
	one := func(scope, kind string) client.Work {
		if ws := works(scope, kind); len(ws) == 1 {
			return ws[0]
		}
		return client.Work{}
	}
	settled := func(w client.Work, state string, attempts int) bool {
		if w.ID == "" || w.State != state {
			return false
		}
		xs := d.executions(w.ID)
		if len(xs) != attempts {
			return false
		}
		for _, x := range xs {
			if x.State == "running" || x.State == "pending" {
				return false
			}
		}
		return true
	}
	sampleAll := func() {
		for _, w := range append(append(works("eng", ""), works("support", "")...), works("ops", "")...) {
			d.sample(w.ID)
		}
	}
	tickWork := func() client.Work {
		w, err := d.c.Work(ctx, sc.WorkID(gateNS, "tick"))
		if err != nil {
			return client.Work{}
		}
		return w
	}
	invocations := func() []string { return lines(filepath.Join(slackDir, "invocations")) }
	slackParked := func(segments int) func() bool {
		return func() bool {
			w := one("support", "slack.conversation")
			if w.State != "waiting" || len(invocations()) != segments || len(sl.AgentReplies("C0SUPPORT", thread)) != segments {
				return false
			}
			x := d.executions(w.ID)
			return len(x) == 1 && x[0].State == "parked"
		}
	}
	reportEvents := func() []client.Event {
		page, err := d.c.Events(ctx, url.Values{"scope_id": {"eng"}, "type": {is.TypeReported}, "limit": {"200"}})
		if err != nil {
			return nil
		}
		return page.Items
	}

	// 1. A SIGKILL at a timer-fire boundary during a mixed startup: the
	// daemon dies right after the interval job's slot fire (and rearm)
	// commits, while the GitHub and Slack registrations come up.
	d.launch("timer:after:" + sc.TypeSlot)
	d.killed()
	if got := lines(filepath.Join(dir, "tick")); len(got) != 0 {
		t.Fatalf("tick ran before the crash: %v", got)
	}
	d.start("")

	// 2. All four workloads progress together.
	//   A: the issue is claimed, the agent reports and parks on the issue.
	//   B: the thread starts a conversation, which replies and parks.
	//   C: the tick job runs once, parks, and its timer wakes the next attempt.
	//   D: the report loop is stopped by the core depth limit.
	d.until("A parked, B parked, C woken by its timer, D loop stopped", 90*time.Second, func() bool {
		sampleAll()
		a := one("eng", "github.issue")
		return a.State == "waiting" && slackParked(1)() && settled(tickWork(), "ready", 2) && exists(filepath.Join(dir, "refused"))
	})
	if got := lines(filepath.Join(dir, "tick")); len(got) != 2 || !strings.HasSuffix(got[0], " "+sc.TypeSlot) || !strings.HasSuffix(got[1], " woke") {
		t.Fatalf("tick runs: %v", got)
	}
	if !json.Valid([]byte(readFile(t, filepath.Join(dir, "tick.context")))) {
		t.Fatalf("os context from the job: %s", readFile(t, filepath.Join(dir, "tick.context")))
	}
	if n := len(reportEvents()); n < 2 || n > gateDepth {
		t.Fatalf("the loop produced %d reports with depth limit %d", n, gateDepth)
	}
	if out := readFile(t, filepath.Join(dir, "loop.err")); !strings.Contains(out, "causal") && !strings.Contains(out, "depth") {
		t.Fatalf("loop refusal: %s", out)
	}
	if _, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "schedule.run", DeliveryID: "nightly-1", CorrelationKey: &client.Key{Source: gateNS, ExternalID: sc.JobKey("nightly")}}, "submit-nightly-1"); err != nil {
		t.Fatal(err)
	}
	d.until("the manual legacy job ran", 30*time.Second, func() bool {
		w, err := d.c.Work(ctx, sc.WorkID(gateNS, "nightly"))
		return err == nil && settled(w, "ready", 1)
	})

	// 3. An external SIGKILL while A and B are parked; restart.
	aWork, bWork := one("eng", "github.issue"), one("support", "slack.conversation")
	d.kill()
	d.start("")
	if got := one("eng", "github.issue"); got.ID != aWork.ID || got.State != "waiting" {
		t.Fatalf("A after restart: %+v", got)
	}
	if got := one("support", "slack.conversation"); got.ID != bWork.ID || got.State != "waiting" {
		t.Fatalf("B after restart: %+v", got)
	}
	restarted := time.Now()

	// 4. Webhooks off: the human comment and the thread reply are never
	// pushed. An operator reconcile (standing in for the next poll) finds
	// them and wakes both parked Work through their Waits.
	// The fakes' clocks tick a second per write; keep both at or behind the
	// daemon's clock so the reconcile windows include the new messages.
	gh.Advance(time.Until(gh.Now()) * -1)
	gh.Advance(-10 * time.Second)
	gh.AddComment(issue, githubtest.User{Login: "alice", Type: "User"}, "It still wobbles on mobile.")
	sl.SetClock(time.Now().Add(-10 * time.Second))
	sl.Reply("C0SUPPORT", thread, "UALICE", "Still dropping after the update")
	for _, scope := range []string{"eng", "support"} {
		s, err := d.c.Scope(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		res, err := d.c.ReconcileScope(ctx, scope, s.Revision, "reconcile-"+scope)
		if err != nil || len(res.Sources) != 1 {
			t.Fatalf("reconcile %s: %+v %v", scope, res, err)
		}
	}
	d.until("A finished after the reconciled comment", 60*time.Second, func() bool {
		sampleAll()
		return settled(one("eng", "github.issue"), "completed", 2)
	})
	d.until("B resumed after the reconciled reply", 60*time.Second, slackParked(2))
	if inv := invocations(); !strings.Contains(inv[1], "resume=1") || strings.Fields(inv[0])[1] != strings.Fields(inv[1])[1] {
		t.Fatalf("B did not resume the same session: %v", inv)
	}
	d.until("A's outcome comment projected once", 30*time.Second, func() bool {
		n := 0
		for _, c := range gh.Comments(issue) {
			if strings.Contains(c.Body, "open-conduit:agent") {
				n++
			}
		}
		return n == 1 && equal(gh.Labels(issue), "agent-in-progress", "bug", "might-be-done")
	})

	// 5. Workflow D: editing triage to an hour ago catches that slot up once;
	// triage requests a fix, which parks on its timer and then resolves.
	slot := time.Now().UTC().Add(-time.Hour)
	next := config(map[string]any{"cron": fmt.Sprintf("%d %d * * *", slot.Minute(), slot.Hour())})
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if _, err = d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "apply-2"); err != nil {
		t.Fatal(err)
	}
	d.until("the fix resolved the reports", 90*time.Second, func() bool {
		sampleAll()
		return exists(filepath.Join(dir, "resolved.json")) && settled(one("eng", is.WorkKindFix), "completed", 2)
	})
	if got := lines(filepath.Join(dir, "triage")); len(got) != 1 || got[0] != is.TypeTriageDue {
		t.Fatalf("triage runs: %v", got)
	}
	fixes, err := d.c.Events(ctx, url.Values{"scope_id": {"eng"}, "type": {is.TypeFixRequested}})
	if err != nil || len(fixes.Items) != 1 {
		t.Fatalf("fix events: %+v %v", fixes, err)
	}
	chain, err := d.c.EventChain(ctx, fixes.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) < 3 || chain[1].Type != is.TypeReported || !strings.HasPrefix(chain[len(chain)-1].Type, "github.") || chain[len(chain)-1].CausationID != "" {
		t.Fatalf("the fix does not trace to the GitHub origin: %+v", chain)
	}

	// The one wake path: each workload's parked Work was woken through its
	// Wait by an Event (A, B: reconciled ingress) or a timer (C, D), and
	// each wake has the same durable and lifecycle shape.
	wakes := map[string]string{
		aWork.ID:                      "github.comment.human",
		bWork.ID:                      "slack.message.human",
		sc.WorkID(gateNS, "tick"):     "core.wait.timeout",
		one("eng", is.WorkKindFix).ID: "core.wait.timeout",
	}
	for work, typ := range wakes {
		page, err := d.c.Waits(ctx, url.Values{"work_id": {work}})
		if err != nil || len(page.Items) == 0 {
			t.Fatalf("waits of %s: %v", work, err)
		}
		woken := 0
		for _, w := range page.Items {
			if w.State != "woken" {
				continue
			}
			woken++
			ev, err := d.c.Event(ctx, w.WakeEventID)
			if err != nil || ev.Type != typ {
				t.Fatalf("Work %s woken by %+v (%v), want %s", work, ev, err, typ)
			}
			// The executor's capability alone decides what the wake does:
			// a resumable parked attempt continues (B, through the
			// core.wait.woken bridge); otherwise the waiting attempt ended
			// and the wake Event is the next attempt's trigger (A, C, D).
			xs := d.executions(work)
			resumed := false
			for _, x := range xs {
				if x.ID == w.ExecutionID && x.TriggerID != w.WakeEventID && len(xs) == 1 {
					resumed = true
				}
			}
			if resumed {
				bridge, err := d.c.Events(ctx, url.Values{"work_id": {work}, "type": {"core.wait.woken"}})
				if err != nil || len(bridge.Items) == 0 {
					t.Fatalf("no core.wait.woken Event for the resumed %s: %v", work, err)
				}
			} else {
				next := false
				for _, x := range xs {
					next = next || x.ID != w.ExecutionID && x.TriggerID == w.WakeEventID
				}
				if !next {
					t.Fatalf("Work %s: no attempt triggered by its wake %s: %+v", work, w.WakeEventID, xs)
				}
			}
			if (work == bWork.ID) != resumed {
				t.Fatalf("Work %s: resumed=%v", work, resumed)
			}
		}
		if woken != 1 {
			t.Fatalf("Work %s: %d woken Waits", work, woken)
		}
	}

	// One run history, one view and one retention model across every scope
	// and executor.
	runs, err := d.c.Runs(ctx, url.Values{"limit": {"200"}})
	if err != nil {
		t.Fatal(err)
	}
	executors, kinds, scopes := map[string]int{}, map[string]int{}, map[string]int{}
	expired := map[string]bool{}
	for _, r := range runs.Items {
		executors[r.Executor]++
		kinds[r.WorkKind]++
		scopes[r.ScopeID]++
		if row := runview.Row(r, time.Now()); !strings.Contains(row, r.ExecutorBinding) {
			t.Fatalf("row %q", row)
		}
		// Explainability from history alone: why it started, who ran it,
		// why it stopped.
		if r.Trigger == nil || r.Trigger.Type == "" || r.Executor == "" || r.ExecutorBinding == "" {
			t.Fatalf("unexplained run %+v", r)
		}
		if r.State == "completed" && r.Outcome == nil {
			t.Fatalf("completed run without an outcome %+v", r)
		}
		if r.Retention.State == "expired" {
			expired[r.WorkKind] = true
		}
	}
	for _, e := range []string{"shell", "claude-code", sc.LegacyExecutorName} {
		if executors[e] == 0 {
			t.Fatalf("history lacks %s: %v", e, executors)
		}
	}
	for _, k := range []string{"github.issue", "slack.conversation", sc.WorkKind, is.WorkKind, is.WorkKindTriage, is.WorkKindFix} {
		if kinds[k] == 0 {
			t.Fatalf("history lacks %s: %v", k, kinds)
		}
	}
	if len(scopes) != 3 {
		t.Fatalf("history scopes %v", scopes)
	}
	d.until("kept_executions=1 expires earlier attempts of every workload", 30*time.Second, func() bool {
		page, err := d.c.Runs(ctx, url.Values{"limit": {"200"}})
		if err != nil {
			return false
		}
		expired = map[string]bool{}
		for _, r := range page.Items {
			if r.Retention.State == "expired" {
				expired[r.WorkKind] = true
			}
		}
		return expired["github.issue"] && expired[sc.WorkKind] && expired[is.WorkKindFix]
	})

	// Lifecycle explainability for attempts after the last kill (telemetry
	// is at most once across a kill): started with its trigger and
	// executor, its toolbelt calls, and its stop.
	explain := func(records []lifecycle) string {
		for work, typ := range wakes {
			found := false
			for _, r := range records {
				found = found || r.Msg == "wait.woken" && r.WorkID == work && r.Attributes["wait.wake_event_type"] == typ
			}
			if !found {
				return fmt.Sprintf("no wait.woken lifecycle record for %s (%s)", work, typ)
			}
		}
		after := 0
		for _, r := range runs.Items {
			if r.StartedAt == "" {
				continue
			}
			started, _ := time.Parse(time.RFC3339Nano, r.StartedAt)
			if started.Before(restarted) || r.FinishedAt == "" {
				continue
			}
			after++
			var start, stop, emitted bool
			for _, rec := range records {
				if rec.ExecutionID != r.ID {
					continue
				}
				switch {
				case rec.Msg == "execution.started" || rec.Msg == "execution.resumed":
					start = rec.Attributes["trigger.type"] != "" && rec.Attributes["executor"] != ""
				case rec.Msg == "execution.parked" || rec.Msg == "execution."+r.State && rec.Attributes["outcome.kind"] != "":
					stop = true
				case rec.Msg == "toolbelt.called" && rec.Attributes["toolbelt.operation"] == is.OpFix:
					emitted = true
				}
			}
			// What it emitted: the triage's fix request is on its record.
			if r.ExecutorBinding == "triager" && !emitted {
				return fmt.Sprintf("triage run %s has no toolbelt.called record for %s", r.ID, is.OpFix)
			}
			if !start || !stop {
				return fmt.Sprintf("run %s (%s) lacks start/stop records (start %v stop %v)", r.ID, r.WorkKind, start, stop)
			}
		}
		if after == 0 {
			return "no attempts after the restart"
		}
		return ""
	}
	// Export is asynchronous: allow the last records to arrive.
	var problem string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if problem = explain(d.records()); problem == "" || time.Now().After(deadline) {
			break
		}
	}
	if problem != "" {
		t.Fatal(problem)
	}

	// Namespace isolation: every routed Event reached Work in its own scope,
	// and every Work's keys lie in its scope's namespaces.
	events := gateEvents(d.c, url.Values{})
	scopeOf := map[string]string{}
	for _, s := range []string{"eng", "support", "ops"} {
		for _, w := range works(s, "") {
			scopeOf[w.ID] = s
			for _, k := range w.CorrelationKeys {
				ok := false
				switch s {
				case "eng":
					ok = strings.HasPrefix(k.Source, "github/github.com/octo/widgets") || strings.HasPrefix(k.Source, "introspection")
				case "support":
					ok = strings.HasPrefix(k.Source, "slack/T0TEAM")
				case "ops":
					ok = strings.HasPrefix(k.Source, gateNS)
				}
				if !ok {
					t.Fatalf("Work %s in %s owns foreign key %+v", w.ID, s, k)
				}
			}
		}
	}
	for _, ev := range events {
		if ev.RoutedWorkID != "" && ev.ScopeID != "" && scopeOf[ev.RoutedWorkID] != "" && scopeOf[ev.RoutedWorkID] != ev.ScopeID {
			t.Fatalf("Event %s in %s routed to Work in %s", ev.ID, ev.ScopeID, scopeOf[ev.RoutedWorkID])
		}
	}

	// Redaction under concurrent activity: no secret value in any state
	// file, log, artifact or fixture output, nor in the public API.
	secrets := [][]byte{[]byte(gateGitHubToken), []byte(gateSlackBot), []byte(gateSlackApp)}
	if err := filepath.WalkDir(d.root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Type()&fs.ModeType != 0 {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, s := range secrets {
			if bytes.Contains(b, s) {
				t.Errorf("secret %q in %s", s[:8], path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	api, _ := json.Marshal([]any{runs, events, works("eng", ""), works("support", ""), works("ops", "")})
	for _, s := range secrets {
		if bytes.Contains(api, s) {
			t.Errorf("secret %q in the management API", s[:8])
		}
	}

	// Each extension's doctor check passes on the shared daemon.
	doc, err := d.c.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"github", "schedule", "introspection"} {
		if c := check(doc, name); c.Status == "fail" || c.Name == "" {
			t.Fatalf("doctor %s: %+v", name, c)
		}
	}
}
