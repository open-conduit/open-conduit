package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Scheduled holds (a `scheduled` label plus a runAt/waitFor marker). A held
// issue is claimed by the `github-hold` route as WorkKindHeld Work bound to
// the `github-hold` executor. Its attempt translates the hold through the
// github.hold operation into a core Wait (timeout timer plus dependency
// subscriptions), so the hold survives restarts in core and wakes through
// the same accepted-Event path as every other wait. When the hold ends the
// executor emits TypeHoldReleased, which the `github` route queues for the
// agent binding: the issue's first agent turn then starts with why.

// WorkKindHeld is the kind of Work claimed for a held (scheduled) issue.
const WorkKindHeld = "github.issue.held"

// HoldHandler is the route handler name for held issues.
const HoldHandler = "github-hold"

// HoldExecutor is the executor registration name for held issues.
const HoldExecutor = "github-hold"

// HoldRoute claims open issues carrying the scheduled label (and otherwise
// admitted: no skip label, a required label when configured, unclaimed). A
// scope enables holds by placing this route before its `github` route.
type HoldRoute struct{}

var _ extension.RouteHandler = HoldRoute{}

// Match creates held Work for an unowned scheduled issue; it never matches
// Events for existing Work (the `github` route selects their input).
func (HoldRoute) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if in.Work != nil {
		return extension.Skip("the issue already has Work; holds claim only new issues"), nil
	}
	if e.Type != TypeIssueOpen || e.Data.Schema != SchemaIssue || e.CorrelationKey == nil {
		return extension.Skip("only an opened issue is held"), nil
	}
	if _, ack := ParseIssueAckKey(e.CorrelationKey.ExternalID); ack {
		return extension.Skip("a copy of an issue's opening, for an acknowledgements route"), nil
	}
	s, ok := settingsFor(in.Scope, e.Source)
	if !ok {
		return extension.Skip("no github entry in this scope reads " + e.Source), nil
	}
	var p IssuePayload
	if json.Unmarshal(e.Data.Data, &p) != nil {
		return extension.Skip("the issue data is unreadable"), nil
	}
	if !has(p.Labels, s.Labels.Scheduled) {
		return extension.Skip("the issue is not labelled " + s.Labels.Scheduled), nil
	}
	l := s.Labels
	l.Scheduled = "\x00" // admit as if unscheduled: skip/require/claimed rules still apply
	if reason := s.issueRefusal(l, p); reason != "" {
		return extension.Skip(reason), nil
	}
	return extension.RouteDecision{Match: true, Kind: WorkKindHeld, Reason: "a new issue labelled " + s.Labels.Scheduled, Display: display(nil, e)}, nil
}

// ReleasedPayload is a TypeHoldReleased event: why the hold ended, as the
// first agent turn reads it.
type ReleasedPayload struct {
	Why string `json:"why"`
}

// HoldExecutorImpl runs a held issue's attempts in process. It holds no
// process: Run calls the toolbelt (SDK transport) with the attempt's own
// credential and finishes at once.
type HoldExecutorImpl struct {
	mu sync.Mutex
	// attempts are this instance's recent attempts, which Recover adopts
	// (a lease lost while it decided, or once it had decided); order is
	// their start order, so the oldest go first past maxHandles.
	attempts map[domain.ID]*holdAttempt
	order    []domain.ID
	// Dial opens a toolbelt client (default client.New over the endpoint).
	Dial func(endpoint, token string) (ToolbeltClient, error)
	Now  func() time.Time
}

// ToolbeltClient is the toolbelt surface the hold executor uses
// (*client.Client over the management endpoint).
type ToolbeltClient interface {
	ToolbeltCall(context.Context, client.ToolCall) (client.ToolResult, error)
}

// NewHoldExecutor returns the executor.
func NewHoldExecutor() *HoldExecutorImpl {
	return &HoldExecutorImpl{attempts: map[domain.ID]*holdAttempt{}, Now: time.Now}
}

func (h *HoldExecutorImpl) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: extension.ToolbeltSDK}
}

