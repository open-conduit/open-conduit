package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// StatusSchema is the outcome data schema an execution uses to declare the
// projected status ("building", "review", "help", "done", "finished") and,
// optionally, one comment to publish, for tracker type typ.
func StatusSchema(typ string) string { return typ + ".status.v1" }

// StatusDeclaration is the StatusSchema payload (unknown fields, such as the
// Claude Code executor's usage, are ignored).
type StatusDeclaration struct {
	Status  string `json:"status,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ProjectorOptions configure a Projector.
type ProjectorOptions struct {
	Store    storage.Store
	Service  Service
	Settings Settings
	Now      func() time.Time
	// SearchDelay is how long after an ambiguous post the marker search
	// waits, so the service's listing reflects a comment that was created
	// (default 30s).
	SearchDelay time.Duration
	// AfterPost runs after a comment was created and before local
	// completion is recorded (tests crash at exactly that boundary).
	AfterPost func(intent string)
}

// Projector converges tracker projections from durable Work state. Labels
// and workflow statuses are level-triggered (re-read and re-applied). Comments
// are not re-derivable: each has a stable intent ID, a durable intent record
// and a discoverable mark, so a post whose local completion was lost is found
// again instead of repeated.
type Projector struct {
	o      ProjectorOptions
	ns     string
	outbox *Outbox
}

// ProjectionResult reports one pass.
type ProjectionResult struct {
	Labeled, Transitioned, Posted, Recovered int
	Codes                                    []string
}

// NewProjector validates options.
func NewProjector(o ProjectorOptions) (*Projector, error) {
	if o.Store == nil || o.Service == nil || o.Settings.Namespace == "" || o.Settings.Type == "" {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SearchDelay <= 0 {
		o.SearchDelay = 30 * time.Second
	}
	p := &Projector{o: o, ns: o.Settings.Type + ".projection." + digest(o.Settings.Namespace)[:16]}
	p.outbox = &Outbox{Store: o.Store, Namespace: p.ns, Now: o.Now, SearchDelay: o.SearchDelay, DryRun: o.Settings.Projection.DryRun, AfterPost: o.AfterPost}
	return p, nil
}

// Settings returns the bound settings.
func (p *Projector) Settings() Settings { return p.o.Settings }

type projectionState struct {
	Work       domain.ID `json:"work"`
	Applied    string    `json:"applied"`
	VerifiedAt time.Time `json:"verified_at"`
}

// Intent is one non-re-derivable comment. ID must be stable across retries
// and restarts (for example "outcome/<execution id>").
type Intent struct {
	ID    string
	Work  domain.ID
	Issue string
	Body  string
	// From is the issue key the agent works on (written into the mark).
	From string
	// Durable keeps the body in the intent record so Converge finishes a
	// delivery whose caller is gone (tool calls).
	Durable bool
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

// MarkerID is the stable mark of an intent (what a recovered post is found by).
func MarkerID(intent string) string { return "i" + digest(intent)[:32] }

// Deliver publishes an intent at most once as observed through its mark. It
// returns the durable record state ("posted", "recovered", or
// "attempted"/"pending" while delivery is in progress or waits for its mark
// search).
func (p *Projector) Deliver(ctx context.Context, in Intent) (string, error) {
	if !ValidKey(in.Issue) {
		return "", storage.ErrInvalid
	}
	from := in.From
	if from == "" {
		from = in.Issue
	}
	return p.outbox.Deliver(ctx, OutboxItem{ID: in.ID, Work: in.Work, Target: in.Issue, Body: in.Body, Durable: in.Durable},
		func(ctx context.Context, mark string) (Posted, error) {
			return p.o.Service.PostComment(ctx, in.Issue, in.Body, Mark{Work: string(in.Work), Intent: mark, From: from})
		},
		func(ctx context.Context, mark string) (Posted, bool, error) {
			comments, err := p.o.Service.Comments(ctx, in.Issue)
			if err != nil {
				return Posted{}, false, err
			}
			for _, c := range comments {
				if c.Agent != nil && c.Agent.Intent == mark {
					return Posted{ID: c.ID, URL: c.URL}, true, nil
				}
			}
			return Posted{}, false, nil
		})
}

// IntentComment returns the comment an intent published, once its durable
// record is posted (ok is false while delivery is pending or attempted).
func (p *Projector) IntentComment(ctx context.Context, id string) (commentID, url string, ok bool, err error) {
	posted, ok, err := p.outbox.Posted(ctx, id)
	return posted.ID, posted.URL, ok, err
}

type projected struct {
	work    domain.Work
	issue   string
	latest  *domain.Execution
	last    string
	intents []Intent
}

func (p *Projector) declaration(x *domain.Execution) (StatusDeclaration, bool) {
	var d StatusDeclaration
	if x == nil || x.Outcome == nil || x.Outcome.Data.Schema != StatusSchema(p.o.Settings.Type) || json.Unmarshal(x.Outcome.Data.Data, &d) != nil {
		return d, false
	}
	return d, true
}

// Desired returns the managed labels and the workflow state a Work's durable
// state projects. Labels: the claimed label while the Work owns the issue
// and at most one status label, absent while an attempt runs (v1: the status
// comes off while a turn runs). State: the declared status of the latest
// finished attempt (done or help for completed or failed Work without one),
// "claimed" before any, kept while a turn runs; cancelled Work leaves the
// workflow status alone. latest is nil while the newest attempt is live;
// last is the newest declared status whether or not an attempt is live.
func (p *Projector) Desired(w domain.Work, latest *domain.Execution, last string) (labels []string, state string) {
	l := p.o.Settings.Labels
	labels = []string{l.Claimed}
	switch w.State {
	case domain.WorkCancelled:
		return labels, ""
	case domain.WorkRunning:
		if last == "" {
			last = "claimed"
		}
		return labels, last
	}
	status := ""
	if d, ok := p.declaration(latest); ok {
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
		labels = append(labels, label)
	}
	state = status
	if state == "" {
		state = last
	}
	if state == "" {
		state = "claimed"
	}
	return labels, state
}

func (p *Projector) snapshot(ctx context.Context) ([]projected, error) {
	var out []projected
	err := p.transact(ctx, func(tx storage.Tx) error {
		out = nil
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		byIssue := map[string]projected{}
		for _, w := range works {
			if string(w.ScopeID) != p.o.Settings.Scope {
				continue
			}
			issue := ""
			for _, k := range w.CorrelationKeys {
				if k.Source != p.o.Settings.Namespace {
					continue
				}
				if key, _, ok := ParseIssueKey(k.ExternalID); ok {
					issue = key
					break
				}
			}
			if issue == "" {
				continue
			}
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
			item := projected{work: w, issue: issue}
			for i := range xs {
				x := xs[i]
				if x.Outcome != nil {
					item.latest = &x
				}
				if d, ok := p.declaration(&x); ok {
					if d.Status != "" {
						item.last = d.Status
					}
					if strings.TrimSpace(d.Comment) != "" {
						item.intents = append(item.intents, Intent{ID: "outcome/" + string(x.ID), Work: w.ID, Issue: issue, Body: d.Comment})
					}
				}
			}
			if n := len(xs); n > 0 && (xs[n-1].State == domain.ExecutionPending || xs[n-1].State == domain.ExecutionRunning) {
				// The newest attempt is live: project no stale status label.
				item.latest = nil
			}
			prev, ok := byIssue[issue]
			switch {
			case !ok:
				byIssue[issue] = item
			case prev.work.State.Terminal() && !w.State.Terminal(),
				prev.work.State.Terminal() == w.State.Terminal() && w.CreatedAt.After(prev.work.CreatedAt):
				item.intents = append(prev.intents, item.intents...)
				byIssue[issue] = item
			default:
				prev.intents = append(prev.intents, item.intents...)
				byIssue[issue] = prev
			}
		}
		keys := make([]string, 0, len(byIssue))
		for k := range byIssue {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, byIssue[k])
		}
		return nil
	})
	return out, err
}

func projectionDigest(labels []string, state string) string {
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	return digest(append(sorted, "state="+state)...)
}

// converge applies one issue's managed labels and workflow status against
// what the tracker shows. It reports whether a transition was made.
func (p *Projector) converge(ctx context.Context, item projected, desired []string, state string) (bool, error) {
	issue, err := p.o.Service.Issue(ctx, item.issue)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	l := p.o.Settings.Labels
	managed := append([]string{l.Claimed}, l.Statuses()...)
	var add, remove []string
	for _, m := range managed {
		want, present := has(desired, m), has(issue.Labels, m)
		switch {
		case want && !present:
			add = append(add, m)
		case !want && present:
			remove = append(remove, m)
		}
	}
	if p.o.Settings.Projection.DryRun {
		return false, nil
	}
	if len(add) > 0 || len(remove) > 0 {
		if err := p.o.Service.EditLabels(ctx, item.issue, add, remove); err != nil {
			return false, err
		}
	}
	target := p.o.Settings.Transitions[state]
	if target == "" || strings.EqualFold(strings.TrimSpace(issue.Status), target) {
		return false, nil
	}
	if !issue.Open && item.work.State.Terminal() {
		// A closed issue is never moved back by a terminal owner.
		return false, nil
	}
	return true, p.o.Service.Transition(ctx, item.issue, target)
}

// Converge runs one bounded projection pass: labels and workflow statuses,
// then comment intents. Rate limits and credential failures stop the pass;
// other per-item failures are reported as safe codes and retried next pass.
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
		code := ErrorCode(err)
		res.Codes = append(res.Codes, stage+":"+code)
		if code == "rate_limited" || code == "credential" {
			return err
		}
		return nil
	}
	budget := limit
	for _, item := range items {
		if budget <= 0 {
			break
		}
		desired, state := p.Desired(item.work, item.latest, item.last)
		want := projectionDigest(desired, state)
		key := "issue/" + item.issue
		var st projectionState
		if err := p.transact(ctx, func(tx storage.Tx) error { _, e := p.get(tx, key, &st); return e }); err != nil {
			return res, err
		}
		verifyDue := !item.work.State.Terminal() && p.o.Now().Sub(st.VerifiedAt) >= p.o.Settings.VerifyInterval
		if st.Applied == want && st.Work == item.work.ID && !verifyDue {
			continue
		}
		budget--
		moved, err := p.converge(ctx, item, desired, state)
		if err != nil {
			if stop := fail("labels", err); stop != nil {
				return res, stop
			}
			continue
		}
		if p.o.Settings.Projection.DryRun {
			continue
		}
		res.Labeled++
		if moved {
			res.Transitioned++
		}
		if err := p.transact(ctx, func(tx storage.Tx) error {
			return p.put(tx, key, projectionState{Work: item.work.ID, Applied: want, VerifiedAt: p.o.Now().UTC()})
		}); err != nil {
			return res, err
		}
	}
	intents := 0
	deliver := func(in Intent) error {
		intents++
		state, err := p.Deliver(ctx, in)
		switch {
		case err != nil:
			return fail("comment", err)
		case state == StateRecovered:
			res.Recovered++
		case state == StatePosted:
			res.Posted++
		}
		return nil
	}
	for _, item := range items {
		for _, in := range item.intents {
			if intents >= limit {
				break
			}
			state, err := p.outbox.State(ctx, in.ID)
			if err != nil {
				return res, err
			}
			if state == StatePosted {
				continue
			}
			if stop := deliver(in); stop != nil {
				return res, stop
			}
		}
	}
	// Durable intents (agent tool calls) whose delivery is unfinished.
	pending, err := p.outbox.Unfinished(ctx)
	if err != nil {
		return res, err
	}
	for _, it := range pending {
		if intents >= limit {
			break
		}
		if stop := deliver(Intent{ID: it.ID, Work: it.Work, Issue: it.Target, Body: it.Body, Durable: true}); stop != nil {
			return res, stop
		}
	}
	return res, nil
}
