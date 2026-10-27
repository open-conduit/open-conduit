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
// resume-versus-fresh choice, the handoff note and attachment state, keyed by
// Work ID in the executor's namespaced coordination KV. Core never reads it,
// and no runtime API carries any of it. Records written while the handoff
// experiment ran may still carry its trial and feedback fields; they are
// ignored.

// pendingTurn is the continuity decision for a segment in flight. It is
// recorded before launch, so a restart reuses the same decision.
type pendingTurn struct {
	Execution domain.ID `json:"execution"`
	Segment   string    `json:"segment"`
	Session   string    `json:"session"`
	Kind      turnKind  `json:"kind"`
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
	// Reused means an earlier undone decision was taken over.
	Reused bool
}

// decide chooses resume or fresh for a new segment. A session that has run
// before is resumed, even when its first turn never ended. A cold one (the
// last turn ended more than handoff_minutes ago, so its prompt cache has
// expired) follows cold_turns: "handoff" starts a fresh session from the last
// turn's note, "resume" resumes it. A session that never got as far as being
// written always starts afresh from the note.
func (e *Executor) decide(rec record, cfg Config, now time.Time, exists func(string) bool) decision {
	if p := rec.Pending; p != nil && p.Session != "" {
		return decision{Session: p.Session, Resume: exists(p.Session), Kind: p.Kind, Reused: true}
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
	case cfg.ColdTurns != ColdHandoff || rec.LastEndedAt == nil:
	case now.Sub(*rec.LastEndedAt) > time.Duration(*cfg.HandoffMinutes)*time.Minute:
		handoff = true
	}
	if handoff {
		d.Kind = kindHandoff
		if d.Resume {
			d.Session, d.Resume = newSession(), false
		}
	}
	return d
}

// beginTurn records a decision before launch.
func beginTurn(rec *record, d decision, execution domain.ID, segment string, now time.Time, attach attachInfo) {
	if d.Session != rec.Session {
		if rec.Session != "" {
			rec.PastSessions = append(rec.PastSessions, rec.Session)
		}
		rec.Session = d.Session
	}
	rec.Pending = &pendingTurn{Execution: execution, Segment: segment, Session: d.Session, Kind: d.Kind, StartedAt: now}
	rec.Attach = &attach
}

// turnEnd is what applying one finished segment changed, for telemetry.
type turnEnd struct {
	parsed  parsed
	session string
	turn    int
}

// finishTurn applies one finished segment exactly once (a repeated apply of
// the same segment returns the same per-turn usage and changes nothing).
func finishTurn(rec *record, segment string, p parsed, now time.Time) turnEnd {
	if rec.Applied == segment {
		return turnEnd{parsed: perTurn(p, rec.AppliedBase), session: rec.Session, turn: rec.Turns}
	}
	end := turnEnd{session: rec.Session}
	if pend := rec.Pending; pend != nil && pend.Segment == segment {
		rec.Pending = nil
	}
	rec.AppliedBase = rec.Totals
	p = perTurn(p, rec.Totals)
	if p.Totals != nil {
		rec.Totals = p.Totals
	}
	rec.Applied = segment
	rec.LastEndedAt = &now
	if p.OK {
		rec.Turns++
		if p.Status != nil {
			rec.Handoff = p.Status.Handoff
			rec.LastState = p.Status.State
		}
	}
	end.parsed, end.turn = p, rec.Turns
	return end
}

func (e *Executor) report(name string, attrs map[string]any) {
	if e.o.Report != nil {
		attrs["executor"] = e.o.Namespace
		e.o.Report(name, attrs)
	}
}

// MaintainReport summarizes one Maintain pass.
type MaintainReport struct {
	Released int
}

// Maintain is the executor's host-driven housekeeping, safe to repeat and
// after restart: it releases the continuity of Work that became terminal (or
// no longer exists), since it runs no further segments.
func (e *Executor) Maintain(ctx context.Context) (MaintainReport, error) {
	var report MaintainReport
	err := e.o.Store.Transact(ctx, storage.Guard{Now: e.o.Now()}, func(tx storage.Tx) error {
		report = MaintainReport{}
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
			case err != nil:
				return err
			case !w.State.Terminal():
				continue
			}
			if err := tx.Delete(e.o.Namespace, workKey(rec.Work)); err != nil {
				return err
			}
			report.Released++
		}
		return nil
	})
	return report, err
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
