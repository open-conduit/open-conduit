package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
	"github.com/open-conduit/open-conduit/storage"
)

// accepted collects the Events the task sweep announces.
type accepted struct {
	mu     sync.Mutex
	events []domain.Event
	parent []domain.ID
	scope  []domain.ID
}

func (a *accepted) accept(_ storage.Tx, scope, parent domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events, a.parent, a.scope = append(a.events, e), append(a.parent, parent), append(a.scope, scope)
	return domain.AcceptedEvent{Event: e}, true, nil
}

func (a *accepted) all() []domain.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]domain.Event(nil), a.events...)
}

type taskHarness struct {
	*harness
	e    *claudecode.Executor
	got  *accepted
	auth extension.Authority
}

// liveTurn starts a turn that stays running until released, so tasks can
// be started from it.
func liveTurn(t *testing.T, extra map[string]any) (*taskHarness, extension.RunHandle) {
	t.Helper()
	h := newHarness(t)
	h.mode("wait")
	e := h.executor()
	got := &accepted{}
	e.SetTaskHost(claudecode.TaskHost{Accept: got.accept})
	r, _ := h.request("work/t", "exec-t", 1, extra)
	handle, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn started", func() bool { _, err := os.Stat(filepath.Join(h.fake, "pid")); return err == nil })
	auth := extension.Authority{ScopeID: "eng", WorkID: "work/t", ExecutionID: "exec-t", Lease: domain.LeaseRef{ExecutionID: "exec-t", Owner: "test", Epoch: 1}, TriggerID: r.Trigger.ID}
	return &taskHarness{harness: h, e: e, got: got, auth: auth}, handle
}

func (th *taskHarness) release(handle extension.RunHandle) extension.Result {
	th.t.Helper()
	if err := os.WriteFile(filepath.Join(th.fake, "release"), nil, 0o600); err != nil {
		th.t.Fatal(err)
	}
	return th.wait(handle)
}

func payload(v any) domain.Payload {
	b, _ := json.Marshal(v)
	return domain.Payload{Data: b}
}

func out(t *testing.T, p domain.Payload) map[string]any {
	t.Helper()
	var m map[string]any
	if p.Schema != claudecode.SchemaTool || json.Unmarshal(p.Data, &m) != nil {
		t.Fatalf("output %s %s", p.Schema, p.Data)
	}
	return m
}

func (th *taskHarness) start(ctx context.Context, in map[string]any) map[string]any {
	th.t.Helper()
	p, err := th.e.TaskStart(ctx, th.auth, payload(in))
	if err != nil {
		th.t.Fatal(err)
	}
	return out(th.t, p)
}

func (th *taskHarness) list() []map[string]any {
	th.t.Helper()
	p, err := th.e.TaskList(context.Background(), th.auth, domain.Payload{})
	if err != nil {
		th.t.Fatal(err)
	}
	var m struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(p.Data, &m)
	return m.Tasks
}

func (th *taskHarness) ended(n int) []domain.Event {
	th.t.Helper()
	waitFor(th.t, "the task's end", func() bool {
		if _, err := th.e.SweepTasks(context.Background()); err != nil {
			th.t.Fatal(err)
		}
		return len(th.got.all()) >= n
	})
	return th.got.all()
}

