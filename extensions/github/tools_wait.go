package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Wait translation. `pr:N:merged`, `pr:N:commented`, `pr:N:pushed`,
// `issue:N:closed`, `issue:N:<state>` and
// `--until` become a generic plan: a Wait on the Work's own keys (types plus
// a timeout) and B04 dependency subscriptions on the named item's
// correlation key, with an opaque filter selecting the item. Already-met
// state is aliased from accepted history at registration, so a hold whose
// condition already happened wakes through the same core path.

// waitInputTypes are the own-key families that still reach an agent while
// it waits (v1: comments, check results and merges arrive meanwhile). Agent
// comments are excluded: the wait's own reason comment is one. Another
// agent's comment wakes it through peerComments instead.
var waitInputTypes = []string{TypeDependencyMet, TypeCommentHuman, TypeIssueClosed, TypePullMerged, TypePullClosed, TypePullConflicts, TypeChecksPassed, TypeChecksFailed, TypeChecksNone, TypeChecksMissing}

// dependencyFilter is the opaque filter of a translated dependency.
type dependencyFilter struct {
	Kind   string `json:"kind"` // pr, pr_comment, pr_push, pr_checks, issue, turn, peer or unscheduled
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	State  string `json:"state,omitempty"`
	// Work is the waiting Work: a peer or pr_comment wait never meets its
	// own comments. Head is the PR's head commit when a pr_push wait began.
	Work string `json:"work,omitempty"`
	Head string `json:"head,omitempty"`
	// Label is the scheduled label an unscheduled filter waits to see gone.
	Label string `json:"label,omitempty"`
}

// unscheduledLabel labels the dependency that releases a held issue when
// the owner takes its scheduled label off.
const unscheduledLabel = "label:unscheduled"

// peerLabel labels the dependency that wakes a parked agent when another
// agent comments on its issue or PR.
const peerLabel = "comment:peer"

// peerComments is the agent comments on the Work's own key that don't come
// from the Work itself. Agent comments can't join waitInputTypes: the
// agent's own, its wait's reason comment among them, would wake it too.
func peerComments(b *bound) extension.Dependency {
	return extension.Dependency{ID: peerLabel, Label: peerLabel, SourceKey: b.key, Types: []string{TypeCommentAgent}, AliasType: TypeDependencyMet, FromNow: true,
		Filter: filterPayload(dependencyFilter{Kind: "peer", Number: b.number, Work: string(b.work.ID)})}
}

// peer reports whether an agent comment came from another Work: the
// route's selfAuthored, from the filter's Work and issue number.
func (f dependencyFilter) peer(p CommentPayload) bool {
	a := p.Agent
	if a == nil || p.Edited || p.Kind == KindReview && strings.TrimSpace(p.Body) == "" {
		return false
	}
	if a.Work != "" {
		return a.Work != f.Work
	}
	return a.From != "" && strings.TrimPrefix(a.From, "#") != strconv.Itoa(f.Number)
}

