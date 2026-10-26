package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/extensions/github/githubtest"
	"github.com/alsiola/open-conduit/internal/cli"
)

// stop is a graceful SIGTERM stop (a drained switch, not a crash).
func (d *drill) stop() {
	d.t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		d.t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(30 * time.Second):
		d.t.Fatal("daemon ignored SIGTERM")
	}
	if !d.cmd.ProcessState.Success() {
		d.t.Fatalf("graceful exit: %v", d.cmd.ProcessState)
	}
}

func lineCount(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

// migrationV1Config is a synthetic Open Supervisor config for one GitHub
// project on the fake API, with a timed job whose slot v1 already ran an
// hour ago and an interval job.
func migrationV1Config(apiURL, checkout, claude, dir string, slot time.Time) string {
	return fmt.Sprintf(`version: 1
settings:
  agent: claude
  sweepAt: ""
  triageAt: ""
  handoffShare: 0
connections:
  gh: {type: github, auth: token, token: "env:OC_MIGRATION_GH_TOKEN", apiUrl: %q}
  claude: {type: claude-code, binary: %q}
projects:
  - name: Widgets
    root: %q
    repo: {connection: gh, repo: octo/widgets}
    tracker: {connection: gh, repo: octo/widgets}
    env:
      FAKE_DIR: %q
      CLAUDE_CONFIG_DIR: %q
    jobs:
      - name: Daily digest
        command: [sh, -c, 'echo "$OPEN_SUPERVISOR_JOB $OPEN_SUPERVISOR_JOB_TRIGGER" >> "$FAKE_DIR/digest"']
        at: [%q]
      - name: tick
        command: [sh, -c, 'echo "$OPEN_SUPERVISOR_JOB_TRIGGER" >> "$FAKE_DIR/tick"']
        everyMinutes: 60
`, apiURL, claude, checkout, dir, filepath.Join(dir, "claude-config"), slot.UTC().Format("15:04"))
}

// TestDaemonMigrationRehearsal is 06-02's migration and recovery rehearsal on
// copied, synthetic state (no Open Supervisor data is read or touched):
//
//   - the v1 config is converted by the CLI (preview, then written) and the
//     daemon starts on it;
//   - an issue v1 still owns (claimed label, drained but not handed over) is
//     never claimed: one owner per issue;
//   - a new issue is claimed and worked once; an issue v1 released is
//     transferred by an explicit manual claim;
//   - the converted timed job does not re-run the slot v1 already ran (its
//     first run waits for the next slot); the interval job runs once;
//   - offline operations refuse while the daemon owns the state;
//   - after a graceful stop, state check reports a drained scope, a backup is
//     created and verified, and a restore into a new state directory starts
//     with a fresh operator credential (the bundle's is revoked) and repeats
//     no external write, claim or job run;
//   - a corrupted database and a database from a newer schema are diagnosed
//     with their remedies, and a tampered bundle is refused.
func TestDaemonMigrationRehearsal(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	fake.SetPermissions(map[string]bool{"pull": true, "push": true, "admin": false})
	alice := githubtest.User{Login: "alice", Type: "User"}
	v1Owned := fake.OpenIssue("v1 is still on it", "", alice, "agent-in-progress")
	released := fake.OpenIssue("v1 finished its turn", "", alice, "agent-in-progress")
	fresh := fake.OpenIssue("New after the switch", "", alice)
	t.Setenv("OC_MIGRATION_GH_TOKEN", "tok")

	d := newDrill(t)
	o := newOperatorCLI(t, d)
	checkout := workspaceRepo(t, d.root)
	claude := filepath.Join(d.root, "claude")
	if err := os.WriteFile(claude, []byte(daemonClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	v1dir := filepath.Join(d.root, "v1")
	if err := os.MkdirAll(v1dir, 0o700); err != nil {
		t.Fatal(err)
	}
	v1 := filepath.Join(v1dir, "config.yaml")
	if err := os.WriteFile(v1, []byte(migrationV1Config(fake.URL, checkout, claude, d.root, time.Now().Add(-time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Convert: a preview writes nothing; the written config validates.
	if out := o.must(nil, "migrate", "config", "--from", v1, "--time-zone", "UTC"); !strings.Contains(out, "Validation: valid") {
		t.Fatalf("preview: %s", out)
	}
	if _, err := os.Stat(d.config); err != nil {
		t.Fatal(err) // newDrill's own config
	}
	d.config = filepath.Join(d.root, "converted.json")
	o.must(nil, "migrate", "config", "--from", v1, "--out", d.config, "--time-zone", "UTC")
	var converted map[string]any
	raw, _ := os.ReadFile(d.config)
	if err := json.Unmarshal(raw, &converted); err != nil {
		t.Fatal(err)
	}

	// 2. First start on the converted scope.
	d.start("")
	if _, errOut, code := o.run("backup", "create", filepath.Join(d.root, "refused")); code == 0 || !strings.Contains(errOut, "stop the daemon first") {
		t.Fatalf("an offline backup ran beside the daemon: %d %s", code, errOut)
	}
	workFor := func(issue int) (client.Work, bool) {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"widgets"}})
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
	worked := func(issue int) bool {
		w, ok := workFor(issue)
		if !ok {
			return false
		}
		xs := d.executions(w.ID)
		return len(xs) == 1 && xs[0].Outcome != nil && xs[0].Outcome.Kind == "handoff"
	}
	d.until("the new issue worked once", 60*time.Second, func() bool { return worked(fresh) })
	d.until("the interval job ran at its first start", 30*time.Second, func() bool { return lineCount(filepath.Join(d.root, "tick")) == 1 })
	d.until("projected labels", 30*time.Second, func() bool { return equal(fake.Labels(fresh), "agent-in-progress", "ready-for-review") })

	// 3. Transfer the issue v1 released by an explicit claim; the one v1
	//    still owns stays untouched.
	o.must(nil, "claim", "widgets#"+itoa(released))
	d.until("the released issue transferred and worked", 60*time.Second, func() bool { return worked(released) })
	o.must(nil, "scopes", "reconcile", "widgets")
	d.until("a reconcile after the transfer", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		return err == nil && strings.Contains(strings.Join(check(r, "github").Items, " "), "reconciled")
	})
	if _, ok := workFor(v1Owned); ok {
		t.Fatal("claimed an issue Open Supervisor still owns")
	}
	if !equal(fake.Labels(v1Owned), "agent-in-progress") {
		t.Fatalf("touched the v1-owned issue's labels: %v", fake.Labels(v1Owned))
	}
	if lineCount(filepath.Join(d.root, "digest")) != 0 {
		t.Fatal("the converted timed job re-ran the slot v1 already ran")
	}
	d.until("the transferred issue's labels converge", 30*time.Second, func() bool {
		return equal(fake.Labels(released), "agent-in-progress", "ready-for-review")
	})
	time.Sleep(time.Second) // a settled projection: nothing further is pending
	writes := len(fake.Writes())

	// 4. Drain and stop; diagnose; back up and verify.
	d.stop()
	var report cli.StateReport
	if _, errOut, code := o.run("state", "check", "--json"); code != 0 {
		t.Fatalf("state check: %s", errOut)
	}
	o.must(&report, "state", "check", "--json")
	if !report.OK || len(report.Scopes) != 1 || !report.Scopes[0].Drained || report.Scopes[0].LiveExecutions != 0 {
		t.Fatalf("state report: %+v", report)
	}
	bundle := filepath.Join(d.root, "backups", "before-return")
	var info cli.BackupInfo
	o.must(&info, "backup", "create", bundle)
	if info.Schema != 1 || info.Artifacts == 0 {
		t.Fatalf("backup: %+v", info)
	}
	o.must(&info, "backup", "verify", bundle)
	if len(info.Scopes) != 1 || info.Scopes[0] != "widgets" {
		t.Fatalf("verify: %+v", info)
	}
	oldToken, err := client.ReadTokenFile(filepath.Join(d.state, "private", "operator.token"))
	if err != nil {
		t.Fatal(err)
	}

	// 5. Forward return from the bundle into a new state directory.
	restored := filepath.Join(d.root, "restored")
	o.must(&info, "backup", "restore", bundle, "--to", restored)
	if info.TokenFile != filepath.Join(restored, "private", "operator.token") {
		t.Fatalf("restore: %+v", info)
	}
	if _, _, code := o.run("backup", "restore", bundle, "--to", restored); code == 0 {
		t.Fatal("a restore replaced an existing state directory")
	}
	d.state, d.c = restored, nil
	o.env["OPEN_CONDUIT_DATA_DIR"] = restored
	d.start("")
	stale, _ := client.New("unix://"+filepath.Join(restored, "management.sock"), oldToken.Token)
	if _, err := stale.Health(ctx); err == nil {
		t.Fatal("the bundle's operator credential still works after restore")
	}
	o.must(nil, "scopes", "reconcile", "widgets")
	d.until("a reconcile after the return", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		return err == nil && strings.Contains(strings.Join(check(r, "github").Items, " "), "reconciled")
	})
	time.Sleep(time.Second) // let any projection pass run
	for _, issue := range []int{fresh, released} {
		w, ok := workFor(issue)
		if !ok || len(d.executions(w.ID)) != 1 {
			t.Fatalf("issue %d after the return: %+v %v", issue, w, ok)
		}
	}
	if _, ok := workFor(v1Owned); ok {
		t.Fatal("the return claimed the v1-owned issue")
	}
	if got := len(fake.Writes()); got != writes {
		for _, w := range fake.Writes() {
			t.Logf("write %s %s %s", w.Method, w.Path, w.Body)
		}
		t.Fatalf("the return repeated external writes: %d then %d", writes, got)
	}
	if lineCount(filepath.Join(d.root, "tick")) != 1 || lineCount(filepath.Join(d.root, "digest")) != 0 {
		t.Fatal("the return re-ran a completed or skipped job slot")
	}
	d.stop()

	// 6. Corrupted-state and migration diagnosis on copies; a tampered bundle.
	diagnose := func(mutate func(db string)) cli.StateReport {
		dir := filepath.Join(d.root, fmt.Sprintf("copy-%d", time.Now().UnixNano()))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if b, err := os.ReadFile(filepath.Join(restored, "conduit.db"+suffix)); err == nil {
				if err := os.WriteFile(filepath.Join(dir, "conduit.db"+suffix), b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		mutate(filepath.Join(dir, "conduit.db"))
		var r cli.StateReport
		out, _, _ := o.run("--data-dir", dir, "state", "check", "--json")
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("state check: %v %s", err, out)
		}
		return r
	}
	codeOf := func(r cli.StateReport, name string) string {
		for _, c := range r.Checks {
			if c.Name == name {
				return c.Status + "/" + c.Code
			}
		}
		return ""
	}
	corrupt := diagnose(func(db string) {
		_ = os.Remove(db + "-wal")
		_ = os.Remove(db + "-shm")
		f, err := os.OpenFile(db, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteAt([]byte("this is not a sqlite header"), 0)
		f.Close()
	})
	if corrupt.OK || codeOf(corrupt, "database") != "fail/corrupt" {
		t.Fatalf("corruption diagnosis: %+v", corrupt)
	}
	newer := diagnose(func(db string) {
		h, err := sql.Open("sqlite", db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Exec("PRAGMA user_version=2"); err != nil {
			t.Fatal(err)
		}
		h.Close()
	})
	if newer.OK || codeOf(newer, "database") != "fail/schema_newer" {
		t.Fatalf("newer schema diagnosis: %+v", newer)
	}
	tampered := filepath.Join(d.root, "backups", "tampered")
	copyTree(t, bundle, tampered)
	f, err := os.OpenFile(filepath.Join(tampered, "conduit.db"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteAt([]byte{0xff, 0xfe}, 4096)
	f.Close()
	if _, errOut, code := o.run("backup", "verify", tampered); code == 0 || !strings.Contains(errOut, "digest mismatch") {
		t.Fatalf("a tampered bundle verified: %d %s", code, errOut)
	}
}
