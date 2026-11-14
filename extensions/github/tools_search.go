package github

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// OpPRSearch reads the repository's PRs, whoever opened them: what Work
// without an issue of its own (a Slack question, a job) needs to answer
// "what is the newest PR" or "is there a PR for this branch". It is its own
// operation, not an input on github.pr.list, so github.pr.list keeps its
// meaning of this Work's own PRs and an operator grants the wide read on
// its own.
const OpPRSearch = "github.pr.search"

// Bounds of a search: the most PRs one answers with, the default, and how
// many pages of GitHub's list it reads looking for matches when it filters
// on what GitHub's list can't (author, merged or not).
const (
	prSearchMax     = 100
	prSearchDefault = 30
	prSearchPages   = 10
)

type prSearchInput struct {
	Repo   string `json:"repo,omitempty" doc:"the repository (owner/name); defaults to this Work's own, else this project's only one"`
	State  string `json:"state,omitempty" doc:"open (default), closed (without merging), merged, or all"`
	Author string `json:"author,omitempty" doc:"only PRs this GitHub login opened"`
	Head   string `json:"head,omitempty" doc:"only PRs from this branch; owner:branch for a fork's"`
	Base   string `json:"base,omitempty" doc:"only PRs into this branch"`
	Sort   string `json:"sort,omitempty" doc:"created (default) or updated; newest first"`
	Limit  int    `json:"limit,omitempty" doc:"the most PRs to return, 1 to 100 (default 30); more is true when there are others"`
}

// prSearch is github.pr.search: the repository's PRs, newest first, each
// without its body (github.pr.view reads one in full).
func (t *Tools) prSearch(ctx context.Context, b *bound, data json.RawMessage) (any, error) {
	var in prSearchInput
	if err := decode(data, &in); err != nil {
		return nil, err
	}
	state := strings.ToLower(strings.TrimSpace(in.State))
	listState := map[string]string{"": "open", "open": "open", "closed": "closed", "merged": "closed", "all": "all"}[state]
	if listState == "" {
		return nil, refuse("invalid_input", "state must be one of open, closed, merged, all")
	}
	sortBy := strings.ToLower(strings.TrimSpace(in.Sort))
	if sortBy == "" {
		sortBy = "created"
	}
	if sortBy != "created" && sortBy != "updated" {
		return nil, refuse("invalid_input", "sort must be created or updated")
	}
	limit := in.Limit
	if limit == 0 {
		limit = prSearchDefault
	}
	if limit < 1 || limit > prSearchMax {
		return nil, refuse("invalid_input", "limit must be between 1 and %d", prSearchMax)
	}
	src, err := t.pullSource(b, in.Repo)
	if err != nil {
		return nil, err
	}
	repo := src.Settings.Repo
	q := url.Values{"state": {listState}, "sort": {sortBy}, "direction": {"desc"}}
	if head := strings.TrimSpace(in.Head); head != "" {
		if !strings.Contains(head, ":") {
			owner, _, _ := strings.Cut(repo, "/")
			head = owner + ":" + head
		}
		q.Set("head", head)
	}
	if base := strings.TrimSpace(in.Base); base != "" {
		q.Set("base", base)
	}
	author := strings.TrimSpace(in.Author)
	keep := func(p rawPull) bool {
		if author != "" && !strings.EqualFold(p.User.Login, author) {
			return false
		}
		switch state {
		case "closed":
			return p.MergedAt == nil
		case "merged":
			return p.MergedAt != nil
		}
		return true
	}
	path := repoPath(repo, "/pulls?%s", q.Encode())
	out := []map[string]any{}
	more := false
	for n := 1; ; n++ {
		items, next, err := page[rawPull](ctx, src.Client, repo, path, n, prSearchMax)
		if err != nil {
			return nil, err
		}
		for _, p := range items {
			if !keep(p) {
				continue
			}
			if len(out) == limit {
				more = true
				break
			}
			out = append(out, searchedPull(p))
		}
		if more || !next {
			break
		}
		// Past the bound, unread pages may hold more matches: say so rather
		// than answer as if there were none.
		if n == prSearchPages {
			more = true
			break
		}
	}
	return map[string]any{"repo": repo, "prs": out, "more": more}, nil
}

func searchedPull(p rawPull) map[string]any {
	m := map[string]any{"number": p.Number, "title": p.Title, "author": p.User.Login, "state": pullState(p), "draft": p.Draft,
		"branch": p.Head.Ref, "base": p.Base.Ref, "created_at": p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": p.UpdatedAt.UTC().Format(time.RFC3339), "url": p.HTMLURL}
	if p.MergedAt != nil {
		m["merged_at"] = p.MergedAt.UTC().Format(time.RFC3339)
	}
	return m
}