// DependencyFilter selects the named item among Events on a shared
// correlation key (for example one of several PRs keyed to the same issue).
// It is pure: core evaluates it inside coordination transactions.
func DependencyFilter(filter domain.Payload, e domain.Event) bool {
	var f dependencyFilter
	if filter.Schema != SchemaFilter || json.Unmarshal(filter.Data, &f) != nil {
		return false
	}
	switch f.Kind {
	case "pr":
		var p PullPayload
		return e.Data.Schema == SchemaPull && json.Unmarshal(e.Data.Data, &p) == nil && p.Number == f.Number && strings.EqualFold(p.Repo, f.Repo)
	case "pr_push":
		var p PullPayload
		return e.Data.Schema == SchemaPull && json.Unmarshal(e.Data.Data, &p) == nil && p.Number == f.Number && strings.EqualFold(p.Repo, f.Repo) && p.HeadSHA != "" && p.HeadSHA != f.Head
	case "pr_checks":
		// A result for the waited head, a push of another head, or the PR
		// merging or closing.
		switch e.Data.Schema {
		case SchemaChecks:
			var p ChecksPayload
			return json.Unmarshal(e.Data.Data, &p) == nil && p.Number == f.Number && strings.EqualFold(p.Repo, f.Repo) && p.SHA == f.Head
		case SchemaPull:
			var p PullPayload
			if json.Unmarshal(e.Data.Data, &p) != nil || p.Number != f.Number || !strings.EqualFold(p.Repo, f.Repo) {
				return false
			}
			return e.Type == TypePullMerged || e.Type == TypePullClosed || p.HeadSHA != "" && p.HeadSHA != f.Head
		}
		return false
	case "pr_comment":
		// A new comment, review or thread reply on the PR (not an edit), from
		// anyone but the waiting Work itself.
		var p CommentPayload
		return e.Data.Schema == SchemaComment && json.Unmarshal(e.Data.Data, &p) == nil && p.Kind != KindIssue && p.Number == f.Number && strings.EqualFold(p.Repo, f.Repo) && !p.Edited && (p.Agent == nil || p.Agent.Work == "" || p.Agent.Work != f.Work)
	case "issue":
		var p IssuePayload
		return e.Data.Schema == SchemaIssue && json.Unmarshal(e.Data.Data, &p) == nil && p.Number == f.Number && strings.EqualFold(p.Repo, f.Repo)
	case "turn":
		// The host's turn-ended Event on the issue's key (the dependency's
		// source key) names the state.
		var p domain.TurnEnded
		return e.Type == domain.TypeTurnEnded && e.Data.Schema == domain.SchemaTurnEnded && json.Unmarshal(e.Data.Data, &p) == nil && p.Status == f.State
	case "peer":
		var p CommentPayload
		return e.Data.Schema == SchemaComment && json.Unmarshal(e.Data.Data, &p) == nil && f.peer(p)
	case "unscheduled":
		// The open issue seen without its scheduled label.
		var p IssuePayload
		return e.Type == TypeIssueOpen && e.Data.Schema == SchemaIssue && json.Unmarshal(e.Data.Data, &p) == nil && p.Number == f.Number &&
			strings.EqualFold(p.Repo, f.Repo) && p.State == "open" && f.Label != "" && !has(p.Labels, f.Label)
	}
	return false
}

func filterPayload(f dependencyFilter) domain.Payload {
	data, _ := json.Marshal(f)
	return domain.Payload{Schema: SchemaFilter, Data: data}
}

// resolvedCond is a condition with its translated dependency. met is set
// when the condition has already happened (the reason, in words).
type resolvedCond struct {
	cond Cond
	dep  extension.Dependency
	met  string
}

// target finds the sources of the scope a condition names: its own when
// Project is empty, else a scope by ID or name.
func (t *Tools) target(ctx context.Context, b *bound, c Cond) (issue, code Source, scope domain.ID, err error) {
	if c.Project == "" || c.Project == string(b.work.ScopeID) {
		return b.issue, b.code, b.work.ScopeID, nil
	}
	if scope, err = t.scopeNamed(ctx, c.Project); err != nil {
		return
	}
	if scope == "" {
		err = refuse("no_project", "%s: no project here is called %s", c, c.Project)
		return
	}
	if err = t.mayRead(b, c, scope); err != nil {
		return
	}
	var found bool
	if issue, code, found = t.trackerIn(scope); !found {
		err = refuse("no_project", "%s: project %s has no GitHub repository registered", c, c.Project)
		return
	}
	return issue, code, scope, nil
}

// scopeNamed is the scope a project names by ID or name, or "" when none
// is called that.
func (t *Tools) scopeNamed(ctx context.Context, project string) (domain.ID, error) {
	var scopes []domain.Scope
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var er error
		scopes, er = tx.ListScopes()
		return er
	}); err != nil {
		return "", err
	}
	var scope domain.ID
	for _, s := range scopes {
		if string(s.ID) == project || strings.EqualFold(s.Name, project) {
			scope = s.ID
		}
	}
	return scope, nil
}

