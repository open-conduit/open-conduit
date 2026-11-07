package management

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// The Work list serves an operator view's query (#544): Work filtered by
// scope, state, kind, source, executor, a changed-since time and words;
// sorted; paged; with how many match, and with each listed Work's runs on
// request. A view then costs one request whose run reads follow the page,
// not one request per Work it has ever held. GET /work/facets gives the
// values a view can filter on.

const (
	// maxListedRuns bounds the runs with=runs adds to one Work: its latest.
	maxListedRuns = 200
	// facetExecutorWork bounds how many of the most recently changed Work
	// GET /work/facets reads the executor of.
	facetExecutorWork = 1000
)

// workSorts are the orders GET /work takes; the first is the default.
var workSorts = []string{"created", "-created", "updated", "-updated"}

// workSource is a Work's source family: the part of its first correlation
// key's source (else its kind) before the first dot or slash, "github" for
// "github.issue".
func workSource(w domain.Work) string {
	s := w.Kind
	if len(w.CorrelationKeys) > 0 {
		s = w.CorrelationKeys[0].Source
	}
	if i := strings.IndexAny(s, "./"); i >= 0 {
		return s[:i]
	}
	return s
}

// latestExecution is Work's latest attempt, if it has one.
func latestExecution(tx storage.Tx, work domain.ID) (domain.Execution, bool, error) {
	xs, err := tx.ListExecutions(work)
	if err != nil || len(xs) == 0 {
		return domain.Execution{}, false, err
	}
	last := xs[0]
	for _, x := range xs[1:] {
		if x.Attempt > last.Attempt {
			last = x
		}
	}
	return last, true, nil
}

// executorOf is the executor registered for an attempt, "" before one is.
func executorOf(tx storage.Tx, x domain.Execution) (string, error) {
	st, err := runtime.AttemptStateInTx(tx, x.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	return st.Executor, err
}

// workFilter is GET /work's query, parsed.
type workFilter struct {
	q            listQuery
	updatedAfter time.Time
	words        []string
	sort         string
}

func parseWorkFilter(q listQuery) (workFilter, error) {
	f := workFilter{q: q, sort: q.filters["sort"]}
	if f.sort == "" {
		f.sort = workSorts[0]
	} else if !slices.Contains(workSorts, f.sort) {
		return f, fail(400, "invalid_filter", "sort takes "+strings.Join(workSorts, ", "))
	}
	if v, ok := q.filters["updated_after"]; ok {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return f, fail(400, "invalid_filter", "updated_after must be an RFC 3339 time")
		}
		f.updatedAfter = t
	}
	f.words = strings.Fields(strings.ToLower(q.filters["q"]))
	return f, nil
}

// cheap says whether w passes the filters on the Work record alone.
func (f workFilter) cheap(w domain.Work) bool {
	q := f.q
	return match(q, "scope_id", string(w.ScopeID)) && matchAny(q, "state", string(w.State)) && match(q, "kind", w.Kind) &&
		match(q, "parent_id", string(w.ParentID)) && match(q, "source", workSource(w)) &&
		(f.updatedAfter.IsZero() || !w.UpdatedAt.Before(f.updatedAfter))
}

// matches says whether w passes every filter, reading its latest attempt
// only when the executor or words ask for it.
func (f workFilter) matches(tx storage.Tx, w domain.Work) (bool, error) {
	if !f.cheap(w) {
		return false, nil
	}
	_, byExecutor := f.q.filters["executor"]
	if !byExecutor && len(f.words) == 0 {
		return true, nil
	}
	x, ok, err := latestExecution(tx, w.ID)
	if err != nil {
		return false, err
	}
	if byExecutor {
		executor := ""
		if ok {
			if executor, err = executorOf(tx, x); err != nil {
				return false, err
			}
		}
		if executor != f.q.filters["executor"] {
			return false, nil
		}
	}
	if len(f.words) == 0 {
		return true, nil
	}
	hay := []string{string(w.ID), w.Kind, string(w.ScopeID), string(w.State)}
	for _, k := range w.CorrelationKeys {
		hay = append(hay, k.Source, k.ExternalID)
	}
	if d := w.Display; d != nil {
		hay = append(hay, d.Title, d.Ref, d.Requester)
		for _, l := range d.Links {
			hay = append(hay, l.Label)
		}
	}
	if ok && x.Outcome != nil {
		hay = append(hay, x.Outcome.Summary)
	}
	text := strings.ToLower(strings.Join(hay, " "))
	for _, word := range f.words {
		if !strings.Contains(text, word) {
			return false, nil
		}
	}
	return true, nil
}

// key orders w for the sort; keys ascend, so a descending sort inverts
// the time.
func (f workFilter) key(w domain.Work) string {
	t := w.CreatedAt
	if strings.HasSuffix(f.sort, "updated") {
		t = w.UpdatedAt
	}
	n := t.UnixNano()
	if strings.HasPrefix(f.sort, "-") {
		n = math.MaxInt64 - n
	}
	return fmt.Sprintf("%020d/%s", n, w.ID)
}

