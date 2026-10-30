package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// The 02-09 core gate drives the real `open-conduit serve` daemon as a child
// process over the public Unix-socket API and kills it with SIGKILL at each
// coordination boundary. The binary is built with the faultinject tag, whose
// OPEN_CONDUIT_FAULT hook kills the process inside (before) or immediately
// after (after) the first matching storage write. A restart is a new process
// on the same state directory; only SQLite, artifacts, the config file and
// the shell executor's run directories survive.

var (
	daemonOnce sync.Once
	daemonBin  string
	daemonErr  error
)

func daemonBinary(t *testing.T) string {
	t.Helper()
	daemonOnce.Do(func() {
		dir, err := os.MkdirTemp("", "oc-daemon-bin-")
		if err != nil {
			daemonErr = err
			return
		}
		for _, b := range []struct{ out, pkg string }{{"open-conduit", "./cmd/open-conduit"}, {"os", "./cmd/os"}} {
			args := []string{"build", "-o", filepath.Join(dir, b.out)}
			if b.out == "open-conduit" {
				args = append(args, "-tags", "faultinject")
				if raceEnabled {
					// Race-test runs also race-check the daemon composition;
					// any report fails the drill (see newDrill cleanup).
					args = append(args, "-race")
				}
			}
			cmd := exec.Command("go", append(args, b.pkg)...)
			cmd.Dir = ".."
			if out, err := cmd.CombinedOutput(); err != nil {
				daemonErr = fmt.Errorf("build %s: %v\n%s", b.out, err, out)
				return
			}
		}
		daemonBin = filepath.Join(dir, "open-conduit")
	})
	if daemonErr != nil {
		t.Fatal(daemonErr)
	}
	return daemonBin
}

const drillLease = 2 * time.Second

// drillScript is the deterministic shell fixture. The first segment parks on a
// wait (drill.event: woken by a reply Event; drill.timer: by its 2s timeout;
// drill.slow: runs until released, so a daemon kill finds it alive). A retry
// after a failed recovery parks like drill.event. A later wake trigger declares the final outcome naming the waking Event type.
const drillScript = `set -e
echo "$OS_EXECUTION_ID $OS_TRIGGER_TYPE" >> "$DRILL_DIR/launches"
case "$OS_TRIGGER_TYPE" in
drill.event|core.retry.due) os wait --type drill.reply --timeout 3600 --request-id wait >/dev/null; echo parked ;;
drill.timer) os wait --type drill.reply --timeout 2 --request-id wait >/dev/null; echo parked ;;
drill.slow)
  printf '%s' "$OS_TOKEN" > "$DRILL_DIR/token"
  printf '%s\n%s\n' "$OS_ENDPOINT" "$OS_TOKEN_FILE" > "$DRILL_DIR/toolbelt"
  echo $$ > "$DRILL_DIR/pid"
  while [ ! -e "$DRILL_DIR/release" ]; do sleep 0.05; done
  os wait --type drill.reply --timeout 2 --request-id wait >/dev/null; echo parked ;;
*) os outcome completed --summary "woken by $OS_TRIGGER_TYPE" --request-id done >/dev/null; echo done ;;
esac
`

