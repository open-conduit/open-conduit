package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Landing work (#608): merging a PR, bringing its branch up to date and
// starting a workflow. Every rule is checked here, where the call is
// served, never left to a playbook: a merge happens only at the head the
// caller read, with an approval and no outstanding change request, every
// check (the base branch's required ones included) passed on that head,
// every commit of the base in it, and no agent on the PR or its issue still
// working. A refusal names what is missing and what to wait on.

// Operation names.
const (
	OpPRMerge        = "github.pr.merge"
	OpPRUpdateBranch = "github.pr.update-branch"
	OpWorkflowRun    = "github.workflow.run"
)

// handedBack are the turn statuses that hand Work back: it has nothing more
// to do on the PR until something new happens.
var handedBack = []string{"review", "done"}

// mergeMethods are GitHub's merge methods; the first is the default.
var mergeMethods = []string{"squash", "merge", "rebase"}

type prMergeInput struct {
	Number int    `json:"number" doc:"the PR to merge"`
	Head   string `json:"head" doc:"the head commit you read the PR at (github.pr.view's head); it merges only at that commit"`
	Method string `json:"method,omitempty" doc:"squash (the default), merge or rebase"`
	Repo   string `json:"repo,omitempty" doc:"the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one"`
}

type prUpdateBranchInput struct {
	Number int    `json:"number" doc:"the PR whose branch to bring up to date with its base"`
	Head   string `json:"head" doc:"the head commit you read the PR at (github.pr.view's head); it updates only from that commit"`
	Repo   string `json:"repo,omitempty" doc:"the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one"`
}

// openPull reads the PR a landing operation names and refuses one that has
// closed or moved from the head the caller read.
func (t *Tools) openPull(ctx context.Context, b *bound, repo string, number int, head string) (Source, rawPull, error) {
	if number <= 0 {
		return Source{}, rawPull{}, refuse("missing_field", "number (the PR) is required")
	}
	if strings.TrimSpace(head) == "" {
		return Source{}, rawPull{}, refuse("missing_field", "head (the head commit you read the PR at, github.pr.view's head) is required")
	}
	src, err := t.pullSource(b, repo)
	if err != nil {
		return Source{}, rawPull{}, err
	}
	p, err := src.Client.Pull(ctx, src.Settings.Repo, number)
	if err != nil {
		return Source{}, rawPull{}, err
	}
	return src, p, nil
}

func headMoved(p rawPull, head string) error {
	return refuse("head_moved", "PR #%d's head is now %s, not %s: read the PR again with %s and decide on its new head", p.Number, short(p.Head.SHA), short(head), OpPRView)
}

func (t *Tools) prMerge(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in prMergeInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	method := strings.ToLower(strings.TrimSpace(in.Method))
	if method == "" {
		method = mergeMethods[0]
	}
	if !slices.Contains(mergeMethods, method) {
		return nil, refuse("invalid_input", "method must be one of %s", strings.Join(mergeMethods, ", "))
	}
	src, p, err := t.openPull(ctx, b, in.Repo, in.Number, in.Head)
	if err != nil {
		return nil, err
	}
	repo, c := src.Settings.Repo, src.Client
	if p.MergedAt != nil {
		return map[string]any{"state": "merged", "number": p.Number, "url": p.HTMLURL, "note": "the PR is already merged"}, nil
	}
	if err := t.landable(ctx, b, src, p, in.Head); err != nil {
		return nil, err
	}
	if err := reviewed(ctx, c, repo, p); err != nil {
		return nil, err
	}
	if err := t.nobodyWorking(ctx, b, src, p); err != nil {
		return nil, err
	}
	behind, err := c.BehindBase(ctx, repo, p.Base.Ref, p.Head.SHA)
	if err != nil {
		return nil, err
	}
	if behind > 0 {
		return nil, refuse("behind_base", "PR #%d's head %s lacks %d commit(s) of %s, so its checks ran against an older %s: call %s with {\"number\": %d, \"head\": %q}, then wait for the new head's checks",
			p.Number, short(p.Head.SHA), behind, p.Base.Ref, p.Base.Ref, OpPRUpdateBranch, p.Number, p.Head.SHA)
	}
	if err := t.checksPassed(ctx, src, p); err != nil {
		return nil, err
	}
	sha, err := c.MergePull(ctx, repo, p.Number, p.Head.SHA, method)
	switch {
	case IsStatus(err, http.StatusConflict):
		return nil, refuse("head_moved", "PR #%d's head moved while it was being merged: read the PR again with %s", p.Number, OpPRView)
	case IsStatus(err, http.StatusMethodNotAllowed), IsStatus(err, http.StatusUnprocessableEntity):
		return nil, refuse("not_mergeable", "GitHub won't merge PR #%d: %s; wait for the PR to change", p.Number, statusMessage(err))
	case err != nil:
		return nil, err
	}
	return map[string]any{"state": "merged", "number": p.Number, "url": p.HTMLURL, "sha": sha, "method": method}, nil
}

func (t *Tools) prUpdateBranch(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in prUpdateBranchInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	src, p, err := t.openPull(ctx, b, in.Repo, in.Number, in.Head)
	if err != nil {
		return nil, err
	}
	if p.MergedAt != nil {
		return nil, refuse("pull_request_closed", "PR #%d is already merged", p.Number)
	}
	if err := t.landable(ctx, b, src, p, in.Head); err != nil {
		return nil, err
	}
	repo, c := src.Settings.Repo, src.Client
	behind, err := c.BehindBase(ctx, repo, p.Base.Ref, p.Head.SHA)
	if err != nil {
		return nil, err
	}
	if behind == 0 {
		return map[string]any{"state": "up_to_date", "number": p.Number, "head": p.Head.SHA, "note": "the head already holds every commit of " + p.Base.Ref}, nil
	}
	// The push goes to the branch an agent on the PR may be pushing to.
	if err := t.nobodyWorking(ctx, b, src, p); err != nil {
		return nil, err
	}
	switch err := c.UpdatePullBranch(ctx, repo, p.Number, p.Head.SHA); {
	case IsStatus(err, http.StatusUnprocessableEntity):
		if strings.Contains(strings.ToLower(statusMessage(err)), "expected head sha") {
			return nil, headMoved(p, in.Head)
		}
		return nil, refuse("update_refused", "GitHub won't update PR #%d's branch: %s; the PR's author resolves that, so wait for the PR to change", p.Number, statusMessage(err))
	case err != nil:
		return nil, err
	}
	return map[string]any{"state": "updating", "number": p.Number, "behind": behind,
		"note": fmt.Sprintf("GitHub is merging %s into the branch; a new head follows and its checks run: wait for them, then read the PR again", p.Base.Ref)}, nil
}

func statusMessage(err error) string {
	var se *StatusError
	if errors.As(err, &se) && se.Message != "" {
		return se.Message
	}
	return "it gave no reason"
}

// landable refuses a PR that isn't open at the head the caller read, is a
// draft or conflicts with its base.
func (t *Tools) landable(_ context.Context, _ *bound, _ Source, p rawPull, head string) error {
	switch {
	case p.State != "open":
		return refuse("pull_request_closed", "PR #%d is closed", p.Number)
	case p.Head.SHA != head:
		return headMoved(p, head)
	case p.Draft:
		return refuse("draft", "PR #%d is a draft; wait until it is marked ready for review", p.Number)
	case p.Mergeable != nil && !*p.Mergeable:
		return refuse("conflicts", "PR #%d conflicts with %s; its author resolves that, so wait for the push that does", p.Number, p.Base.Ref)
	}
	return nil
}

// reviewed refuses a PR without an approval, or with a reviewer whose
// latest review still requests changes. A comment-only review leaves a
// reviewer's standing as it was; a dismissed one clears it.
func reviewed(ctx context.Context, c *Client, repo string, p rawPull) error {
	reviews, err := c.Reviews(ctx, repo, p.Number)
	if err != nil {
		return err
	}
	sort.SliceStable(reviews, func(i, j int) bool {
		a, b := reviews[i].SubmittedAt, reviews[j].SubmittedAt
		return a != nil && b != nil && a.Before(*b)
	})
	standing := map[string]string{}
	for _, r := range reviews {
		login := strings.ToLower(r.User.Login)
		switch strings.ToUpper(r.State) {
		case "APPROVED", "CHANGES_REQUESTED":
			standing[login] = strings.ToUpper(r.State)
		case "DISMISSED":
			delete(standing, login)
		}
	}
	var approved, changes []string
	for login, s := range standing {
		if s == "APPROVED" {
			approved = append(approved, login)
		} else {
			changes = append(changes, login)
		}
	}
	sort.Strings(changes)
	switch {
	case len(changes) > 0:
		return refuse("changes_requested", "%s still request(s) changes on PR #%d; wait for the PR to change and for a new review", strings.Join(changes, ", "), p.Number)
	case len(approved) == 0:
		return refuse("not_approved", "PR #%d has no approving review; wait for one", p.Number)
	}
	return nil
}

// checksPassed refuses a head whose checks haven't all passed, the base
// branch's required checks included. A head whose check runs this
// connection can't read is refused too: required checks can't be verified.
func (t *Tools) checksPassed(ctx context.Context, src Source, p rawPull) error {
	runs, statuses, from, err := src.Client.CheckState(ctx, src.Settings.Repo, p.Head.SHA)
	if err != nil {
		return err
	}
	if from == RunsUnreadable {
		return refuse("checks_unreadable", "this connection can't read check runs or workflow runs, so PR #%d's checks can't be verified; grant it Checks: read (or Actions: read) and report it as a problem", p.Number)
	}
	n := &normalizer{s: src.Settings, c: src.Client, m: newMemo(), received: t.o.Now()}
	n.remember(p)
	var o extension.Observation
	var ok bool
	if len(runs)+len(statuses) == 0 {
		o, ok, err = n.noChecksObservation(ctx, p, from)
	} else {
		o, ok, err = n.checksObservation(ctx, p, runs, statuses, from)
	}
	if err != nil {
		return err
	}
	if !ok {
		return refuse("checks_pending", "no checks have reported on PR #%d's head %s yet; wait for them", p.Number, short(p.Head.SHA))
	}
	var cp ChecksPayload
	_ = json.Unmarshal(o.Event.Data.Data, &cp)
	switch o.Event.Type {
	case TypeChecksPassed:
		return nil
	case TypeChecksFailed:
		return refuse("checks_failed", "checks failed on PR #%d's head %s (%s); wait for a push that fixes them", p.Number, short(p.Head.SHA), strings.Join(cp.Failed, ", "))
	case TypeChecksMissing:
		return refuse("checks_missing", "checks %s branch requires never reported on PR #%d's head %s; wait for a push that starts them", strings.Join(cp.Missing, ", "), p.Number, short(p.Head.SHA))
	case TypeChecksNone:
		return refuse("no_checks", "no checks ran on PR #%d's head %s, so nothing shows it works; it merges only once checks pass", p.Number, short(p.Head.SHA))
	}
	if len(cp.Missing) > 0 {
		return refuse("checks_pending", "PR #%d's head %s is waiting on checks %s's protection requires (%s); wait for them", p.Number, short(p.Head.SHA), p.Base.Ref, strings.Join(cp.Missing, ", "))
	}
	return refuse("checks_pending", "checks are still running on PR #%d's head %s; wait for them", p.Number, short(p.Head.SHA))
}

// agentOn is live Work on a PR: the Work that owns its issue, the PR's own
// Work (a reviewer), or Work that opened the PR without an issue.
type agentOn struct {
	work  domain.Work
	issue int // the issue's number when the Work owns the PR's issue
}

// agentsOn finds every live Work other than the caller's own on PR p, in
// every scope that registers p's repository: each entry keys the PR's news
// to its tracker issue's key, or to `pr/N`, and its PR copies to
// `pull_request/N`.
func (t *Tools) agentsOn(ctx context.Context, b *bound, src Source, p rawPull) ([]agentOn, error) {
	keys := map[domain.CorrelationKey]int{}
	m := newMemo()
	for _, s := range t.o.Sources() {
		if !strings.EqualFold(s.Settings.Repo, src.Settings.Repo) {
			continue
		}
		n := &normalizer{s: s.Settings, c: s.Client, m: m}
		n.remember(p)
		key, ref, err := n.pullKey(ctx, p)
		if err != nil {
			return nil, err
		}
		keys[key] = 0
		if ref != nil {
			keys[key] = ref.Number
		}
		keys[domain.CorrelationKey{Source: s.Settings.Namespace, ExternalID: PullKey(p.Number)}] = 0
		keys[domain.CorrelationKey{Source: s.Settings.Namespace, ExternalID: PullRequestKey(p.Number)}] = 0
	}
	var out []agentOn
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		works, err := tx.LiveWorks()
		if err != nil {
			return err
		}
		for _, w := range works {
			if w.ID == b.work.ID || w.State.Terminal() {
				continue
			}
			for _, k := range w.CorrelationKeys {
				if issue, ok := keys[k]; ok {
					out = append(out, agentOn{work: w, issue: issue})
					break
				}
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].work.ID < out[j].work.ID })
	return out, err
}

