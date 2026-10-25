package github

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

// Wait translation. `pr:N:merged`, `issue:N:closed`, `issue:N:<state>` and
// `--until` become a generic plan: a Wait on the Work's own keys (types plus
// a timeout) and B04 dependency subscriptions on the named item's
// correlation key, with an opaque filter selecting the item. Already-met
// state is aliased from accepted history at registration, so a hold whose
// condition already happened wakes through the same core path.

// waitInputTypes are the own-key families that still reach an agent while
// it waits (v1: comments, check results and merges arrive meanwhile). Agent
// comments are excluded: the wait's own reason comment is one.
var waitInputTypes = []string{TypeDependencyMet, TypeCommentHuman, TypeIssueClosed, TypePullMerged, TypePullClosed, TypeChecksPassed, TypeChecksFailed, TypeChecksNone}

// TurnPayload is a TypeAgentTurn event: an agent on an issue ended a turn.
type TurnPayload struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	State     string `json:"state"`
	Work      string `json:"work"`
	Execution string `json:"execution"`
}

// dependencyFilter is the opaque filter of a translated dependency.
type dependencyFilter struct {
	Kind   string `json:"kind"` // pr, issue or turn
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	State  string `json:"state,omitempty"`
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
	case "issue":
		var p IssuePayload
		return e.Data.Schema == SchemaIssue && json.Unmarshal(e.Data.Data, &p) == nil && p.Number == f.Number && strings.EqualFold(p.Repo, f.Repo)
	case "turn":
		var p TurnPayload
		return e.Data.Schema == SchemaTurn && json.Unmarshal(e.Data.Data, &p) == nil && p.Number == f.Number && p.State == f.State && strings.EqualFold(p.Repo, f.Repo)
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
	var scopes []domain.Scope
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var er error
		scopes, er = tx.ListScopes()
		return er
	}); err != nil {
		return
	}
	for _, s := range scopes {
		if string(s.ID) == c.Project || strings.EqualFold(s.Name, c.Project) {
			scope = s.ID
		}
	}
	if scope == "" {
		err = refuse("no_project", "%s: no project here is called %s", c, c.Project)
		return
	}
	var found bool
	for _, s := range t.o.Sources() {
		if s.Settings.Scope != string(scope) {
			continue
		}
		if s.Settings.TrackIssues && !found {
			issue, code, found = s, s, true
		}
	}
	if !found {
		err = refuse("no_project", "%s: project %s has no GitHub repository registered", c, c.Project)
		return
	}
	for _, s := range t.o.Sources() {
		if s.Settings.Scope == string(scope) && s.Settings.SharedTracker && s.Settings.TrackerNS == issue.Settings.Namespace {
			code = s
		}
	}
	return issue, code, scope, nil
}

func (t *Tools) resolveCond(ctx context.Context, b *bound, c Cond) (resolvedCond, error) {
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
		if p.State != "open" {
			r.met = "PR #" + c.Ref + " is already " + pullState(p)
		}
		n := &normalizer{s: codeSrc.Settings, c: codeSrc.Client, m: newMemo()}
		key, _, err := n.pullKey(ctx, p)
		if err != nil {
			return r, err
		}
		r.dep.SourceKey, r.dep.Types = key, []string{TypePullMerged, TypePullClosed}
		r.dep.Filter = filterPayload(dependencyFilter{Kind: "pr", Repo: repo, Number: c.Number()})
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
			r.dep.Types, r.dep.FromNow = []string{TypeAgentTurn}, true
			r.dep.Filter = filterPayload(dependencyFilter{Kind: "turn", Repo: repo, Number: c.Number(), State: c.On})
		}
	case CondJob:
		return r, refuse("jobs_unavailable", "%s: job waits arrive with scheduled jobs; wait until a time instead", c)
	}
	return r, nil
}

// waitPlan translates github.wait (an agent parking its turn) or
// github.hold (a held issue's first attempt).
func (t *Tools) waitPlan(hold bool) func(context.Context, extension.Authority, domain.Payload) (extension.WaitPlan, error) {
	return func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
		b, err := t.bind(ctx, a)
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
		return plan, nil
	}
}

