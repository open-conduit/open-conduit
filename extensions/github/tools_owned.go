package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/storage"
)

// Code work for Work that owns no GitHub issue: a Slack thread, a job, a
// webhook delivery, an operator's Work or child Work, in a scope that
// registers the repository. Such Work opens PRs from a branch it names in a
// repository its scope registers (the PR operations' repo rule), lists and
// edits the PRs it opened, and hears their news. A PR it opens becomes its
// own through TypePullOwned, which the tools accept for it once the PR is
// open and the `github` route turns into the PR's key (PullKey) on the
// Work: the key the PR's observations carry when no tracker issue claims
// the PR. Nothing here reads the prompt to choose the repository or the
// owner: the scope's registrations and the Work's durable keys decide.

// PullOwned is the SchemaPullOwned payload: the entry (by namespace), its
// repository, and the PR a Work opened there.
type PullOwned struct {
	Namespace string `json:"namespace"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	URL       string `json:"url,omitempty"`
}

// PullOwnedLoss is the declared loss limitation of TypePullOwned.
const PullOwnedLoss = "accepted by the tools when Work without an issue opens a PR; GitHub does not record which Work opened a PR, so nothing upstream re-derives it"

// owns reports whether the Work owns the PR number of src's repository: it
// opened it, and the route gave it the PR's key.
func (b *bound) owns(src Source, number int) bool {
	return slices.Contains(b.work.CorrelationKeys, domain.CorrelationKey{Source: src.Settings.Namespace, ExternalID: PullKey(number)})
}

// ownedPull is a PR the Work opened, with the entry it is in.
type ownedPull struct {
	src    Source
	number int
}

// ownedPulls are the PRs the Work opened: its PullKey keys in the
// namespaces of its scope's entries, those of repo alone when it is set.
func (t *Tools) ownedPulls(b *bound, repo string) []ownedPull {
	var out []ownedPull
	for _, s := range t.o.Sources() {
		if s.Settings.Scope != string(b.work.ScopeID) || repo != "" && !strings.EqualFold(s.Settings.Repo, repo) {
			continue
		}
		for _, k := range b.work.CorrelationKeys {
			if k.Source != s.Settings.Namespace || !strings.HasPrefix(k.ExternalID, "pr/") {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(k.ExternalID, "pr/")); err == nil && n > 0 && PullKey(n) == k.ExternalID {
				out = append(out, ownedPull{src: s, number: n})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].src.Settings.Repo != out[j].src.Settings.Repo {
			return out[i].src.Settings.Repo < out[j].src.Settings.Repo
		}
		return out[i].number < out[j].number
	})
	return out
}

// ownRepoOnly refuses a repo an issue's Work names other than its own code
// repository: its PRs go where its issue's branch is.
func ownRepoOnly(b *bound, repo string) error {
	if repo = strings.TrimSpace(repo); repo == "" || strings.EqualFold(repo, b.code.Settings.Repo) {
		return nil
	}
	return refuse("other_repo", "this Work's PRs are in %s, the repository of its issue's branch", b.code.Settings.Repo)
}

// noIssueLink refuses a body that names a tracker issue with a closing or
// reference keyword: the PR would then be keyed to that issue and its news
// would go to the issue's Work instead of this one.
func noIssueLink(src Source, body string) error {
	if refs := bodyRefs(src.Settings, body); len(refs) > 0 {
		return refuse("issue_link", "the body links issue #%d with a keyword (Closes, Fixes, Refs …), which would give the PR to that issue's Work; drop the keyword, or mention the issue without one", refs[0])
	}
	return nil
}

// ownedList lists the PRs the Work opened, open or not.
func (t *Tools) ownedList(ctx context.Context, b *bound, repo string) (any, error) {
	if repo = strings.TrimSpace(repo); repo != "" {
		if _, err := t.pullSource(b, repo); err != nil {
			return nil, err
		}
	}
	out := []map[string]any{}
	for _, o := range t.ownedPulls(b, repo) {
		p, err := o.src.Client.Pull(ctx, o.src.Settings.Repo, o.number)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"repo": o.src.Settings.Repo, "number": p.Number, "state": pullState(p), "branch": p.Head.Ref, "title": p.Title, "url": p.HTMLURL})
	}
	return map[string]any{"prs": out}, nil
}

// ownedContext is github.context for Work without an issue: its scope's
// repository (or the list to choose from), the base branch, and the PRs it
// opened.
func (t *Tools) ownedContext(ctx context.Context, b *bound) (any, error) {
	out := map[string]any{"work": string(b.work.ID), "scope": string(b.work.ScopeID),
		"rules": "the repository's CLAUDE.md or AGENTS.md (and any rule skills they list)"}
	src, err := t.pullSource(b, "")
	var op *extension.OperationError
	switch {
	case err == nil:
		base := src.Settings.BaseBranch
		if base == "" {
			if base, err = src.Client.DefaultBranch(ctx, src.Settings.Repo); err != nil {
				return nil, err
			}
		}
		out["repo"], out["base"] = src.Settings.Repo, base
	case errors.As(err, &op) && op.Code == "repo_required":
		var repos []string
		for _, s := range t.o.Sources() {
			if s.Settings.Scope == string(b.work.ScopeID) && !slices.ContainsFunc(repos, func(r string) bool { return strings.EqualFold(r, s.Settings.Repo) }) {
				repos = append(repos, s.Settings.Repo)
			}
		}
		out["repos"], out["note"] = repos, "this project registers several repositories: name one with repo in each PR operation"
	default:
		return nil, err
	}
	pulls := []map[string]any{}
	for _, o := range t.ownedPulls(b, "") {
		pulls = append(pulls, map[string]any{"repo": o.src.Settings.Repo, "number": o.number,
			"url": o.src.Client.web + "/" + o.src.Settings.Repo + "/pull/" + strconv.Itoa(o.number)})
	}
	out["prs"] = pulls
	return out, nil
}

// ownedCreate opens a PR for Work without an issue, from the branch it
// names in the repository the repo rule picks, and gives the PR to it. The
// branch must be the repository's own (not a fork's) and not an issue's
// branch, and the body must link no issue, or the PR's news would go to
// that issue's Work. A retried or lost create finds the open PR from the
// branch, and takes it only if it carries this Work's marker.
func (t *Tools) ownedCreate(ctx context.Context, b *bound, in prCreateInput) (any, error) {
	if in.ClosesIssue != nil {
		return nil, refuse("no_issue", "closes_issue applies only to Work that owns an issue; this Work's PR closes none")
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	head := strings.TrimSpace(in.Head)
	switch {
	case head == "":
		return nil, refuse("head_required", "this Work owns no issue, so it has no branch of its own: pass head, the branch you pushed to %s", src.Settings.Repo)
	case strings.Contains(head, ":"):
		return nil, refuse("invalid_head", "head is a branch of %s, not owner:branch", src.Settings.Repo)
	}
	if src.Settings.Branch != nil && src.Settings.Branch.MatchString(head) {
		return nil, refuse("issue_branch", "%s is an issue's branch, whose PRs belong to that issue's Work; push to a branch of your own", head)
	}
	if err := noIssueLink(src, in.Body); err != nil {
		return nil, err
	}
	body := withPullMarker(in.Body, string(b.work.ID))
	base := in.Base
	if base == "" {
		if base = src.Settings.BaseBranch; base == "" {
			if base, err = src.Client.DefaultBranch(ctx, src.Settings.Repo); err != nil {
				return nil, err
			}
		}
	}
	if src.Settings.Projection.DryRun {
		return map[string]any{"state": "dry_run", "note": fmt.Sprintf("dry run: would open %s → %s in %s", head, base, src.Settings.Repo)}, nil
	}
	repo := src.Settings.Repo
	p, err := src.Client.CreatePull(ctx, repo, in.Title, body, head, base, in.Draft)

	note := ""
	if err != nil {
		// GitHub allows one open PR per head: a retried (or lost) create
		// finds the PR the first attempt opened. Only one carrying this
		// Work's marker, from the repository itself, is taken as its own.
		pulls, e := src.Client.PullsForHead(ctx, repo, head)
		if e != nil {
			return nil, err
		}
		i := slices.IndexFunc(pulls, func(x rawPull) bool { return x.State == "open" })
		if i < 0 {
			return nil, err
		}
		x := pulls[i]
		a := ParseAgent(x.Body)
		if a == nil || a.Work != string(b.work.ID) || x.Head.Repo == nil || !strings.EqualFold(x.Head.Repo.FullName, repo) {
			return nil, refuse("head_taken", "PR #%d is already open from %s, and this Work didn't open it; push to another branch", x.Number, head)
		}
		p, note = x, "a PR from this branch is already open"
	}
	out := map[string]any{"state": "open", "number": p.Number, "url": p.HTMLURL, "repo": repo}
	if note != "" {
		out["note"] = note
	}
	if err := t.own(ctx, b, src, p); err != nil {
		if !errors.Is(err, inproc.ErrCausalLimit) && !errors.Is(err, errNoOwner) {
			return nil, err
		}
		out["note"] = "the PR is open, but Open Conduit couldn't give it to this Work, so its checks and reviews won't reach you; report it as a problem"
	}
	return out, nil
}

// errNoOwner: the host gave the tools no way to accept TypePullOwned.
var errNoOwner = errors.New("github: no TypePullOwned acceptor")

// own accepts TypePullOwned for the calling Work and PR p of src, derived
// from the calling attempt's trigger. Its delivery is the PR's, so a retry
// accepts it once, and a PR is given to one Work.
func (t *Tools) own(ctx context.Context, b *bound, src Source, p rawPull) error {
	if t.o.Own == nil {
		return errNoOwner
	}
	data, err := json.Marshal(PullOwned{Namespace: src.Settings.Namespace, Repo: src.Settings.Repo, Number: p.Number, URL: p.HTMLURL})
	if err != nil {
		return err
	}
	cause := b.a.TriggerID
	if cause == "" {
		if cause, err = t.trigger(ctx, b); err != nil {
			return err
		}
	}
	at := t.o.Now().UTC()
	e := domain.Event{Type: TypePullOwned, Source: src.Settings.Namespace, DeliveryID: "owned/" + digest(src.Settings.Namespace, PullKey(p.Number))[:32],
		WorkID: b.work.ID, OccurredAt: at, ReceivedAt: at, Data: domain.Payload{Schema: SchemaPullOwned, Data: data}}
	return t.o.Own(ctx, b.work.ScopeID, cause, e)
}

// trigger is the calling execution's trigger, read from durable state.
func (t *Tools) trigger(ctx context.Context, b *bound) (domain.ID, error) {
	var id domain.ID
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(b.work.ID)
		if err != nil {
			return err
		}
		for _, x := range xs {
			if x.ID == b.a.ExecutionID {
				id = x.TriggerID
			}
		}
		return nil
	})
	return id, err
}