// nobodyWorking refuses while any agent on the PR or its issue hasn't
// handed back: it is mid-turn, has input routed to it that no turn has read
// (a comment that has just arrived), or ended its last turn in a state
// other than review or done. The refusal names the conditions to wait on.
func (t *Tools) nobodyWorking(ctx context.Context, b *bound, src Source, p rawPull) error {
	agents, err := t.agentsOn(ctx, b, src, p)
	if err != nil {
		return err
	}
	var busy, conds []string
	for _, a := range agents {
		who := fmt.Sprintf("the agent on PR #%d (Work %s)", p.Number, a.work.ID)
		if a.issue > 0 {
			who = fmt.Sprintf("the agent on issue #%d (Work %s)", a.issue, a.work.ID)
		}
		if a.work.ScopeID != b.work.ScopeID && t.o.MayRead != nil && t.o.MayRead(b.work.ScopeID, a.work.ScopeID) != nil {
			return refuse("not_allowed", "%s works in project %s, which project %s may not read, so whether it has finished can't be checked; to allow it, add %q to %s's authorization.read_scopes",
				who, a.work.ScopeID, b.work.ScopeID, string(a.work.ScopeID), b.work.ScopeID)
		}
		why, err := t.stillWorking(ctx, a.work)
		if err != nil {
			return err
		}
		if why == "" {
			continue
		}
		busy = append(busy, who+" "+why)
		if a.issue > 0 {
			for _, s := range handedBack {
				conds = append(conds, fmt.Sprintf("issue:%s/%d:%s", a.work.ScopeID, a.issue, s))
			}
		}
	}
	if len(busy) == 0 {
		return nil
	}
	next := "wait for news of the PR"
	if len(conds) > 0 {
		q := make([]string, len(conds))
		for i, c := range conds {
			q[i] = fmt.Sprintf("%q", c)
		}
		next = "wait with conditions [" + strings.Join(q, ", ") + "]"
	}
	return refuse("work_active", "%s; it merges once every agent on the PR has handed back: %s", strings.Join(busy, "; "), next)
}

