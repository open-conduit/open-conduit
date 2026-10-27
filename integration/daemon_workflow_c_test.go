package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// Workflow C (05-02) on the composed daemon: scheduled jobs run as ordinary
// Executions of standing job Work through the shell executor (directly, or
// through the legacy-job compatibility wrapper) beside an agent turn on the
// same core, with one run history.

const workflowCNS = "schedule/ops"

// workflowCConfig is one scope with four jobs and an agent:
//
//   - tick: every 60 minutes; reads its context through the toolbelt (os).
//   - report: at a UTC time of day; records v1's trigger label.
//   - fresh: manual; runs in a fresh worktree at the remote head and reads
//     v1's workspace variables.
//   - refresh: manual; a long, detached v1 status job. Its timer slots queue
//     behind a run going; deliveries and manual runs while it runs drop.
//   - agent: a fake Claude Code turn on the manual route.
func workflowCConfig(root, checkout, claude, reportAt string) []byte {
	status := filepath.Join(root, "status")
	legacy := func(name, command string, extra map[string]any) map[string]any {
		cfg := map[string]any{"shell": map[string]any{"command": command, "env": map[string]string{"DIR": root}}}
		for k, v := range extra {
			cfg[k] = v
		}
		return map[string]any{"name": name, "executor": sc.LegacyExecutorName, "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"}, "secret_names": []any{}, "config": cfg}
	}
	fresh := legacy("fresh-cmd", `pwd > "$DIR/fresh.pwd"; cat a.txt > "$DIR/fresh.a"; echo "$OPEN_SUPERVISOR_BASE_BRANCH $OPEN_SUPERVISOR_WORKTREE" > "$DIR/fresh.env"`, nil)
	fresh["requires"] = map[string]any{"toolbelt": "cli", "workspace": true}
	fresh["resource"] = map[string]any{"provider": "git", "config": map[string]any{"checkout": checkout, "mode": "fresh"}}
	refresh := legacy("refresh-cmd", `echo "$OPEN_SUPERVISOR_JOB_TRIGGER" >> "$DIR/refresh"; echo $$ > "$DIR/pid"
while [ ! -e "$DIR/release" ]; do sleep 0.05; done
printf '{"slot":"x","outcome":"done","finishedAt":"2026-10-21T05:30:00Z","sha":"abc1234"}' > "$DIR/status/last.json"`, map[string]any{"status": status})
	refresh["timeout_seconds"] = 600
	ops := []string{"read_context", "declare_outcome", "wait"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{}}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "ops", "name": "Ops",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{
				map[string]any{"name": "tick-cmd", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"}, "secret_names": []any{},
					"config": map[string]any{"command": `echo "$OS_EXECUTION_ID $OS_TRIGGER_TYPE" >> "$DIR/tick"; os context --request-id ctx > "$DIR/tick.context"`, "env": map[string]string{"DIR": root}}},
				legacy("report-cmd", `echo "$OPEN_SUPERVISOR_JOB_TRIGGER" >> "$DIR/report"`, nil),
				fresh, refresh,
				map[string]any{"name": "agent", "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp"}, "operations": ops, "secret_names": []any{},
					"config": map[string]any{"binary": claude, "status_schema": "github.status.v1", "cold_turns": "resume",
						"env": map[string]string{"FAKE_DIR": root, "CLAUDE_CONFIG_DIR": filepath.Join(root, "claude-config")}}},
			},
			"routes": []any{
				map[string]any{"name": "jobs-tick", "handler": "schedule", "binding": "tick-cmd", "active_policy": "queue", "config": map[string]any{"jobs": []string{"tick"}}},
				map[string]any{"name": "jobs-report", "handler": "schedule", "binding": "report-cmd", "active_policy": "queue", "config": map[string]any{"jobs": []string{"report"}}},
				map[string]any{"name": "jobs-fresh", "handler": "schedule", "binding": "fresh-cmd", "active_policy": "queue", "config": map[string]any{"jobs": []string{"fresh"}}},
				map[string]any{"name": "jobs-refresh-slots", "handler": "schedule", "binding": "refresh-cmd", "active_policy": "queue", "config": map[string]any{"jobs": []string{"refresh"}, "triggers": []string{"timer"}}},
				map[string]any{"name": "jobs-refresh", "handler": "schedule", "binding": "refresh-cmd", "active_policy": "drop", "config": map[string]any{"jobs": []string{"refresh"}}},
				map[string]any{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "queue", "config": map[string]any{}},
			},
			"extensions": []any{map[string]any{"name": "jobs", "type": "schedule", "secret_names": []any{}, "config": map[string]any{"namespace": workflowCNS, "scope": "ops", "jobs": []any{
				map[string]any{"name": "tick", "every_minutes": 60},
				map[string]any{"name": "report", "at": []string{reportAt}},
				map[string]any{"name": "fresh"},
				map[string]any{"name": "refresh"},
			}}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// launch starts the daemon armed with a fault that may fire before the
// management API answers (a timer due at startup); killed() waits for it.
func (d *drill) launch(fault string) {
	d.t.Helper()
	d.starts++
	fmt.Fprintf(d.log, "--- start %d fault=%q\n", d.starts, fault)
	cmd := exec.Command(daemonBinary(d.t), append([]string{"--data-dir", d.state, "serve", "--config", d.config, "--poll-interval", "50ms", "--lease-duration", drillLease.String()}, d.extra...)...)
	cmd.Env = append(os.Environ(), "OPEN_CONDUIT_FAULT="+fault)
	cmd.Stdout, cmd.Stderr = d.log, d.log
	if err := cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	d.cmd, d.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) { _ = cmd.Wait(); close(exited) }(d.exited)
}

func lines(path string) []string {
	b, _ := os.ReadFile(path)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestDaemonWorkflowC demonstrates workflow C on the composed daemon:
//
//   - crash-safe recurrence: the daemon is SIGKILLed right after the
//     interval job's timer fire commits, and after a restart the job runs
//     exactly once;
//   - six-hour catch-up: an edited daily time one hour past runs its slot
//     once at apply; seven hours past is skipped;
//   - a fresh checkout at the remote head with v1's workspace variables,
//     removed after success;
//   - detached adoption: a long status job survives a daemon SIGKILL and is
//     adopted without relaunch, a manual run while it runs drops, and its
//     last.json becomes the outcome;
//   - a non-agent toolbelt call (`os context` from the shell job);
//   - an agent turn and the jobs in one run history and one run view.
func TestDaemonWorkflowC(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	d := newDrill(t)
	checkout := workspaceRepo(t, d.root)
	claude := filepath.Join(d.root, "claude")
	if err := os.WriteFile(claude, []byte(daemonClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.root, "status"), 0o700); err != nil {
		t.Fatal(err)
	}
	hm := func(offset time.Duration) string { return time.Now().UTC().Add(offset).Format("15:04") }
	if err := os.WriteFile(d.config, workflowCConfig(d.root, checkout, claude, hm(3*time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	jobWork := func(job string) client.Work {
		w, err := d.c.Work(ctx, sc.WorkID(workflowCNS, job))
		if err != nil {
			return client.Work{}
		}
		return w
	}
	manual := func(job, delivery string) {
		t.Helper()
		if _, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "schedule.run", DeliveryID: delivery, CorrelationKey: &client.Key{Source: workflowCNS, ExternalID: sc.JobKey(job)}}, "submit-"+delivery); err != nil {
			t.Fatal(err)
		}
	}
	idle := func(job string, runs int) func() bool {
		return func() bool {
			w := jobWork(job)
			if w.State != "ready" {
				return false
			}
			xs := d.executions(w.ID)
			if len(xs) != runs {
				return false
			}
			for _, x := range xs {
				if x.State == "running" || x.State == "pending" {
					return false
				}
			}
			return true
		}
	}
	generation := 1
	apply := func(reportAt string) {
		t.Helper()
		next := workflowCConfig(d.root, checkout, claude, reportAt)
		v, err := d.c.ValidateConfig(ctx, next)
		if err != nil || !v.Valid {
			t.Fatalf("validate: %+v %v", v, err)
		}
		if _, err := d.c.ApplyConfig(ctx, next, v.CandidateHash, strconv.Itoa(generation), "apply-"+strconv.Itoa(generation)); err != nil {
			t.Fatalf("apply: %v", err)
		}
		generation++
	}

	// Crash-safe recurrence: a new interval job runs at once; the daemon dies
	// right after its timer fire (and rearm) commits, before routing.
	d.launch("timer:after:" + sc.TypeSlot)
	d.killed()
	if got := lines(filepath.Join(d.root, "tick")); len(got) != 0 {
		t.Fatalf("tick ran before the crash: %v", got)
	}
	d.start("")
	d.until("tick runs once after the restart", 30*time.Second, idle("tick", 1))
	time.Sleep(500 * time.Millisecond)
	if got := lines(filepath.Join(d.root, "tick")); len(got) != 1 || !strings.HasSuffix(got[0], " "+sc.TypeSlot) {
		t.Fatalf("tick runs: %v", got)
	}
	d.until("tick's next slot is an hour out", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		return err == nil && strings.Contains(strings.Join(check(r, "schedule").Items, "\n"), "job tick: next ")
	})
	// The non-agent toolbelt call: the shell job read its own context.
	tickWork := jobWork("tick")
	if ctxOut := readFile(t, filepath.Join(d.root, "tick.context")); !json.Valid([]byte(ctxOut)) {
		t.Fatalf("os context: %s", ctxOut)
	}
	calls := d.recordsNamed("toolbelt.called", d.executions(tickWork.ID)[0].ID)
	if len(calls) != 1 || calls[0].Attributes["toolbelt.operation"] != "read_context" {
		t.Fatalf("toolbelt.called records for the shell job: %+v", calls)
	}

	// Six-hour catch-up through config edits (v1's edited-timing rule).
	apply(hm(-61 * time.Minute))
	d.until("the slot an hour past runs once", 30*time.Second, idle("report", 1))
	if got := lines(filepath.Join(d.root, "report")); len(got) != 1 || !strings.HasPrefix(got[0], "at ") || !strings.HasSuffix(got[0], " UTC") {
		t.Fatalf("report runs: %v", got)
	}
	apply(hm(-7 * time.Hour))
	time.Sleep(2 * time.Second)
	if got := lines(filepath.Join(d.root, "report")); len(got) != 1 {
		t.Fatalf("a slot seven hours past ran: %v", got)
	}

	// A fresh checkout at the remote head, with v1's workspace names.
	manual("fresh", "fresh-1")
	d.until("the fresh job ran", 60*time.Second, idle("fresh", 1))
	workspaces := filepath.Join(d.state, "resources", "git", "workspaces")
	ran := strings.TrimSpace(readFile(t, filepath.Join(d.root, "fresh.pwd")))
	if !strings.HasPrefix(ran, mustReal(t, workspaces)) || strings.TrimSpace(readFile(t, filepath.Join(d.root, "fresh.a"))) != "v2" {
		t.Fatalf("fresh run in %s read %q", ran, readFile(t, filepath.Join(d.root, "fresh.a")))
	}
	if env := strings.Fields(readFile(t, filepath.Join(d.root, "fresh.env"))); len(env) != 2 || env[0] != "main" || mustReal(t, env[1]) != ran {
		t.Fatalf("legacy workspace env %v", env)
	}
	d.until("fresh worktree removed after success", 30*time.Second, func() bool { return !exists(ran) })

	// Detached adoption: a long status job survives the daemon.
	manual("refresh", "refresh-1")
	pidFile := filepath.Join(d.root, "pid")
	d.until("refresh started", 30*time.Second, func() bool { return fileHasLine(pidFile) })
	pid, _ := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	manual("refresh", "refresh-2") // while it runs: dropped (v1 started nothing)
	d.until("the second manual run is routed", 30*time.Second, func() bool {
		page, err := d.c.Events(ctx, url.Values{"scope_id": {"ops"}, "type": {"schedule.run"}})
		if err != nil {
			return false
		}
		for _, ev := range page.Items {
			if strings.HasSuffix(ev.DeliveryID, "/refresh-2") && ev.Routed {
				return ev.Disposition == "drop"
			}
		}
		return false
	})
	d.kill()
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the detached job did not survive the daemon")
	}
	d.start("")
	time.Sleep(drillLease)
	if err := os.WriteFile(filepath.Join(d.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.until("the adopted run ends", 60*time.Second, idle("refresh", 1))
	refresh := d.executions(jobWork("refresh").ID)
	if len(refresh) != 1 || refresh[0].State != "completed" || refresh[0].Outcome == nil || !strings.Contains(refresh[0].Outcome.Summary, "status done abc1234") {
		t.Fatalf("refresh executions %+v", refresh)
	}
	if got := lines(filepath.Join(d.root, "refresh")); len(got) != 1 || got[0] != "by hand" {
		t.Fatalf("refresh launches %v", got)
	}
	if rec := d.recordsNamed("execution.recovered", refresh[0].ID); len(rec) == 0 {
		t.Fatal("the refresh run was not adopted")
	}

	// An agent turn beside the jobs.
	if _, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "ops.question", DeliveryID: "turn-1", CorrelationKey: &client.Key{Source: "ops", ExternalID: "turn-1"}}, "submit-turn-1"); err != nil {
		t.Fatal(err)
	}
	var turn client.Run
	d.until("the agent turn ends", 60*time.Second, func() bool {
		page, err := d.c.Runs(ctx, url.Values{"scope_id": {"ops"}, "executor": {"claude-code"}})
		if err != nil || len(page.Items) != 1 || page.Items[0].FinishedAt == "" {
			return false
		}
		turn = page.Items[0]
		return true
	})

	// One run history and one run view for the turn and the jobs.
	page, err := d.c.Runs(ctx, url.Values{"scope_id": {"ops"}, "limit": {"100"}})
	if err != nil {
		t.Fatal(err)
	}
	executors, kinds := map[string]int{}, map[string]int{}
	var freshRun client.Run
	for _, r := range page.Items {
		executors[r.Executor]++
		kinds[r.WorkKind]++
		if r.ExecutorBinding == "fresh-cmd" {
			freshRun = r
		}
		if row := runview.Row(r, time.Now()); !strings.Contains(row, r.ExecutorBinding) {
			t.Fatalf("row %q", row)
		}
	}
	if executors["shell"] != 1 || executors[sc.LegacyExecutorName] != 3 || executors["claude-code"] != 1 || kinds[sc.WorkKind] != 4 || kinds["manual.work"] != 1 {
		t.Fatalf("mixed history: executors %v kinds %v", executors, kinds)
	}
	if freshRun.Workspace == nil || !strings.Contains(freshRun.Workspace.Summary, "fresh worktree at origin/main") {
		t.Fatalf("fresh run workspace %+v", freshRun.Workspace)
	}
	detail := runview.Detail(freshRun, nil, time.Now())
	if !strings.Contains(detail, "workspace") || !strings.Contains(detail, "fresh worktree at origin/main") {
		t.Fatalf("fresh run detail:\n%s", detail)
	}
	if turn.WorkKind != "manual.work" || !strings.Contains(runview.Row(turn, time.Now()), "claude-code") {
		t.Fatalf("turn row %q", runview.Row(turn, time.Now()))
	}
}
