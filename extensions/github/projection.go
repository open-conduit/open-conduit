package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// StatusSchema is the outcome data schema an execution uses to declare the
// projected tracker status ("help", "review", "done") and, optionally, one
// comment to publish. The projector publishes that comment exactly once per
// execution, recovering ambiguous posts through its marker.
const StatusSchema = "github.status.v1"

// StatusDeclaration is the StatusSchema payload.
type StatusDeclaration struct {
	Status  string `json:"status,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ErrIntentConflict means a stable intent ID was reused for different content.
var ErrIntentConflict = errors.New("github intent reused with different content")

// ProjectorOptions configure a Projector.
type ProjectorOptions struct {
	Store    storage.Store
	Client   *Client
	Settings Settings
	Now      func() time.Time
	// SearchDelay is how long after an ambiguous post the marker search waits,
	// so GitHub's listing reflects a comment that was created (default 30s).
	SearchDelay time.Duration
	// AfterPost runs after a comment was created and before local completion
	// is recorded. Tests use it to crash at exactly that boundary.
	AfterPost func(intent string)
}

// Projector converges tracker projections from durable Work state. Labels are
// level-triggered (re-read and re-applied, no marker needed). Comments are
// not re-derivable: each has a stable intent ID, a durable intent record and
// a discoverable marker, so a post whose local completion was lost is found
// again instead of repeated. Comment reactions acknowledge routed human input.
type Projector struct {
	o  ProjectorOptions
	ns string
}

// ProjectionResult reports one pass.
type ProjectionResult struct {
	Labeled, Posted, Recovered, Reacted int
	Codes                               []string
}

// NewProjector validates options.
func NewProjector(o ProjectorOptions) (*Projector, error) {
	if o.Store == nil || o.Client == nil || o.Settings.Namespace == "" {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SearchDelay <= 0 {
		o.SearchDelay = 30 * time.Second
	}
	return &Projector{o: o, ns: "github.projection." + digest(o.Settings.Namespace)[:16]}, nil
}

type labelState struct {
	Work       domain.ID `json:"work"`
	Applied    string    `json:"applied"`
	VerifiedAt time.Time `json:"verified_at"`
}

type intentRecord struct {
	ID          string    `json:"id"`
	Work        domain.ID `json:"work"`
	Repo        string    `json:"repo"`
	Number      int       `json:"number"`
	BodyDigest  string    `json:"body_digest"`
	State       string    `json:"state"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	CommentID   int64     `json:"comment_id,omitempty"`
	URL         string    `json:"url,omitempty"`
}

const (
	intentPending   = "pending"
	intentAttempted = "attempted"
	intentPosted    = "posted"
)

// Intent is one non-re-derivable comment. ID must be stable across retries
// and restarts (for example "outcome/<execution id>").
type Intent struct {
	ID     string
	Work   domain.ID
	Number int
	Body   string
}

