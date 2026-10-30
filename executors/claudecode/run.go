package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// run is one launched segment. done, stdin and the follower exist only in
// the instance that started the process; adopted segments are observed by
// polling and take no injected input.
type run struct {
	id    domain.ID
	dir   string
	nonce string
	mu    sync.Mutex
	epoch uint64
	pid   int
	done  chan struct{}
	state *os.ProcessState

	stdin   io.WriteCloser
	open    bool
	offset  int64
	partial string
	waiting []pendingInput
}

type pendingInput struct {
	id   domain.ID
	text string
}

// turnRecord is the segment's own description of its turn, written before
// launch and read back when it is finalized (possibly by a later instance).
type turnRecord struct {
	ExecutionID  string    `json:"execution_id"`
	WorkID       string    `json:"work_id"`
	ScopeID      string    `json:"scope_id"`
	Nonce        string    `json:"nonce"`
	StartedAt    time.Time `json:"started_at"`
	Kind         turnKind  `json:"kind"`
	Resume       bool      `json:"resume"`
	Structured   bool      `json:"structured"`
	StatusSchema string    `json:"status_schema"`
	FollowUps    int       `json:"follow_ups"`
	InputTypes   []string  `json:"input_types,omitempty"`
}

type processRecord struct {
	PID int `json:"pid"`
}
type stopRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// injected records input handed to the running turn: accepted for delivery,
// and consumed once claude echoed it back.
type injected struct {
	Accepted []string `json:"accepted"`
	Consumed []string `json:"consumed"`
}

