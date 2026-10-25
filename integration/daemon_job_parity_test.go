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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
	sc "github.com/alsiola/open-conduit/extensions/schedule"
)

const parityNS = "schedule/jobs"

// jobParityConfig ports v1's job fixtures onto bindings of one scope:
//
//   - slow (webhook): waits for a gate and appends its delivery body (v1
//     TestJobDeliveryWhileRunningRunsAgain), through the legacy-job wrapper;
//   - check: alternates exit codes (v1 TestJobRunsAreKept);
//   - hold, behind: two jobs in the shared main checkout (v1
//     TestJobCatchesUpTheMainCheckout);
//   - env: a secret and a plain variable (v1 TestJobEnvResolvesSecrets);
//   - sleepy: the shell's process deadline (v1 TestJobTimesOut);
//   - stuck: the binding's timeout_seconds, the runtime deadline every
//     executor shares.
func jobParityConfig(root, checkout string) []byte {
	shell := func(name, command string) map[string]any {
		return map[string]any{"name": name, "executor": "shell", "requires": map[string]any{}, "operations": []string{}, "secret_names": []any{},
			"config": map[string]any{"command": command, "env": map[string]string{"DIR": root}}}
	}
	legacy := func(name, command string) map[string]any {
		return map[string]any{"name": name, "executor": sc.LegacyExecutorName, "requires": map[string]any{}, "operations": []string{}, "secret_names": []any{},
			"config": map[string]any{"shell": map[string]any{"command": command, "env": map[string]string{"DIR": root}}}}
	}
	slow := legacy("slow-cmd", `while [ ! -f "$DIR/gate" ]; do sleep 0.02; done; cat "$OPEN_SUPERVISOR_EVENT_FILE" >> "$DIR/slow"`)
	check := shell("check-cmd", `n=$(cat "$DIR/check.n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$DIR/check.n"; code=0; [ $n -eq 2 ] && code=3; echo "run $code"; exit $code`)
	shared := map[string]any{"provider": "git", "config": map[string]any{"checkout": checkout, "mode": "shared"}}
	hold := shell("hold-cmd", `echo $$ > "$DIR/hold.pid"; while [ ! -f "$DIR/hold.release" ]; do sleep 0.02; done`)
	hold["resource"], hold["requires"] = shared, map[string]any{"workspace": true}
	behind := shell("behind-cmd", `cat a.txt`)
	behind["resource"], behind["requires"] = shared, map[string]any{"workspace": true}
	env := legacy("env-cmd", `echo "$POST_TOKEN $PLAIN $OPEN_SUPERVISOR_JOB" > "$DIR/env"`)
	envShell := env["config"].(map[string]any)["shell"].(map[string]any)
	envShell["env"].(map[string]string)["PLAIN"] = "https://example.com"
	envShell["secret_env"] = map[string]string{"post-token": "POST_TOKEN"}
	env["secret_names"] = []string{"post-token"}
	sleepy := shell("sleepy-cmd", `echo $$ > "$DIR/sleepy.pid"; echo started; sleep 30 & wait`)
	sleepy["config"].(map[string]any)["timeout_seconds"] = 1
	stuck := shell("stuck-cmd", `echo $$ > "$DIR/stuck.pid"; sleep 30 & wait`)
	stuck["timeout_seconds"] = 2
	route := func(job, binding string) map[string]any {
		return map[string]any{"name": "jobs-" + job, "handler": "schedule", "binding": binding, "active_policy": "queue", "config": map[string]any{"jobs": []string{job}}}
	}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{}, "kept_executions": 2}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "jobs", "name": "Jobs",
			"authorization": map[string]any{"connections": []any{}, "operations": []string{}, "read_scopes": []any{}},
			"bindings":      []any{slow, check, hold, behind, env, sleepy, stuck},
			"routes": []any{route("slow", "slow-cmd"), route("check", "check-cmd"), route("hold", "hold-cmd"), route("behind", "behind-cmd"),
				route("env", "env-cmd"), route("sleepy", "sleepy-cmd"), route("stuck", "stuck-cmd")},
			"extensions": []any{map[string]any{"name": "jobs", "type": "schedule", "secret_names": []string{"hook"}, "config": map[string]any{"namespace": parityNS, "scope": "jobs", "webhook_secret": "hook", "jobs": []any{
				map[string]any{"name": "slow", "on": map[string]any{"webhook": true}},
				map[string]any{"name": "check"}, map[string]any{"name": "hold"}, map[string]any{"name": "behind"},
				map[string]any{"name": "env"}, map[string]any{"name": "sleepy"}, map[string]any{"name": "stuck"},
			}}}},
			"secret_refs": map[string]any{"hook": "env:OC_TEST_JOB_HOOK", "post-token": "env:OC_TEST_POST_TOKEN"},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonJobParity runs the ported v1 job fixtures on the composed