func drillConfig(root string, settings map[string]any) []byte {
	cfg := map[string]any{
		"version": 1, "settings": settings, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "drill", "name": "Drill",
			"authorization": map[string]any{"connections": []any{}, "operations": []string{"read_context", "declare_outcome", "wait"}, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context", "declare_outcome", "wait"}, "secret_names": []any{},
				"config": map[string]any{"command": drillScript, "env": map[string]string{"DRILL_DIR": root}}}},
			"routes":      []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

type drill struct {
	t      *testing.T
	root   string
	state  string
	config string
	cmd    *exec.Cmd
	exited chan struct{}
	log    *os.File
	c      *client.Client
	starts int
	// extra serve flags
	extra []string
	// bin is the daemon to run, when not the shared build.
	bin string
}

func newDrill(t *testing.T) *drill {
	t.Helper()
	daemonBinary(t)
	root, err := os.MkdirTemp("/tmp", "oc-drill-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	d := &drill{t: t, root: root, state: filepath.Join(root, "state"), config: filepath.Join(root, "config.json")}
	if err = os.WriteFile(d.config, drillConfig(root, map[string]any{"retry_delays_seconds": []int{1, 1}}), 0o600); err != nil {
		t.Fatal(err)
	}
	if d.log, err = os.Create(filepath.Join(root, "daemon.log")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.kill()
		// A failed drill can leave the detached fixture process waiting for a
		// release file; it is its own process-group leader.
		if b, err := os.ReadFile(filepath.Join(root, "pid")); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 1 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
		d.log.Close()
		out, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
		if bytes.Contains(out, []byte("WARNING: DATA RACE")) {
			t.Error("race detector reported a data race in the daemon")
		}
		if t.Failed() {
			t.Logf("daemon log:\n%s", out)
		}
	})
	return d
}

// start launches the daemon, optionally armed with a fault, and waits until
// the management API answers.
func (d *drill) start(fault string) {
	d.t.Helper()
	d.starts++
	fmt.Fprintf(d.log, "--- start %d fault=%q\n", d.starts, fault)
	bin := d.bin
	if bin == "" {
		bin = daemonBinary(d.t)
	}
	cmd := exec.Command(bin, append([]string{"--data-dir", d.state, "serve", "--config", d.config, "--poll-interval", "50ms", "--lease-duration", drillLease.String()}, d.extra...)...)
	cmd.Env = append(os.Environ(), "OPEN_CONDUIT_FAULT="+fault)
	cmd.Stdout, cmd.Stderr = d.log, d.log
	if err := cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	d.cmd, d.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) { _ = cmd.Wait(); close(exited) }(d.exited)
	d.until("daemon ready", 20*time.Second, func() bool {
		if d.c == nil {
			f, err := client.ReadTokenFile(filepath.Join(d.state, "private", "operator.token"))
			if err != nil {
				return false
			}
			if d.c, err = client.New("unix://"+filepath.Join(d.state, "management.sock"), f.Token); err != nil {
				return false
			}
		}
		_, err := d.c.Health(context.Background())
		return err == nil
	})
}

// killed waits for the armed fault to SIGKILL the daemon.
func (d *drill) killed() {
	d.t.Helper()
	select {
	case <-d.exited:
	case <-time.After(30 * time.Second):
		d.t.Fatal("fault boundary was never reached")
	}
	ws, ok := d.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		d.t.Fatalf("daemon did not die by SIGKILL: %v", d.cmd.ProcessState)
	}
	log, _ := os.ReadFile(d.log.Name())
	if !strings.Contains(string(log), "fault injected: SIGKILL") {
		d.t.Fatal("daemon exited without reaching the injected fault")
	}
}

// kill is an external SIGKILL of a running daemon.
func (d *drill) kill() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	select {
	case <-d.exited:
		return
	default:
	}
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	<-d.exited
}