// workspaceRecord is the acquired resource's opaque lease reference and
// description, kept with the segment and reported in its result.
type workspaceRecord struct {
	Ref     string          `json:"ref"`
	Dir     string          `json:"dir,omitempty"`
	Schema  string          `json:"schema,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Summary string          `json:"summary,omitempty"`
}

// historyWorkspace is the generic history record of the segment's resource
// (extension.Result.Workspace), read before the private copy drops Dir.
func historyWorkspace(dir string) *extension.WorkspaceRecord {
	var ws workspaceRecord
	if readJSON(filepath.Join(dir, "workspace.json"), &ws) != nil || ws.Ref == "" {
		return nil
	}
	return &extension.WorkspaceRecord{Kind: ws.Kind, Ref: ws.Ref, Dir: ws.Dir, Summary: ws.Summary, Data: domain.Payload{Schema: ws.Schema, Data: ws.Data}}
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// launch persists recovery intent before anything else, so a crash can never
// leave an unacknowledged launch that recovery cannot find. prev is the
// parked attempt's metadata when resuming (its workspace is looked up, never
// created).
func (e *Executor) launch(ctx context.Context, r extension.RunRequest, prev domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.PersistRecovery == nil || r.Execution.ID == "" || r.Work.ID == "" {
		return nil, extension.ErrInvalid
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	key := hashKey(string(r.Execution.ID))
	dir := filepath.Join(e.o.StateDir, "runs", key, nonce)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	rn := &run{id: r.Execution.ID, dir: dir, nonce: nonce, epoch: r.Execution.Lease.Epoch}
	if err = writeLease(dir, rn.epoch); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	data, _ := json.Marshal(recovery{Run: key + "/" + nonce})
	if err = r.PersistRecovery(domain.Payload{Schema: RecoverySchema, Data: data}); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	cfg, err := ParseConfig(r.Context.Config.Data)
	if err != nil {
		return e.preset(ctx, rn, ResultSchema, "invalid_config", "invalid claude code binding config", nil)
	}
	now := e.o.Now().UTC()
	// A usage limit hit by any turn holds every turn on this executor until it
	// lifts; a held turn fails without a process and retries after the reset.
	var limit limitRecord
	if err = e.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		_, err := getJSON(tx, e.o.Namespace, limitKey, &limit)
		return err
	}); err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "continuity_unavailable", "claude code continuity unavailable", nil)
	}
	if now.Before(limit.Until) {
		reset := limit.Until.Add(-limitGrace)
		if reset.Before(now) {
			reset = now
		}
		return e.preset(ctx, rn, cfg.StatusSchema, "usage_limit_hold", "the usage limit holds turns until "+limit.Until.Format(time.RFC3339), &extension.RetryHint{ResetAt: &reset})
	}
	errLog, err := os.OpenFile(filepath.Join(dir, "err.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "start_failed", "could not prepare claude process", nil)
	}
	defer errLog.Close()
	// The workspace: the binding's resource (a work worktree the session's
	// transcript is tied to), the configured dir used as-is, or a private
	// directory stable for the Work.
	workdir := filepath.Join(e.o.StateDir, "work", hashKey(string(r.Work.ID)))
	var layer map[string]string
	if cfg.Dir != "" && r.Context.Resources != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "invalid_config", "a binding with a resource cannot also set dir; remove one", nil)
	}
	if cfg.Dir != "" {
		if !isDir(cfg.Dir) {
			return e.preset(ctx, rn, cfg.StatusSchema, "workspace_unavailable", "the binding's dir is not an existing directory", nil)
		}
		workdir = cfg.Dir
	} else if r.Context.Resources != nil {
		res, err := e.workspace(ctx, r, cfg, prev, errLog)
		if res.Ref != "" {
			_ = writeJSON(filepath.Join(dir, "workspace.json"), workspaceRecord{Ref: res.Ref, Dir: res.Dir, Schema: res.Data.Schema, Data: res.Data.Data, Kind: res.Kind, Summary: res.Summary})
		}
		switch {
		case errors.Is(err, extension.ErrResourceSetup):
			return e.preset(ctx, rn, cfg.StatusSchema, "workspace_setup_failed", "workspace setup failed", nil)
		case errors.Is(err, extension.ErrResourceExhausted):
			// Refused before anything was made (a full disk); the provider's
			// message says what to free.
			return e.preset(ctx, rn, cfg.StatusSchema, "workspace_no_space", err.Error(), nil)
		case err != nil || res.Dir == "":
			return e.preset(ctx, rn, cfg.StatusSchema, "workspace_unavailable", unavailable(err), nil)
		}
		workdir, layer = res.Dir, res.Env
	} else if err = os.MkdirAll(workdir, 0o700); err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "start_failed", "could not prepare claude process", nil)
	}
	env, err := e.environment(cfg, r, layer)
	if err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "invalid_config", "invalid claude code binding config", nil)
	}
	if cfg.PluginDir == "" && cfg.Playbook != "-" {
		cfg.PluginDir = e.o.PluginDir
	}
	configDir := claudeConfigDir(env)
	sessionExists := func(session string) bool { return transcriptExists(configDir, workdir, session) }
	attach := attachInfo{Dir: workdir, Binary: cfg.Binary, Model: cfg.model(r.Work), PermissionMode: cfg.PermissionMode, PluginDir: cfg.PluginDir, ExtraArgs: append(cfg.toolArgs(), cfg.ExtraArgs...)}

	// Decide resume or fresh and record the decision before launch, fenced by
	// this attempt's lease, so a restart reuses the same decision.
	var d decision
	var rec record
	err = e.o.Store.Transact(ctx, e.guard(r.Execution.ID, r.Execution.Lease), func(tx storage.Tx) error {
		var err error
		if rec, err = e.loadRecord(tx, r.Work); err != nil {
			return err
		}
		d = e.decide(rec, cfg, now, sessionExists)
		beginTurn(&rec, d, r.Execution.ID, nonce, now, attach)
		return putJSON(tx, e.o.Namespace, workKey(r.Work.ID), rec)
	})
	if err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "continuity_unavailable", "claude code continuity unavailable", nil)
	}

	inputs := r.Inputs
	if len(inputs) == 0 && r.Trigger.ID != "" {
		inputs = []domain.Event{r.Trigger}
	}
	args, text := body(d.Kind, expand(cfg.Args, r.Work), rec.Handoff, e.o.Render(inputs))
	text = withAdditions(text, projectAdditions(workdir, cfg.Playbook))
	first := prompt(cfg.Playbook, args, text)
	model := cfg.model(r.Work)
	if e.o.DryRun {
		model = e.o.DryRunModel
		if model == "" {
			model = "haiku"
		}
		first = dryRunPrompt(first, cfg.structured())
	}
	turn := turnRecord{ExecutionID: string(r.Execution.ID), WorkID: string(r.Work.ID), ScopeID: string(r.Work.ScopeID), Nonce: nonce, StartedAt: now, Kind: d.Kind, Resume: d.Resume, Structured: cfg.structured(), StatusSchema: cfg.StatusSchema, FollowUps: len(inputs)}
	for _, ev := range inputs {
		turn.InputTypes = append(turn.InputTypes, ev.Type)
	}
	if err = writeJSON(filepath.Join(dir, "turn.json"), turn); err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "start_failed", "could not prepare claude process", nil)
	}
	argv := []string{cfg.Binary, "-p", "--model", model, "--permission-mode", cfg.PermissionMode,
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages"}
	if cfg.PluginDir != "" {
		argv = append(argv, "--plugin-dir", cfg.PluginDir)
	}
	if cfg.structured() {
		schema, _ := json.Marshal(StatusSchema)
		argv = append(argv, "--json-schema", string(schema))
	}
	if d.Resume {
		argv = append(argv, "--resume", d.Session)
	} else {
		argv = append(argv, "--session-id", d.Session)
	}
	if mcp, err := e.mcpConfig(r, dir); err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "start_failed", "could not prepare claude process", nil)
	} else if mcp != "" {
		argv = append(argv, "--mcp-config", mcp)
	}
	argv = append(argv, cfg.toolArgs()...)
	argv = append(argv, cfg.ExtraArgs...)
	out, err := os.OpenFile(filepath.Join(dir, "out.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "start_failed", "could not prepare claude process", nil)
	}
	defer out.Close()
	cmd := exec.Command("/bin/sh", append([]string{"-c", wrapper, marker + nonce, dir}, argv...)...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = workdir, env, out, errLog
	detach(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "start_failed", "could not prepare claude process", nil)
	}
	if err = cmd.Start(); err != nil {
		return e.preset(ctx, rn, cfg.StatusSchema, "start_failed", "could not start claude process", nil)
	}
	rn.pid, rn.done, rn.stdin, rn.open = cmd.Process.Pid, make(chan struct{}), stdin, true
	_ = writeJSON(filepath.Join(dir, "process.json"), processRecord{PID: rn.pid})
	rn.mu.Lock()
	rn.write(first)
	rn.mu.Unlock()
	go func() {
		state, _ := cmd.Process.Wait()
		rn.mu.Lock()
		rn.state = state
		rn.mu.Unlock()
		close(rn.done)
	}()
	hold := testHookFollow
	go func() {
		if hold != nil {
			hold()
		}
		e.follow(rn)
	}()
	e.register(rn)
	return &handle{e: e, r: rn}, nil
}

// workspace acquires the binding's resource for a new segment, or looks up
// the parked attempt's lease when resuming (Lookup never creates).
func (e *Executor) workspace(ctx context.Context, r extension.RunRequest, cfg Config, prev domain.Payload, log io.Writer) (extension.Resource, error) {
	if prevDir, _, ok := e.segmentDir(r.Execution.ID, prev); ok {
		var ws workspaceRecord
		if readJSON(filepath.Join(prevDir, "workspace.json"), &ws) == nil && ws.Ref != "" {
			return r.Context.Resources.Lookup(ctx, ws.Ref)
		}
	}
	req := extension.ResourceRequest{Mode: cfg.WorkspaceMode, Log: log}
	if cfg.Branch != "" {
		params, _ := json.Marshal(map[string]string{"branch": expand(cfg.Branch, r.Work)})
		req.Params = domain.Payload{Schema: "open-conduit.git.request.v1", Data: params}
	}
	return r.Context.Resources.Acquire(ctx, req)
}

// mcpConfig writes the toolbelt's MCP server definition for this segment:
// `os mcp` with the endpoint and the replaceable credential file. Without a
// toolbelt token or `os` binary the segment gets no toolbelt.
func (e *Executor) mcpConfig(r extension.RunRequest, dir string) (string, error) {
	if r.Context.ToolbeltToken == "" || r.Context.ToolbeltEndpoint == "" || e.o.ToolbeltDir == "" {
		return "", nil
	}
	token := r.Context.ToolbeltTokenFile
	if token == "" {
		token = filepath.Join(dir, "token")
		if err := writeFile(token, []byte(r.Context.ToolbeltToken+"\n")); err != nil {
			return "", err
		}
	}
	doc := map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{
		"type":    "stdio",
		"command": filepath.Join(e.o.ToolbeltDir, "os"),
		"args":    []string{"mcp"},
		"env":     map[string]string{"OS_ENDPOINT": r.Context.ToolbeltEndpoint, "OS_TOKEN_FILE": token},
	}}}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp.json")
	return path, writeFile(path, append(b, '\n'))
}

// preset finishes a segment that never started a process with a structured
// failed result, so no uncertain resource hold is left behind.
func (e *Executor) preset(ctx context.Context, rn *run, schema, reason, summary string, hint *extension.RetryHint) (extension.RunHandle, error) {
	f := resultFields{Reason: reason}
	if hint != nil && hint.ResetAt != nil {
		f.Limited, f.ResetsAt = true, hint.ResetAt
	}
	var ws workspaceRecord
	if readJSON(filepath.Join(rn.dir, "workspace.json"), &ws) == nil && ws.Ref != "" {
		ws.Dir = ""
		f.Workspace = &ws
	}
	res := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: summary, Data: resultData(schema, f)}, RetryHint: hint, Workspace: historyWorkspace(rn.dir)}
	if _, err := e.finalizeWith(ctx, rn, &res); err != nil {
		return nil, err
	}
	e.register(rn)
	return &handle{e: e, r: rn}, nil
}

// environment applies the B05 order: sanitized baseline → resource-provider
// layer → binding env → selected secrets → trusted execution identity, which
// no earlier layer can override. Toolbelt credentials never enter claude's
// own environment; only the MCP server receives them.
func (e *Executor) environment(cfg Config, r extension.RunRequest, layer map[string]string) ([]string, error) {
	vars := map[string]string{}
	for _, kv := range e.o.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if ok && baseline(name, cfg.PassEnv) {
			vars[name] = value
		}
	}
	if vars["PATH"] == "" {
		vars["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	for name, value := range layer {
		if envName.MatchString(name) && !reserved(name) && !strings.ContainsRune(value, 0) {
			vars[name] = value
		}
	}
	for name, value := range cfg.Env {
		vars[name] = value
	}
	for secret, value := range r.Context.Secrets {
		name := cfg.secretVariable(secret)
		if reserved(name) || strings.ContainsRune(value, 0) {
			return nil, configError("secret maps to a reserved variable")
		}
		vars[name] = value
	}
	vars["OS_SCOPE_ID"] = string(r.Work.ScopeID)
	vars["OS_WORK_ID"] = string(r.Work.ID)
	vars["OS_EXECUTION_ID"] = string(r.Execution.ID)
	vars["OS_ATTEMPT"] = strconv.FormatUint(uint64(r.Execution.Attempt), 10)
	out := make([]string, 0, len(vars))
	for name, value := range vars {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out, nil
}

// baseline admits only safe process variables plus claude's own config
// location and any names the binding passes through. Unrelated credentials
// (ANTHROPIC_API_KEY among them, which comes only from a selected secret),
// nested-session markers (CLAUDECODE, CLAUDE_CODE_*), toolbelt variables and
// Open Conduit/Open Supervisor settings never reach claude.
func baseline(name string, pass []string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TMPDIR", "TERM", "CLAUDE_CONFIG_DIR":
		return true
	}
	for _, p := range pass {
		if p == name {
			return true
		}
	}
	return strings.HasPrefix(name, "LC_")
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeConfigDir is where claude keeps its state for this environment.
func claudeConfigDir(env []string) string {
	var home string
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case "CLAUDE_CONFIG_DIR":
			if value != "" {
				return value
			}
		case "HOME":
			home = value
		}
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".claude")
}

// TranscriptPath: claude keeps a session's transcript under
// <config>/projects/<cwd with every non-alphanumeric as "-">/<session>.jsonl.
func TranscriptPath(configDir, cwd, session string) string {
	return filepath.Join(configDir, "projects", nonAlnum.ReplaceAllString(cwd, "-"), session+".jsonl")
}

// transcriptExists reports whether the session has been written in this
// working directory (by its literal or resolved path).
func transcriptExists(configDir, cwd, session string) bool {
	paths := []string{cwd}
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		paths = append(paths, real)
	}
	for _, p := range paths {
		if _, err := os.Stat(TranscriptPath(configDir, p, session)); err == nil {
			return true
		}
	}
	return false
}

// write sends a user message on stdin. The caller holds rn.mu.
func (rn *run) write(text string) bool {
	if !rn.open || rn.stdin == nil {
		return false
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	if _, err := rn.stdin.Write(append(line, '\n')); err != nil {
		rn.open = false
		return false
	}
	return true
}

// send hands the running turn one injected input.
func (rn *run) send(id domain.ID, text string) error {
	rn.readNew()
	rn.mu.Lock()
	defer rn.mu.Unlock()
	var in injected
	_ = readJSON(filepath.Join(rn.dir, "injected.json"), &in)
	for _, a := range in.Accepted {
		if a == string(id) && rn.open {
			// A retried ambiguous dispatch of the same Event: it is already
			// on claude's stdin, so it is not written (or echoed) twice.
			return nil
		}
	}
	if !rn.write(text) {
		return ErrNotAccepting
	}
	rn.waiting = append(rn.waiting, pendingInput{id: id, text: text})
	return rn.recordInjected(string(id), "")
}

// recordInjected updates the segment's injected record. The caller holds
// rn.mu.
func (rn *run) recordInjected(accepted, consumed string) error {
	var in injected
	_ = readJSON(filepath.Join(rn.dir, "injected.json"), &in)
	if accepted != "" {
		in.Accepted = append(in.Accepted, accepted)
	}
	if consumed != "" {
		in.Consumed = append(in.Consumed, consumed)
	}
	return writeJSON(filepath.Join(rn.dir, "injected.json"), in)
}

// testHookFollow, when a test sets it, runs before a launched turn's
// follower reads anything, so a test can hold the follower back and show
// that settling a turn does not depend on how far it got.
var testHookFollow func()

// follow reads what claude writes while this instance holds its stdin: the
// echoes of injected input and the result, after which stdin is closed so
// claude exits.
func (e *Executor) follow(rn *run) {
	ticker := time.NewTicker(min(e.o.PollInterval, 200*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-rn.done:
			rn.readNew()
			rn.mu.Lock()
			if rn.open {
				rn.open = false
				_ = rn.stdin.Close()
			}
			rn.mu.Unlock()
			return
		case <-ticker.C:
			rn.readNew()
		}
	}
}

func (rn *run) readNew() {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	f, err := os.Open(filepath.Join(rn.dir, "out.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(rn.offset, io.SeekStart); err != nil {
		return
	}
	data, _ := io.ReadAll(f)
	rn.offset += int64(len(data))
	rn.partial += string(data)
	lines := strings.Split(rn.partial, "\n")
	rn.partial = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		var m struct {
			Type     string `json:"type"`
			IsReplay bool   `json:"isReplay"`
			Message  struct {
				Content any `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch {
		case m.Type == "user" && m.IsReplay:
			text, _ := m.Message.Content.(string)
			for i, p := range rn.waiting {
				if p.text == text {
					rn.waiting = append(rn.waiting[:i], rn.waiting[i+1:]...)
					_ = rn.recordInjected("", string(p.id))
					break
				}
			}
		case m.Type == "result":
			if rn.open {
				rn.open = false
				_ = rn.stdin.Close()
			}
		}
	}
}