type holdHandle struct {
	done   chan struct{}
	result extension.Result
	err    error
}

func (r *holdHandle) Wait(ctx context.Context) (extension.Result, error) {
	select {
	case <-r.done:
		return r.result, r.err
	case <-ctx.Done():
		return extension.Result{}, ctx.Err()
	}
}

// holdAttempt is one attempt this instance runs, under the credential of
// the lease that holds it now. Its fields are guarded by the executor's mu.
//
// A lease the host lost (a stalled runner missing its renewals) moves the
// attempt to a newer epoch when it is recovered: the old credential is
// refused from the moment the lease lapses, and Recover hands over the new
// one (gen counts them). A decision refused under an older credential than
// the attempt now holds is decided again under the newest: a refused call
// changed nothing, and every call the decision makes is idempotent (a read,
// a wait the newer epoch declares, an emit keyed by the attempt).
type holdAttempt struct {
	tb       ToolbeltClient
	gen      int
	deciding bool
	// refusedGen is the credential a finished decision was refused under
	// (0: it was not refused).
	refusedGen int
	handle     *holdHandle
}

// Run decides the held attempt in the background: wait (a declared
// translated wait) or release (emit TypeHoldReleased, hand off).
func (h *HoldExecutorImpl) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := r.PersistRecovery(domain.Payload{Schema: SchemaHoldRecovery, Data: json.RawMessage(`{}`)}); err != nil {
		return nil, err
	}
	if r.Context.ToolbeltEndpoint == "" {
		// Without the toolbelt it can't read or declare the hold: the
		// attempt fails, saying so, rather than refusing to launch.
		done := make(chan struct{})
		close(done)
		handle := &holdHandle{done: done, result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed,
			Summary: "github-hold needs the toolbelt: this attempt has no toolbelt endpoint"}}}
		h.mu.Lock()
		h.keep(r.Execution.ID, &holdAttempt{handle: handle})
		h.mu.Unlock()
		return handle, nil
	}
	tb, err := h.dial(r)
	if err != nil {
		return nil, err
	}
	att := &holdAttempt{tb: tb, gen: 1}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keep(r.Execution.ID, att)
	h.decideLocked(att, r)
	return att.handle, nil
}

func (h *HoldExecutorImpl) dial(r extension.RunRequest) (ToolbeltClient, error) {
	dial := h.Dial
	if dial == nil {
		dial = func(endpoint, token string) (ToolbeltClient, error) { return client.New(endpoint, token) }
	}
	return dial(r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken)
}

// decideLocked starts deciding att in the background on a new handle.
func (h *HoldExecutorImpl) decideLocked(att *holdAttempt, r extension.RunRequest) {
	handle := &holdHandle{done: make(chan struct{})}
	att.handle, att.deciding, att.refusedGen = handle, true, 0
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for {
			h.mu.Lock()
			tb, gen := att.tb, att.gen
			h.mu.Unlock()
			result, err := h.decide(cctx, tb, r)
			h.mu.Lock()
			lost := authorityLost(err)
			if lost && att.gen != gen && cctx.Err() == nil {
				// Recovered while it decided: decide again under the
				// credential the attempt holds now.
				h.mu.Unlock()
				continue
			}
			if lost {
				att.refusedGen = gen
			}
			att.deciding = false
			handle.result, handle.err = result, err
			close(handle.done)
			h.mu.Unlock()
			return
		}
	}()
}

// authorityLost reports a call refused for the attempt's authority (its
// lease lapsed or moved on), as against a declared wait or outcome having
// released it. Over the management endpoint that is a forbidden refusal,
// or authority_recovering where the host says recovery is under way.
func authorityLost(err error) bool {
	if err == nil || errors.Is(err, extension.ErrAuthorityReleased) {
		return false
	}
	var api *client.Error
	var op *extension.OperationError
	code := ""
	switch {
	case errors.As(err, &api):
		code = api.Code
	case errors.As(err, &op):
		code = op.Code
	}
	return errors.Is(err, extension.ErrUnauthorized) || code == "forbidden" || code == "authority_recovering"
}