func (t *Tools) agentWaitPlan(ctx context.Context, b *bound, data json.RawMessage) (extension.WaitPlan, error) {
	var in struct {
		Conditions []string `json:"conditions"`
		Until      string   `json:"until"`
		Reason     string   `json:"reason"`
	}
	if err := decode(data, &in); err != nil {
		return extension.WaitPlan{}, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return extension.WaitPlan{}, refuse("missing_field", "reason (what you're waiting for and why) is required")
	}
	h, resolved, err := t.holdFor(ctx, b, in.Conditions, in.Until)
	if err != nil {
		return extension.WaitPlan{}, err
	}
	// The reason goes on the issue with the hold marker, as in v1: people see
	// what the agent waits for, and Open Supervisor reads the same marker.
	posted, err := t.post(ctx, b, b.issue, Intent{ID: intentID(ctx, b, OpWait, data), Work: b.work.ID, Number: b.number, Body: strings.TrimSpace(in.Reason) + "\n\n" + h.Marker(), Durable: true})
	if err != nil {
		return extension.WaitPlan{}, err
	}
	plan := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: append([]string(nil), waitInputTypes...)}, Timeout: h.Until}}
	for _, r := range resolved {
		plan.Dependencies = append(plan.Dependencies, r.dep)
	}
	out, _ := json.Marshal(map[string]any{"waiting_for": h.Describe(), "comment": posted,
		"next": "end your turn now; you are resumed with what happened (comments and check results also resume you)"})
	plan.Output = domain.Payload{Schema: "github.tool.v1", Data: out}
	return plan, nil
}

// latestHold is the latest hold marker on an issue: its body, then its
// comments in order (v1 latestHold). A cancel marker drops the hold.
func (t *Tools) latestHold(ctx context.Context, b *bound) (*Hold, bool, error) {
	repo := b.issue.Settings.TrackerRepo
	i, err := b.issue.Client.Issue(ctx, repo, b.number)
	if err != nil {
		return nil, false, err
	}
	var found *Hold
	if h, ok := HoldIn(i.Body); ok {
		found = &h
	}
	comments, err := b.issue.Client.IssueComments(ctx, repo, b.number, time.Time{})
	if err != nil {
		return nil, false, err
	}
	for _, c := range comments {
		if h, ok := HoldIn(c.Body); ok {
			found = &h
		}
	}
	if found != nil && found.Cancel {
		return nil, true, nil
	}
	return found, false, nil
}

// HoldStatus reports whether a held issue still has something to wait for.
type HoldStatus struct {
	Held      bool       `json:"held"`
	Spec      string     `json:"spec,omitempty"`
	Describe  string     `json:"describe,omitempty"`
	Reason    string     `json:"reason,omitempty"` // when not held: scheduled_now, cancelled or time_came
	Until     *time.Time `json:"until,omitempty"`
	Waits     []string   `json:"waits,omitempty"`
	Unmatched []string   `json:"unmatched,omitempty"` // conditions nothing here can meet (jobs, unknown projects)
}

func (t *Tools) status(ctx context.Context, b *bound) (HoldStatus, *Hold, error) {
	h, cancelled, err := t.latestHold(ctx, b)
	if err != nil {
		return HoldStatus{}, nil, err
	}
	switch {
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
	plan := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{TypeDependencyMet}}}}
	if h.Until != nil && h.Until.After(t.o.Now()) {
		until := *h.Until
		plan.Wait.Timeout = &until
	}
	for _, c := range h.Conds {
		r, err := t.resolveCond(ctx, b, c)
		if err != nil {
			// Conditions nothing here can meet (a job before stage 05, an
			// unknown project) leave only the time, as in v1.
			s.Unmatched = append(s.Unmatched, c.String())
			continue
		}
		plan.Dependencies = append(plan.Dependencies, r.dep)
	}
	if len(plan.Dependencies) == 0 && plan.Wait.Timeout == nil {
		return extension.WaitPlan{}, refuse("not_held", "none of the hold's conditions can be met here and it has no time: %s", strings.Join(s.Unmatched, ", "))
	}
	raw, _ := json.Marshal(s)
	plan.Output = domain.Payload{Schema: SchemaHold, Data: raw}
	return plan, nil
}