// bound is the traversal's upper key: Work created after the first page is
// left out of the default order; the others take every Work, since a
// change moves a Work in them and an inverted key has no "now".
func (s *Server) workBound(f workFilter) string {
	if f.sort == workSorts[0] {
		return s.nowBound()
	}
	return "\xff"
}

// runsOf is each listed Work's runs, oldest first: its latest
// maxListedRuns.
func runsOf(tx storage.Tx, items []any, works map[string]domain.Work) (map[string][]wireRun, error) {
	out := map[string][]wireRun{}
	byID := map[domain.ID]domain.Work{}
	for _, w := range works {
		byID[w.ID] = w
	}
	for _, it := range items {
		id := it.(wireWork).ID
		xs, err := tx.ListExecutions(domain.ID(id))
		if err != nil {
			return nil, err
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i].Attempt < xs[j].Attempt })
		xs = xs[max(0, len(xs)-maxListedRuns):]
		runs := make([]wireRun, 0, len(xs))
		for _, x := range xs {
			r, err := runtime.RunOfInTx(tx, x, byID)
			if err != nil {
				return nil, err
			}
			runs = append(runs, toRun(r))
		}
		out[id] = runs
	}
	return out, nil
}

// listWork lists Work by the query, with the total that match.
func (s *Server) listWork(c *call) (int, any, error) {
	q, err := s.query(c, "work", "scope_id", "state", "kind", "parent_id", "source", "executor", "updated_after", "q", "sort", "with")
	if err != nil {
		return 0, nil, err
	}
	ask, err := with(q, "actions", "panels", "runs")
	if err != nil {
		return 0, nil, err
	}
	if len(ask) > 0 && q.limit > maxWithLimit {
		return 0, nil, fail(400, "invalid_filter", "limit must be 1-50 with actions, panels or runs")
	}
	f, err := parseWorkFilter(q)
	if err != nil {
		return 0, nil, err
	}
	var (
		out  map[string]any
		runs map[string][]wireRun
	)
	byID := map[string]domain.Work{}
	scopes := map[domain.ID]domain.Scope{}
	err = s.read(c.ctx, func(tx storage.Tx) error {
		works, e := tx.ListWorks()
		if e != nil {
			return e
		}
		var items []keyed
		for _, w := range works {
			ok, e := f.matches(tx, w)
			if e != nil {
				return e
			}
			if ok {
				items = append(items, keyed{f.key(w), toWork(w)})
				byID[string(w.ID)] = w
			}
		}
		out = s.page("work", q, s.workBound(f), items)
		out["total"] = len(items)
		page := out["items"].([]any)
		if ask["runs"] {
			if runs, e = runsOf(tx, page, byID); e != nil {
				return e
			}
		}
		if !ask["actions"] && !ask["panels"] {
			return nil
		}
		for _, it := range page {
			w := byID[it.(wireWork).ID]
			if _, ok := scopes[w.ScopeID]; !ok {
				sc, e := tx.Scope(w.ScopeID)
				if e != nil {
					return e
				}
				scopes[w.ScopeID] = sc
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if len(ask) > 0 {
		s.decorate(c.ctx, out["items"].([]any), ask, runs, byID, scopes)
	}
	return 200, out, nil
}

// workFacets is what a view of Work can filter on among the Work that
// match the query (state, scope_id, kind): the scopes and sources of every
// such Work, and the executors of the most recently changed
// facetExecutorWork of them.
func (s *Server) workFacets(c *call) (int, any, error) {
	q, err := s.query(c, "work_facets", "scope_id", "state", "kind")
	if err != nil {
		return 0, nil, err
	}
	if q.cursor != nil || c.r.URL.Query().Has("limit") {
		return 0, nil, fail(400, "invalid_filter", "facets take no limit or cursor")
	}
	f := workFilter{q: q}
	scopes, sources, executors := map[string]bool{}, map[string]bool{}, map[string]bool{}
	total := 0
	err = s.read(c.ctx, func(tx storage.Tx) error {
		works, e := tx.ListWorks()
		if e != nil {
			return e
		}
		var in []domain.Work
		for _, w := range works {
			if !f.cheap(w) {
				continue
			}
			in = append(in, w)
			scopes[string(w.ScopeID)] = true
			if src := workSource(w); src != "" {
				sources[src] = true
			}
		}
		total = len(in)
		sort.Slice(in, func(i, j int) bool { return in[i].UpdatedAt.After(in[j].UpdatedAt) })
		for _, w := range in[:min(len(in), facetExecutorWork)] {
			x, ok, e := latestExecution(tx, w.ID)
			if e != nil {
				return e
			}
			if !ok {
				continue
			}
			ex, e := executorOf(tx, x)
			if e != nil {
				return e
			}
			if ex != "" {
				executors[ex] = true
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"total": total, "scope_ids": setKeys(scopes), "sources": setKeys(sources), "executors": setKeys(executors)}, nil
}

func setKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