// trackerIn is a scope's first registered issue tracker and the code
// repository that names it as its shared tracker (the tracker itself
// without one).
func (t *Tools) trackerIn(scope domain.ID) (issue, code Source, found bool) {
	for _, s := range t.o.Sources() {
		if s.Settings.Scope == string(scope) && s.Settings.TrackIssues && !found {
			issue, code, found = s, s, true
		}
	}
	if !found {
		return
	}
	for _, s := range t.o.Sources() {
		if s.Settings.Scope == string(scope) && s.Settings.SharedTracker && s.Settings.TrackerNS == issue.Settings.Namespace {
			code = s
		}
	}
	return issue, code, true
}

// trackerOf is the registered source holding a code repository's issues:
// its shared tracker when it has one, else itself.
func (t *Tools) trackerOf(code Source) Source {
	if !code.Settings.SharedTracker {
		return code
	}
	for _, s := range t.o.Sources() {
		if s.Settings.Scope == code.Settings.Scope && s.Settings.TrackIssues && s.Settings.Namespace == code.Settings.TrackerNS {
			return s
		}
	}
	return code
}

// jobScope is the scope a job condition names: its own when Project is
// empty, else a scope by ID or name (a job's scope needs no repository).
func (t *Tools) jobScope(ctx context.Context, b *bound, c Cond) (domain.ID, error) {
	if c.Project == "" || c.Project == string(b.work.ScopeID) {
		return b.work.ScopeID, nil
	}
	var scopes []domain.Scope
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var er error
		scopes, er = tx.ListScopes()
		return er
	}); err != nil {
		return "", err
	}
	for _, s := range scopes {
		if string(s.ID) == c.Project || strings.EqualFold(s.Name, c.Project) {
			return s.ID, t.mayRead(b, c, s.ID)
		}
	}
	return "", refuse("no_project", "%s: no project here is called %s", c, c.Project)
}

// mayRead refuses a condition on another scope this Work's scope is not
// authorized to read: such a wait can never register until the config
// changes, so the refusal says what to change.
func (t *Tools) mayRead(b *bound, c Cond, target domain.ID) error {
	if t.o.MayRead == nil || t.o.MayRead(b.work.ScopeID, target) == nil {
		return nil
	}
	return refuse("not_allowed", "%s: project %s may not wait on %s; to allow it, add %q to %s's authorization.read_scopes", c, b.work.ScopeID, target, string(target), b.work.ScopeID)
}

// resolveJob translates `job:<name>`: the next run of the job ending, in any
// way, is a dependency on the job's own key (v1 waited for the next run, not
// an earlier one).
func (t *Tools) resolveJob(ctx context.Context, b *bound, c Cond) (resolvedCond, error) {
	r := resolvedCond{cond: c}
	if t.o.Jobs == nil {
		return r, refuse("jobs_unavailable", "%s: no scheduled jobs are configured here; wait until a time instead", c)
	}
	scope, err := t.jobScope(ctx, b, c)
	if err != nil {
		return r, err
	}
	key, types, ok := t.o.Jobs(ctx, scope, c.Ref)
	if !ok {
		return r, refuse("no_job", "%s: there is no job called %s", c, c.Ref)
	}
	if scope != b.work.ScopeID {
		r.dep.SourceScopeID = scope
	}
	r.dep.ID, r.dep.Label, r.dep.AliasType = c.String(), c.String(), TypeDependencyMet
	r.dep.SourceKey, r.dep.Types, r.dep.FromNow = key, types, true
	return r, nil
}