func code(err error) string {
	var oe *extension.OperationError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

// TestTaskOutlivesTurnAndAnnouncesItsEnd is the case #556 reported: work
// started during a turn keeps running after the segment's wrapper signals
// its group, and its end is announced on the task's own key as a derived
// Event of the turn's trigger, once.
func TestTaskOutlivesTurnAndAnnouncesItsEnd(t *testing.T) {
	th, handle := liveTurn(t, nil)
	ctx := extension.WithCallRequestID(context.Background(), "req-1")
	cmd := `echo started >> "$FAKE_DIR/task-runs"; while [ ! -e "$FAKE_DIR/go" ]; do sleep 0.05; done; echo first; echo "  the last line  "; exit 3`
	first := th.start(ctx, map[string]any{"command": cmd, "name": "suite"})
	if first["state"] != claudecode.TaskRunning || first["name"] != "suite" || first["task"] == "" {
		t.Fatalf("start %v", first)
	}
	// A retried start (same request ID) starts nothing more.
	if again := th.start(ctx, map[string]any{"command": cmd, "name": "suite"}); again["task"] != first["task"] {
		t.Fatalf("retry %v, first %v", again, first)
	}
	waitFor(t, "the task started", func() bool { _, err := os.Stat(filepath.Join(th.fake, "task-runs")); return err == nil })

	th.release(handle)
	if _, err := th.e.SweepTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := th.got.all(); len(got) != 0 {
		t.Fatalf("announced while running: %v", got)
	}
	if l := th.list(); len(l) != 1 || l[0]["state"] != claudecode.TaskRunning {
		t.Fatalf("the task did not outlive the turn: %v", l)
	}

	if err := os.WriteFile(filepath.Join(th.fake, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	events := th.ended(1)
	ev := events[0]
	if ev.Type != claudecode.TypeTaskEnded || ev.Source != claudecode.Name || ev.ExecutionID != "" || ev.CausationID != "" ||
		ev.CorrelationKey == nil || *ev.CorrelationKey != (domain.CorrelationKey{Source: claudecode.Name, ExternalID: "task/" + first["task"].(string)}) ||
		ev.DeliveryID != "task/"+first["task"].(string)+"/ended" || ev.Data.Schema != claudecode.SchemaTask {
		t.Fatalf("event %+v", ev)
	}
	if th.got.parent[0] != th.auth.TriggerID || th.got.scope[0] != "eng" {
		t.Fatalf("parent %s scope %s", th.got.parent[0], th.got.scope[0])
	}
	var p map[string]any
	_ = json.Unmarshal(ev.Data.Data, &p)
	if p["outcome"] != claudecode.TaskFailed || p["exit_code"] != float64(3) || p["last_line"] != "the last line" || p["name"] != "suite" || p["work"] != "work/t" {
		t.Fatalf("payload %v", p)
	}
	if b, _ := os.ReadFile(filepath.Join(th.fake, "task-runs")); strings.Count(string(b), "started") != 1 {
		t.Fatalf("the command ran %q", b)
	}
	// Another sweep announces nothing more.
	if _, err := th.e.SweepTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := th.got.all(); len(got) != 1 {
		t.Fatalf("announced again: %v", got)
	}
	if l := th.list(); len(l) != 1 || l[0]["state"] != claudecode.TaskFailed || l[0]["exit_code"] != float64(3) {
		t.Fatalf("list %v", l)
	}
	if text, ok := claudecode.RenderTask(ev, nil); !ok || !strings.Contains(text, "Task suite failed") || !strings.Contains(text, "Exit code: 3") || !strings.Contains(text, "Last line: the last line") {
		t.Fatalf("render %q", text)
	}
	alias := domain.Event{Type: claudecode.TypeTaskFinished, CausationID: "src"}
	if text, ok := claudecode.RenderTask(alias, func(id domain.ID) (domain.Event, bool) { return ev, id == "src" }); !ok || !strings.Contains(text, "Task suite failed") {
		t.Fatalf("alias render %q", text)
	}
}

// TestTaskWithoutWrapperIsNegativeControl shows the problem the task
// exists for: a background command left by the turn itself is killed with
// the segment's group.
func TestTaskWithoutWrapperIsNegativeControl(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	// The fake claude runs in the segment's group; a child it leaves in
	// the background is signaled by the wrapper when claude exits.
	script := filepath.Join(h.root, "claude-bg")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n(trap 'echo killed > \"$FAKE_DIR/bg\"; exit 1' TERM; echo $$ > \"$FAKE_DIR/bgpid\"; while :; do sleep 0.05; done) &\nwhile [ ! -e \"$FAKE_DIR/bgpid\" ]; do sleep 0.05; done\nexec "+h.binary+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ := h.request("work/n", "exec-n", 1, map[string]any{"binary": script})
	h.run(e, r)
	waitFor(t, "the background child was signaled", func() bool { _, err := os.Stat(filepath.Join(h.fake, "bg")); return err == nil })
}

func TestTaskStopTimeoutAndLost(t *testing.T) {
	th, handle := liveTurn(t, nil)
	defer th.release(handle)
	long := `trap 'echo stopping; exit 143' TERM; echo up; while :; do sleep 0.05; done`
	stopped := th.start(context.Background(), map[string]any{"command": long, "name": "stopme"})
	timed := th.start(context.Background(), map[string]any{"command": long, "name": "slow", "timeout_minutes": 5})
	lost := th.start(context.Background(), map[string]any{"command": long, "name": "lost"})
	pidOf := func(id string) int {
		var pid int
		waitFor(t, "the task's pid", func() bool {
			b, err := os.ReadFile(filepath.Join(th.state, "tasks", id, "pid"))
			if err != nil {
				return false
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			return err == nil && pid > 1
		})
		return pid
	}
	if _, err := th.e.TaskStop(context.Background(), th.auth, payload(map[string]any{"task": "stopme"})); err != nil {
		t.Fatal(err)
	}
	th.clock.Advance(6 * time.Minute)
	if err := syscall.Kill(-pidOf(lost["task"].(string)), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	events := th.ended(3)
	outcomes := map[string]string{}
	for _, ev := range events {
		var p map[string]any
		_ = json.Unmarshal(ev.Data.Data, &p)
		outcomes[p["task"].(string)] = p["outcome"].(string)
	}
	if outcomes[stopped["task"].(string)] != claudecode.TaskStopped || outcomes[timed["task"].(string)] != claudecode.TaskTimedOut || outcomes[lost["task"].(string)] != claudecode.TaskLost {
		t.Fatalf("outcomes %v", outcomes)
	}
}

func TestTaskEndsWithItsWork(t *testing.T) {
	th, handle := liveTurn(t, nil)
	task := th.start(context.Background(), map[string]any{"command": `while :; do sleep 0.05; done`})
	th.release(handle)
	th.store.setWork(domain.Work{ID: "work/t", ScopeID: "eng", State: domain.WorkCompleted})
	events := th.ended(1)
	var p map[string]any
	_ = json.Unmarshal(events[0].Data.Data, &p)
	if p["task"] != task["task"] || p["outcome"] != claudecode.TaskStopped {
		t.Fatalf("payload %v", p)
	}
	// A later sweep releases the finished Work's task.
	if _, err := th.e.SweepTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l := th.list(); len(l) != 0 {
		t.Fatalf("task record kept: %v", l)
	}
	if _, err := os.Stat(filepath.Join(th.state, "tasks", task["task"].(string))); !os.IsNotExist(err) {
		t.Fatalf("task files kept: %v", err)
	}
}

func TestTaskWaitPlan(t *testing.T) {
	th, handle := liveTurn(t, nil)
	defer th.release(handle)
	if _, err := th.e.TaskWait(context.Background(), th.auth, domain.Payload{}); code(err) != "no_running_tasks" {
		t.Fatalf("nothing to wait for: %v", err)
	}
	a := th.start(context.Background(), map[string]any{"command": `while :; do sleep 0.05; done`, "name": "a"})
	b := th.start(context.Background(), map[string]any{"command": `while :; do sleep 0.05; done`, "name": "b"})
	defer func() {
		for _, ref := range []string{"a", "b"} {
			_, _ = th.e.TaskStop(context.Background(), th.auth, payload(map[string]any{"task": ref}))
		}
	}()
	plan, err := th.e.TaskWait(context.Background(), th.auth, payload(map[string]any{"until": "2026-10-26T09:00Z"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Wait.Match.Types) != 1 || plan.Wait.Match.Types[0] != claudecode.TypeTaskFinished || len(plan.Wait.Match.Keys) != 0 ||
		plan.Wait.Timeout == nil || !plan.Wait.Timeout.Equal(time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)) || len(plan.Dependencies) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	for _, d := range plan.Dependencies {
		if d.ID != a["task"] && d.ID != b["task"] {
			t.Fatalf("dependency %+v", d)
		}
		if d.SourceKey != (domain.CorrelationKey{Source: claudecode.Name, ExternalID: "task/" + d.ID}) || d.AliasType != claudecode.TypeTaskFinished ||
			len(d.Types) != 1 || d.Types[0] != claudecode.TypeTaskEnded || d.FromNow {
			t.Fatalf("dependency %+v", d)
		}
	}
	if plan, err = th.e.TaskWait(context.Background(), th.auth, payload(map[string]any{"tasks": []string{"b"}})); err != nil || len(plan.Dependencies) != 1 || plan.Dependencies[0].ID != b["task"] {
		t.Fatalf("named %+v %v", plan, err)
	}
	if _, err := th.e.TaskWait(context.Background(), th.auth, payload(map[string]any{"tasks": []string{"nope"}})); code(err) != "unknown_task" {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := th.e.TaskWait(context.Background(), th.auth, payload(map[string]any{"until": "2026-10-24T09:00Z"})); code(err) != "invalid_until" {
		t.Fatalf("past: %v", err)
	}
	// Another Work's caller sees none of these tasks.
	other := th.auth
	other.WorkID = "work/other"
	th.store.setWork(domain.Work{ID: "work/other", ScopeID: "eng", State: domain.WorkRunning})
	if _, err := th.e.TaskStop(context.Background(), other, payload(map[string]any{"task": "a"})); code(err) != "unknown_task" {
		t.Fatalf("another Work stopped a task: %v", err)
	}
}

func TestTaskStartRefusals(t *testing.T) {
	th, handle := liveTurn(t, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		in   map[string]any
		code string
	}{
		{map[string]any{}, "invalid_input"},
		{map[string]any{"command": "true", "name": "Bad Name"}, "invalid_input"},
		{map[string]any{"command": "true", "timeout_minutes": 100000}, "invalid_input"},
		{map[string]any{"command": "true", "extra": 1}, "invalid_input"},
	} {
		if _, err := th.e.TaskStart(ctx, th.auth, payload(tc.in)); code(err) != tc.code {
			t.Fatalf("%v: %v", tc.in, err)
		}
	}
	stale := th.auth
	stale.Lease.Epoch = 2
	if _, err := th.e.TaskStart(ctx, stale, payload(map[string]any{"command": "true"})); code(err) != "not_supported" {
		t.Fatalf("stale epoch: %v", err)
	}
	unknown := th.auth
	unknown.ExecutionID = "exec-other"
	if _, err := th.e.TaskStart(ctx, unknown, payload(map[string]any{"command": "true"})); code(err) != "not_supported" {
		t.Fatalf("unknown execution: %v", err)
	}
	th.release(handle)
	if _, err := th.e.TaskStart(ctx, th.auth, payload(map[string]any{"command": "true"})); err == nil {
		t.Fatal("started a task from a finished turn")
	}
}

// TestTaskRefusedInSandbox: a task would run outside the binding's sandbox,
// so a sandboxed turn starts none (fail closed).
func TestTaskRefusedInSandbox(t *testing.T) {
	h := newHarness(t)
	h.mode("wait")
	e := h.executor()
	r, _ := h.request("work/s", "exec-s", 1, nil)
	r.Context.Sandbox = &recordingBroker{}
	handle, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn started", func() bool { _, err := os.Stat(filepath.Join(h.fake, "pid")); return err == nil })
	auth := extension.Authority{ScopeID: "eng", WorkID: "work/s", ExecutionID: "exec-s", Lease: domain.LeaseRef{ExecutionID: "exec-s", Epoch: 1}, TriggerID: r.Trigger.ID}
	if _, err := e.TaskStart(context.Background(), auth, payload(map[string]any{"command": "true"})); code(err) != "not_supported" || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("sandboxed: %v", err)
	}
	_ = os.WriteFile(filepath.Join(h.fake, "release"), nil, 0o600)
	h.wait(handle)
}

// TestTurnEndingWithUnwatchedTasksIsFlagged: a turn that ends with its
// tasks running and no claude-code.task.wait leaves nothing to tell the Work
// when they end, so its result and telemetry say so; one that parked on them
// (the negative control) is not flagged.
func TestTurnEndingWithUnwatchedTasksIsFlagged(t *testing.T) {
	for _, parked := range []bool{false, true} {
		th, handle := liveTurn(t, nil)
		th.start(context.Background(), map[string]any{"command": `while :; do sleep 0.05; done`, "name": "bg"})
		if parked {
			if _, err := th.e.TaskWait(context.Background(), th.auth, domain.Payload{}); err != nil {
				t.Fatal(err)
			}
		}
		res := th.release(handle)
		var data map[string]any
		_ = json.Unmarshal(res.Outcome.Data.Data, &data)
		turns := th.reports.named("agent_turn")
		if parked {
			if data["tasks_unwatched"] != nil || len(turns) != 1 || turns[0]["tasks_unwatched"] != nil {
				t.Fatalf("parked turn flagged: %v %v", data, turns)
			}
		} else if data["tasks_unwatched"] != float64(1) || len(turns) != 1 || turns[0]["tasks_unwatched"] != 1 {
			t.Fatalf("unwatched task not flagged: %v %v", data, turns)
		}
		if _, err := th.e.TaskStop(context.Background(), th.auth, payload(map[string]any{"task": "bg"})); err != nil {
			t.Fatal(err)
		}
		th.ended(1)
	}
}

// TestTaskExitingDuringSweepIsNotLost: a wrapper that records its exit and
// quits between the sweep's liveness check and its read of the exit
// record is announced with its exit, not as lost (review of #557).
func TestTaskExitingDuringSweepIsNotLost(t *testing.T) {
	th, handle := liveTurn(t, nil)
	defer th.release(handle)
	task := th.start(context.Background(), map[string]any{"command": `while [ ! -e "$FAKE_DIR/go" ]; do sleep 0.05; done; echo built`, "name": "build"})
	id := task["task"].(string)
	waitFor(t, "the task's pid", func() bool {
		_, err := os.Stat(filepath.Join(th.state, "tasks", id, "pid"))
		return err == nil
	})
	// The wrapper records its exit and quits after the sweep has checked
	// whether it is alive and before it reads the exit record.
	exitFile := filepath.Join(th.state, "tasks", id, "exit")
	var once sync.Once
	restore := claudecode.OnTaskObserved(func(task string) {
		if task != id {
			return
		}
		once.Do(func() {
			if err := os.WriteFile(filepath.Join(th.fake, "go"), nil, 0o600); err != nil {
				t.Error(err)
			}
			waitFor(t, "the wrapper's exit", func() bool { _, err := os.Stat(exitFile); return err == nil })
		})
	})
	defer restore()
	events := th.ended(1)
	var p map[string]any
	_ = json.Unmarshal(events[0].Data.Data, &p)
	if p["outcome"] != claudecode.TaskSucceeded || p["last_line"] != "built" {
		t.Fatalf("payload %v", p)
	}
}