// load restores a segment from its files: the daemon's process record, or the
// PID the wrapper itself wrote, or a scan for the nonce when neither exists.
func (rn *run) load() error {
	if rn.pid != 0 {
		return nil
	}
	var p processRecord
	if err := readJSON(filepath.Join(rn.dir, "process.json"), &p); err == nil && p.PID > 1 {
		rn.pid = p.PID
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(rn.dir, "pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			rn.pid = pid
			return nil
		}
	}
	list, err := scanProcesses()
	if err != nil {
		return err
	}
	for _, p := range list {
		if rn.matches(p) {
			rn.pid = p.PID
		}
	}
	return nil
}

// matches checks identity beyond PID: a session/group leader whose argument
// vector carries this segment's nonce.
func (rn *run) matches(p procInfo) bool {
	return p.PID > 1 && p.PGID == p.PID && strings.Contains(p.Args, marker+rn.nonce)
}

type procInfo struct {
	PID, PGID int
	Args      string
}

func (e *Executor) leaderAlive(rn *run) (bool, error) {
	if rn.done != nil {
		select {
		case <-rn.done:
			return false, nil
		default:
			return true, nil
		}
	}
	if rn.pid <= 1 {
		return false, nil
	}
	p, ok, err := inspectProcess(rn.pid)
	if err != nil {
		return false, err
	}
	return ok && rn.matches(p), nil
}

