package claudecode

// Tasks: commands an agent starts that outlive its turn (#556). When a
// segment's claude exits, its wrapper signals the whole process group, so
// anything the agent left running in the background goes with it. A task
// is started by the executor instead, in its own session and process
// group, in the segment's workspace and environment. Its outcome is
// recorded by its own wrapper, a background sweep announces its end as a
// claude-code.task.ended Event on the task's own key, and
// claude-code.task.wait parks the Work on that Event through an ordinary
// dependency, so the platform does the waiting and nothing has to stay
// alive for it.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/storage"
)

// The task operations, their Event and schemas.
const (
	OpTaskStart = Name + ".task.start"
	OpTaskList  = Name + ".task.list"
	OpTaskStop  = Name + ".task.stop"
	OpTaskWait  = Name + ".task.wait"

	// TypeTaskEnded is a task's end, accepted on the task's own key.
	TypeTaskEnded = Name + ".task.ended"
	// TypeTaskFinished is TypeTaskEnded aliased into a Work parked on the
	// task by claude-code.task.wait.
	TypeTaskFinished = Name + ".task.finished"
	// SchemaTask is TypeTaskEnded's payload.
	SchemaTask = Name + ".task.v1"
	// SchemaTool is the task operations' output.
	SchemaTool = Name + ".tool.v1"

	// ToolsetTasks grants the task operations.
	ToolsetTasks = Name + ".tasks"
)

// Task outcomes.
const (
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskStopped   = "stopped"
	TaskTimedOut  = "timed_out"
	// TaskLost is a task whose wrapper is gone without recording how the
	// command ended (killed outright, or the machine restarted).
	TaskLost = "lost"
)

// Task limits.
const (
	maxTaskCommand = 8 << 10
	maxTasksLive   = 8
	maxTaskLine    = 200
	// defaultTaskMinutes and maxTaskMinutes bound a task's run: one that
	// runs longer is stopped as timed_out, so a wait on it always ends.
	defaultTaskMinutes = 240
	maxTaskMinutes     = 24 * 60
	// taskStartGrace is how long a record may say starting with no
	// process found before the sweep counts it lost.
	taskStartGrace = time.Minute
	taskPrefix     = "task/"
	taskMarker     = "open-conduit-task:"
)

// taskWrapper records its PID, runs the command with the group's
// termination signals reaching it with default disposition, signals what
// is left of its own group, and records the exit status atomically: the
// segment wrapper's shape, for a group of the task's own.
const taskWrapper = `d=$1; shift
printf '%s\n' "$$" > "$d/pid.tmp" && mv -f "$d/pid.tmp" "$d/pid"
trap ':' HUP INT TERM
"$@"
c=$?
trap '' HUP INT TERM
kill -TERM 0 2>/dev/null
printf '%s\n' "$c" > "$d/exit.tmp" && mv -f "$d/exit.tmp" "$d/exit"
exit "$c"`

var taskName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

//go:embed guidance/tasks.md
var taskGuidance string

// TaskGuidance is the task operations' guidance, which the package
// declares in its manifest.
func TaskGuidance(op string) string {
	if op == OpTaskStart {
		return strings.TrimSpace(taskGuidance)
	}
	return ""
}

// TaskOperations are the task operations' names.
func TaskOperations() []string { return []string{OpTaskStart, OpTaskList, OpTaskStop, OpTaskWait} }

type taskStartInput struct {
	Command string `json:"command" doc:"the shell command, run with /bin/sh -c in your working directory, at most 8 KiB"`
	Name    string `json:"name,omitempty" doc:"a short name to refer to it by: lower case letters, digits, '.', '_' and '-', at most 40"`
	Minutes int    `json:"timeout_minutes,omitempty" doc:"stop it as timed_out after this many minutes (default 240, at most 1440)"`
}

type taskRefInput struct {
	Task string `json:"task" doc:"the task's id or name"`
}

type taskWaitInput struct {
	Tasks []string `json:"tasks,omitempty" doc:"the tasks' ids or names; omit for every task still running"`
	Until string   `json:"until,omitempty" doc:"a future ISO time with Z or an offset: you are resumed then if no task has ended"`
}