// stillWorking says why live Work hasn't handed back, or "" when it has:
// no attempt running, nothing routed to it unread, and its last turn ended
// in review or done.
func (t *Tools) stillWorking(ctx context.Context, w domain.Work) (string, error) {
	if w.State == domain.WorkRunning {
		return "is working on a turn", nil
	}
	if t.o.Pending == nil {
		// Without the host's read, unread input can't be ruled out.
		return "may have unread input (this host can't say)", nil
	}
	var latest *domain.Execution
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(w.ID)
		if err != nil {
			return err
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
		for i := range xs {
			// An attempt that hasn't ended (pending, running, parked
			// mid-turn) is still working.
			if !xs[i].State.Terminal() {
				return errWorking
			}
			if xs[i].Outcome != nil {
				latest = &xs[i]
			}
		}
		return nil
	})
	if errors.Is(err, errWorking) {
		return "is working on a turn", nil
	}
	if err != nil {
		return "", err
	}
	pending, err := t.o.Pending(ctx, w.ID)
	if err != nil {
		return "", err
	}
	if len(pending) > 0 {
		return fmt.Sprintf("has %d unread input(s) (%s) it hasn't started on", len(pending), pending[0].Type), nil
	}
	d, ok := declaration(latest)
	switch {
	case !ok || d.Status == "":
		return "hasn't ended a turn yet", nil
	case !slices.Contains(handedBack, d.Status):
		return "ended its last turn in " + d.Status, nil
	}
	return "", nil
}