func (d *drill) until(what string, timeout time.Duration, fn func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for !fn() {
		if time.Now().After(deadline) {
			d.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (d *drill) submit(typ string) (client.Event, error) {
	return d.c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: "drill", Type: typ, DeliveryID: "drill-1", CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-drill-1")
}

func (d *drill) work() (client.Work, bool) {
	page, err := d.c.Works(context.Background(), url.Values{"scope_id": {"drill"}})
	if err != nil || len(page.Items) == 0 {
		return client.Work{}, false
	}
	if len(page.Items) != 1 {
		d.t.Fatalf("expected exactly one Work, got %d", len(page.Items))
	}
	return page.Items[0], true
}

func (d *drill) executions(work string) []client.Execution {
	page, err := d.c.Executions(context.Background(), url.Values{"work_id": {work}})
	if err != nil {
		return nil
	}
	return page.Items
}

// sample asserts the exclusivity invariant on every observation: a Work never
// has two running attempts.
func (d *drill) sample(work string) {
	running := 0
	for _, x := range d.executions(work) {
		if x.State == "running" || x.State == "pending" {
			running++
		}
	}
	if running > 1 {
		d.t.Fatalf("Work %s has %d live attempts", work, running)
	}
}

func (d *drill) waitState(state string) client.Work {
	d.t.Helper()
	var w client.Work
	d.until("work "+state, 30*time.Second, func() bool {
		var ok bool
		if w, ok = d.work(); !ok {
			return false
		}
		d.sample(w.ID)
		return w.State == state
	})
	return w
}

// wakeIfWaiting delivers the reply once the Work parks, unless the reply was
// already accepted before the kill and the Work has moved on.
func (d *drill) wakeIfWaiting() {
	d.t.Helper()
	var w client.Work
	d.until("work waiting or completed", 30*time.Second, func() bool {
		var ok bool
		if w, ok = d.work(); !ok {
			return false
		}
		d.sample(w.ID)
		return w.State == "waiting" || w.State == "completed"
	})
	if w.State == "waiting" {
		if err := d.wake(w); err != nil {
			d.t.Fatal(err)
		}
	}
}

func (d *drill) wake(w client.Work) error {
	r, err := d.c.Wake(context.Background(), w.ID, client.WakeRequest{ExpectedRevision: w.Revision, Type: "drill.reply", DeliveryID: "reply-1", CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "wake-1")
	if err == nil && !r.Matched {
		d.t.Fatalf("reply did not match the active Wait: %+v", r)
	}
	return err
}

type lifecycle struct {
	Msg         string            `json:"msg"`
	At          time.Time         `json:"at"`
	TraceID     string            `json:"trace_id"`
	EventID     string            `json:"event_id"`
	WorkID      string            `json:"work_id"`
	ExecutionID string            `json:"execution_id"`
	Attributes  map[string]string `json:"attributes"`
}

func (d *drill) records() []lifecycle {
	f, err := os.Open(filepath.Join(d.state, "logs", "lifecycle.jsonl"))
	if err != nil {
		d.t.Fatal(err)
	}
	defer f.Close()
	var out []lifecycle
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var r lifecycle
		if json.Unmarshal(s.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// verify checks the gate invariants after the final outcome and returns the
// durable causal trace (from the public API) plus the exported lifecycle
// records. Telemetry is at most once from memory, so records queued at a kill
// may be lost; the durable history is the audit source and is asserted fully.
func (d *drill) verify(root client.Event, want string, lossy bool) string {
	d.t.Helper()
	ctx := context.Background()
	w := d.waitState("completed")
	events, err := d.c.Events(ctx, url.Values{"scope_id": {"drill"}, "type": {root.Type}})
	if err != nil || len(events.Items) != 1 || events.Items[0].ID != root.ID || events.Items[0].RoutedWorkID != w.ID {
		d.t.Fatalf("accepted root event not exactly once/routed: %+v %v", events.Items, err)
	}
	xs := d.executions(w.ID)
	sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
	last := xs[len(xs)-1]
	if last.State != "completed" || last.Outcome == nil || last.Outcome.Summary != want {
		d.t.Fatalf("final outcome: %s %+v", last.State, last.Outcome)
	}
	if xs[0].TriggerID != root.ID {
		d.t.Fatalf("first attempt not triggered by the root event: %s", xs[0].TriggerID)
	}
	steps := []string{"event " + root.Type + " -> work " + w.ID[len(w.ID)-8:]}
	for _, x := range xs {
		if x.State == "running" || x.State == "pending" || x.State == "parked" {
			d.t.Fatalf("attempt left live after completion: %+v", x)
		}
		trigger, err := d.c.Event(ctx, x.TriggerID)
		if err != nil || trigger.WorkID != w.ID && trigger.RoutedWorkID != w.ID {
			d.t.Fatalf("attempt %d trigger %s not linked to the Work: %+v %v", x.Attempt, x.TriggerID, trigger, err)
		}
		outcome := ""
		if x.Outcome != nil {
			outcome = x.Outcome.Kind
		}
		steps = append(steps, fmt.Sprintf("a%d[trigger=%s epoch=%s %s/%s]", x.Attempt, trigger.Type, x.Lease.Epoch, x.State, outcome))
	}
	steps = append(steps, "outcome "+strconv.Quote(last.Outcome.Summary))
	// Each segment launches at most once: recovery never relaunches a
	// (execution, trigger) pair.
	launches := strings.Split(strings.TrimSpace(readFile(d.t, filepath.Join(d.root, "launches"))), "\n")
	seen := map[string]bool{}
	for _, l := range launches {
		if seen[l] {
			d.t.Fatalf("segment launched twice: %q in %q", l, launches)
		}
		seen[l] = true
	}
	runs, err := d.c.Runs(ctx, url.Values{"work_id": {w.ID}})
	if err != nil || len(runs.Items) != len(xs) {
		d.t.Fatalf("history: %d runs for %d executions: %v", len(runs.Items), len(xs), err)
	}
	// The exported lifecycle records explain the final attempt on one trace.
	var trace []string
	if lossy {
		// The kill landed right after the final commit, so its records died
		// with the process; give any survivors time to flush.
		time.Sleep(500 * time.Millisecond)
	} else {
		d.until("final lifecycle records exported", 10*time.Second, func() bool {
			return len(d.recordsNamed("execution.completed", last.ID)) == 1
		})
	}
	traces := map[string]string{}
	for _, r := range d.records() {
		if r.WorkID != w.ID && r.EventID != root.ID {
			continue
		}
		if r.ExecutionID != "" && strings.HasPrefix(r.Msg, "execution.") {
			if first, ok := traces[r.ExecutionID]; ok && first != r.TraceID {
				d.t.Fatalf("execution %s changed trace across restart", r.ExecutionID)
			}
			traces[r.ExecutionID] = r.TraceID
		}
		step := r.Msg
		if r.ExecutionID != "" {
			step += "[a" + r.ExecutionID[strings.LastIndex(r.ExecutionID, "/")+1:]
			if e := r.Attributes["lease.epoch"]; e != "" {
				step += " e" + e
			}
			if m := r.Attributes["launch.mode"]; m != "" {
				step += " " + m
			}
			step += "]"
		}
		trace = append(trace, step)
	}
	if !lossy && len(d.recordsNamed("execution.launched", last.ID)) == 0 {
		d.t.Fatalf("no launch record for the final attempt: %v", trace)
	}
	return strings.Join(steps, " > ") + "\n    lifecycle: " + strings.Join(trace, " > ")
}

func (d *drill) recordsNamed(name, execution string) []lifecycle {
	var out []lifecycle
	for _, r := range d.records() {
		if r.Msg == name && (execution == "" || r.ExecutionID == execution) {
			out = append(out, r)
		}
	}
	return out
}

// TestDaemonKillDrills kills the real daemon at each coordination boundary,
// before and after commit, restarts it on the same state and proves the
// event -> work -> execution -> wait -> wake -> outcome flow completes once.
func TestDaemonKillDrills(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	daemonBinary(t)
	type scenario struct {
		boundary, match, root, want string
		// beforeKill drives the flow up to the fault; afterRestart finishes it.
		trigger func(d *drill) client.Event
	}
	// submitThroughKill submits the root Event to a daemon armed at a
	// boundary that the submission itself drives. The accept commits before
	// the response, and the host loop routes and claims asynchronously, so a
	// claim (or later) fault may SIGKILL the daemon before the response is
	// written. Only a transport failure is tolerated (d.killed then proves
	// the fault killed the daemon); an API error answer fails the drill. The
	// client retries with the same idempotency key after restart.
	submitThroughKill := func(typ string) func(d *drill) client.Event {
		return func(d *drill) client.Event {
			e, err := d.submit(typ)
			var apiErr *client.Error
			if errors.As(err, &apiErr) {
				d.t.Fatalf("submission refused before the fault: %v", err)
			}
			return e
		}
	}
	submit := func(typ string) func(d *drill) client.Event {
		return func(d *drill) client.Event {
			e, err := d.submit(typ)
			if err != nil {
				d.t.Fatal(err)
			}
			return e
		}
	}
	wakeThroughKill := func(d *drill) client.Event {
		e := submit("drill.event")(d)
		_ = d.wake(d.waitState("waiting"))
		return e
	}
	scenarios := []scenario{
		{"accept", "drill.event", "drill.event", "woken by drill.reply", submitThroughKill("drill.event")},
		{"route", "drill.event", "drill.event", "woken by drill.reply", submitThroughKill("drill.event")},
		{"claim", "", "drill.event", "woken by drill.reply", submitThroughKill("drill.event")},
		{"wait", "", "drill.event", "woken by drill.reply", submitThroughKill("drill.event")},
		{"outcome", "", "drill.event", "woken by drill.reply", wakeThroughKill},
		{"timer", "core.wait.timeout", "drill.timer", "woken by core.wait.timeout", submitThroughKill("drill.timer")},
	}
	for _, s := range scenarios {
		for _, phase := range []string{"before", "after"} {
			s, phase := s, phase
			t.Run(s.boundary+"/"+phase, func(t *testing.T) {
				t.Parallel()
				d := newDrill(t)
				fault := s.boundary + ":" + phase
				if s.match != "" {
					fault += ":" + s.match
				}
				d.start(fault)
				s.trigger(d)
				d.killed()
				d.start("")
				// A client whose request died with the daemon retries with the
				// same idempotency key; acceptance must not duplicate.
				root, err := d.submit(s.root)
				if err != nil {
					t.Fatal(err)
				}
				if s.root == "drill.event" {
					d.wakeIfWaiting()
				}
				trace := d.verify(root, s.want, fault == "outcome:after")
				if s.root == "drill.timer" {
					// The timer wake is causally derived from the root Event.
					w, _ := d.work()
					xs := d.executions(w.ID)
					sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
					chain, err := d.c.EventChain(context.Background(), xs[len(xs)-1].TriggerID)
					if err != nil || len(chain) < 2 || chain[0].Type != "core.wait.timeout" || chain[len(chain)-1].ID != root.ID {
						t.Fatalf("timer wake causal chain: %+v %v", chain, err)
					}
				}
				t.Logf("fault=%s trace: %s", fault, trace)
			})
		}
	}
}

// TestDaemonKillWithDetachedProcess kills the daemon while the shell process
// runs. The restarted daemon must wait out the live lease, adopt the process
// under a higher epoch without relaunching it, refuse the stale credential and
// finish the flow. The expired-lease variant restarts only after the dead
// daemon's lease expired, with the process still running.
func TestDaemonKillWithDetachedProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	daemonBinary(t)
	for _, expired := range []bool{false, true} {
		expired := expired
		t.Run("lease_expired="+strconv.FormatBool(expired), func(t *testing.T) {
			t.Parallel()
			d := newDrill(t)
			d.start("")
			root, err := d.submit("drill.slow")
			if err != nil {
				t.Fatal(err)
			}
			pidFile := filepath.Join(d.root, "pid")
			d.until("shell process running", 20*time.Second, func() bool { return fileHasLine(pidFile) })
			pid, _ := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
			oldToken := readFile(t, filepath.Join(d.root, "token"))
			// The process got its own toolbelt socket and a token file in a
			// token directory, both in its run directory.
			toolbelt := strings.Fields(readFile(t, filepath.Join(d.root, "toolbelt")))
			if len(toolbelt) != 2 {
				t.Fatalf("toolbelt environment: %q", toolbelt)
			}
			endpoint, tokenFile := toolbelt[0], toolbelt[1]
			runDir := filepath.Dir(filepath.Dir(tokenFile))
			if endpoint != "unix://"+filepath.Join(runDir, "toolbelt.sock") || filepath.Dir(runDir) != filepath.Join(d.state, "runs") {
				t.Fatalf("toolbelt endpoint %s and token file %s are not in one run directory", endpoint, tokenFile)
			}
			if got := strings.TrimSpace(readFile(t, tokenFile)); got != oldToken {
				t.Fatalf("token file does not hold the process token")
			}
			w, _ := d.work()
			first := d.executions(w.ID)[0]
			if first.State != "running" {
				t.Fatalf("attempt not running before the kill: %+v", first)
			}
			d.kill()
			// Renewal only extends a lease, so the last observed expiry is a
			// lower bound for when the dead daemon's lease really expired.
			leaseExpiry, err := time.Parse(time.RFC3339Nano, first.Lease.ExpiresAt)
			if err != nil {
				t.Fatal(err)
			}
			if syscall.Kill(pid, 0) != nil {
				t.Fatal("shell process did not survive the daemon")
			}
			if expired {
				time.Sleep(time.Until(leaseExpiry) + 500*time.Millisecond)
			}
			d.start("")
			d.until("adoption record", 30*time.Second, func() bool { return len(d.recordsNamed("execution.recovered", first.ID)) > 0 })
			// Exactly one takeover, never before the dead daemon's lease expired.
			adopted := d.recordsNamed("execution.recovered", first.ID)
			if len(adopted) != 1 {
				t.Fatalf("expected one recovery record, got %d", len(adopted))
			}
			if adopted[0].At.Before(leaseExpiry) {
				t.Fatalf("live lease taken over at %s before expiry %s", adopted[0].At, leaseExpiry)
			}
			recovered, err := d.c.Execution(context.Background(), first.ID)
			if err != nil || recovered.Lease.Epoch == first.Lease.Epoch {
				t.Fatalf("adoption did not advance the epoch: %+v %v", recovered.Lease, err)
			}
			old, _ := client.New("unix://"+filepath.Join(d.state, "management.sock"), oldToken)
			if _, err = old.ToolbeltCall(context.Background(), client.ToolCall{Operation: "read_context"}); err == nil {
				t.Fatal("pre-restart credential still authorized after adoption")
			}
			// The restarted daemon serves the adopted process's socket again
			// and rotated the token in its token directory.
			rotated := strings.TrimSpace(readFile(t, tokenFile))
			if rotated == oldToken {
				t.Fatal("adoption did not rotate the token file")
			}
			own, _ := client.New(endpoint, rotated)
			if _, err = own.ToolbeltCall(context.Background(), client.ToolCall{Operation: "read_context"}); err != nil {
				t.Fatalf("adopted process's socket after restart: %v", err)
			}
			stale, _ := client.New(endpoint, oldToken)
			if _, err = stale.ToolbeltCall(context.Background(), client.ToolCall{Operation: "read_context"}); err == nil {
				t.Fatal("pre-restart credential served on the adopted process's socket")
			}
			if syscall.Kill(pid, 0) != nil {
				t.Fatal("adopted process is gone")
			}
			if err = os.WriteFile(filepath.Join(d.root, "release"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			trace := d.verify(root, "woken by core.wait.timeout", false)
			d.until("run directory removed", 20*time.Second, func() bool { _, err := os.Stat(runDir); return os.IsNotExist(err) })
			launched := d.recordsNamed("execution.launched", first.ID)
			modes := []string{}
			for _, r := range launched {
				modes = append(modes, r.Attributes["launch.mode"]+"@"+r.Attributes["lease.epoch"])
			}
			// The pre-kill launch record may be lost with the killed process
			// (telemetry is at most once); the recovery launch must continue
			// the attempt's original durable trace.
			last := launched[len(launched)-1]
			if last.Attributes["launch.mode"] != "recover" || last.TraceID != first.TraceID || len(launched) > 2 {
				t.Fatalf("adoption trace: %v (trace %s vs %s)", modes, last.TraceID, first.TraceID)
			}
			if got := strings.Count(readFile(t, filepath.Join(d.root, "launches")), "drill.slow"); got != 1 {
				t.Fatalf("detached segment launched %d times", got)
			}
			t.Logf("recovered epoch %s (was %s), launches %v; trace: %s", recovered.Lease.Epoch, first.Lease.Epoch, modes, trace)
		})
	}
}

// TestDaemonGracefulRestart covers SIGTERM: the daemon releases the live lease
// so the next daemon adopts the running process at once.
func TestDaemonGracefulRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	d := newDrill(t)
	d.start("")
	root, err := d.submit("drill.slow")
	if err != nil {
		t.Fatal(err)
	}
	d.until("shell process running", 20*time.Second, func() bool { return fileHasLine(filepath.Join(d.root, "pid")) })
	if err = d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon ignored SIGTERM")
	}
	if !d.cmd.ProcessState.Success() {
		t.Fatalf("graceful exit: %v", d.cmd.ProcessState)
	}
	stopped := time.Now()
	d.start("")
	w, _ := d.work()
	first := d.executions(w.ID)[0]
	d.until("adoption", 10*time.Second, func() bool { return len(d.recordsNamed("execution.recovered", first.ID)) == 1 })
	if at := d.recordsNamed("execution.recovered", first.ID)[0].At; at.Sub(stopped) >= drillLease {
		t.Fatalf("released lease was not recovered before it would have expired: %s", at.Sub(stopped))
	}
	if err = os.WriteFile(filepath.Join(d.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.verify(root, "woken by core.wait.timeout", false)
	if b, _ := os.ReadFile(d.log.Name()); !bytes.Contains(b, []byte("open-conduit stopped")) {
		t.Fatal("no graceful stop logged")
	}
}

// TestDaemonConfigReload applies a new generation to the running daemon: the
// parked attempt keeps generation 1, the wake runs under generation 2, and a
// restart restores the committed generation and warns about an unapplied edit.
func TestDaemonConfigReload(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	d := newDrill(t)
	d.start("")
	root, err := d.submit("drill.event")
	if err != nil {
		t.Fatal(err)
	}
	w := d.waitState("waiting")
	next := drillConfig(d.root, map[string]any{"retry_delays_seconds": []int{1, 1}, "kept_executions": 5})
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	applied, err := d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "apply-2")
	if err != nil || applied.Generation != "2" {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	if err = d.wake(w); err != nil {
		t.Fatal(err)
	}
	d.verify(root, "woken by drill.reply", false)
	runs, err := d.c.Runs(ctx, url.Values{"work_id": {w.ID}})
	if err != nil || len(runs.Items) != 2 {
		t.Fatalf("runs: %+v %v", runs.Items, err)
	}
	gens := map[uint32]string{}
	for _, r := range runs.Items {
		gens[r.Attempt] = r.ConfigGeneration
	}
	if gens[1] != "1" || gens[2] != "2" {
		t.Fatalf("generation pinning: %v", gens)
	}
	// An edit saved to the file while stopped is not applied by the restart,
	// and the daemon says so at startup.
	d.kill()
	if err = os.WriteFile(d.config, drillConfig(d.root, map[string]any{"retry_delays_seconds": []int{1, 1}, "kept_executions": 6}), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	if h, err := d.c.Health(ctx); err != nil || h.ConfigGeneration != "2" {
		t.Fatalf("restart generation: %+v %v", h, err)
	}
	d.until("pending edit warning", 5*time.Second, func() bool {
		b, _ := os.ReadFile(d.log.Name())
		return bytes.Contains(b, []byte("config file differs from the active generation")) && bytes.Contains(b, []byte("config apply "+d.config+" --generation 2"))
	})
}

// TestServeCommandResolution checks B05 path precedence for `serve` and that
// the toolbelt-free operator package only reaches the daemon through Env.Serve.
func TestServeCommandResolution(t *testing.T) {
	var got cli.ServeRequest
	env := map[string]string{"HOME": "/home/u", "OPEN_CONDUIT_CONFIG": "rel/config.json", "OPEN_CONDUIT_SOCKET": "/run/oc.sock"}
	run := func(serve func(cli.ServeRequest) int, args ...string) int {
		var out, errOut strings.Builder
		return cli.RunOperator("v", "c", args, cli.Env{Getenv: func(k string) string { return env[k] }, Cwd: "/work", Serve: serve}, &out, &errOut)
	}
	capture := func(r cli.ServeRequest) int { got = r; return 0 }
	if code := run(capture, "serve"); code != 0 || got.ConfigPath != "/work/rel/config.json" || got.StateDir != "/home/u/.local/state/open-conduit" || got.Socket != "/run/oc.sock" || got.LeaseDuration != time.Minute || got.PollInterval != time.Second {
		t.Fatalf("env resolution: %d %+v", code, got)
	}
	if code := run(capture, "--data-dir", "state", "--socket", "/s.sock", "serve", "--config", "/etc/oc.json", "--lease-duration", "5s", "--toolbelt-dir", "tb"); code != 0 || got.ConfigPath != "/etc/oc.json" || got.StateDir != "/work/state" || got.Socket != "/s.sock" || got.LeaseDuration != 5*time.Second || got.ToolbeltDir != "/work/tb" {
		t.Fatalf("flag resolution: %d %+v", code, got)
	}
	delete(env, "OPEN_CONDUIT_CONFIG")
	env["XDG_CONFIG_HOME"] = "/xdg"
	if code := run(capture, "serve"); code != 0 || got.ConfigPath != "/xdg/open-conduit/config.json" {
		t.Fatalf("default config: %+v", got)
	}
	if code := run(capture, "serve", "--lease-duration", "-1s"); code != 2 {
		t.Fatalf("invalid flag accepted: %d", code)
	}
	if code := run(nil, "serve"); code != 1 {
		t.Fatalf("serve without composition: %d", code)
	}
}

// TestDaemonOperatorCLI drives the core through the shipped CLI only: emit an
// Event with a correlation key, inspect history and run doctor.
func TestDaemonOperatorCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	d := newDrill(t)
	d.start("")
	cliRun := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(daemonBinary(t), append([]string{"--data-dir", d.state}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	var e client.Event
	if err := json.Unmarshal([]byte(cliRun("events", "emit", "--scope-id", "drill", "--type", "drill.timer", "--delivery-id", "drill-1", "--key-source", "drill", "--key-external-id", "one", "--key", "submit-drill-1")), &e); err != nil || e.CorrelationKey == nil || e.CorrelationKey.ExternalID != "one" {
		t.Fatalf("emit: %+v %v", e, err)
	}
	d.verify(e, "woken by core.wait.timeout", false)
	if table := cliRun("runs", "table"); !strings.Contains(table, "woken by core.wait.timeout") || !strings.Contains(table, "core.wait.timeout") {
		t.Fatalf("runs table:\n%s", table)
	}
	if doctor := cliRun("doctor"); !strings.Contains(doctor, "telemetry") || strings.Contains(doctor, "FAIL") {
		t.Fatalf("doctor:\n%s", doctor)
	}
}