// TaskDeclarations declare the task operations (inproc.Declarer).
func TaskDeclarations() []extension.Operation {
	return []extension.Operation{
		{Name: OpTaskStart, Summary: "Start a command that keeps running after your turn ends, with its output in a log; claude-code.task.wait wakes you when it ends.",
			Input: extension.InputSchema[taskStartInput]()},
		{Name: OpTaskList, Summary: "List this Work's tasks: each one's state, exit code, last line of output and log.", Input: extension.InputSchema[struct{}]()},
		{Name: OpTaskStop, Summary: "Stop one of this Work's running tasks.", Input: extension.InputSchema[taskRefInput]()},
		{Name: OpTaskWait, Summary: "End your turn until one of the tasks ends (or until a time), then be resumed with how it ended.",
			Input: extension.InputSchema[taskWaitInput]()},
	}
}

// TaskToolsets are the extension's toolsets: the task operations.
func TaskToolsets() []extension.Toolset {
	ops := make([]extension.ToolsetOperation, 0, 4)
	for _, n := range TaskOperations() {
		ops = append(ops, extension.ToolsetOperation{Name: n, Since: 1})
	}
	return []extension.Toolset{{Name: ToolsetTasks, Summary: "Run commands that outlive the turn, and be woken when they end.", Operations: ops}}
}

// taskSpace is where a segment's tasks run: its Work, workspace and the
// environment its claude has, without the toolbelt's variables (a task
// outlives the segment's authority). refusal, when set, says why the
// segment's tasks can't run outside it.
type taskSpace struct {
	scope, work domain.ID
	dir         string
	env         []string
	refusal     string
}

func newTaskSpace(r extension.RunRequest, cfg Config, dir string, env []string) *taskSpace {
	s := &taskSpace{scope: r.Work.ScopeID, work: r.Work.ID, dir: dir}
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); !domain.ReservedEnv(name) {
			s.env = append(s.env, kv)
		}
	}
	switch {
	case r.Context.Container != nil:
		s.refusal = "this binding runs its turns in a container, which ends with the turn, so tasks can't run here yet"
	case r.Context.Sandbox != nil:
		s.refusal = "this binding runs its turns in a sandbox, and a task would run outside it, so tasks can't run here yet"
	case r.Context.Resources != nil && (cfg.WorkspaceMode == "fresh" || cfg.WorkspaceMode == "shared"):
		s.refusal = "this binding's workspace (" + cfg.WorkspaceMode + ") is given up when the turn ends, so a task would lose it"
	}
	return s
}