var errWorking = errors.New("an attempt is running")

// ---- workflows ---------------------------------------------------------------

// dispatchNamespace keys, by call intent, the workflow runs the tools
// started, so a retried call never starts a second run.
const dispatchNamespace = "github.tools.dispatch"

type dispatchRecord struct {
	State    string    `json:"state"` // dispatching or started
	Repo     string    `json:"repo"`
	Workflow string    `json:"workflow"`
	Ref      string    `json:"ref"`
	At       time.Time `json:"at"`
	RunURL   string    `json:"run_url,omitempty"`
}

type workflowRunInput struct {
	Workflow string         `json:"workflow" doc:"the workflow file to start, such as release.yml; only the ones the repository's entry lists in workflows"`
	Ref      string         `json:"ref,omitempty" doc:"the branch or tag to run it on; defaults to the repository's base branch"`
	Inputs   map[string]any `json:"inputs,omitempty" doc:"the workflow's workflow_dispatch inputs by name: strings, numbers or booleans"`
	Repo     string         `json:"repo,omitempty" doc:"the repository (owner/name) the workflow is in; defaults to this Work's own, else this project's only one"`
}

func (t *Tools) workflowRun(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in workflowRunInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	in.Workflow = strings.TrimSpace(in.Workflow)
	if in.Workflow == "" {
		return nil, refuse("missing_field", "workflow (the workflow file, such as release.yml) is required")
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	repo := src.Settings.Repo
	if !slices.Contains(src.Settings.Workflows, in.Workflow) {
		if len(src.Settings.Workflows) == 0 {
			return nil, refuse("workflow_not_allowed", "%s's entry allows no workflow to be started; the operator lists the ones it may in the entry's workflows", repo)
		}
		return nil, refuse("workflow_not_allowed", "%s's entry allows only %s to be started, not %s", repo, strings.Join(src.Settings.Workflows, ", "), in.Workflow)
	}
	if len(in.Inputs) > 25 {
		return nil, refuse("invalid_input", "a workflow takes at most 25 inputs")
	}
	for k, v := range in.Inputs {
		switch v.(type) {
		case string, bool, float64:
		default:
			return nil, refuse("invalid_input", "input %s must be a string, number or boolean", k)
		}
	}
	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		if ref = src.Settings.BaseBranch; ref == "" {
			if ref, err = src.Client.DefaultBranch(ctx, repo); err != nil {
				return nil, err
			}
		}
	}
	key := intentID(ctx, b, OpWorkflowRun, data)
	rec, found, err := t.dispatchRecord(ctx, key)
	if err != nil {
		return nil, err
	}
	out := func(r dispatchRecord, again bool) map[string]any {
		o := map[string]any{"state": "started", "repo": r.Repo, "workflow": r.Workflow, "ref": r.Ref}
		if r.RunURL != "" {
			o["run_url"] = r.RunURL
		}
		if again {
			o["note"] = "this request already started the run; it wasn't started again"
		}
		return o
	}
	if found && rec.State == "started" {
		return out(rec, true), nil
	}
	if found {
		// An earlier attempt's dispatch may have gone out unacknowledged:
		// a run since it began is that dispatch's.
		runs, err := src.Client.DispatchedRuns(ctx, repo, rec.Workflow, rec.Ref, rec.At)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			rec.State, rec.RunURL = "started", runs[0].HTMLURL
			if err := t.putDispatch(ctx, key, rec); err != nil {
				return nil, err
			}
			return out(rec, true), nil
		}
	} else {
		rec = dispatchRecord{State: "dispatching", Repo: repo, Workflow: in.Workflow, Ref: ref, At: t.o.Now().UTC().Add(-time.Minute)}
		if err := t.putDispatch(ctx, key, rec); err != nil {
			return nil, err
		}
	}
	switch err := src.Client.DispatchWorkflow(ctx, repo, rec.Workflow, rec.Ref, in.Inputs); {
	case IsStatus(err, http.StatusNotFound), IsStatus(err, http.StatusUnprocessableEntity), IsStatus(err, http.StatusForbidden):
		// GitHub refused it outright, so nothing started.
		_ = t.deleteDispatch(ctx, key)
		if IsStatus(err, http.StatusUnprocessableEntity) {
			return nil, refuse("workflow_refused", "GitHub won't start %s on %s: %s", rec.Workflow, rec.Ref, statusMessage(err))
		}
		return nil, err
	case err != nil:
		// The dispatch may or may not have gone out; a retry checks.
		return nil, err
	}
	rec.State = "started"
	if err := t.putDispatch(ctx, key, rec); err != nil {
		return nil, err
	}
	return out(rec, false), nil
}