func (t *Tools) resolveCond(ctx context.Context, b *bound, c Cond) (resolvedCond, error) {
	if c.Kind == CondJob {
		return t.resolveJob(ctx, b, c)
	}
	r := resolvedCond{cond: c}
	issueSrc, codeSrc, scope, err := t.target(ctx, b, c)
	if err != nil {
		return r, err
	}
	if scope != b.work.ScopeID {
		r.dep.SourceScopeID = scope
	}
	r.dep.ID, r.dep.Label, r.dep.AliasType = c.String(), c.String(), TypeDependencyMet
	switch c.Kind {
	case CondPR:
		repo := codeSrc.Settings.Repo
		p, err := codeSrc.Client.Pull(ctx, repo, c.Number())
		if err != nil {
			if IsStatus(err, 404) {
				return r, refuse("not_found", "%s: PR #%d doesn't exist", c, c.Number())
			}
			return r, err
		}
		n := &normalizer{s: codeSrc.Settings, c: codeSrc.Client, m: newMemo()}
		key, _, err := n.pullKey(ctx, p)
		if err != nil {
			return r, err
		}
		r.dep.SourceKey = key
		switch c.On {
		case "commented":
			// The next comment from now on, not an earlier one.
			r.dep.Types, r.dep.FromNow = []string{TypeCommentHuman, TypeCommentAgent}, true
			r.dep.Filter = filterPayload(dependencyFilter{Kind: "pr_comment", Repo: repo, Number: c.Number(), Work: string(b.work.ID)})
		case "pushed":
			if p.State != "open" {
				return r, refuse("invalid_condition", "%s: PR #%d is already %s, so it won't get new pushes", c, c.Number(), pullState(p))
			}
			r.dep.Types, r.dep.FromNow = []string{TypePullOpen}, true
			r.dep.Filter = filterPayload(dependencyFilter{Kind: "pr_push", Repo: repo, Number: c.Number(), Head: p.Head.SHA})
		case "checks":
			// The checks on the head as the wait begins finishing; a new push
			// or the PR merging or closing first ends the wait too, so it
			// never outlives what it waits for.
			if p.State != "open" {
				return r, refuse("invalid_condition", "%s: PR #%d is already %s, so its checks won't run", c, c.Number(), pullState(p))
			}
			o, ok, err := n.checksForPull(ctx, p)
			if err != nil {
				return r, err
			}
			r.dep.Types = []string{TypeChecksPassed, TypeChecksFailed, TypeChecksNone, TypeChecksMissing, TypePullOpen, TypePullMerged, TypePullClosed}
			r.dep.Filter = filterPayload(dependencyFilter{Kind: "pr_checks", Repo: repo, Number: c.Number(), Head: p.Head.SHA})
			if ok && o.Event.Type != TypeChecksPending {
				// Finished on this head: its result, in history, meets it
				// (a hold keeps it; an agent's wait is refused as met).
				// Earlier heads' pushes in history must not.
				r.met = fmt.Sprintf("the checks on PR #%d have already finished (%s)", c.Number(), checksResult(o.Event.Type))
				r.dep.Types = r.dep.Types[:4]
			} else {
				// Pending (perhaps re-run): only results from now count,
				// never an earlier run's on the same head.
				r.dep.FromNow = true
			}
		default:
			if p.State != "open" {
				r.met = "PR #" + c.Ref + " is already " + pullState(p)
			}
			r.dep.Types = []string{TypePullMerged, TypePullClosed}
			r.dep.Filter = filterPayload(dependencyFilter{Kind: "pr", Repo: repo, Number: c.Number()})
		}
	case CondIssue:
		repo := issueSrc.Settings.TrackerRepo
		i, err := issueSrc.Client.Issue(ctx, repo, c.Number())
		if err != nil {
			if IsStatus(err, 404) {
				return r, refuse("not_found", "%s: issue #%d doesn't exist", c, c.Number())
			}
			return r, err
		}
		if i.PullRequest != nil {
			return r, refuse("invalid_condition", "%s: #%d is a PR; write pr:%d", c, c.Number(), c.Number())
		}
		n := &normalizer{s: issueSrc.Settings, c: issueSrc.Client, m: newMemo()}
		gen, _, _, err := n.generation(ctx, repo, i, time.Time{})
		if err != nil {
			return r, err
		}
		r.dep.SourceKey = domain.CorrelationKey{Source: issueSrc.Settings.TrackerNS, ExternalID: IssueKey(c.Number(), gen)}
		if c.On == "closed" {
			if i.State == "closed" {
				r.met = "issue #" + c.Ref + " is already closed"
			}
			r.dep.Types = []string{TypeIssueClosed}
			r.dep.Filter = filterPayload(dependencyFilter{Kind: "issue", Repo: repo, Number: c.Number()})
		} else {
			// The next turn ending in that state, not an earlier one (v1).
			r.dep.Types, r.dep.FromNow = []string{domain.TypeTurnEnded}, true
			r.dep.Filter = filterPayload(dependencyFilter{Kind: "turn", Repo: repo, Number: c.Number(), State: c.On})
		}
	}
	return r, nil
}