// owner refuses process control once a newer epoch has adopted the segment.
func (e *Executor) owner(rn *run) error {
	current, err := readLease(rn.dir)
	if err != nil {
		return err
	}
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if current > rn.epoch {
		return ErrSuperseded
	}
	return nil
}

// verified returns the leader PID only after re-checking identity immediately
// before a signal.
func (e *Executor) verified(rn *run) (int, bool, error) {
	if err := e.owner(rn); err != nil {
		return 0, false, err
	}
	if rn.pid <= 1 {
		return 0, false, nil
	}
	p, ok, err := inspectProcess(rn.pid)
	if err != nil || !ok || !rn.matches(p) {
		return 0, false, err
	}
	return p.PID, true, nil
}

// requestStop records the first stop reason and sends SIGTERM to the verified
// process group.
func (e *Executor) requestStop(rn *run, reason string) error {
	pid, ok, err := e.verified(rn)
	if err != nil || !ok {
		return err
	}
	path := filepath.Join(rn.dir, "stop.json")
	if !exists(path) {
		if err = writeJSON(path, stopRecord{Reason: reason, At: e.o.Now().UTC()}); err != nil {
			return err
		}
	}
	return signalGroup(pid, sigTerm)
}

func (e *Executor) killVerified(rn *run) error {
	pid, ok, err := e.verified(rn)
	if err != nil || !ok {
		return err
	}
	return signalGroup(pid, sigKill)
}

