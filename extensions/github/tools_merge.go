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
	"github.com/open-conduit/open-conduit/storage"
)

// Landing work (#608): merging a PR, bringing its branch up to date and
// starting a workflow. Whether a PR may merge is the repository's policy,
// which GitHub owns and enforces through branch protection and rulesets
// (#631), so github.pr.merge asks GitHub rather than copying its rules. It
// adds, checked here where the call is served, only what GitHub can't know:
// the merge happens at the head the caller read, and no agent on the PR or
// its issue is still working. GitHub's refusal is relayed with its reason.

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
	switch {
	case p.State != "open":
		return nil, refuse("pull_request_closed", "PR #%d is closed", p.Number)
	case p.Head.SHA != in.Head:
		return nil, headMoved(p, in.Head)
	}
	if err := t.nobodyWorking(ctx, b, src, p); err != nil {
		return nil, err
	}
	sha, err := c.MergePull(ctx, repo, p.Number, p.Head.SHA, method)
	switch {
	case IsStatus(err, http.StatusConflict):
		return nil, refuse("head_moved", "PR #%d's head moved while it was being merged: read the PR again with %s", p.Number, OpPRView)
	case IsStatus(err, http.StatusMethodNotAllowed), IsStatus(err, http.StatusUnprocessableEntity):
		// A fresh read says why more clearly than the refusal alone: what
		// GitHub reported before the merge may still have been unknown.
		if now, rerr := c.Pull(ctx, repo, p.Number); rerr == nil {
			if now.Head.SHA != in.Head {
				return nil, headMoved(now, in.Head)
			}
			p = now
		}
		return nil, mergeRefused(p, method, statusMessage(err))
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

// mergeRefused relays GitHub's refusal to merge PR p as one the caller can
// act on, telling the reason apart by the PR's mergeable_state: behind is
// the repository requiring branches to be up to date, the one rule the
// caller can satisfy itself, with github.pr.update-branch.
func mergeRefused(p rawPull, method, reason string) error {
	if r := strings.ToLower(reason); strings.Contains(r, "not allowed") && strings.Contains(r, "merge") {
		return refuse("method_not_allowed", "GitHub won't merge PR #%d by %s: %s; merge it with a method the repository allows", p.Number, method, reason)
	}
	switch p.MergeableState {
	case "behind":
		return refuse("out_of_date", "GitHub won't merge PR #%d: %s; the repository requires branches to be up to date with %s, so call %s with {\"number\": %d, \"head\": %q}, then wait for the new head's checks",
			p.Number, reason, p.Base.Ref, OpPRUpdateBranch, p.Number, p.Head.SHA)
	case "dirty":
		return refuse("conflicts", "GitHub won't merge PR #%d: %s; it conflicts with %s and its author resolves that, so wait for the push that does", p.Number, reason, p.Base.Ref)
	case "draft":
		return refuse("draft", "GitHub won't merge PR #%d: %s; it is a draft, so wait until it is marked ready for review", p.Number, reason)
	case "blocked":
		return refuse("blocked", "GitHub won't merge PR #%d yet: %s; the repository's rules for %s still want something (a required review or check), so wait for the PR to change", p.Number, reason, p.Base.Ref)
	}
	return refuse("not_mergeable", "GitHub won't merge PR #%d: %s; wait for the PR to change", p.Number, reason)
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
// other than review or done. The refusal names the conditions to wait on,
// for every Work it names: an issue's agent's turn, or the PR's own.
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
		// Every busy Work comes with the conditions its handing back meets:
		// its next turn ending in review or done, which the host publishes on
		// its keys whether or not it posts anything on GitHub.
		item := fmt.Sprintf("pr:%s/%d", a.work.ScopeID, p.Number)
		if a.issue > 0 {
			item = fmt.Sprintf("issue:%s/%d", a.work.ScopeID, a.issue)
		}
		for _, s := range handedBack {
			if c := item + ":" + s; !slices.Contains(conds, c) {
				conds = append(conds, c)
			}
		}
	}
	if len(busy) == 0 {
		return nil
	}
	q := make([]string, len(conds))
	for i, c := range conds {
		q[i] = fmt.Sprintf("%q", c)
	}
	return refuse("work_active", "%s; it merges once every agent on the PR has handed back: wait with conditions [%s]", strings.Join(busy, "; "), strings.Join(q, ", "))
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
	// The newest attempt decides, whatever it ended in: one cancelled or
	// interrupted mid-turn has consumed its input and declared nothing, and
	// an earlier attempt's review must not stand in for it.
	var latest *domain.Execution
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(w.ID)
		if err != nil {
			return err
		}
		for i := range xs {
			// An attempt that hasn't ended (pending, running, parked
			// mid-turn) is still working.
			if !xs[i].State.Terminal() {
				return errWorking
			}
			if latest == nil || xs[i].Attempt > latest.Attempt {
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
	if latest == nil {
		return "hasn't started yet", nil
	}
	d, ok := declaration(latest)
	switch {
	case ok && slices.Contains(handedBack, d.Status):
		return "", nil
	case ok && d.Status != "":
		return "ended its last turn in " + d.Status, nil
	case latest.State == domain.ExecutionCancelled:
		return "had its last turn cancelled before it ended, so what that turn read is unanswered", nil
	case latest.State == domain.ExecutionFailed:
		return "had its last turn fail before it ended, so what that turn read is unanswered", nil
	}
	return "ended its last turn without handing back (no review or done)", nil
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