// waitPlan translates github.wait (an agent parking its turn) or
// github.hold (a held issue's first attempt).
func (t *Tools) waitPlan(hold bool) func(context.Context, extension.Authority, domain.Payload) (extension.WaitPlan, error) {
	return func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
		// A hold is its issue's; an agent's wait binds any Work, with or
		// without a GitHub issue of its own (a Slack request reviewing a PR).
		bind := t.bindWork
		if hold {
			bind = t.bind
		}
		b, err := bind(ctx, a)
		if err != nil {
			return extension.WaitPlan{}, err
		}
		var plan extension.WaitPlan
		if hold {
			plan, err = t.holdPlan(ctx, b, in.Data)
		} else {
			plan, err = t.agentWaitPlan(ctx, b, in.Data)
		}
		if err != nil {
			return extension.WaitPlan{}, refusal(err)
		}
		if err = t.watchChecks(ctx, b.work.ID, plan); err != nil {
			return extension.WaitPlan{}, err
		}
		return plan, nil
	}
}

func (t *Tools) agentWaitPlan(ctx context.Context, b *bound, data json.RawMessage) (extension.WaitPlan, error) {
	var in struct {
		Conditions []string `json:"conditions"`
		Until      string   `json:"until"`
		Reason     string   `json:"reason"`
		Repo       string   `json:"repo"`
	}
	if err := decode(data, &in); err != nil {
		return extension.WaitPlan{}, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return extension.WaitPlan{}, refuse("missing_field", "reason (what you're waiting for and why) is required")
	}
	// Conditions in this project name the repository the call names, else
	// the Work's own, else the scope's only one (as the PR operations do).
	rb := b
	if b.number == 0 || strings.TrimSpace(in.Repo) != "" {
		src, err := t.pullSource(b, in.Repo)
		if err != nil {
			return extension.WaitPlan{}, err
		}
		c := *b
		c.code, c.issue = src, t.trackerOf(src)
		rb = &c
	}
	h, resolved, err := t.holdFor(ctx, rb, in.Conditions, in.Until)
	if err != nil {
		return extension.WaitPlan{}, err
	}
	if b.number == 0 {
		// No issue to carry the reason: the Work's own conversation (a Slack
		// thread) is where people hear it, from the agent, before it waits:
		// the plan releases its authority at once. Only what it
		// waits for (or its time) wakes it, never its own posts.
		if len(b.work.CorrelationKeys) == 0 {
			return extension.WaitPlan{}, refuse("cannot_wait", "this Work has nothing of its own to be woken on")
		}
		plan := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{TypeDependencyMet}}, Timeout: h.Until, Description: h.WaitDescription()}}
		for _, r := range resolved {
			plan.Dependencies = append(plan.Dependencies, r.dep)
		}
		out, _ := json.Marshal(map[string]any{"waiting_for": h.Describe(), "reason": strings.TrimSpace(in.Reason),
			"next": "end your turn now; any other call is refused until you are resumed with what happened"})
		plan.Output = domain.Payload{Schema: "github.tool.v1", Data: out}
		return plan, nil
	}
	resolved, kept, err := t.keepHeld(ctx, b, &h, resolved)
	if err != nil {
		return extension.WaitPlan{}, err
	}
	// The reason goes on the issue with the hold marker, as in v1: people see
	// what the agent waits for, and Open Supervisor reads the same marker.
	posted, err := t.post(ctx, b, b.issue, Intent{ID: intentID(ctx, b, OpWait, data), Work: b.work.ID, Number: b.number, Body: strings.TrimSpace(in.Reason) + "\n\n" + h.Marker(), Durable: true})
	if err != nil {
		return extension.WaitPlan{}, err
	}
	plan := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: append([]string(nil), waitInputTypes...)}, Timeout: h.Until, Description: h.WaitDescription()}}
	for _, r := range resolved {
		plan.Dependencies = append(plan.Dependencies, r.dep)
	}
	plan.Dependencies = append(plan.Dependencies, peerComments(b))
	result := map[string]any{"waiting_for": h.Describe(), "comment": posted,
		"next": "end your turn now; any other call is refused until you are resumed with what happened (comments and check results also resume you)"}
	if len(kept) > 0 {
		result["kept"] = kept
		result["note"] = "the issue was already waiting for " + strings.Join(kept, ", ") + "; that still wakes you too"
	}
	out, _ := json.Marshal(result)
	plan.Output = domain.Payload{Schema: "github.tool.v1", Data: out}
	return plan, nil
}

