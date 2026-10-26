package claudecode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Continuity is private to this executor: session identifiers, the
// resume-versus-fresh choice, the handoff note, attachment state and the
// handoff experiment's trials and feedback, keyed by Work ID in the
// executor's namespaced coordination KV. Core never reads it, and no runtime
// API carries any of it.

// Handoff experiment arms.
const (
	ArmHandoff = "handoff"
	ArmResume  = "resume"
)

// FeedbackText is the comment after a turn drawn into the handoff experiment.
// It reads the same whichever arm the draw picked, so whoever answers does not
// know whether the session resumed or started afresh from a handoff.
const FeedbackText = "There was a long gap between messages, did you find performance was maintained from previous turns? " +
	"Hit 👍 on this comment if it was, or 👎 if it got worse."

// FeedbackWindow is how long a feedback comment is watched for a 👍 or 👎.
const FeedbackWindow = 7 * 24 * time.Hour

// Trial is the run of turns that follows one draw in the handoff experiment,
// added up so the two arms compare through to completion rather than on the
// drawn turn alone.
type Trial struct {
	ID           string    `json:"id"`
	Arm          string    `json:"arm"`
	StartedAt    time.Time `json:"started_at"`
	Turns        int       `json:"turns"`
	Failed       int       `json:"failed,omitempty"`
	Cost         float64   `json:"cost"`
	Tokens       *Tokens   `json:"tokens,omitempty"`
	ModelCalls   int       `json:"model_calls"`
	ToolCalls    int       `json:"tool_calls"`
	AgentSeconds float64   `json:"agent_seconds"`
	// FollowUps are the inputs and messages the turns were handed after the
	// draw.
	FollowUps int `json:"follow_ups"`
}

func (t *Trial) add(p parsed, seconds float64, followUps int) {
	t.Turns++
	if !p.OK {
		t.Failed++
	}
	if p.Cost != nil {
		t.Cost += *p.Cost
	}
	t.Tokens = t.Tokens.Add(p.Tokens)
	t.ModelCalls += p.ModelCalls
	t.ToolCalls += p.ToolCalls
	t.AgentSeconds += seconds
	t.FollowUps += followUps
}

