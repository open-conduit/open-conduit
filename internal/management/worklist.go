package management

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
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

// maxListedRuns bounds the runs with=runs adds to one Work: its latest.
const maxListedRuns = 200

// workSorts are the orders GET /work takes; the first is the default.
var workSorts = []string{"created", "-created", "updated", "-updated"}

// sourceFamily is a source's family: the part before the first dot or
// slash, "github" for "github.issue".
func sourceFamily(s string) string {
	if i := strings.IndexAny(s, "./"); i >= 0 {
		return s[:i]
	}
	return s
}

// workSource is a Work's source family: that of storage.WorkSource.
func workSource(w domain.Work) string { return sourceFamily(storage.WorkSource(w)) }

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
	if byExecutor && x.Executor != f.q.filters["executor"] {
		return false, nil
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

// listedWork is a page of Work as queryWork reads it: the response, the
// runs asked for, and the records and scopes decorate needs.
type listedWork struct {
	out    map[string]any
	runs   map[string][]wireRun
	byID   map[string]domain.Work
	scopes map[domain.ID]domain.Scope
}

// storageQuery is what of f storage selects through its indexes: every
// filter given a value but the words. A source family becomes the sources
// in it, from facets; sources is nil when f names none.
func (f workFilter) storageQuery(sources []string) storage.WorkQuery {
	q := storage.WorkQuery{Order: storage.WorkOrder(f.sort), UpdatedSince: f.updatedAfter, Sources: sources}
	if v, ok := f.q.filters["state"]; ok {
		for _, st := range strings.Split(v, ",") {
			q.States = append(q.States, domain.WorkState(st))
		}
	}
	q.ScopeID, q.Kind, q.ParentID = domain.ID(f.q.filters["scope_id"]), f.q.filters["kind"], domain.ID(f.q.filters["parent_id"])
	q.Executor = f.q.filters["executor"]
	return q
}

// residual says whether some of f is left for queryWork to check on each
// Work storage selects: the words, and a filter given an empty value,
// which storage reads as no filter.
func (f workFilter) residual() bool {
	if len(f.words) > 0 {
		return true
	}
	for _, name := range []string{"scope_id", "kind", "parent_id", "source", "executor"} {
		if v, ok := f.q.filters[name]; ok && v == "" {
			return true
		}
	}
	return false
}

// sources is the sources of f's source family among the Work f's other
// filters on the record select, read from storage's facets; nil when f
// names no family, and empty when none is in it.
func (f workFilter) sources(tx storage.Tx) ([]string, error) {
	family, ok := f.q.filters["source"]
	if !ok || family == "" {
		return nil, nil
	}
	q := f.storageQuery(nil)
	q.Executor = ""
	facets, err := tx.WorkFacets(q)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, src := range facets.Sources {
		if sourceFamily(src) == family {
			out = append(out, src)
		}
	}
	return out, nil
}

// after is the storage position a cursor's last key stands for: key's
// inverse, whose time a descending order inverted.
func (f workFilter) after(key string) (*storage.WorkKey, error) {
	n, id, ok := strings.Cut(key, "/")
	at, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil {
		return nil, fail(400, "invalid_cursor", "cursor is invalid")
	}
	if strings.HasPrefix(f.sort, "-") {
		at = math.MaxInt64 - at
	}
	return &storage.WorkKey{At: time.Unix(0, at), ID: domain.ID(id)}, nil
}

// workQueryBatch is how many Work queryWork reads from storage at a time
// when it checks words on each.
const workQueryBatch = 200

// queryWork reads the page of Work f asks for in tx, with the total that
// match and, as ask says, each listed Work's runs. Storage selects, orders,
// pages and counts by its indexes, so the page costs its own Work; only
// words (and an empty filter) are checked here, on each Work storage
// selects, and those cost the Work they read.
func (s *Server) queryWork(tx storage.Tx, f workFilter, ask map[string]bool) (listedWork, error) {
	lw := listedWork{byID: map[string]domain.Work{}, scopes: map[domain.ID]domain.Scope{}}
	sources, err := f.sources(tx)
	if err != nil {
		return lw, err
	}
	q := f.storageQuery(sources)
	bound := s.workBound(f)
	if f.q.cursor != nil {
		bound = f.q.cursor.Bound
		if q.After, err = f.after(f.q.cursor.Last); err != nil {
			return lw, err
		}
	}
	var page []domain.Work
	more, total := false, 0
	if sources != nil && len(sources) == 0 {
		// No Work is in the family.
	} else if !f.residual() {
		if total, err = tx.CountWorks(q); err != nil {
			return lw, err
		}
		q.Limit = f.q.limit + 1
		ws, err := tx.QueryWorks(q)
		if err != nil {
			return lw, err
		}
		for _, w := range ws {
			if f.key(w) > bound {
				break
			}
			page = append(page, w)
		}
		if more = len(page) > f.q.limit; more {
			page = page[:f.q.limit]
		}
	} else {
		// The words are checked on every Work storage selects, for the
		// total; the page is the first that pass after the cursor.
		all := q
		all.After, all.Limit = nil, workQueryBatch
		after := q.After
		for {
			ws, err := tx.QueryWorks(all)
			if err != nil {
				return lw, err
			}
			for _, w := range ws {
				ok, err := f.matches(tx, w)
				if err != nil {
					return lw, err
				}
				if !ok {
					continue
				}
				total++
				k := q.Order.Key(w)
				if after != nil && !(q.Order.Descending() && k.Before(*after) || !q.Order.Descending() && after.Before(k)) || f.key(w) > bound {
					continue
				}
				if len(page) == f.q.limit {
					more = true
					continue
				}
				page = append(page, w)
			}
			if len(ws) < workQueryBatch {
				break
			}
			k := q.Order.Key(ws[len(ws)-1])
			all.After = &k
		}
	}
	items := make([]any, 0, len(page))
	for _, w := range page {
		items = append(items, toWork(w))
		lw.byID[string(w.ID)] = w
	}
	next := ""
	if more && len(page) > 0 {
		next = s.sign(cursor{Endpoint: "work", Filters: f.q.norm, Bound: bound, Last: f.key(page[len(page)-1]), Expires: s.o.Now().Add(15 * time.Minute).Unix()})
	}
	lw.out = map[string]any{"items": items, "next_cursor": next, "total": total}
	if ask["runs"] {
		if lw.runs, err = runsOf(tx, items, lw.byID); err != nil {
			return lw, err
		}
	}
	if !ask["actions"] && !ask["panels"] {
		return lw, nil
	}
	for _, w := range page {
		if _, ok := lw.scopes[w.ScopeID]; !ok {
			sc, err := tx.Scope(w.ScopeID)
			if err != nil {
				return lw, err
			}
			lw.scopes[w.ScopeID] = sc
		}
	}
	return lw, nil
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
	var lw listedWork
	err = s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		lw, e = s.queryWork(tx, f, ask)
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	out, runs, byID, scopes := lw.out, lw.runs, lw.byID, lw.scopes
	if len(ask) > 0 {
		s.decorate(c.ctx, out["items"].([]any), ask, runs, byID, scopes)
	}
	return 200, out, nil
}

// workFacets is what a view of Work can filter on among the Work that
// match the query (state, scope_id, kind): the scopes, sources and latest
// attempts' executors of every such Work.
func (s *Server) workFacets(c *call) (int, any, error) {
	q, err := s.query(c, "work_facets", "scope_id", "state", "kind")
	if err != nil {
		return 0, nil, err
	}
	if q.cursor != nil || c.r.URL.Query().Has("limit") {
		return 0, nil, fail(400, "invalid_filter", "facets take no limit or cursor")
	}
	var out map[string]any
	err = s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		out, e = facetsOf(tx, workFilter{q: q})
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

// facetsOf is GET /work/facets's answer for f in tx, from storage's
// facets of the Work f selects: their sources as families.
func facetsOf(tx storage.Tx, f workFilter) (map[string]any, error) {
	q := f.storageQuery(nil)
	q.Order = ""
	if f.residual() {
		return residualFacets(tx, f)
	}
	facets, err := tx.WorkFacets(q)
	if err != nil {
		return nil, err
	}
	scopes, sources := []string{}, map[string]bool{}
	for _, id := range facets.ScopeIDs {
		scopes = append(scopes, string(id))
	}
	for _, src := range facets.Sources {
		sources[sourceFamily(src)] = true
	}
	return map[string]any{"total": facets.Count, "scope_ids": scopes, "sources": setKeys(sources), "executors": facets.Executors}, nil
}

// residualFacets is facetsOf for a filter given an empty value, which
// storage can't select: every Work storage selects without it, checked.
func residualFacets(tx storage.Tx, f workFilter) (map[string]any, error) {
	q := f.storageQuery(nil)
	q.Order = ""
	ws, err := tx.QueryWorks(q)
	if err != nil {
		return nil, err
	}
	total, scopes, sources, executors := 0, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, w := range ws {
		if !f.cheap(w) {
			continue
		}
		total++
		scopes[string(w.ScopeID)] = true
		sources[workSource(w)] = true
		x, ok, err := latestExecution(tx, w.ID)
		if err != nil {
			return nil, err
		}
		if ok && x.Executor != "" {
			executors[x.Executor] = true
		}
	}
	return map[string]any{"total": total, "scope_ids": setKeys(scopes), "sources": setKeys(sources), "executors": setKeys(executors)}, nil
}

func setKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