// keepHeld carries the issue's last hold's unmet conditions into a new
// wait, so parking again (say until a time) never drops what the Work was
// held for. Conditions are alternatives: a kept one can only wake the agent
// sooner. The earlier time is not kept; the new wait's replaces it. A
// condition that has happened or can no longer be met here is let go.
func (t *Tools) keepHeld(ctx context.Context, b *bound, h *Hold, resolved []resolvedCond) ([]resolvedCond, []string, error) {
	prev, _, _, err := t.latestHold(ctx, b)
	if err != nil || prev == nil {
		return resolved, nil, err
	}
	have := map[string]bool{}
	for _, c := range h.Conds {
		have[c.String()] = true
	}
	var kept []string
	for _, c := range prev.Conds {
		if have[c.String()] {
			continue
		}
		have[c.String()] = true
		r, err := t.resolveCond(ctx, b, c)
		var op *extension.OperationError
		if errors.As(err, &op) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if r.met != "" {
			continue
		}
		h.Conds = append(h.Conds, r.cond)
		resolved = append(resolved, r)
		kept = append(kept, c.String())
	}
	return resolved, kept, nil
}

// latestHold is the latest hold marker on an issue: its body, then its
// comments in order (v1 latestHold). A cancel marker drops the hold.
// unscheduled reports that the issue no longer carries the scheduled label.
func (t *Tools) latestHold(ctx context.Context, b *bound) (h *Hold, cancelled, unscheduled bool, err error) {
	repo := b.issue.Settings.TrackerRepo
	i, err := b.issue.Client.Issue(ctx, repo, b.number)
	if err != nil {
		return nil, false, false, err
	}
	if l := b.issue.Settings.Labels.Scheduled; l != "" {
		unscheduled = true
		for _, x := range i.Labels {
			unscheduled = unscheduled && !strings.EqualFold(x.Name, l)
		}
	}
	var found *Hold
	if h, ok := HoldIn(i.Body); ok {
		found = &h
	}
	comments, err := b.issue.Client.IssueComments(ctx, repo, b.number, time.Time{})
	if err != nil {
		return nil, false, false, err
	}
	for _, c := range comments {
		if h, ok := HoldIn(c.Body); ok {
			found = &h
		}
	}
	if found != nil && found.Cancel {
		return nil, true, unscheduled, nil
	}
	return found, false, unscheduled, nil
}

// HoldStatus reports whether a held issue still has something to wait for.
type HoldStatus struct {
	Held      bool       `json:"held"`
	Spec      string     `json:"spec,omitempty"`
	Describe  string     `json:"describe,omitempty"`
	Reason    string     `json:"reason,omitempty"` // when not held: scheduled_now, cancelled, unscheduled or time_came
	Until     *time.Time `json:"until,omitempty"`
	Waits     []string   `json:"waits,omitempty"`
	Unmatched []string   `json:"unmatched,omitempty"` // conditions nothing here can meet (unknown jobs or projects)
	// Refused says why each unmatched condition can't be met, as its
	// refusal reads (say, a project this one may not wait on, and how to
	// allow it).
	Refused []string `json:"refused,omitempty"`
}