// Feedback is one blinded question about a turn drawn into the experiment.
// It is asked through the host's Feedback hook (a tracker comment with a
// discoverable marker) and watched for reactions within FeedbackWindow.
type Feedback struct {
	Intent    string    `json:"intent"`
	Session   string    `json:"session"`
	Arm       string    `json:"arm"`
	Trial     string    `json:"trial,omitempty"`
	Turn      int       `json:"turn"`
	State     string    `json:"state"`
	Ref       string    `json:"ref,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	PostedAt  time.Time `json:"posted_at,omitempty"`
}

const (
	feedbackAsk      = "ask"
	feedbackWatching = "watching"
)

// pendingTurn is the continuity decision for a segment in flight. It is
// recorded before launch, so a restart reuses the same draw instead of
// drawing again.
type pendingTurn struct {
	Execution domain.ID `json:"execution"`
	Segment   string    `json:"segment"`
	Session   string    `json:"session"`
	Kind      turnKind  `json:"kind"`
	Arm       string    `json:"arm,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// attachInfo is what an operator needs to attach to the Work's session.
type attachInfo struct {
	Dir            string   `json:"dir"`
	Binary         string   `json:"binary"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permission_mode"`
	PluginDir      string   `json:"plugin_dir,omitempty"`
	ExtraArgs      []string `json:"extra_args,omitempty"`
}

// record is a Work's private continuity.
type record struct {
	Work         domain.ID `json:"work"`
	Scope        domain.ID `json:"scope"`
	Session      string    `json:"session,omitempty"`
	PastSessions []string  `json:"past_sessions,omitempty"`
	// Turns counts turns that ended with a status.
	Turns int `json:"turns"`
	// Handoff is the last turn's note to a fresh session.
	Handoff     string     `json:"handoff,omitempty"`
	LastState   string     `json:"last_state,omitempty"`
	LastEndedAt *time.Time `json:"last_ended_at,omitempty"`
	// Totals are the session's cumulative totals at its latest result;
	// AppliedBase the totals the latest applied turn was measured against,
	// so applying that turn again yields the same per-turn delta.
	Totals      *Totals      `json:"totals,omitempty"`
	AppliedBase *Totals      `json:"applied_base,omitempty"`
	Applied     string       `json:"applied,omitempty"`
	Pending     *pendingTurn `json:"pending,omitempty"`
	Trial       *Trial       `json:"trial,omitempty"`
	Feedback    []Feedback   `json:"feedback,omitempty"`
	Attach      *attachInfo  `json:"attach,omitempty"`
}

// limitRecord holds every turn on this executor until the usage limit lifts.
type limitRecord struct {
	Until time.Time `json:"until"`
}

const (
	workPrefix = "work/"
	limitKey   = "limit"
	// limitGrace is how long after the usage limit lifts a turn it stopped
	// runs again; limitWait is the wait when claude did not say when.
	limitGrace = 2 * time.Minute
	limitWait  = 30 * time.Minute
)

func workKey(id domain.ID) string { return workPrefix + string(id) }

func getJSON(tx storage.Tx, ns, key string, v any) (bool, error) {
	b, err := tx.Get(ns, key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func putJSON(tx storage.Tx, ns, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(ns, key, b)
}

// guard fences a continuity write with the attempt's lease when there is one,
// so a superseded epoch cannot change a Work's continuity.
func (e *Executor) guard(execution domain.ID, lease domain.Lease) storage.Guard {
	g := storage.Guard{Now: e.o.Now()}
	if execution != "" && lease.Owner != "" {
		g.Lease = &domain.LeaseRef{ExecutionID: execution, Owner: lease.Owner, Epoch: lease.Epoch}
	}
	return g
}

func (e *Executor) loadRecord(tx storage.Tx, w domain.Work) (record, error) {
	rec := record{Work: w.ID, Scope: w.ScopeID}
	_, err := getJSON(tx, e.o.Namespace, workKey(w.ID), &rec)
	return rec, err
}

// newSession makes a random UUID, as claude --session-id wants.
func newSession() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// decision is how one segment continues the Work's conversation.
type decision struct {
	Session string
	Resume  bool
	Kind    turnKind
	Arm     string
	// Reused means an earlier undone decision was taken over.
	Reused bool
}

// decide chooses resume or fresh for a new segment. A session that has run
// before is resumed, even when its first turn never ended. A cold one (the
// last turn ended more than handoff_minutes ago, so its prompt cache has
// expired) is in the experiment: a draw sends handoff_share of them to a fresh
// session started from the last turn's note, and resumes the rest. A session
// that never got as far as being written always starts afresh from the note.
func (e *Executor) decide(rec record, cfg Config, now time.Time, exists func(string) bool) decision {
	if p := rec.Pending; p != nil && p.Session != "" {
		return decision{Session: p.Session, Resume: exists(p.Session), Kind: p.Kind, Arm: p.Arm, Reused: true}
	}
	d := decision{Session: rec.Session, Kind: kindEvents}
	if rec.Turns == 0 {
		d.Kind = kindStart
	}
	if d.Session == "" {
		d.Session = newSession()
		return d
	}
	d.Resume = exists(d.Session)
	handoff := false
	switch {
	case rec.Turns == 0 || strings.TrimSpace(rec.Handoff) == "":
	case !d.Resume:
		handoff = true
	case *cfg.HandoffShare <= 0 || rec.LastEndedAt == nil:
	case now.Sub(*rec.LastEndedAt) <= time.Duration(*cfg.HandoffMinutes)*time.Minute:
	case e.o.Draw() < *cfg.HandoffShare:
		handoff, d.Arm = true, ArmHandoff
	default:
		d.Arm = ArmResume
	}
	if handoff {
		d.Kind = kindHandoff
		if d.Resume {
			d.Session, d.Resume = newSession(), false
		}
	}
	return d
}

// begin records a decision before launch. It returns a trial the new draw
// ended, if any.
func beginTurn(rec *record, d decision, execution domain.ID, segment string, now time.Time, attach attachInfo) *Trial {
	if d.Session != rec.Session {
		if rec.Session != "" {
			rec.PastSessions = append(rec.PastSessions, rec.Session)
		}
		rec.Session = d.Session
	}
	var ended *Trial
	if d.Arm != "" && !d.Reused {
		ended = rec.Trial
		rec.Trial = &Trial{ID: fmt.Sprintf("%s-%d", rec.Work, rec.Turns+1), Arm: d.Arm, StartedAt: now}
	}
	rec.Pending = &pendingTurn{Execution: execution, Segment: segment, Session: d.Session, Kind: d.Kind, Arm: d.Arm, StartedAt: now}
	rec.Attach = &attach
	return ended
}

// turnEnd is what applying one finished segment changed, for telemetry.
type turnEnd struct {
	parsed      parsed
	arm         string
	kind        turnKind
	session     string
	trial       *Trial
	ended       *Trial
	askFeedback bool
	turn        int
}

// finishTurn applies one finished segment exactly once (a repeated apply of
// the same segment returns the same per-turn usage and changes nothing).
func finishTurn(rec *record, segment string, execution domain.ID, p parsed, now time.Time, followUps int, feedback bool) turnEnd {
	if rec.Applied == segment {
		return turnEnd{parsed: perTurn(p, rec.AppliedBase), session: rec.Session, turn: rec.Turns}
	}
	end := turnEnd{session: rec.Session}
	var started time.Time
	if pend := rec.Pending; pend != nil && pend.Segment == segment {
		end.arm, end.kind, started = pend.Arm, pend.Kind, pend.StartedAt
		rec.Pending = nil
	}
	rec.AppliedBase = rec.Totals
	p = perTurn(p, rec.Totals)
	if p.Totals != nil {
		rec.Totals = p.Totals
	}
	rec.Applied = segment
	rec.LastEndedAt = &now
	seconds := 0.0
	if !started.IsZero() {
		seconds = now.Sub(started).Seconds()
	}
	if p.OK {
		rec.Turns++
		if p.Status != nil {
			rec.Handoff = p.Status.Handoff
			rec.LastState = p.Status.State
		}
		if rec.Trial != nil {
			rec.Trial.add(p, seconds, followUps)
		}
		if end.arm != "" && feedback {
			f := Feedback{Intent: "claude-code/handoff-feedback/" + string(execution) + "/" + segment, Session: rec.Session, Arm: end.arm, Turn: rec.Turns, State: feedbackAsk, CreatedAt: now}
			if rec.Trial != nil {
				f.Trial = rec.Trial.ID
			}
			rec.Feedback = append(rec.Feedback, f)
			end.askFeedback = true
		}
		if p.Status != nil && p.Status.State == StateFinished && rec.Trial != nil {
			end.ended, rec.Trial = rec.Trial, nil
		}
	} else if rec.Trial != nil && !p.Limited {
		// A limit hit says nothing about the arm, so it is not counted.
		rec.Trial.add(p, seconds, 0)
	}
	end.parsed, end.trial, end.turn = p, rec.Trial, rec.Turns
	return end
}

// trialAttrs is the experiment telemetry for one trial.
func trialAttrs(rec record, t *Trial) map[string]any {
	return map[string]any{"scope": string(rec.Scope), "work": string(rec.Work), "trial": t.ID, "arm": t.Arm}
}

func (e *Executor) reportTrialEnd(rec record, t *Trial, endedBy, state string) {
	a := trialAttrs(rec, t)
	a["ended_by"] = endedBy
	a["completed"] = endedBy == "finished"
	a["final_state"] = state
	a["turns"] = t.Turns
	a["failed_turns"] = t.Failed
	a["cost_usd"] = t.Cost
	a["model_calls"] = t.ModelCalls
	a["tool_calls"] = t.ToolCalls
	a["agent_seconds"] = int(t.AgentSeconds)
	a["elapsed_hours"] = e.o.Now().Sub(t.StartedAt).Hours()
	a["follow_ups"] = t.FollowUps
	if t.Tokens != nil {
		a["input_tokens"] = t.Tokens.Input
		a["output_tokens"] = t.Tokens.Output
		a["cache_read_tokens"] = t.Tokens.CacheRead
		a["cache_creation_tokens"] = t.Tokens.CacheCreation
	}
	e.report("handoff_trial", a)
}

func feedbackAttrs(rec record, f Feedback) map[string]any {
	return map[string]any{"scope": string(rec.Scope), "work": string(rec.Work), "session": f.Session, "arm": f.Arm, "trial": f.Trial, "turn": f.Turn, "comment": f.Ref}
}

// verdict is what a feedback comment's reactions say: "maintained" on a 👍,
// "worse" on a 👎, "mixed" on both.
func verdict(up, down int) string {
	switch {
	case up > 0 && down > 0:
		return "mixed"
	case down > 0:
		return "worse"
	default:
		return "maintained"
	}
}

func (e *Executor) report(name string, attrs map[string]any) {
	if e.o.Report != nil {
		attrs["executor"] = e.o.Namespace
		e.o.Report(name, attrs)
	}
}

// MaintainReport summarizes one Maintain pass.
type MaintainReport struct {
	Asked, Answered, Expired, TrialsEnded, Released int
	Codes                                           []string
}

// Maintain is the executor's host-driven housekeeping, safe to repeat and
// after restart. It asks pending blinded feedback questions through the
// Feedback hook (exactly once by intent marker), reads reactions on watched
// questions within FeedbackWindow, ends the trial of a Work that became
// terminal without finishing ("dropped") and releases the continuity of
// terminal Work once nothing is left to watch.
func (e *Executor) Maintain(ctx context.Context) (MaintainReport, error) {
	var report MaintainReport
	type item struct {
		rec  record
		work domain.Work
		ok   bool
	}
	var items []item
	err := e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		items = nil
		entries, err := tx.List(e.o.Namespace, workPrefix)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			var rec record
			if json.Unmarshal(entry.Value, &rec) != nil || rec.Work == "" {
				continue
			}
			w, err := tx.Work(rec.Work)
			switch {
			case errors.Is(err, storage.ErrNotFound):
				items = append(items, item{rec: rec})
			case err != nil:
				return err
			default:
				items = append(items, item{rec: rec, work: w, ok: true})
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	var errs []error
	for _, it := range items {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if err := e.maintainOne(ctx, it.rec, it.work, it.ok, &report); err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

func (e *Executor) maintainOne(ctx context.Context, rec record, w domain.Work, known bool, report *MaintainReport) error {
	now := e.o.Now()
	type change struct {
		intent string
		drop   bool
		posted string
	}
	var changes []change
	var errs []error
	for _, f := range rec.Feedback {
		switch {
		case f.State == feedbackAsk && (e.o.Feedback == nil || !known):
			changes = append(changes, change{intent: f.Intent, drop: true})
			report.Codes = append(report.Codes, "feedback_unavailable")
		case f.State == feedbackAsk:
			ref, err := e.o.Feedback.Ask(ctx, w, f.Intent, FeedbackText)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ref != "" {
				changes = append(changes, change{intent: f.Intent, posted: ref})
				f.Ref = ref
				e.report("handoff_feedback_asked", feedbackAttrs(rec, f))
				report.Asked++
			}
		case f.State == feedbackWatching && (now.Sub(f.PostedAt) > FeedbackWindow || e.o.Feedback == nil || !known):
			changes = append(changes, change{intent: f.Intent, drop: true})
			report.Expired++
		case f.State == feedbackWatching:
			counts, err := e.o.Feedback.Reactions(ctx, w, f.Ref)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			up, down := counts["+1"], counts["-1"]
			if up == 0 && down == 0 {
				continue
			}
			a := feedbackAttrs(rec, f)
			a["thumbs_up"], a["thumbs_down"], a["verdict"] = up, down, verdict(up, down)
			a["hours_after"] = now.Sub(f.PostedAt).Hours()
			e.report("handoff_feedback", a)
			changes = append(changes, change{intent: f.Intent, drop: true})
			report.Answered++
		}
	}
	terminal := !known || w.State.Terminal()
	if len(changes) == 0 && !terminal {
		return errors.Join(errs...) // nothing to write
	}
	var ended *Trial
	var final record
	err := e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		ended = nil
		var cur record
		found, err := getJSON(tx, e.o.Namespace, workKey(rec.Work), &cur)
		if err != nil || !found {
			return err
		}
		var keep []Feedback
		for _, f := range cur.Feedback {
			dropped := false
			for _, c := range changes {
				if c.intent != f.Intent {
					continue
				}
				if c.drop {
					dropped = true
				} else if f.State == feedbackAsk {
					f.State, f.Ref, f.PostedAt = feedbackWatching, c.posted, now
				}
			}
			if !dropped {
				keep = append(keep, f)
			}
		}
		cur.Feedback = keep
		if terminal {
			// Terminal Work runs no further segments: an undone decision
			// will never be taken over, and an open trial was dropped.
			cur.Pending = nil
			if cur.Trial != nil {
				ended, cur.Trial = cur.Trial, nil
			}
		}
		final = cur
		if terminal && len(cur.Feedback) == 0 {
			report.Released++
			return tx.Delete(e.o.Namespace, workKey(rec.Work))
		}
		return putJSON(tx, e.o.Namespace, workKey(rec.Work), cur)
	})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if ended != nil {
		e.reportTrialEnd(final, ended, "dropped", final.LastState)
		report.TrialsEnded++
	}
	return errors.Join(errs...)
}

// ErrNoSession means the Work has no Claude Code session to attach to. It
// is the executor's extension.ErrNothingToAttach.
var ErrNoSession = fmt.Errorf("claude code: no session for this work: %w", extension.ErrNothingToAttach)

// Attach implements extension.AttachExecutor: an interactive `claude
// --resume` on the Work's session, for an operator to attach to. It clears
// the handoff note, so the next turn resumes the session even when it is
// cold, since what the operator did in it is not in the note.
func (e *Executor) Attach(ctx context.Context, work domain.ID) (extension.AttachSpec, error) {
	var spec extension.AttachSpec
	err := e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		var rec record
		found, err := getJSON(tx, e.o.Namespace, workKey(work), &rec)
		if err != nil {
			return err
		}
		if !found || rec.Session == "" || rec.Attach == nil {
			return ErrNoSession
		}
		a := rec.Attach
		spec.Argv = append([]string{a.Binary, "--resume", rec.Session, "--model", a.Model, "--permission-mode", a.PermissionMode}, a.ExtraArgs...)
		if a.PluginDir != "" {
			spec.Argv = append(spec.Argv, "--plugin-dir", a.PluginDir)
		}
		spec.Dir = a.Dir
		rec.Handoff = ""
		return putJSON(tx, e.o.Namespace, workKey(work), rec)
	})
	if err != nil {
		return extension.AttachSpec{}, err
	}
	return spec, nil
}