// Recover adopts an attempt this instance started (deciding or decided)
// under the recovering lease: it re-acknowledges the recovery state, as the
// host requires of an adopting executor, and hands the attempt the newly
// issued credential, the old one being fenced. A decision its old
// credential was refused is decided again under the new one. One
// interrupted by a restart, or past maxHandles, is retried.
func (h *HoldExecutorImpl) Recover(_ context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if state.Schema != SchemaHoldRecovery {
		return nil, extension.ErrNotAdoptable
	}
	h.mu.Lock()
	att := h.attempts[r.Execution.ID]
	h.mu.Unlock()
	if att == nil {
		return nil, extension.ErrNotAdoptable
	}
	if r.PersistRecovery == nil {
		return nil, extension.ErrInvalid
	}
	if err := r.PersistRecovery(state); err != nil {
		return nil, err
	}
	var tb ToolbeltClient
	if r.Context.ToolbeltEndpoint != "" {
		var err error
		if tb, err = h.dial(r); err != nil {
			return nil, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if tb != nil && att.tb != nil {
		att.tb, att.gen = tb, att.gen+1
		if !att.deciding && att.refusedGen != 0 && att.refusedGen < att.gen {
			h.decideLocked(att, r)
		}
	}
	return att.handle, nil
}

// maxHandles bounds the attempts Recover can adopt; an older one is
// retried instead, as one interrupted by a restart is.
const maxHandles = 256

// keep remembers an attempt for Recover. The caller holds h.mu.
func (h *HoldExecutorImpl) keep(id domain.ID, att *holdAttempt) {
	if h.attempts == nil {
		h.attempts = map[domain.ID]*holdAttempt{}
	}
	if _, ok := h.attempts[id]; !ok {
		h.order = append(h.order, id)
	}
	h.attempts[id] = att
	for len(h.order) > maxHandles {
		delete(h.attempts, h.order[0])
		h.order = h.order[1:]
	}
}

// Stop proves the attempt is not running: nothing outlives Run in another
// process, and this instance reports its in-flight attempts.
func (h *HoldExecutorImpl) Stop(_ context.Context, id domain.ID, _ uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if att := h.attempts[id]; att != nil && att.deciding {
		return errors.New("github-hold: attempt still deciding")
	}
	return nil
}

func call(ctx context.Context, tb ToolbeltClient, op string, data any) (client.ToolResult, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return client.ToolResult{}, err
	}
	input, _ := json.Marshal(map[string]json.RawMessage{"data": raw})
	return tb.ToolbeltCall(ctx, client.ToolCall{Operation: op, Input: input})
}

func (h *HoldExecutorImpl) decide(ctx context.Context, tb ToolbeltClient, r extension.RunRequest) (extension.Result, error) {
	woken := r.Trigger.Type == TypeDependencyMet || r.Trigger.Type == "core.wait.timeout"
	var why string
	if woken && r.Trigger.Type == TypeDependencyMet {
		why = heldWake(r.Trigger)
	} else {
		res, err := call(ctx, tb, OpHoldStatus, map[string]any{})
		if err != nil {
			return extension.Result{}, refusalOf(err)
		}
		var s HoldStatus
		if res.Output != nil {
			_ = json.Unmarshal(res.Output.Data, &s)
		}
		expired := woken && s.Until != nil && !s.Until.After(h.Now())
		if s.Held && !expired {
			// Still held (first attempt, or the owner moved the time while
			// it waited): declare the translated wait and park.
			held, err := call(ctx, tb, OpHold, map[string]any{})
			if err == nil {
				summary := "held: waiting for " + s.Describe
				var plan HoldStatus
				if held.Output != nil {
					_ = json.Unmarshal(held.Output.Data, &plan)
				}
				if len(plan.Refused) > 0 {
					// Part of the hold can't be met (a project this one may
					// not wait on): it waits for the rest, and says why.
					summary += "; it can't wait on " + strings.Join(plan.Refused, "; ")
				}
				return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Summary: summary}}, nil
			}
			if !notHeld(err) {
				return extension.Result{}, refusalOf(err)
			}
			// Nothing here can meet the hold (an unknown job or project, a
			// project this one may not wait on) and it has no time: start
			// now, saying why, so neither the Work nor the agent retries a
			// hold that can't register until the config changes.
			why = fmt.Sprintf("This issue was held, waiting for %s, which nothing here can tell has happened; it starts now.", s.Describe)
			if msg := refusalMessage(err); msg != "" {
				why += " " + msg
			}
		} else {
			why = releasedWhy(s, woken)
		}
	}
	key := r.Work.CorrelationKeys
	if len(key) == 0 {
		return extension.Result{}, errors.New("github-hold: Work has no issue key")
	}
	data, _ := json.Marshal(ReleasedPayload{Why: why})
	emit := map[string]any{"type": TypeHoldReleased, "delivery_id": "released/" + string(r.Execution.ID) + "/" + string(r.Trigger.ID),
		"data": map[string]any{"schema": SchemaHoldReleased, "data": json.RawMessage(data)}, "correlation_key": map[string]string{"source": key[0].Source, "external_id": key[0].ExternalID}}
	raw, _ := json.Marshal(emit)
	if _, err := tb.ToolbeltCall(ctx, client.ToolCall{Operation: extension.Emit, Input: raw}); err != nil {
		return extension.Result{}, refusalOf(err)
	}
	return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Summary: "released: " + why}}, nil
}