// daemon: queueing (one pending run, newest delivery), kept runs under the
// shared retention, a stale shared checkout and its catch-up, secrets and
// the legacy environment, the shell's process timeout and the binding's
// runtime deadline. There is no separate job record or scheduler: every run
// is an Execution of the job's standing Work in the unified history.
func TestDaemonJobParity(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	t.Setenv("OC_TEST_JOB_HOOK", "hook-secret")
	t.Setenv("OC_TEST_POST_TOKEN", "t0ken")
	d := newDrill(t)
	checkout := workspaceRepo(t, d.root)
	if err := os.WriteFile(d.config, jobParityConfig(d.root, checkout), 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test", "--retention-interval", "200ms"}
	d.start("")
	ctx := context.Background()
	t.Cleanup(func() {
		for _, name := range []string{"hold.pid", "sleepy.pid", "stuck.pid"} {
			if b, err := os.ReadFile(filepath.Join(d.root, name)); err == nil {
				var pid int
				if _, err := fmtSscan(strings.TrimSpace(string(b)), &pid); err == nil && pid > 1 {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
			}
		}
	})
	jobWork := func(job string) string { return sc.WorkID(parityNS, job) }
	runs := func(job string) []client.Run {
		page, err := d.c.Runs(ctx, url.Values{"work_id": {jobWork(job)}})
		if err != nil {
			return nil
		}
		return page.Items
	}
	settled := func(job string, n int) func() bool {
		return func() bool {
			rs := runs(job)
			if len(rs) != n {
				return false
			}
			for _, r := range rs {
				if r.FinishedAt == "" {
					return false
				}
			}
			return true
		}
	}
	manual := func(job, delivery string) {
		t.Helper()
		if _, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "jobs", Type: "schedule.run", DeliveryID: delivery, CorrelationKey: &client.Key{Source: parityNS, ExternalID: sc.JobKey(job)}}, "submit-"+delivery); err != nil {
			t.Fatal(err)
		}
	}
	logOf := func(r client.Run) string {
		for _, a := range r.Artifacts {
			if strings.HasPrefix(a.MediaType, "text/") {
				var b bytes.Buffer
				if err := d.c.ArtifactContent(ctx, a.ID, "", &b); err != nil {
					return "error: " + err.Error()
				}
				return b.String()
			}
		}
		return ""
	}

	// Queueing (v1 TestJobDeliveryWhileRunningRunsAgain): deliveries 2 and 3
	// while 1 runs make one pending run, for the newest delivery.
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
		return true
	})
	token := sc.Token("hook-secret", parityNS, "slow")
	post := func(body, key string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/schedule/jobs/jobs/slow/"+token, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("webhook %s: %d", key, res.StatusCode)
		}
	}
	post("1", "k1")
	d.until("the first run goes", 30*time.Second, func() bool { rs := runs("slow"); return len(rs) == 1 && rs[0].State == "running" })
	post("2", "k2")
	post("3", "k3")
	d.until("both deliveries queue", 30*time.Second, func() bool {
		page, err := d.c.Events(ctx, url.Values{"scope_id": {"jobs"}, "type": {sc.TypeWebhook}})
		if err != nil || len(page.Items) != 3 {
			return false
		}
		for _, ev := range page.Items {
			if !ev.Routed {
				return false
			}
		}
		return true
	})
	if err := os.WriteFile(filepath.Join(d.root, "gate"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.until("both runs", 30*time.Second, settled("slow", 2))
	if got := readFile(t, filepath.Join(d.root, "slow")); got != "13" {
		t.Fatalf("slow ran on %q; want deliveries 1 then 3", got)
	}
	// The delivery body is also the run's payload artifact.
	if second := runs("slow")[1]; second.Inputs != 2 || !hasMedia(second, "application/json") {
		t.Fatalf("second slow run %+v", second)
	}

	// Kept runs (v1 TestJobRunsAreKept): the shared kept_executions (2 here)
	// releases the oldest run's content and keeps every record.
	for i, k := range []string{"c1", "c2", "c3"} {
		manual("check", k)
		d.until("check run "+k, 30*time.Second, settled("check", i+1))
	}
	d.until("the oldest check run's content expires", 30*time.Second, func() bool {
		rs := runs("check")
		return len(rs) == 3 && rs[0].Retention.State == "expired" && rs[1].Retention.State == "retained" && rs[2].Retention.State == "retained"
	})
	rs := runs("check")
	if rs[1].Outcome == nil || rs[1].Outcome.Kind != "failed" || rs[1].Outcome.Summary != "exit status 3" || logOf(rs[1]) != "run 3\n" || rs[2].Outcome.Kind != "completed" {
		t.Fatalf("kept check runs: %+v / %+v log %q", rs[1].Outcome, rs[2].Outcome, logOf(rs[1]))
	}

	// A stale shared checkout (v1 TestJobCatchesUpTheMainCheckout): hold
	// catches the checkout up and keeps it; a new remote commit lands; behind
	// runs alongside and is only told how far behind it is; once hold ends,
	// behind fast-forwards.
	manual("hold", "h1")
	d.until("hold runs in the checkout", 30*time.Second, func() bool { return fileHasLine(filepath.Join(d.root, "hold.pid")) })
	other := filepath.Join(d.root, "other")
	if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "commit", "-qam", "three")
	gitCmd(t, other, "push", "-q", "origin", "HEAD:main")
	manual("behind", "b1")
	d.until("behind ran beside hold", 30*time.Second, settled("behind", 1))
	first := runs("behind")[0]
	if first.Workspace == nil || !strings.Contains(first.Workspace.Summary, "1 commit(s) behind origin/main: another execution is using the checkout") {
		t.Fatalf("stale run workspace %+v", first.Workspace)
	}
	if l := logOf(first); !strings.Contains(l, "running 1 commit(s) behind origin/main") || !strings.HasSuffix(l, "v2") {
		t.Fatalf("stale run log %q", l)
	}
	if err := os.WriteFile(filepath.Join(d.root, "hold.release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.until("hold ends", 30*time.Second, settled("hold", 1))
	// The host releases hold's shared lease on its next resource pass; until
	// then a run is still told another execution has the checkout.
	var caught client.Run
	for i := 2; caught.ID == ""; i++ {
		if i > 15 {
			t.Fatal("behind never caught up once hold's lease was released")
		}
		manual("behind", "b"+strconv.Itoa(i))
		d.until("behind ran", 30*time.Second, settled("behind", i))
		if r := runs("behind")[i-1]; r.Workspace != nil && strings.Contains(r.Workspace.Summary, "fast-forwarded") {
			caught = r
		} else {
			time.Sleep(time.Second)
		}
	}
	if !strings.Contains(caught.Workspace.Summary, "fast-forwarded 1 commit(s) to origin/main") || !strings.HasSuffix(logOf(caught), "v3") {
		t.Fatalf("caught-up run %+v log %q", caught.Workspace, logOf(caught))
	}

	// Secrets (v1 TestJobEnvResolvesSecrets): the binding's selected secret
	// and plain env, beside the legacy job name.
	manual("env", "e1")
	d.until("env ran", 30*time.Second, settled("env", 1))
	if got := strings.TrimSpace(readFile(t, filepath.Join(d.root, "env"))); got != "t0ken https://example.com env" {
		t.Fatalf("env job saw %q", got)
	}

	// Process timeout (v1 TestJobTimesOut): the shell's deadline stops the
	// whole tree.
	began := time.Now()
	manual("sleepy", "s1")
	d.until("sleepy timed out", 30*time.Second, settled("sleepy", 1))
	if took := time.Since(began); took > 20*time.Second {
		t.Fatalf("timeout took %s", took)
	}
	if r := runs("sleepy")[0]; r.Outcome == nil || r.Outcome.Kind != "failed" || r.Outcome.Summary != "shell command timed out" {
		t.Fatalf("sleepy %+v", r.Outcome)
	}
	// The binding's timeout_seconds is the runtime deadline for any executor.
	manual("stuck", "t1")
	d.until("stuck passed its deadline", 30*time.Second, settled("stuck", 1))
	stuckRun := runs("stuck")[0]
	if stuckRun.Outcome == nil || stuckRun.Outcome.Kind != "failed" {
		t.Fatalf("stuck %+v", stuckRun.Outcome)
	}
	d.until("stuck's process is stopped", 30*time.Second, func() bool {
		var pid int
		_, _ = fmtSscan(strings.TrimSpace(readFile(t, filepath.Join(d.root, "stuck.pid"))), &pid)
		return pid > 1 && syscall.Kill(pid, 0) != nil
	})

	// One record type: every job run is an Execution of its standing Work.
	page, err := d.c.Runs(ctx, url.Values{"scope_id": {"jobs"}, "limit": {"100"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Items {
		if r.WorkKind != sc.WorkKind {
			t.Fatalf("run %s of kind %s", r.ID, r.WorkKind)
		}
	}
}

func hasMedia(r client.Run, media string) bool {
	for _, a := range r.Artifacts {
		if a.MediaType == media {
			return true
		}
	}
	return false
}

func fmtSscan(s string, pid *int) (int, error) { return fmt.Sscan(s, pid) }
