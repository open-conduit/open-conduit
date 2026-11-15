// Package chatcompletions is a toolbelt-only executor: each attempt is a
// Chat Completions tool loop against an OpenAI-compatible endpoint
// (OpenAI, OpenRouter, Ollama, LiteLLM and so on), whose tools are the
// toolbelt operations the binding is granted and nothing else. There is no
// CLI process, workspace or container: a run is HTTP requests made from
// the extension's own process, so a daemon can run many cheaply. Every
// tool call is a toolbelt call, so authority, secrets, gates and the trail
// stay at the host.
package chatcompletions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Name is the executor's registration name.
const Name = "chat-completions"

const (
	// RecoverySchema is the recovery metadata an attempt persists before
	// its first request: the execution it belongs to.
	RecoverySchema = "open-conduit.chat-completions.recovery.v1"
	// ResultSchema is a result's data when the turn declared no status or
	// result (those use domain.TurnSchema, with the same fields).
	ResultSchema = "open-conduit.chat-completions.result.v1"
	// TranscriptMediaType is the conversation artifact: one message per
	// line, the system message first.
	TranscriptMediaType = "application/x-ndjson"
)

var (
	// ErrUnknownExecution is returned for a control naming an execution
	// this instance never ran.
	ErrUnknownExecution = errors.New("unknown chat-completions execution")
	// ErrSuperseded is Stop at an epoch older than the one holding the
	// attempt.
	ErrSuperseded = errors.New("chat-completions execution held at a newer epoch")
	// ErrRunning is Forget of an attempt still running.
	ErrRunning = errors.New("chat-completions execution is still running")

	errInterrupted = errors.New("interrupted")
	errStopped     = errors.New("stopped")
)

// Capabilities are interrupt, inject, resume and the toolbelt, which the
// executor calls itself (sdk). It needs no workspace, sandbox or container:
// it runs no process.
func Capabilities() domain.Capabilities {
	return domain.Capabilities{Interrupt: true, Inject: true, Resume: true, Toolbelt: extension.ToolbeltSDK}
}

// Options configure an Executor.
type Options struct {
	// StateDir holds the Works' conversations and each attempt's result.
	StateDir string
	// Artifacts receives each attempt's transcript. Optional.
	Artifacts storage.Artifacts
	// HTTPClient makes the requests (default: one with no overall timeout;
	// each request has the binding's timeout_seconds).
	HTTPClient *http.Client
	// Dial opens the attempt's toolbelt (default client.New).
	Dial func(endpoint, token string) (Toolbelt, error)
	Now  func() time.Time
}

// Executor runs attempts as Chat Completions tool loops.
type Executor struct {
	o     Options
	mu    sync.Mutex
	runs  map[domain.ID]*run
	works map[domain.ID]*sync.Mutex
}