// stopTree terminates and waits for one segment's process tree.
func (e *Executor) stopTree(ctx context.Context, rn *run) error {
	if alive, err := e.leaderAlive(rn); err != nil {
		return err
	} else if alive {
		if err = e.requestStop(rn, "interrupted"); err != nil && !errors.Is(err, ErrSuperseded) {
			return err
		}
		killAt := e.o.Now().Add(e.o.Grace)
		for {
			alive, err = e.leaderAlive(rn)
			if err != nil {
				return err
			}
			if !alive {
				break
			}
			if !e.o.Now().Before(killAt) {
				if err = e.killVerified(rn); err != nil {
					return err
				}
			}
			if err = sleep(ctx, 20*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return e.drainGroup(ctx, rn)
}

// drainGroup waits for remaining group members after the leader has gone,
// escalating to SIGKILL after the grace period. Signals are only sent while
// the group is live, when its ID cannot have been reused.
func (e *Executor) drainGroup(ctx context.Context, rn *run) error {
	if rn.pid <= 1 {
		return nil
	}
	if err := e.owner(rn); err != nil {
		return err
	}
	reused := func() (bool, error) {
		p, ok, err := inspectProcess(rn.pid)
		return ok && !rn.matches(p), err
	}
	if r, err := reused(); err != nil || r {
		return err
	}
	start := e.o.Now()
	killed := false
	for groupAlive(rn.pid) {
		elapsed := e.o.Now().Sub(start)
		if elapsed >= e.o.Grace && !killed {
			if r, err := reused(); err != nil || r {
				return err
			}
			if err := signalGroup(rn.pid, sigKill); err != nil {
				return err
			}
			killed = true
		}
		if killed && elapsed >= e.o.Grace+5*time.Second {
			return ErrNotStopped
		}
		if err := sleep(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type handle struct {
	e *Executor
	r *run
}

// Wait observes the segment. Cancelling ctx stops observation only; it never
// terminates the detached process.
func (h *handle) Wait(ctx context.Context) (extension.Result, error) {
	e, rn := h.e, h.r
	for {
		if result, ok, err := readResult(rn.dir); err != nil || ok {
			return result, err
		}
		alive, err := e.leaderAlive(rn)
		if err != nil {
			return extension.Result{}, err
		}
		if !alive {
			break
		}
		if err = e.escalate(rn); err != nil && !errors.Is(err, ErrSuperseded) {
			return extension.Result{}, err
		}
		wake := e.o.PollInterval
		if rn.done == nil || exists(filepath.Join(rn.dir, "stop.json")) {
			wake = min(wake, 50*time.Millisecond)
		}
		timer := time.NewTimer(wake)
		select {
		case <-ctx.Done():
			timer.Stop()
			return extension.Result{}, ctx.Err()
		case <-rn.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	if err := e.drainGroup(ctx, rn); err != nil {
		return extension.Result{}, err
	}
	return e.finalizeWith(ctx, rn, nil)
}

// escalate applies SIGKILL after the grace period of a recorded stop, so it
// holds across daemon restart.
func (e *Executor) escalate(rn *run) error {
	var stop stopRecord
	err := readJSON(filepath.Join(rn.dir, "stop.json"), &stop)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case e.o.Now().Sub(stop.At) >= e.o.Grace:
		return e.killVerified(rn)
	}
	return nil
}

// resultFields is the structured outcome data. It never carries the session,
// or the handoff note: those are private continuity.
type resultFields struct {
	// Status is the turn's reported state (building, review, help, done,
	// finished) for projections; empty without a status.
	Status     string     `json:"status,omitempty"`
	Reason     string     `json:"reason"`
	PRs        []int      `json:"prs,omitempty"`
	Preview    string     `json:"preview,omitempty"`
	Touches    string     `json:"touches,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Models     []string   `json:"models,omitempty"`
	ModelCalls int        `json:"model_calls,omitempty"`
	ToolCalls  int        `json:"tool_calls,omitempty"`
	Tokens     *Tokens    `json:"tokens,omitempty"`
	Limited    bool       `json:"limited,omitempty"`
	ResetsAt   *time.Time `json:"resets_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Injected   *struct {
		Accepted int `json:"accepted"`
		Consumed int `json:"consumed"`
	} `json:"injected,omitempty"`
	Activity      string           `json:"activity,omitempty"`
	Transcript    string           `json:"transcript,omitempty"`
	Log           string           `json:"log,omitempty"`
	ArtifactError string           `json:"artifact_error,omitempty"`
	Workspace     *workspaceRecord `json:"workspace,omitempty"`
}

func resultData(schema string, f resultFields) domain.Payload {
	b, _ := json.Marshal(f)
	return domain.Payload{Schema: schema, Data: b}
}

// mapOutcome maps a parsed turn to the common structured outcome without any
// new core state: a finished status completes the Work; every other status
// (building, review, help, done) hands it back (handoff: Work returns to ready
// for the next accepted input); errors fail it, a usage limit with a retry
// hint at its reset.
func mapOutcome(p parsed, f *resultFields) (domain.Outcome, *extension.RetryHint) {
	f.Models, f.ModelCalls, f.ToolCalls, f.Tokens = p.Models, p.ModelCalls, p.ToolCalls, p.Tokens
	if !p.OK {
		f.Reason = "error"
		var hint *extension.RetryHint
		if p.Limited {
			f.Reason, f.Limited, f.ResetsAt = "usage_limit", true, p.ResetsAt
			hint = &extension.RetryHint{ResetAt: p.ResetsAt}
		}
		return domain.Outcome{Kind: domain.OutcomeFailed, Summary: truncate(p.Error, 300)}, hint
	}
	if p.Status == nil {
		f.Reason = "result"
		return domain.Outcome{Kind: domain.OutcomeCompleted, Summary: truncate(strings.TrimSpace(p.Text), 300)}, nil
	}
	s := p.Status
	f.Reason, f.Status, f.PRs, f.Preview, f.Touches = "status", s.State, s.PRs, s.Preview, s.Touches
	kind := domain.OutcomeHandoff
	if s.State == StateFinished {
		kind = domain.OutcomeCompleted
	}
	return domain.Outcome{Kind: kind, Summary: truncate(s.Summary, 300)}, nil
}

func usageOf(p parsed) extension.Usage {
	var u extension.Usage
	if p.Tokens != nil {
		in, out, cached := uint64(max(p.Tokens.Input, 0)), uint64(max(p.Tokens.Output, 0)), uint64(max(p.Tokens.CacheRead, 0))
		u.InputTokens, u.OutputTokens, u.CachedTokens = &in, &out, &cached
	}
	if p.Cost != nil && *p.Cost >= 0 {
		cost := *p.Cost
		u.Cost, u.Currency = &cost, "USD"
	}
	return u
}

// finalizeWith applies the turn to the Work's continuity, publishes its
// artifacts and durably records the result exactly once across instances; a
// later Recover returns this identical result.
func (e *Executor) finalizeWith(ctx context.Context, rn *run, preset *extension.Result) (extension.Result, error) {
	unlock, err := lockFile(filepath.Join(rn.dir, "finalize.lock"))
	if err != nil {
		return extension.Result{}, err
	}
	defer unlock()
	if result, ok, err := readResult(rn.dir); err != nil || ok {
		return result, err
	}
	var result extension.Result
	if preset != nil {
		result = *preset
	} else if result, err = e.settle(ctx, rn); err != nil {
		return extension.Result{}, err
	}
	if err = writeJSON(filepath.Join(rn.dir, "result.json"), result); err != nil {
		return extension.Result{}, err
	}
	_ = os.Remove(filepath.Join(rn.dir, "token"))
	result, _, err = readResult(rn.dir)
	return result, err
}

// settle reads a finished process's output into its result, applying the
// turn to continuity first (idempotently by segment).
func (e *Executor) settle(ctx context.Context, rn *run) (extension.Result, error) {
	var turn turnRecord
	if err := readJSON(filepath.Join(rn.dir, "turn.json"), &turn); err != nil {
		return extension.Result{}, err
	}
	var f resultFields
	f.Kind = string(turn.Kind)
	var stop stopRecord
	stopped := readJSON(filepath.Join(rn.dir, "stop.json"), &stop) == nil
	if b, err := os.ReadFile(filepath.Join(rn.dir, "exit")); err == nil {
		if code, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			f.ExitCode = &code
		}
	}
	rn.mu.Lock()
	state := rn.state
	rn.mu.Unlock()
	if f.ExitCode == nil && state != nil {
		if ws, ok := state.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
			code := state.ExitCode()
			f.ExitCode = &code
		}
	}
	now := e.o.Now().UTC()
	stdout, stderr := readText(filepath.Join(rn.dir, "out.jsonl")), readText(filepath.Join(rn.dir, "err.log"))
	p := parse(stdout, stderr, f.ExitCode, turn.Structured, now)
	if stopped {
		p.OK, p.Status, p.Limited, p.Error = false, nil, false, "claude turn "+stop.Reason
	}
	// The process has ended, so its output is complete. Read the rest of it
	// here rather than trusting the follower to have got there first: claude
	// can echo an injected input and end the turn within one poll, and the
	// follower's last read races this settle once done is closed. Missing
	// that echo would leave a consumed input unconsumed, and core would start
	// another attempt to present it again.
	rn.readNew()
	var in injected
	_ = readJSON(filepath.Join(rn.dir, "injected.json"), &in)
	if len(in.Accepted) > 0 {
		f.Injected = &struct {
			Accepted int `json:"accepted"`
			Consumed int `json:"consumed"`
		}{len(in.Accepted), len(in.Consumed)}
	}
	// Continuity first: the per-turn usage depends on the session's earlier
	// totals, and applying the same segment again changes nothing.
	var end turnEnd
	var rec record
	fresh := false
	err := e.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var err error
		if rec, err = e.loadRecord(tx, domain.Work{ID: domain.ID(turn.WorkID), ScopeID: domain.ID(turn.ScopeID)}); err != nil {
			return err
		}
		fresh = rec.Applied != rn.nonce
		end = finishTurn(&rec, rn.nonce, p, now)
		if !fresh {
			return nil
		}
		if p.Limited {
			until := now.Add(limitWait)
			if p.ResetsAt != nil {
				until = p.ResetsAt.Add(limitGrace)
			}
			var cur limitRecord
			if _, err := getJSON(tx, e.o.Namespace, limitKey, &cur); err != nil {
				return err
			}
			if until.After(cur.Until) {
				if err := putJSON(tx, e.o.Namespace, limitKey, limitRecord{Until: until}); err != nil {
					return err
				}
			}
		}
		return putJSON(tx, e.o.Namespace, workKey(rec.Work), rec)
	})
	if err != nil {
		return extension.Result{}, err
	}
	p = end.parsed
	if p.Limited && p.ResetsAt != nil && p.ResetsAt.Before(now) {
		// Core accepts a reset at most its margin in the past; one already
		// gone retries after the margin.
		p.ResetsAt = &now
	}
	outcome, hint := mapOutcome(p, &f)
	result := extension.Result{Outcome: outcome, Usage: usageOf(p), RetryHint: hint}
	// claude echoed these injected inputs back (--replay-user-messages): the
	// running turn took them in, so core need not present them again.
	seen := map[domain.ID]bool{}
	for _, id := range in.Consumed {
		if id != "" && !seen[domain.ID(id)] {
			seen[domain.ID(id)] = true
			result.ConsumedInputs = append(result.ConsumedInputs, domain.ID(id))
		}
	}
	if e.o.Artifacts != nil {
		// The rendered activity comes first, so an operator's log view shows
		// it; the raw transcript and stderr follow.
		if activity := RenderActivity(readText(filepath.Join(rn.dir, "out.jsonl"))); activity != "" {
			_ = os.WriteFile(filepath.Join(rn.dir, "activity.txt"), []byte(activity), 0o600)
		}
		for _, a := range []struct {
			file, media string
			field       *string
		}{{"activity.txt", ActivityMediaType, &f.Activity}, {"out.jsonl", TranscriptMediaType, &f.Transcript}, {"err.log", LogMediaType, &f.Log}} {
			ref, err := e.publish(ctx, filepath.Join(rn.dir, a.file), a.media)
			switch {
			case err == nil:
				*a.field = ref.ID
				result.Artifacts = append(result.Artifacts, ref)
			case !errors.Is(err, os.ErrNotExist):
				f.ArtifactError = "artifact store unavailable"
			}
		}
	}
	var ws workspaceRecord
	if readJSON(filepath.Join(rn.dir, "workspace.json"), &ws) == nil && ws.Ref != "" {
		ws.Dir = ""
		f.Workspace = &ws
	}
	result.Outcome.Data = resultData(turn.StatusSchema, f)
	result.Workspace = historyWorkspace(rn.dir)
	if fresh {
		e.reportTurn(turn, end, outcome, now)
	}
	return result, nil
}

// reportTurn sends one telemetry record per turn, outside core.
func (e *Executor) reportTurn(turn turnRecord, end turnEnd, outcome domain.Outcome, now time.Time) {
	p := end.parsed
	a := map[string]any{
		"scope": turn.ScopeID, "work": turn.WorkID, "execution": turn.ExecutionID,
		"ok": p.OK, "outcome": string(outcome.Kind), "error": p.Error,
		"duration_s": int(now.Sub(turn.StartedAt).Seconds()), "models": p.Models,
		"model_calls": p.ModelCalls, "tool_calls": p.ToolCalls, "kind": string(turn.Kind),
		"handoff": turn.Kind == kindHandoff, "session": end.session, "turn": end.turn,
		"events": turn.InputTypes, "limited": p.Limited,
	}
	if p.Cost != nil {
		a["cost_usd"] = *p.Cost
	}
	if p.Status != nil {
		a["state"] = p.Status.State
	} else if !p.OK {
		a["state"] = "failed"
	}
	if p.Tokens != nil {
		a["input_tokens"], a["output_tokens"] = p.Tokens.Input, p.Tokens.Output
		a["cache_read_tokens"], a["cache_creation_tokens"] = p.Tokens.CacheRead, p.Tokens.CacheCreation
	}
	e.report("agent_turn", a)
}

func (e *Executor) publish(ctx context.Context, path, media string) (domain.ArtifactRef, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return domain.ArtifactRef{}, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	defer f.Close()
	w, err := e.o.Artifacts.Create(ctx, media)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	if _, err = io.Copy(w, f); err != nil {
		w.Close()
		return domain.ArtifactRef{}, err
	}
	return w.Commit(ctx)
}

func readText(file string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return string(b)
}

func readResult(dir string) (extension.Result, bool, error) {
	var r extension.Result
	err := readJSON(filepath.Join(dir, "result.json"), &r)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	return r, err == nil, err
}

func readLease(dir string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(dir, "lease"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}
func writeLease(dir string, epoch uint64) error {
	return writeFile(filepath.Join(dir, "lease"), []byte(strconv.FormatUint(epoch, 10)+"\n"))
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err = json.Compact(&out, b); err != nil {
		return err
	}
	return writeFile(path, out.Bytes())
}

// writeFile atomically replaces an owner-only file.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// unavailable is the workspace_unavailable summary: the provider's reason in
// one line, so the run's history and tracker say why without the run's log.
func unavailable(err error) string {
	if err == nil {
		return "workspace acquisition failed"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	return "workspace acquisition failed: " + line
}