// taskRecord is a task's coordination record (KV task/<id>).
type taskRecord struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     domain.ID  `json:"scope"`
	Work      domain.ID  `json:"work"`
	Execution domain.ID  `json:"execution"`
	Trigger   domain.ID  `json:"trigger"`
	Nonce     string     `json:"nonce"`
	State     string     `json:"state"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Signal    string     `json:"signal,omitempty"`
	LastLine  string     `json:"last_line,omitempty"`
	Log       string     `json:"log"`
	Started   time.Time  `json:"started_at"`
	Deadline  time.Time  `json:"deadline"`
	Ended     *time.Time `json:"ended_at,omitempty"`
	Published bool       `json:"published,omitempty"`
}

func (t taskRecord) key() domain.CorrelationKey {
	return domain.CorrelationKey{Source: Name, ExternalID: taskPrefix + t.ID}
}

// view is what the agent is shown of a task.
func (t taskRecord) view() map[string]any {
	out := map[string]any{"task": t.ID, "name": t.Name, "state": t.State, "log": t.Log, "started_at": t.Started.Format(time.RFC3339)}
	if t.ExitCode != nil {
		out["exit_code"] = *t.ExitCode
	}
	if t.Signal != "" {
		out["signal"] = t.Signal
	}
	if t.LastLine != "" {
		out["last_line"] = t.LastLine
	}
	if t.Ended != nil {
		out["ended_at"] = t.Ended.Format(time.RFC3339)
	}
	return out
}

// taskStop is a stop request recorded beside the task's files.
type taskStop struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	Killed bool      `json:"killed,omitempty"`
}

// TaskHost is what the task sweep needs from the host to announce a
// task's end.
type TaskHost struct {
	// Accept accepts a derived Event of the extension's own family in the
	// extension's transaction (inproc.Host.AcceptDerivedInTx).
	Accept func(tx storage.Tx, scope, parent domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error)
	// Nudge asks the host loop for a prompt pass. Optional.
	Nudge func()
}

// tasks is the executor's task state: the host's accept and the
// in-process reapers of the tasks this instance started.
type tasks struct {
	mu   sync.Mutex
	host TaskHost
	live map[string]bool
}

// SetTaskHost gives the executor the host services the task sweep needs.
func (e *Executor) SetTaskHost(h TaskHost) {
	e.tasks.mu.Lock()
	e.tasks.host = h
	e.tasks.mu.Unlock()
}

func taskRefuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func decodeStrict(data []byte, v any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func toolOutput(v any) domain.Payload {
	data, _ := json.Marshal(v)
	return domain.Payload{Schema: SchemaTool, Data: data}
}

func (e *Executor) taskDir(id string) string { return filepath.Join(e.o.StateDir, "tasks", id) }

// taskID derives the task's identity from the execution and the caller's
// request ID, so a retried start starts nothing more; else it is fresh.
func taskID(ctx context.Context, a extension.Authority) string {
	if req := extension.CallRequestID(ctx); req != "" {
		h := sha256.Sum256([]byte(string(a.ExecutionID) + "\x00" + req))
		return hex.EncodeToString(h[:12])
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// space is the calling segment's task space, when this instance launched
// the live segment at the caller's epoch.
func (e *Executor) space(a extension.Authority) (*taskSpace, error) {
	e.mu.Lock()
	rn := e.runs[a.ExecutionID]
	e.mu.Unlock()
	if rn == nil {
		return nil, taskRefuse("not_supported", "tasks start only from a running turn of a claude-code binding")
	}
	rn.mu.Lock()
	epoch, space := rn.epoch, rn.space
	rn.mu.Unlock()
	if epoch != a.Lease.Epoch || space == nil {
		return nil, taskRefuse("not_supported", "this turn was taken over by a restarted daemon, which can't start tasks for it; start the task in your next turn")
	}
	if alive, err := e.leaderAlive(rn); err != nil || !alive {
		return nil, taskRefuse("not_supported", "this turn has ended; start the task in a running turn")
	}
	if space.work != a.WorkID || space.scope != a.ScopeID {
		return nil, extension.ErrUnauthorized
	}
	if space.refusal != "" {
		return nil, taskRefuse("not_supported", "%s", space.refusal)
	}
	return space, nil
}

// TaskStart starts a task (claude-code.task.start).
func (e *Executor) TaskStart(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var input taskStartInput
	if err := decodeStrict(in.Data, &input); err != nil {
		return domain.Payload{}, taskRefuse("invalid_input", "give {command, name?, timeout_minutes?}")
	}
	input.Command = strings.TrimSpace(input.Command)
	switch {
	case input.Command == "":
		return domain.Payload{}, taskRefuse("invalid_input", "command is required")
	case len(input.Command) > maxTaskCommand || !utf8.ValidString(input.Command) || strings.ContainsRune(input.Command, 0):
		return domain.Payload{}, taskRefuse("invalid_input", "command must be text of at most %d bytes", maxTaskCommand)
	case input.Name != "" && !taskName.MatchString(input.Name):
		return domain.Payload{}, taskRefuse("invalid_input", "name must be lower case letters, digits, '.', '_' and '-', at most 40")
	case input.Minutes < 0 || input.Minutes > maxTaskMinutes:
		return domain.Payload{}, taskRefuse("invalid_input", "timeout_minutes must be between 1 and %d", maxTaskMinutes)
	}
	if input.Minutes == 0 {
		input.Minutes = defaultTaskMinutes
	}
	space, err := e.space(a)
	if err != nil {
		return domain.Payload{}, err
	}
	id := taskID(ctx, a)
	if input.Name == "" {
		input.Name = "task-" + id[:6]
	}
	nonce, err := newNonce()
	if err != nil {
		return domain.Payload{}, err
	}
	now := e.o.Now().UTC()
	dir := e.taskDir(id)
	rec := taskRecord{ID: id, Name: input.Name, Scope: a.ScopeID, Work: a.WorkID, Execution: a.ExecutionID, Trigger: a.TriggerID,
		Nonce: nonce, State: TaskRunning, Log: filepath.Join(dir, "log"), Started: now, Deadline: now.Add(time.Duration(input.Minutes) * time.Minute)}
	var existing bool
	err = e.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var old taskRecord
		if found, err := getJSON(tx, e.o.Namespace, taskPrefix+id, &old); err != nil {
			return err
		} else if found {
			rec, existing = old, true
			return nil
		}
		all, err := e.workTasksInTx(tx, a.WorkID)
		if err != nil {
			return err
		}
		running := 0
		for _, t := range all {
			if t.State == TaskRunning {
				running++
				if t.Name == input.Name {
					return taskRefuse("name_taken", "a task named %s is still running; stop it or give another name", input.Name)
				}
			}
		}
		if running >= maxTasksLive {
			return taskRefuse("too_many_tasks", "this Work already has %d tasks running; wait for or stop one first", running)
		}
		// The record comes first: a crash before the process starts leaves
		// a record the sweep counts lost, never a process nothing knows of.
		return putJSON(tx, e.o.Namespace, taskPrefix+id, rec)
	})
	if err != nil {
		return domain.Payload{}, err
	}
	if !existing {
		if err := e.spawnTask(rec, space, input.Command); err != nil {
			e.report("task_start_failed", map[string]any{"work": string(a.WorkID), "task": id})
			// The sweep finds no process and records the task lost.
			return domain.Payload{}, taskRefuse("start_failed", "the task could not be started")
		}
		e.report("task_started", map[string]any{"work": string(a.WorkID), "execution": string(a.ExecutionID), "task": id, "name": rec.Name})
	}
	out := rec.view()
	out["next"] = "the task keeps running after your turn; call " + OpTaskWait + " as the last thing in your turn to be woken when it ends"
	return toolOutput(out), nil
}

// spawnTask starts the task's wrapper as a session and group leader of its
// own, so neither the segment's group signal nor the daemon reaches it.
func (e *Executor) spawnTask(rec taskRecord, space *taskSpace, command string) error {
	dir := e.taskDir(rec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(rec.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command("/bin/sh", "-c", taskWrapper, taskMarker+rec.Nonce, dir, "/bin/sh", "-c", command)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = space.dir, space.env, log, log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	e.tasks.mu.Lock()
	if e.tasks.live == nil {
		e.tasks.live = map[string]bool{}
	}
	e.tasks.live[rec.ID] = true
	e.tasks.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		e.tasks.mu.Lock()
		delete(e.tasks.live, rec.ID)
		nudge := e.tasks.host.Nudge
		e.tasks.mu.Unlock()
		// Announce it now rather than at the next sweep.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = e.SweepTasks(ctx)
		cancel()
		if nudge != nil {
			nudge()
		}
	}()
	return nil
}

// workTasksInTx are a Work's tasks, oldest first.
func (e *Executor) workTasksInTx(tx storage.Tx, work domain.ID) ([]taskRecord, error) {
	entries, err := tx.List(e.o.Namespace, taskPrefix)
	if err != nil {
		return nil, err
	}
	var out []taskRecord
	for _, entry := range entries {
		var t taskRecord
		if json.Unmarshal(entry.Value, &t) == nil && t.Work == work {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, nil
}

// resolve finds the caller's Work's task by id, or by name (the latest of
// that name).
func resolveTask(all []taskRecord, ref string) (taskRecord, bool) {
	ref = strings.TrimSpace(ref)
	for _, t := range all {
		if t.ID == ref {
			return t, true
		}
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Name == ref {
			return all[i], true
		}
	}
	return taskRecord{}, false
}

func (e *Executor) authorizedTasks(ctx context.Context, a extension.Authority) ([]taskRecord, error) {
	var all []taskRecord
	err := e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		w, err := tx.Work(a.WorkID)
		if err != nil || w.ScopeID != a.ScopeID {
			return extension.ErrUnauthorized
		}
		all, err = e.workTasksInTx(tx, a.WorkID)
		return err
	})
	return all, err
}

// TaskList lists the Work's tasks (claude-code.task.list).
func (e *Executor) TaskList(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	if err := decodeStrict(in.Data, &struct{}{}); err != nil {
		return domain.Payload{}, taskRefuse("invalid_input", "give {}")
	}
	all, err := e.authorizedTasks(ctx, a)
	if err != nil {
		return domain.Payload{}, err
	}
	views := make([]map[string]any, 0, len(all))
	for _, t := range all {
		views = append(views, t.view())
	}
	return toolOutput(map[string]any{"tasks": views}), nil
}

// TaskStop stops a running task (claude-code.task.stop): SIGTERM to its
// group now, SIGKILL from the sweep after the grace period.
func (e *Executor) TaskStop(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var input taskRefInput
	if err := decodeStrict(in.Data, &input); err != nil || strings.TrimSpace(input.Task) == "" {
		return domain.Payload{}, taskRefuse("invalid_input", "give {task}")
	}
	all, err := e.authorizedTasks(ctx, a)
	if err != nil {
		return domain.Payload{}, err
	}
	t, ok := resolveTask(all, input.Task)
	if !ok {
		return domain.Payload{}, taskRefuse("unknown_task", "this Work has no task %q", input.Task)
	}
	if t.State == TaskRunning {
		if err := e.stopTask(t, TaskStopped); err != nil {
			return domain.Payload{}, err
		}
	}
	out := t.view()
	if t.State == TaskRunning {
		out["state"] = "stopping"
	}
	return toolOutput(out), nil
}

// stopTask records why the task is being stopped (the first reason wins)
// and signals its verified group.
func (e *Executor) stopTask(t taskRecord, reason string) error {
	dir := e.taskDir(t.ID)
	path := filepath.Join(dir, "stop.json")
	if !exists(path) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := writeJSON(path, taskStop{Reason: reason, At: e.o.Now().UTC()}); err != nil {
			return err
		}
	}
	pid, ok := e.taskLeader(t)
	if !ok {
		return nil
	}
	return signalGroup(pid, sigTerm)
}

// taskLeader is the task wrapper's PID, re-verified as a group leader
// carrying the task's nonce, so a reused PID is never signaled.
func (e *Executor) taskLeader(t taskRecord) (int, bool) {
	b, err := os.ReadFile(filepath.Join(e.taskDir(t.ID), "pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	p, ok, err := inspectProcess(pid)
	if err != nil || !ok || p.PGID != p.PID || !strings.Contains(p.Args, taskMarker+t.Nonce) {
		return 0, false
	}
	return pid, true
}

// TaskWait parks the Work until one of the tasks ends (claude-code.task.wait).
// An already ended task wakes it at once (the dependency scans history).
func (e *Executor) TaskWait(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
	var input taskWaitInput
	if err := decodeStrict(in.Data, &input); err != nil {
		return extension.WaitPlan{}, taskRefuse("invalid_input", "give {tasks?, until?}")
	}
	var until *time.Time
	if input.Until != "" {
		t, err := parseUntil(input.Until)
		if err != nil {
			return extension.WaitPlan{}, taskRefuse("invalid_until", "until must be an ISO time with Z or an offset, such as 2026-10-25T09:00Z")
		}
		if !t.After(e.o.Now()) {
			return extension.WaitPlan{}, taskRefuse("invalid_until", "%s has already passed", input.Until)
		}
		until = &t
	}
	all, err := e.authorizedTasks(ctx, a)
	if err != nil {
		return extension.WaitPlan{}, err
	}
	var chosen []taskRecord
	if len(input.Tasks) == 0 {
		for _, t := range all {
			if t.State == TaskRunning {
				chosen = append(chosen, t)
			}
		}
		if len(chosen) == 0 {
			return extension.WaitPlan{}, taskRefuse("no_running_tasks", "this Work has no task running; there is nothing to wait for")
		}
	}
	for _, ref := range input.Tasks {
		t, ok := resolveTask(all, ref)
		if !ok {
			return extension.WaitPlan{}, taskRefuse("unknown_task", "this Work has no task %q", ref)
		}
		if !slices.ContainsFunc(chosen, func(c taskRecord) bool { return c.ID == t.ID }) {
			chosen = append(chosen, t)
		}
	}
	// The segment parked on its tasks: its turn's end isn't flagged.
	if err := e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		return putJSON(tx, e.o.Namespace, taskWaitKey(a.ExecutionID), taskWaited{Work: a.WorkID, Epoch: a.Lease.Epoch})
	}); err != nil {
		return extension.WaitPlan{}, err
	}
	plan := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{TypeTaskFinished}}, Timeout: until}}
	names := make([]string, 0, len(chosen))
	for _, t := range chosen {
		names = append(names, t.Name)
		plan.Dependencies = append(plan.Dependencies, extension.Dependency{ID: t.ID, SourceKey: t.key(), Types: []string{TypeTaskEnded},
			AliasType: TypeTaskFinished, Label: "task " + t.Name})
	}
	plan.Wait.Description = "task " + strings.Join(names, ", ") + " to end"
	if len(names) > 1 {
		plan.Wait.Description = "one of the tasks " + strings.Join(names, ", ") + " to end"
	}
	plan.Output = toolOutput(map[string]any{"waiting": names, "next": "end your turn now; any other call is refused until a task ends and you are resumed with how it went"})
	return plan, nil
}

const taskWaitPrefix = "taskwait/"

func taskWaitKey(execution domain.ID) string { return taskWaitPrefix + hashKey(string(execution)) }

// taskWaited records that an execution's segment at Epoch parked on its
// tasks.
type taskWaited struct {
	Work  domain.ID `json:"work"`
	Epoch uint64    `json:"epoch"`
}

// unwatchedTasks counts the Work's running tasks when the segment at epoch
// declared no claude-code.task.wait: a turn that ends so leaves nothing to
// tell the Work when they end.
func (e *Executor) unwatchedTasks(ctx context.Context, work, execution domain.ID, epoch uint64) int {
	n := 0
	_ = e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		n = 0
		var waited taskWaited
		if found, err := getJSON(tx, e.o.Namespace, taskWaitKey(execution), &waited); err != nil || found && waited.Epoch == epoch {
			return err
		}
		all, err := e.workTasksInTx(tx, work)
		for _, t := range all {
			if t.State == TaskRunning {
				n++
			}
		}
		return err
	})
	return n
}

func parseUntil(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q", s)
}

// TaskSweepReport counts one sweep's work.
type TaskSweepReport struct {
	Ended, Stopped, Released int
}

// SweepTasks reconciles every task record with its process and files, and
// announces each task that ended: an exit recorded by its wrapper, a
// wrapper gone without one (lost), a task past its deadline (stopped as
// timed_out), or one whose Work finished (stopped). It is safe to repeat
// and after restart: the announcement and the record's update commit
// together, and the Event's delivery identity makes a repeat a duplicate.
func (e *Executor) SweepTasks(ctx context.Context) (TaskSweepReport, error) {
	var report TaskSweepReport
	e.tasks.mu.Lock()
	host := e.tasks.host
	e.tasks.mu.Unlock()
	now := e.o.Now().UTC()
	var records []taskRecord
	terminal := map[domain.ID]bool{}
	if err := e.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		records, terminal = nil, map[domain.ID]bool{}
		entries, err := tx.List(e.o.Namespace, taskPrefix)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			var t taskRecord
			if json.Unmarshal(entry.Value, &t) != nil || t.ID == "" {
				continue
			}
			records = append(records, t)
			if _, err := workDone(tx, terminal, t.Work); err != nil {
				return err
			}
		}
		// What finished Work's segments recorded of their waits goes too.
		waits, err := tx.List(e.o.Namespace, taskWaitPrefix)
		if err != nil {
			return err
		}
		for _, entry := range waits {
			var w taskWaited
			if json.Unmarshal(entry.Value, &w) != nil {
				continue
			}
			if done, err := workDone(tx, terminal, w.Work); err != nil {
				return err
			} else if done {
				if err := tx.Delete(e.o.Namespace, entry.Key); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return report, err
	}
	for _, t := range records {
		if t.State != TaskRunning {
			if terminal[t.Work] {
				if err := e.releaseTask(ctx, t); err != nil {
					return report, err
				}
				report.Released++
			}
			continue
		}
		ended, stopped := e.observeTask(t, now, terminal[t.Work])
		if stopped {
			report.Stopped++
		}
		if ended == nil {
			continue
		}
		if err := e.announceTask(ctx, host, *ended, now); err != nil {
			return report, err
		}
		report.Ended++
	}
	return report, nil
}

// workDone reports whether the Work is terminal or gone, remembering the
// answer in seen.
func workDone(tx storage.Tx, seen map[domain.ID]bool, id domain.ID) (bool, error) {
	if done, ok := seen[id]; ok {
		return done, nil
	}
	w, err := tx.Work(id)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		seen[id] = true
	case err != nil:
		return false, err
	default:
		seen[id] = w.State.Terminal()
	}
	return seen[id], nil
}

// observeTask reads a running task's files and process. It returns the
// ended record when the task has ended, and whether it signaled it.
func (e *Executor) observeTask(t taskRecord, now time.Time, workDone bool) (*taskRecord, bool) {
	dir := e.taskDir(t.ID)
	var stop taskStop
	stopped := readJSON(filepath.Join(dir, "stop.json"), &stop) == nil
	if b, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil {
		code, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			code = -1
		}
		t.ExitCode = &code
		switch {
		case stopped && stop.Reason == TaskTimedOut:
			t.State = TaskTimedOut
		case stopped:
			t.State = TaskStopped
		case code == 0:
			t.State = TaskSucceeded
		default:
			t.State = TaskFailed
		}
		if code > 128 && code < 128+65 {
			t.Signal = signalName(code - 128)
		}
		return e.ended(t, now), false
	}
	e.tasks.mu.Lock()
	reaping := e.tasks.live[t.ID]
	e.tasks.mu.Unlock()
	pid, alive := e.taskLeader(t)
	if !alive && !reaping {
		if !exists(filepath.Join(dir, "pid")) && now.Sub(t.Started) < taskStartGrace {
			return nil, false // still starting
		}
		if !stopped || !stop.Killed {
			t.State = TaskLost
		} else if stop.Reason == TaskTimedOut {
			t.State = TaskTimedOut
		} else {
			t.State = TaskStopped
		}
		return e.ended(t, now), false
	}
	if !alive {
		return nil, false
	}
	switch {
	case stopped && !stop.Killed && now.Sub(stop.At) >= e.o.Grace:
		stop.Killed = true
		_ = writeJSON(filepath.Join(dir, "stop.json"), stop)
		_ = signalGroup(pid, sigKill)
		return nil, true
	case stopped:
		return nil, false
	case workDone:
		_ = e.stopTask(t, TaskStopped)
		return nil, true
	case !now.Before(t.Deadline):
		_ = e.stopTask(t, TaskTimedOut)
		return nil, true
	}
	return nil, false
}

func (e *Executor) ended(t taskRecord, now time.Time) *taskRecord {
	at := now
	if info, err := os.Stat(filepath.Join(e.taskDir(t.ID), "exit")); err == nil && !info.ModTime().After(now) && !info.ModTime().Before(t.Started) {
		at = info.ModTime().UTC()
	}
	t.Ended = &at
	t.LastLine = lastLine(t.Log)
	return &t
}

// announceTask accepts the task's end on its key and records it, in one
// transaction.
func (e *Executor) announceTask(ctx context.Context, host TaskHost, t taskRecord, now time.Time) error {
	data, _ := json.Marshal(taskEnded{Task: t.ID, Name: t.Name, Work: t.Work, Outcome: t.State, ExitCode: t.ExitCode, Signal: t.Signal,
		LastLine: t.LastLine, Log: t.Log, StartedAt: t.Started, EndedAt: *t.Ended})
	key := t.key()
	ev := domain.Event{Type: TypeTaskEnded, Source: Name, DeliveryID: taskPrefix + t.ID + "/ended", OccurredAt: *t.Ended,
		CorrelationKey: &key, Data: domain.Payload{Schema: SchemaTask, Data: data}}
	err := e.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var cur taskRecord
		if found, err := getJSON(tx, e.o.Namespace, taskPrefix+t.ID, &cur); err != nil || !found || cur.State != TaskRunning {
			return err
		}
		t.Published = host.Accept != nil && t.Trigger != ""
		if t.Published {
			// A refusal by the causal limits is recorded by the host for
			// the operator; the task has still ended.
			if _, _, err := host.Accept(tx, t.Scope, t.Trigger, ev); errors.Is(err, inproc.ErrCausalLimit) {
				t.Published = false
			} else if err != nil {
				return err
			}
		}
		return putJSON(tx, e.o.Namespace, taskPrefix+t.ID, t)
	})
	if errors.Is(err, inproc.ErrCausalLimit) {
		err = nil // over the protocol the refusal arrives at commit
	}
	if err == nil {
		attrs := map[string]any{"work": string(t.Work), "task": t.ID, "name": t.Name, "outcome": t.State}
		if t.ExitCode != nil {
			attrs["exit_code"] = *t.ExitCode
		}
		e.report("task_ended", attrs)
	}
	return err
}

// releaseTask drops an announced task of finished Work: its record and
// files.
func (e *Executor) releaseTask(ctx context.Context, t taskRecord) error {
	if err := e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		return tx.Delete(e.o.Namespace, taskPrefix+t.ID)
	}); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return os.RemoveAll(e.taskDir(t.ID))
}

// taskEnded is TypeTaskEnded's payload.
type taskEnded struct {
	Task      string    `json:"task"`
	Name      string    `json:"name"`
	Work      domain.ID `json:"work"`
	Outcome   string    `json:"outcome"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Signal    string    `json:"signal,omitempty"`
	LastLine  string    `json:"last_line,omitempty"`
	Log       string    `json:"log"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// lastLine is the log's last non-empty line, at most maxTaskLine bytes.
func lastLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 4096
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		_, _ = f.Seek(info.Size()-tail, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, tail))
	lines := strings.Split(strings.ReplaceAll(string(b), "\r", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			for len(l) > maxTaskLine || !utf8.ValidString(l) {
				l = l[:min(len(l), maxTaskLine)]
				for !utf8.ValidString(l) && l != "" {
					l = l[:len(l)-1]
				}
			}
			return l
		}
	}
	return ""
}

func signalName(n int) string {
	switch syscall.Signal(n) {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	}
	return "signal " + strconv.Itoa(n)
}

// RenderTask renders a task's end, and its alias into a waiting Work,
// for the agent's next turn. lookup reads the aliased Event.
func RenderTask(e domain.Event, lookup func(domain.ID) (domain.Event, bool)) (string, bool) {
	src := e
	switch e.Type {
	case TypeTaskEnded:
	case TypeTaskFinished:
		if lookup == nil {
			return "### A task ended\nHow it ended could not be read; call " + OpTaskList + ".", true
		}
		var ok bool
		if src, ok = lookup(e.CausationID); !ok || src.Type != TypeTaskEnded {
			return "### A task ended\nHow it ended could not be read; call " + OpTaskList + ".", true
		}
	default:
		return "", false
	}
	var p taskEnded
	if src.Data.Schema != SchemaTask || json.Unmarshal(src.Data.Data, &p) != nil {
		return "### A task ended\nHow it ended could not be read; call " + OpTaskList + ".", true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Task %s %s\n", p.Name, taskVerb(p.Outcome))
	fmt.Fprintf(&b, "Task: %s\nOutcome: %s\n", p.Task, p.Outcome)
	if p.ExitCode != nil {
		fmt.Fprintf(&b, "Exit code: %d\n", *p.ExitCode)
	}
	if p.Signal != "" {
		fmt.Fprintf(&b, "Signal: %s\n", p.Signal)
	}
	fmt.Fprintf(&b, "Ran: %s to %s\n", p.StartedAt.Format(time.RFC3339), p.EndedAt.Format(time.RFC3339))
	if p.LastLine != "" {
		fmt.Fprintf(&b, "Last line: %s\n", p.LastLine)
	}
	fmt.Fprintf(&b, "Log: %s", p.Log)
	return b.String(), true
}

func taskVerb(outcome string) string {
	switch outcome {
	case TaskSucceeded:
		return "succeeded"
	case TaskFailed:
		return "failed"
	case TaskStopped:
		return "was stopped"
	case TaskTimedOut:
		return "timed out"
	case TaskLost:
		return "was lost (its process ended without recording how)"
	}
	return "ended"
}