func (t *Tools) dispatchRecord(ctx context.Context, key string) (rec dispatchRecord, found bool, err error) {
	err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		data, err := tx.Get(dispatchNamespace, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	return
}

func (t *Tools) putDispatch(ctx context.Context, key string, rec dispatchRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error { return tx.Put(dispatchNamespace, key, data) })
}

func (t *Tools) deleteDispatch(ctx context.Context, key string) error {
	return t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		err := tx.Delete(dispatchNamespace, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	})
}

// ---- every open PR -------------------------------------------------------------

// openList is github.pr.list with open: every open PR in the repository,
// whoever opened it, drafts included.
func (t *Tools) openList(ctx context.Context, b *bound, repo string) (any, error) {
	src, err := t.pullSource(b, repo)
	if err != nil {
		return nil, err
	}
	pulls, err := all[rawPull](ctx, src.Client, src.Settings.Repo, repoPath(src.Settings.Repo, "/pulls?state=open"))
	if err != nil {
		return nil, err
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	out := []map[string]any{}
	for _, p := range pulls {
		out = append(out, map[string]any{"repo": src.Settings.Repo, "number": p.Number, "state": pullState(p), "draft": p.Draft, "branch": p.Head.Ref,
			"head": p.Head.SHA, "author": p.User.Login, "title": p.Title, "url": p.HTMLURL})
	}
	return map[string]any{"prs": out}, nil
}