func (p *Projector) get(tx storage.Tx, key string, v any) (bool, error) {
	data, err := tx.Get(p.ns, key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

func (p *Projector) put(tx storage.Tx, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(p.ns, key, data)
}

func (p *Projector) transact(ctx context.Context, fn func(storage.Tx) error) error {
	return p.o.Store.Transact(ctx, storage.Guard{Now: p.o.Now()}, fn)
}

func markerID(intent string) string { return "i" + digest(intent)[:32] }

// Deliver publishes an intent at most once as observed through its marker.
// It returns the durable record state ("posted", or "attempted"/"pending"
// when delivery is still in progress or waiting for its marker search).
func (p *Projector) Deliver(ctx context.Context, in Intent) (string, error) {
	if in.ID == "" || in.Number <= 0 || strings.TrimSpace(in.Body) == "" {
		return "", storage.ErrInvalid
	}
	repo, key, bodyDigest := p.o.Settings.Repo, "intent/"+digest(in.ID)[:40], digest(in.Body)
	var rec intentRecord
	err := p.transact(ctx, func(tx storage.Tx) error {
		found, err := p.get(tx, key, &rec)
		if err != nil {
			return err
		}
		if !found {
			rec = intentRecord{ID: in.ID, Work: in.Work, Repo: repo, Number: in.Number, BodyDigest: bodyDigest, State: intentPending, CreatedAt: p.o.Now().UTC()}
			return p.put(tx, key, rec)
		}
		if rec.BodyDigest != bodyDigest || rec.Number != in.Number || rec.Repo != repo {
			return ErrIntentConflict
		}
		return nil
	})
	if err != nil || rec.State == intentPosted {
		return rec.State, err
	}
	marker := markerID(in.ID)
	if rec.State == intentAttempted {
		if p.o.Now().Sub(rec.AttemptedAt) < p.o.SearchDelay {
			return rec.State, nil
		}
		comments, err := p.o.Client.IssueComments(ctx, repo, in.Number, rec.CreatedAt.Add(-time.Minute))
		if err != nil {
			return rec.State, err
		}
		for _, c := range comments {
			if hasIntent(c.Body, marker) {
				return p.complete(ctx, key, c.ID, c.HTMLURL, true)
			}
		}
	}
	if p.o.Settings.Projection.DryRun {
		return rec.State, nil
	}
	err = p.transact(ctx, func(tx storage.Tx) error {
		var cur intentRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		if cur.State != rec.State || cur.Attempts != rec.Attempts {
			return storage.ErrConflict
		}
		cur.State, cur.Attempts, cur.AttemptedAt = intentAttempted, cur.Attempts+1, p.o.Now().UTC()
		return p.put(tx, key, cur)
	})
	if err != nil {
		return rec.State, err
	}
	posted, err := p.o.Client.Comment(ctx, repo, in.Number, WithMarker(in.Body, string(in.Work), marker))
	if err != nil {
		// Ambiguous: the marker search decides after SearchDelay.
		return intentAttempted, err
	}
	if p.o.AfterPost != nil {
		p.o.AfterPost(in.ID)
	}
	return p.complete(ctx, key, posted.ID, posted.URL, false)
}

func (p *Projector) complete(ctx context.Context, key string, id int64, url string, recovered bool) (string, error) {
	err := p.transact(ctx, func(tx storage.Tx) error {
		var cur intentRecord
		if _, err := p.get(tx, key, &cur); err != nil {
			return err
		}
		cur.State, cur.CommentID, cur.URL = intentPosted, id, url
		return p.put(tx, key, cur)
	})
	if err != nil {
		return intentAttempted, err
	}
	if recovered {
		return "recovered", nil
	}
	return intentPosted, nil
}

type projected struct {
	work    domain.Work
	number  int
	latest  *domain.Execution
	intents []Intent
}

func declaration(x *domain.Execution) (StatusDeclaration, bool) {
	var d StatusDeclaration
	if x == nil || x.Outcome == nil || x.Outcome.Data.Schema != StatusSchema || json.Unmarshal(x.Outcome.Data.Data, &d) != nil {
		return d, false
	}
	return d, true
}

// Desired returns the managed labels a Work's durable state projects: the
// claimed label while the Work owns the issue, and at most one status label,
// absent while an attempt runs (v1: the status comes off while a turn runs).
func (p *Projector) Desired(w domain.Work, latest *domain.Execution) []string {
	l := p.o.Settings.Labels
	out := []string{l.Claimed}
	if w.State == domain.WorkRunning || w.State == domain.WorkCancelled {
		return out
	}
	status := ""
	if d, ok := declaration(latest); ok {
		status = d.Status
	}
	if status == "" {
		switch w.State {
		case domain.WorkCompleted:
			status = "done"
		case domain.WorkFailed:
			status = "help"
		}
	}
	if label := l.Status(status); label != "" {
		out = append(out, label)
	}
	return out
}

func (p *Projector) snapshot(ctx context.Context) ([]projected, error) {
	var out []projected
	err := p.transact(ctx, func(tx storage.Tx) error {
		out = nil
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		byIssue := map[int]projected{}
		for _, w := range works {
			if string(w.ScopeID) != p.o.Settings.Scope {
				continue
			}
			number := 0
			for _, k := range w.CorrelationKeys {
				if k.Source != p.o.Settings.Namespace {
					continue
				}
				if n, _, ok := ParseIssueKey(k.ExternalID); ok {
					number = n
					break
				}
			}
			if number == 0 {
				continue
			}
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
			item := projected{work: w, number: number}
			for i := range xs {
				x := xs[i]
				if x.Outcome != nil {
					item.latest = &x
				}
				if d, ok := declaration(&x); ok && strings.TrimSpace(d.Comment) != "" {
					item.intents = append(item.intents, Intent{ID: "outcome/" + string(x.ID), Work: w.ID, Number: number, Body: d.Comment})
				}
			}
			if n := len(xs); n > 0 && (xs[n-1].State == domain.ExecutionPending || xs[n-1].State == domain.ExecutionRunning) {
				// The newest attempt is live: project no stale status.
				item.latest = nil
			}
			prev, ok := byIssue[number]
			switch {
			case !ok:
				byIssue[number] = item
			case prev.work.State.Terminal() && !w.State.Terminal(),
				prev.work.State.Terminal() == w.State.Terminal() && w.CreatedAt.After(prev.work.CreatedAt):
				item.intents = append(prev.intents, item.intents...)
				byIssue[number] = item
			default:
				prev.intents = append(prev.intents, item.intents...)
				byIssue[number] = prev
			}
		}
		numbers := make([]int, 0, len(byIssue))
		for n := range byIssue {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		for _, n := range numbers {
			out = append(out, byIssue[n])
		}
		return nil
	})
	return out, err
}

func labelDigest(labels []string) string {
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	return digest(sorted...)
}

// converge applies one issue's managed labels against what GitHub shows.
func (p *Projector) converge(ctx context.Context, item projected, desired []string) error {
	issue, err := p.o.Client.Issue(ctx, p.o.Settings.Repo, item.number)
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusGone) {
		return nil
	}
	if err != nil {
		return err
	}
	l := p.o.Settings.Labels
	managed := append([]string{l.Claimed}, l.Statuses()...)
	var current []string
	for _, x := range issue.Labels {
		current = append(current, x.Name)
	}
	var add, remove []string
	for _, m := range managed {
		want, present := has(desired, m), has(current, m)
		switch {
		case want && !present:
			add = append(add, m)
		case !want && present:
			remove = append(remove, m)
		}
	}
	if (len(add) == 0 && len(remove) == 0) || p.o.Settings.Projection.DryRun {
		return nil
	}
	return p.o.Client.EditLabels(ctx, p.o.Settings.Repo, item.number, add, remove)
}

// Converge runs one bounded projection pass: labels, comment intents and
// acknowledgement reactions. Rate limits stop the pass; other per-item
// failures are reported as safe codes and retried on the next pass.
func (p *Projector) Converge(ctx context.Context, limit int) (ProjectionResult, error) {
	var res ProjectionResult
	if p.o.Settings.Projection.Disabled {
		return res, nil
	}
	if limit <= 0 {
		limit = 50
	}
	items, err := p.snapshot(ctx)
	if err != nil {
		return res, err
	}
	fail := func(stage string, err error) error {
		var limitErr *RateLimitError
		if errors.As(err, &limitErr) {
			res.Codes = append(res.Codes, stage+":rate_limited")
			return err
		}
		if errors.Is(err, ErrCredential) {
			res.Codes = append(res.Codes, stage+":credential")
			return err
		}
		res.Codes = append(res.Codes, stage+":"+errorCode(err))
		return nil
	}
	budget := limit
	if p.o.Settings.TrackIssues {
		for _, item := range items {
			if budget <= 0 {
				break
			}
			desired := p.Desired(item.work, item.latest)
			want := labelDigest(desired)
			key := "labels/" + strconv.Itoa(item.number)
			var st labelState
			if err := p.transact(ctx, func(tx storage.Tx) error { _, e := p.get(tx, key, &st); return e }); err != nil {
				return res, err
			}
			verifyDue := !item.work.State.Terminal() && p.o.Now().Sub(st.VerifiedAt) >= p.o.Settings.VerifyInterval
			if st.Applied == want && st.Work == item.work.ID && !verifyDue {
				continue
			}
			budget--
			if err := p.converge(ctx, item, desired); err != nil {
				if stop := fail("labels", err); stop != nil {
					return res, stop
				}
				continue
			}
			if p.o.Settings.Projection.DryRun {
				continue
			}
			res.Labeled++
			if err := p.transact(ctx, func(tx storage.Tx) error {
				return p.put(tx, key, labelState{Work: item.work.ID, Applied: want, VerifiedAt: p.o.Now().UTC()})
			}); err != nil {
				return res, err
			}
		}
	}
	intents := 0
	for _, item := range items {
		for _, in := range item.intents {
			if intents >= limit {
				break
			}
			state, err := p.state(ctx, in.ID)
			if err != nil {
				return res, err
			}
			if state == intentPosted {
				continue
			}
			intents++
			state, err = p.Deliver(ctx, in)
			switch {
			case err != nil:
				if stop := fail("comment", err); stop != nil {
					return res, stop
				}
			case state == "recovered":
				res.Recovered++
			case state == intentPosted:
				res.Posted++
			}
		}
	}
	reacted, err := p.acknowledge(ctx, 200)
	res.Reacted = reacted
	if err != nil {
		if stop := fail("react", err); stop != nil {
			return res, stop
		}
	}
	return res, nil
}

func (p *Projector) state(ctx context.Context, intent string) (string, error) {
	var rec intentRecord
	err := p.transact(ctx, func(tx storage.Tx) error { _, e := p.get(tx, "intent/"+digest(intent)[:40], &rec); return e })
	return rec.State, err
}

// delivered dispositions hand an Event to its Work as input.
var delivered = map[string]bool{"queue": true, "inject": true, "interrupt": true, "wake": true, "wait_context": true}

type ackCursor struct {
	After uint64 `json:"after"`
}

// acknowledge reacts +1 to human comments the router delivered to Work, the
// v1 "heard" signal. Reactions are idempotent at GitHub, so a lost cursor
// update only repeats a no-op. It never passes an unrouted comment.
func (p *Projector) acknowledge(ctx context.Context, limit int) (int, error) {
	var cur ackCursor
	var batch []domain.AcceptedEvent
	err := p.transact(ctx, func(tx storage.Tx) error {
		if _, err := p.get(tx, "ack/cursor", &cur); err != nil {
			return err
		}
		var err error
		batch, err = tx.Events(cur.After, limit)
		return err
	})
	if err != nil {
		return 0, err
	}
	reacted := 0
	next := cur.After
	for _, a := range batch {
		e := a.Event
		if e.Source != p.o.Settings.Namespace || e.Type != TypeCommentHuman {
			next = a.Sequence
			continue
		}
		if !a.Routed {
			break
		}
		var c CommentPayload
		if a.RoutedWorkID != "" && delivered[a.Disposition] &&
			json.Unmarshal(e.Data.Data, &c) == nil && !c.Edited && c.ID != 0 && !p.o.Settings.Projection.DryRun {
			err := p.o.Client.React(ctx, p.o.Settings.Repo, c.Kind, c.ID, "+1")
			var limitErr *RateLimitError
			if errors.As(err, &limitErr) || errors.Is(err, ErrCredential) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Retry this comment later; never skip it for a transient cause.
				if saveErr := p.saveCursor(ctx, next); saveErr != nil {
					return reacted, saveErr
				}
				return reacted, err
			}
			// A permanent refusal (deleted comment, locked conversation) is
			// skipped: the acknowledgement is a courtesy, not durable state.
			if err == nil && c.Kind != KindReview {
				reacted++
			}
		}
		next = a.Sequence
	}
	return reacted, p.saveCursor(ctx, next)
}

func (p *Projector) saveCursor(ctx context.Context, after uint64) error {
	return p.transact(ctx, func(tx storage.Tx) error { return p.put(tx, "ack/cursor", ackCursor{After: after}) })
}

func errorCode(err error) string {
	var s *StatusError
	switch {
	case errors.As(err, &s):
		return fmt.Sprintf("http_%d", s.Status)
	case errors.Is(err, ErrIntentConflict):
		return "intent_conflict"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	return "failed"
}