func (t *Tools) status(ctx context.Context, b *bound) (HoldStatus, *Hold, error) {
	h, cancelled, unscheduled, err := t.latestHold(ctx, b)
	if err != nil {
		return HoldStatus{}, nil, err
	}
	switch {
	case unscheduled && h != nil:
		// The owner took the scheduled label off: the issue starts now,
		// whatever its marker still says.
		return HoldStatus{Reason: "unscheduled"}, nil, nil
	case cancelled:
		return HoldStatus{Reason: "cancelled"}, nil, nil
	case h == nil:
		return HoldStatus{Reason: "scheduled_now"}, nil, nil
	}
	s := HoldStatus{Spec: h.Spec(), Describe: h.Describe(), Until: h.Until}
	for _, c := range h.Conds {
		s.Waits = append(s.Waits, c.String())
	}
	if len(h.Conds) == 0 && (h.Until == nil || !h.Until.After(t.o.Now())) {
		s.Reason = "time_came"
		return s, h, nil
	}
	s.Held = true
	return s, h, nil
}

func (t *Tools) holdStatus(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	s, _, err := t.status(ctx, b)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(s)
	return domain.Payload{Schema: SchemaHold, Data: raw}, nil
}

func (t *Tools) holdPlan(ctx context.Context, b *bound, data json.RawMessage) (extension.WaitPlan, error) {
	var in struct{}
	if err := decode(data, &in); err != nil {
		return extension.WaitPlan{}, err
	}
	s, h, err := t.status(ctx, b)
	if err != nil {
		return extension.WaitPlan{}, err
	}
	if !s.Held {
		return extension.WaitPlan{}, refuse("not_held", "the issue has nothing left to wait for (%s)", s.Reason)
	}
	plan := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{TypeDependencyMet}}, Description: h.WaitDescription()}}
	if h.Until != nil && h.Until.After(t.o.Now()) {
		until := *h.Until
		plan.Wait.Timeout = &until
	}
	for _, c := range h.Conds {
		r, err := t.resolveCond(ctx, b, c)
		if err != nil {
			// Conditions nothing here can meet (an unknown job or project)
			// leave only the time, as in v1.
			s.Unmatched = append(s.Unmatched, c.String())
			var op *extension.OperationError
			if errors.As(err, &op) {
				s.Refused = append(s.Refused, op.Message)
			}
			continue
		}
		plan.Dependencies = append(plan.Dependencies, r.dep)
	}
	if len(plan.Dependencies) == 0 && plan.Wait.Timeout == nil {
		why := strings.Join(s.Unmatched, ", ")
		if len(s.Refused) > 0 {
			why = strings.Join(s.Refused, "; ")
		}
		return extension.WaitPlan{}, refuse("not_held", "none of the hold's conditions can be met here and it has no time: %s", why)
	}
	if l := b.issue.Settings.Labels.Scheduled; l != "" {
		// The owner taking the scheduled label off starts the issue now.
		plan.Dependencies = append(plan.Dependencies, extension.Dependency{ID: unscheduledLabel, Label: unscheduledLabel, SourceKey: b.key,
			Types: []string{TypeIssueOpen}, AliasType: TypeDependencyMet, FromNow: true,
			Filter: filterPayload(dependencyFilter{Kind: "unscheduled", Repo: b.issue.Settings.TrackerRepo, Number: b.number, Label: l})})
	}
	raw, _ := json.Marshal(s)
	plan.Output = domain.Payload{Schema: SchemaHold, Data: raw}
	return plan, nil
}

// checksResult is how a finished checks Event type reads.
func checksResult(typ string) string {
	switch typ {
	case TypeChecksPassed:
		return "passed"
	case TypeChecksFailed:
		return "failed"
	case TypeChecksNone:
		return "no checks ran"
	case TypeChecksMissing:
		return "required checks never reported"
	}
	return "finished"
}