// New returns an executor keeping its state in o.StateDir.
func New(o Options) (*Executor, error) {
	if o.StateDir == "" || !filepath.IsAbs(o.StateDir) {
		return nil, fmt.Errorf("%w: state dir must be absolute", extension.ErrInvalid)
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	if o.Dial == nil {
		o.Dial = func(endpoint, token string) (Toolbelt, error) { return client.New(endpoint, token) }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Executor{o: o, runs: map[domain.ID]*run{}, works: map[domain.ID]*sync.Mutex{}}, nil
}

// run is one attempt this instance holds.
type run struct {
	id     domain.ID
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu       sync.Mutex
	epoch    uint64
	closed   bool // takes no more input
	finished bool
	result   extension.Result
	inbox    []domain.Event
	seen     map[domain.ID]bool
	folded   int // inbox events folded into the conversation
	accepted int
}

type handle struct{ rn *run }

func (h handle) Wait(ctx context.Context) (extension.Result, error) {
	select {
	case <-h.rn.done:
		h.rn.mu.Lock()
		defer h.rn.mu.Unlock()
		return h.rn.result, nil
	case <-ctx.Done():
		return extension.Result{}, ctx.Err()
	}
}

type finished struct{ result extension.Result }

func (f finished) Wait(context.Context) (extension.Result, error) { return f.result, nil }

type recovery struct {
	Execution domain.ID `json:"execution"`
}

func (e *Executor) Capabilities() domain.Capabilities { return Capabilities() }

// Run starts an attempt.
func (e *Executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	return e.launch(ctx, r)
}

// Resume starts the wake of a parked attempt; the conversation is the
// Work's, so it carries on as any attempt does.
func (e *Executor) Resume(ctx context.Context, r extension.RunRequest, _ domain.Payload) (extension.RunHandle, error) {
	return e.launch(ctx, r)
}

// launch persists recovery intent before the first request, so a crash
// can never leave an unacknowledged attempt.
func (e *Executor) launch(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.PersistRecovery == nil || r.Execution.ID == "" {
		return nil, extension.ErrInvalid
	}
	e.mu.Lock()
	if held := e.runs[r.Execution.ID]; held != nil {
		held.mu.Lock()
		running := !held.finished
		held.mu.Unlock()
		if running {
			e.mu.Unlock()
			return nil, fmt.Errorf("%w: execution %s is already running", extension.ErrConflict, r.Execution.ID)
		}
	}
	e.mu.Unlock()
	data, _ := json.Marshal(recovery{Execution: r.Execution.ID})
	if err := r.PersistRecovery(domain.Payload{Schema: RecoverySchema, Data: data}); err != nil {
		return nil, err
	}
	// A wake reuses the parked attempt's execution: its earlier result is
	// replaced by this one's.
	if err := os.Remove(e.resultPath(r.Execution.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	rn := &run{id: r.Execution.ID, cancel: cancel, done: make(chan struct{}), epoch: r.Execution.Lease.Epoch, seen: map[domain.ID]bool{}}
	e.mu.Lock()
	e.runs[r.Execution.ID] = rn
	e.mu.Unlock()
	go e.drive(runCtx, rn, r)
	return handle{rn}, nil
}

// Recover adopts an attempt this instance still holds, or returns the
// result one recorded. It never starts a request: an attempt whose
// process is gone (the extension restarted) is ErrNotAdoptable, and the
// runtime's next attempt carries the conversation on.
func (e *Executor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rec recovery
	if state.Schema != RecoverySchema || json.Unmarshal(state.Data, &rec) != nil || rec.Execution == "" || rec.Execution != r.Execution.ID || r.PersistRecovery == nil {
		return nil, extension.ErrNotAdoptable
	}
	e.mu.Lock()
	rn := e.runs[rec.Execution]
	e.mu.Unlock()
	if rn != nil {
		rn.mu.Lock()
		running := !rn.finished
		if running && r.Execution.Lease.Epoch < rn.epoch {
			rn.mu.Unlock()
			return nil, extension.ErrNotAdoptable
		}
		if running {
			rn.epoch = r.Execution.Lease.Epoch
		}
		rn.mu.Unlock()
		if running {
			if err := r.PersistRecovery(state); err != nil {
				return nil, err
			}
			return handle{rn}, nil
		}
	}
	result, ok, err := e.readResult(rec.Execution)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, extension.ErrNotAdoptable
	}
	if err := r.PersistRecovery(state); err != nil {
		return nil, err
	}
	return finished{result}, nil
}

// Inject queues an Event for the running attempt, which folds it into the
// conversation before its next request. Consumption is proved by the
// result's ConsumedInputs once a request carrying it was answered.
func (e *Executor) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	rn := e.runs[id]
	e.mu.Unlock()
	if rn == nil {
		return extension.ErrNotAccepting
	}
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.closed {
		return extension.ErrNotAccepting
	}
	if !rn.seen[ev.ID] {
		rn.seen[ev.ID] = true
		rn.inbox = append(rn.inbox, ev)
		rn.accepted++
	}
	return nil
}

// Interrupt cancels the request in flight; the attempt fails as
// interrupted and its conversation stays open for the next.
func (e *Executor) Interrupt(ctx context.Context, id domain.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	rn := e.runs[id]
	e.mu.Unlock()
	if rn == nil {
		return ErrUnknownExecution
	}
	rn.cancel(errInterrupted)
	return nil
}

// Stop ends the attempt at epoch and returns once its loop has returned.
// An attempt this instance doesn't hold has nothing running: the loop
// lives in this process.
func (e *Executor) Stop(ctx context.Context, id domain.ID, epoch uint64) error {
	e.mu.Lock()
	rn := e.runs[id]
	e.mu.Unlock()
	if rn == nil {
		return nil
	}
	rn.mu.Lock()
	held := rn.epoch
	rn.mu.Unlock()
	if held > epoch {
		return ErrSuperseded
	}
	rn.cancel(errStopped)
	select {
	case <-rn.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Forget removes an attempt's recorded result. The Work's conversation
// stays: it is the Work's continuity, not the attempt's.
func (e *Executor) Forget(ctx context.Context, id domain.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if rn := e.runs[id]; rn != nil {
		rn.mu.Lock()
		running := !rn.finished
		rn.mu.Unlock()
		if running {
			return ErrRunning
		}
		delete(e.runs, id)
	}
	if err := os.Remove(e.resultPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (e *Executor) resultPath(id domain.ID) string {
	return filepath.Join(e.o.StateDir, "runs", key(id)+".json")
}

func (e *Executor) readResult(id domain.ID) (extension.Result, bool, error) {
	data, err := os.ReadFile(e.resultPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return extension.Result{}, false, nil
	} else if err != nil {
		return extension.Result{}, false, err
	}
	var r extension.Result
	if err := json.Unmarshal(data, &r); err != nil {
		return extension.Result{}, false, err
	}
	return r, true, nil
}

// drive runs the attempt and records its result, then returns the
// recorded copy, so a recovered handle returns exactly the same result.
func (e *Executor) drive(ctx context.Context, rn *run, r extension.RunRequest) {
	defer rn.cancel(nil)
	result := e.attempt(ctx, rn, r)
	rn.mu.Lock()
	rn.closed = true
	rn.mu.Unlock()
	if err := writeAtomic(e.resultPath(rn.id), result); err == nil {
		if recorded, ok, err := e.readResult(rn.id); err == nil && ok {
			result = recorded
		}
	}
	rn.mu.Lock()
	rn.result, rn.finished = result, true
	rn.mu.Unlock()
	close(rn.done)
}

// workLock serialises the attempts of one Work over its conversation.
func (e *Executor) workLock(id domain.ID) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.works[id]
	if m == nil {
		m = &sync.Mutex{}
		e.works[id] = m
	}
	return m
}

// data is a result's data, under ResultSchema or domain.TurnSchema.
type data struct {
	Status        string          `json:"status,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Reason        string          `json:"reason"`
	Detail        string          `json:"detail,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	HandoffReason string          `json:"handoff_reason,omitempty"`
	Model         string          `json:"model,omitempty"`
	Rounds        []roundUsage    `json:"rounds"`
	Tokens        tokens          `json:"tokens"`
	Cost          *float64        `json:"cost,omitempty"`
	ToolCalls     int             `json:"tool_calls"`
	Injected      injected        `json:"injected"`
	Transcript    string          `json:"transcript,omitempty"`
	ContextTokens uint64          `json:"context_tokens,omitempty"`
}

type tokens struct {
	Input  uint64 `json:"input"`
	Output uint64 `json:"output"`
	Cached uint64 `json:"cached"`
}

type injected struct {
	Accepted int `json:"accepted"`
	Consumed int `json:"consumed"`
}

// attemptState is what one attempt accumulates for its result.
type attemptState struct {
	d        data
	consumed []domain.ID
	// publish writes the conversation as the attempt's transcript, once
	// there is one.
	publish func() *domain.ArtifactRef
}

func (s *attemptState) round(u roundUsage) {
	s.d.Rounds = append(s.d.Rounds, u)
	s.d.Model = u.Model
	s.d.Tokens.Input += u.PromptTokens
	s.d.Tokens.Output += u.CompletionTokens
	s.d.Tokens.Cached += u.CachedTokens
	s.d.ContextTokens = u.PromptTokens + u.CompletionTokens
	if u.Cost != nil {
		c := *u.Cost
		if s.d.Cost != nil {
			c += *s.d.Cost
		}
		s.d.Cost = &c
	}
}

// Reasons an attempt ended, besides a failed request's.
const (
	reasonDeclared    = "declared"
	reasonWait        = "wait"
	reasonReplied     = "replied"
	reasonNoOutcome   = "no_outcome"
	reasonMaxRounds   = "max_rounds"
	reasonConfig      = "invalid_config"
	reasonToolbelt    = "toolbelt_unavailable"
	reasonState       = "state_unavailable"
	reasonInterrupted = "interrupted"
	reasonStopped     = "stopped"
)

// nudge reminds a model that replied without ending its turn.
const nudge = "You replied without ending your turn. If the task is done, call declare_outcome with kind completed; to hand back until something new arrives, kind handoff; if it can't go on until something happens, call wait."

// attempt is the tool loop.
func (e *Executor) attempt(ctx context.Context, rn *run, r extension.RunRequest) extension.Result {
	s := &attemptState{d: data{Rounds: []roundUsage{}}}
	cfg, err := ParseConfig(r.Context.Config.Data)
	if err != nil {
		return s.fail(rn, reasonConfig, err.Error(), nil, true)
	}
	apiKey := ""
	if cfg.APIKeySecret != "" {
		if apiKey = r.Context.Secrets[cfg.APIKeySecret]; apiKey == "" {
			return s.fail(rn, reasonConfig, "api_key_secret did not resolve to a value", nil, true)
		}
	}
	for _, v := range cfg.Headers {
		for _, secret := range r.Context.Secrets {
			if secret != "" && strings.Contains(v, secret) {
				return s.fail(rn, reasonConfig, "a header carries a selected secret; use api_key_secret", nil, true)
			}
		}
	}
	ep := &endpoint{url: cfg.BaseURL, key: apiKey, headers: cfg.Headers, model: cfg.Model, extra: cfg.Request,
		timeout: time.Duration(cfg.TimeoutSeconds) * time.Second, client: e.o.HTTPClient, now: e.o.Now}

	var tb Toolbelt
	ts := toolset{ops: map[string]string{}}
	if r.Context.ToolbeltEndpoint != "" && r.Context.ToolbeltToken != "" {
		if tb, err = e.o.Dial(r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken); err != nil {
			return s.fail(rn, reasonToolbelt, "the toolbelt could not be opened", nil, false)
		}
		guide, err := tb.ToolbeltGuide(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && ctx.Err() != nil {
				return s.cancelled(rn, cause)
			}
			return s.fail(rn, reasonToolbelt, "the toolbelt's operations could not be read: "+toolError(err), nil, false)
		}
		ts = buildTools(guide, cfg)
	}

	lock := e.workLock(r.Work.ID)
	lock.Lock()
	defer lock.Unlock()
	th, err := e.loadThread(r.Work.ID)
	if err != nil {
		return s.fail(rn, reasonState, "the Work's conversation is unreadable", nil, false)
	}
	save := func() error {
		th.At = e.o.Now().UTC()
		return e.saveThread(r.Work.ID, th)
	}
	s.publish = func() *domain.ArtifactRef { return e.transcript(ts.system, th.Messages) }

	// Begin or continue the turn.
	if th.Open && len(th.Messages) > 0 && !th.Oversize {
		s.d.Kind = kindContinue
	} else {
		if err := e.begin(&th, s, cfg, r); err != nil {
			return s.fail(rn, reasonConfig, err.Error(), nil, true)
		}
	}
	if err := save(); err != nil {
		return s.fail(rn, reasonState, "the Work's conversation could not be saved", nil, false)
	}

	// A continued turn first answers the calls its last reply asked for
	// that have no result (the attempt ended partway through them) as
	// maybe run, so the model checks rather than repeats them.
	if s.d.Kind == kindContinue {
		if out, done := e.finishCalls(ctx, tb, ts, &th, r.Work.ID, true, s, save); done {
			return s.end(rn, out, &th, save)
		}
		var fresh []domain.Event
		for _, in := range inputs(r) {
			if !slices.Contains(th.Presented, in.ID) {
				fresh = append(fresh, in)
				th.Presented = append(th.Presented, in.ID)
			}
		}
		if len(fresh) > 0 {
			th.Messages = append(th.Messages, Message{Role: "user", Content: interjection(fresh)})
		}
		if err := save(); err != nil {
			return s.fail(rn, reasonState, "the Work's conversation could not be saved", nil, false)
		}
	}

	for round := 0; ; round++ {
		folded := e.fold(rn, &th)
		if len(folded) > 0 {
			if err := save(); err != nil {
				return s.fail(rn, reasonState, "the Work's conversation could not be saved", nil, false)
			}
		}
		if round == cfg.MaxRounds {
			return s.fail(rn, reasonMaxRounds, fmt.Sprintf("the turn did not end within %d requests", cfg.MaxRounds), nil, false)
		}
		messages := th.Messages
		if ts.system != "" {
			messages = append([]Message{{Role: "system", Content: ts.system}}, th.Messages...)
		}
		rep, err := ep.complete(ctx, messages, ts.tools)
		if err != nil {
			if cause := context.Cause(ctx); ctx.Err() != nil {
				return s.cancelled(rn, cause)
			}
			re, _ := asRequestError(err)
			if re == nil {
				re = &requestError{Reason: failProvider, Detail: "the request failed"}
			}
			if re.Reason == failContext {
				th.Oversize = true
				_ = save()
			}
			var hint *extension.RetryHint
			if re.Limited {
				hint = &extension.RetryHint{ResetAt: re.ResetAt}
			}
			return s.fail(rn, re.Reason, re.Error(), hint, re.Reason == failAuth || re.Reason == failNotFound)
		}
		for _, ev := range folded {
			if !slices.Contains(s.consumed, ev) {
				s.consumed = append(s.consumed, ev)
			}
		}
		s.round(rep.Usage)
		th.PromptTokens = rep.Usage.PromptTokens
		normalizeCalls(&rep.Message, len(th.Messages))
		th.Messages = append(th.Messages, rep.Message)
		if err := save(); err != nil {
			return s.fail(rn, reasonState, "the Work's conversation could not be saved", nil, false)
		}
		if len(rep.Message.ToolCalls) == 0 {
			if e.pending(rn, tb == nil) {
				continue // fold what arrived and go on
			}
			if tb == nil {
				// Without a toolbelt there is nothing to declare with:
				// the reply ends the turn.
				return s.end(rn, callOutcome{ended: reasonReplied, kind: domain.OutcomeCompleted, summary: rep.Message.Content}, &th, save)
			}
			if th.Nudged {
				return s.fail(rn, reasonNoOutcome, "the model replied twice without declaring an outcome or waiting", nil, false)
			}
			th.Nudged = true
			th.Messages = append(th.Messages, Message{Role: "user", Content: nudge})
			if err := save(); err != nil {
				return s.fail(rn, reasonState, "the Work's conversation could not be saved", nil, false)
			}
			continue
		}
		if out, done := e.finishCalls(ctx, tb, ts, &th, r.Work.ID, false, s, save); done {
			return s.end(rn, out, &th, save)
		}
		if cause := context.Cause(ctx); ctx.Err() != nil {
			return s.cancelled(rn, cause)
		}
	}
}

// begin starts a turn: a Work's first, a fresh conversation from the last
// turn's note when the last one grew too large, or one more on the events
// since.
func (e *Executor) begin(th *thread, s *attemptState, cfg Config, r extension.RunRequest) error {
	kind, note := kindEvents, th.Note
	switch {
	case len(th.Messages) == 0:
		kind = kindStart
	case th.Oversize:
		s.d.HandoffReason = handoffContext
		kind = kindHandoff
	case *cfg.HandoffTokens > 0 && th.PromptTokens > uint64(*cfg.HandoffTokens):
		s.d.HandoffReason = handoffSize
		kind = kindHandoff
	}
	if kind == kindHandoff && strings.TrimSpace(note) == "" {
		if th.Oversize {
			note = "(Your last conversation outgrew the model's context and left no note.)"
		} else {
			kind, s.d.HandoffReason = kindEvents, ""
		}
	}
	playbookText := ""
	if kind != kindEvents {
		dir := cfg.PluginDir
		if dir == "" {
			dir = r.Context.PlaybookDir
		}
		text, err := readPlaybook(dir, cfg.Playbook)
		if err != nil {
			return fmt.Errorf("the playbook %q could not be read", cfg.Playbook)
		}
		playbookText = text
	}
	ins := inputs(r)
	args, b := body(kind, expand(cfg.Args, r.Work), note, RenderEvents(ins))
	if kind == kindStart || kind == kindHandoff {
		th.Messages = nil
	}
	text := prompt(playbookText, args, b)
	if playbookText == "" && cfg.Playbook != "" && cfg.Playbook != "-" {
		text = prompt("", "ARGUMENTS: "+args, b)
	}
	th.Messages = append(th.Messages, Message{Role: "user", Content: text})
	th.Open, th.Nudged, th.Oversize = true, false, false
	th.Presented = nil
	for _, in := range ins {
		th.Presented = append(th.Presented, in.ID)
	}
	th.Turns++
	s.d.Kind = kind
	return nil
}

// inputs are the attempt's inputs: the batch, or the trigger alone.
func inputs(r extension.RunRequest) []domain.Event {
	if len(r.Inputs) > 0 {
		return r.Inputs
	}
	if r.Trigger.ID != "" {
		return []domain.Event{r.Trigger}
	}
	return nil
}

// normalizeCalls gives every tool call an ID unique in its message (a
// provider may send none, or repeat one), so each result answers exactly
// one call.
func normalizeCalls(m *Message, index int) {
	seen := map[string]bool{}
	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		if tc.ID == "" || seen[tc.ID] {
			tc.ID = fmt.Sprintf("call_%d_%d", index, i)
		}
		seen[tc.ID] = true
		tc.Type = "function"
	}
}

// finishCalls answers, in order, the last reply's tool calls that have no
// result yet. With resumed (an earlier attempt ended partway through
// them), each is answered as maybe run rather than made again: the host
// replays a request ID only within one execution, so making it again could
// repeat its effect. Otherwise each is made through the toolbelt. Once one
// ends the turn (wait or declare_outcome), the rest are answered as not
// run. A call cut off by an interrupt or stop is left unanswered, for the
// next attempt to answer as maybe run.
func (e *Executor) finishCalls(ctx context.Context, tb Toolbelt, ts toolset, th *thread, work domain.ID, resumed bool, s *attemptState, save func() error) (callOutcome, bool) {
	index := -1
	for i := len(th.Messages) - 1; i >= 0; i-- {
		if th.Messages[i].Role == "assistant" {
			index = i
			break
		}
		if th.Messages[i].Role != "tool" {
			return callOutcome{}, false
		}
	}
	if index < 0 || len(th.Messages[index].ToolCalls) == 0 {
		return callOutcome{}, false
	}
	answered := map[string]bool{}
	for _, m := range th.Messages[index+1:] {
		answered[m.ToolCallID] = true
	}
	var ended callOutcome
	for _, tc := range th.Messages[index].ToolCalls {
		if answered[tc.ID] {
			continue
		}
		var text string
		switch {
		case ended.ended != "":
			text = "not run: your turn ended with " + ended.ended + " before this call"
		case resumed:
			text = maybeRun
		case ctx.Err() != nil:
			return callOutcome{}, false
		case tb == nil:
			text = "error: this turn has no toolbelt"
		default:
			var out callOutcome
			text, out = call(ctx, tb, ts, tc, requestID(work, th.Turns, index, tc.ID))
			s.d.ToolCalls++
			if out.ended == cutOff {
				return callOutcome{}, false
			}
			if out.ended != "" {
				ended = out
			}
		}
		th.Messages = append(th.Messages, Message{Role: "tool", ToolCallID: tc.ID, Content: text})
		_ = save()
	}
	return ended, ended.ended != ""
}

// fold moves what was injected since the last request into the
// conversation, after the last reply's tool results, and returns the
// Events folded.
func (e *Executor) fold(rn *run, th *thread) []domain.ID {
	rn.mu.Lock()
	pending := rn.inbox[rn.folded:]
	rn.folded = len(rn.inbox)
	rn.mu.Unlock()
	var fresh []domain.Event
	var ids []domain.ID
	for _, ev := range pending {
		ids = append(ids, ev.ID)
		if slices.Contains(th.Presented, ev.ID) {
			continue // a retried attempt already showed it
		}
		fresh = append(fresh, ev)
		th.Presented = append(th.Presented, ev.ID)
	}
	if len(fresh) > 0 {
		th.Messages = append(th.Messages, Message{Role: "user", Content: interjection(fresh)})
	}
	return ids
}

// pending reports Events injected and not yet folded. With close, when
// there are none it closes the attempt to input in the same step, so
// nothing is accepted that no request will carry.
func (e *Executor) pending(rn *run, close bool) bool {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.folded < len(rn.inbox) {
		return true
	}
	rn.closed = rn.closed || close
	return false
}

// end closes the turn on a wait, a declared outcome or a final reply.
func (s *attemptState) end(rn *run, out callOutcome, th *thread, save func() error) extension.Result {
	rn.mu.Lock()
	rn.closed = true
	rn.mu.Unlock()
	th.Open, th.Nudged, th.Presented = false, false, nil
	th.Note = strings.TrimSpace(out.summary)
	_ = save()
	s.d.Status, s.d.Result = out.status, out.result
	var o domain.Outcome
	switch out.ended {
	case extension.Wait:
		s.d.Reason = reasonWait
		// The host turns it into waiting: the wait is declared.
		o = domain.Outcome{Kind: domain.OutcomeHandoff, Summary: firstNonEmpty(out.summary, "waiting")}
	case extension.DeclareOutcome:
		s.d.Reason = reasonDeclared
		kind := out.kind
		if kind != domain.OutcomeCompleted && kind != domain.OutcomeFailed {
			kind = domain.OutcomeHandoff
		}
		o = domain.Outcome{Kind: kind, Summary: out.summary}
	default:
		s.d.Reason = reasonReplied
		o = domain.Outcome{Kind: domain.OutcomeCompleted, Summary: truncate(strings.TrimSpace(out.summary), 2000)}
	}
	return s.result(rn, o, nil, false)
}

// cancelled is an attempt its context ended: an interrupt or a stop.
func (s *attemptState) cancelled(rn *run, cause error) extension.Result {
	reason := reasonStopped
	if errors.Is(cause, errInterrupted) {
		reason = reasonInterrupted
	}
	return s.fail(rn, reason, "the attempt was "+reason, nil, false)
}

func (s *attemptState) fail(rn *run, reason, detail string, hint *extension.RetryHint, permanent bool) extension.Result {
	s.d.Reason, s.d.Detail = reason, truncate(detail, 1000)
	return s.result(rn, domain.Outcome{Kind: domain.OutcomeFailed, Summary: reason}, hint, permanent && hint == nil)
}

func (s *attemptState) result(rn *run, o domain.Outcome, hint *extension.RetryHint, permanent bool) extension.Result {
	rn.mu.Lock()
	s.d.Injected = injected{Accepted: rn.accepted, Consumed: len(s.consumed)}
	rn.mu.Unlock()
	var artifacts []domain.ArtifactRef
	if s.publish != nil {
		if ref := s.publish(); ref != nil {
			s.d.Transcript = ref.ID
			artifacts = []domain.ArtifactRef{*ref}
		}
	}
	schema := ResultSchema
	if s.d.Status != "" || len(s.d.Result) > 0 {
		schema = domain.TurnSchema
	}
	raw, _ := json.Marshal(s.d)
	o.Data = domain.Payload{Schema: schema, Data: raw}
	res := extension.Result{Outcome: o, RetryHint: hint, Permanent: permanent, ConsumedInputs: s.consumed, Artifacts: artifacts}
	u := &res.Usage
	in, out, cached := s.d.Tokens.Input, s.d.Tokens.Output, s.d.Tokens.Cached
	if len(s.d.Rounds) > 0 {
		u.InputTokens, u.OutputTokens, u.CachedTokens = &in, &out, &cached
	}
	if s.d.Cost != nil {
		c := *s.d.Cost
		u.Cost, u.Currency = &c, "USD"
	}
	return res
}

// transcript publishes the conversation as an artifact.
func (e *Executor) transcript(system string, messages []Message) *domain.ArtifactRef {
	if e.o.Artifacts == nil || len(messages) == 0 {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if system != "" {
		_ = enc.Encode(Message{Role: "system", Content: system})
	}
	for _, m := range messages {
		_ = enc.Encode(m)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, err := e.o.Artifacts.Create(ctx, TranscriptMediaType)
	if err != nil {
		return nil
	}
	if _, err := w.Write(b.Bytes()); err != nil {
		_ = w.Close()
		return nil
	}
	ref, err := w.Commit(ctx)
	if err != nil {
		return nil
	}
	return &ref
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
