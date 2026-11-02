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

// Scheduled holds (v1 `scheduled` label plus a runAt/waitFor marker). A held
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
	mu      sync.Mutex
	running map[domain.ID]bool
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
	return &HoldExecutorImpl{running: map[domain.ID]bool{}, Now: time.Now}
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

// Run decides the held attempt in the background: wait (a declared
// translated wait) or release (emit TypeHoldReleased, hand off).
func (h *HoldExecutorImpl) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := r.PersistRecovery(domain.Payload{Schema: "github.hold.recovery.v1", Data: json.RawMessage(`{}`)}); err != nil {
		return nil, err
	}
	dial := h.Dial
	if dial == nil {
		dial = func(endpoint, token string) (ToolbeltClient, error) { return client.New(endpoint, token) }
	}
	tb, err := dial(r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.running[r.Execution.ID] = true
	h.mu.Unlock()
	handle := &holdHandle{done: make(chan struct{})}
	go func() {
		defer close(handle.done)
		defer func() {
			h.mu.Lock()
			delete(h.running, r.Execution.ID)
			h.mu.Unlock()
		}()
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		handle.result, handle.err = h.decide(cctx, tb, r)
	}()
	return handle, nil
}

// Recover never adopts: an attempt interrupted by a restart is retried.
func (h *HoldExecutorImpl) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrNotAdoptable
}

// Stop proves the attempt is not running: nothing outlives Run in another
// process, and this instance reports its in-flight attempts.
func (h *HoldExecutorImpl) Stop(_ context.Context, id domain.ID, _ uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running[id] {
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

// heldWake is v1's wording for a held issue whose condition happened.
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