// unscheduledWhy is why a hold ends when the owner takes the scheduled
// label off.
const unscheduledWhy = "The scheduled label was taken off this issue, so it starts now."

// heldWake is the wording for a held issue whose condition happened.
func heldWake(alias domain.Event) string {
	var p struct {
		SourceType string `json:"source_type"`
		Label      string `json:"label"`
	}
	_ = json.Unmarshal(alias.Data.Data, &p)
	if p.Label == unscheduledLabel {
		return unscheduledWhy
	}
	if c, err := ParseCond(p.Label); err == nil {
		return fmt.Sprintf("This issue was held, waiting for %s. That has happened: %s.", c.Describe(), c.Happened(p.SourceType))
	}
	return "This issue was held, and what it waited for has happened."
}

func releasedWhy(s HoldStatus, timedOut bool) string {
	at := ""
	if s.Until != nil {
		at = s.Until.UTC().Format(time.RFC3339)
	}
	switch {
	case s.Reason == "cancelled":
		return "The wait was cancelled."
	case s.Reason == "unscheduled":
		return unscheduledWhy
	case s.Reason == "scheduled_now":
		return "This issue was scheduled to start now."
	case len(s.Waits) > 0:
		var conds []Cond
		for _, w := range s.Waits {
			if c, err := ParseCond(w); err == nil {
				conds = append(conds, c)
			}
		}
		return fmt.Sprintf("This issue was held, waiting for %s, or until %s at the latest. That time has come, and none of the rest has happened.", CondsText(conds), at)
	case at != "":
		return fmt.Sprintf("This issue was scheduled to start at %s, and that time has come.", at)
	}
	if timedOut {
		return "The time this issue was held until has come."
	}
	return "This issue was scheduled to start now."
}

// notHeld reports the github.hold refusal for a hold nothing can meet.
func notHeld(err error) bool {
	var api *client.Error
	var op *extension.OperationError
	return errors.As(err, &api) && api.Code == "not_held" || errors.As(err, &op) && op.Code == "not_held"
}

// refusalOf presents a toolbelt refusal as an extension.OperationError, so
// the failed attempt's outcome says what was refused and why instead of a
// generic failure. Any other error is returned as it is.
func refusalOf(err error) error {
	var api *client.Error
	if errors.As(err, &api) && api.Code != "" {
		return &extension.OperationError{Code: api.Code, Message: api.Message}
	}
	return err
}

// refusalMessage is the reason a not_held refusal gives, as a sentence.
func refusalMessage(err error) string {
	var api *client.Error
	var op *extension.OperationError
	msg := ""
	switch {
	case errors.As(err, &api):
		msg = api.Message
	case errors.As(err, &op):
		msg = op.Message
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}
