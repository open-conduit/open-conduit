package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// The Runs page, ported from the prototype's Runs view: one row per Work,
// the state rail, the facet toolbar, the ledger with lifelines, and bulk
// hold and release. Every filter, sort and group is query state, so a
// view survives reload and back.

// railGroups are the state rail's cells after Open, in the order the
// ledger groups them.
var railGroups = []struct{ G, Label, Blurb string }{
	{"attention", "Needs you", "decisions and repairs"},
	{"running", "Running", "executing now"},
	{"queued", "Queued", "ready, awaiting a slot"},
	{"waiting", "Waiting", "on CI, people or time"},
	{"held", "Held", "paused on purpose"},
	{"closed", "Closed", "completed or cancelled"},
}

// windows are the Changed facet's choices.
var windows = []struct {
	V, Label string
	D        time.Duration
}{
	{"1h", "last hour", time.Hour},
	{"24h", "last 24 hours", 24 * time.Hour},
	{"7d", "last 7 days", 7 * 24 * time.Hour},
}

// sorts are the ledger's orders; the first is the default.
var sorts = []struct{ V, Label string }{
	{"attention", "What needs me first"},
	{"updated", "Recently changed"},
	{"waiting", "Longest in this state"},
	{"active", "Most execution time"},
	{"created", "Newest work"},
}

// facetKeys are the query keys the facets set, which Clear all removes.
var facetKeys = []string{"scope", "source", "executor", "updated", "q"}

// runsQuery is the Runs page's URL state.
type runsQuery struct {
	G, W, Q, Scope, Source, Executor, Updated, Sort, By string
}

func parseRunsQuery(q url.Values) runsQuery {
	r := runsQuery{G: q.Get("g"), W: q.Get("w"), Q: strings.TrimSpace(q.Get("q")), Scope: q.Get("scope"), Source: q.Get("source"), Executor: q.Get("executor"), Updated: q.Get("updated"), Sort: q.Get("sort"), By: q.Get("by")}
	if r.G == "" {
		r.G = "open"
	}
	if r.Sort == "" {
		r.Sort = "attention"
	}
	if r.By == "" {
		r.By = "state"
	}
	return r
}

// rowCtx is what a ledger row renders from, and the contract for the
// "row-actions" template (templates/actions.html): W is the row's Work and
// Page the page (for its CSRF token and Back). "row-actions" renders the
// inline primary action for repairs and rebinds and the row's menu; the
// Decide link for a decision is rendered by runs.html before it.
type rowCtx struct {
	W    workView
	Page *page
	// Index is the row's place in the ledger, for keyboard movement.
	Index int
	Life  lifeline
	// Active and Elapsed are the Work's executing time and lifetime.
	Active, Elapsed time.Duration
	// Holdable and Held say what bulk hold and release can do to it.
	Holdable, Held bool
	// For is how long the disposition has held ("for 2h"), or for closed
	// Work how long ago it closed.
	For string
}

type railCell struct {
	Label, Blurb, Href string
	N                  int
	On, Signal         bool
}

type option struct {
	V, Label string
	On       bool
}

type token struct{ Label, Href string }

// facetView is one toolbar select: Any is the no-filter choice's label,
// absent for group and sort; Value is the filter set.
type facetView struct {
	Label, Name, Any, Value string
	Opts                    []option
}

type ledgerSection struct {
	Key, Label string
	Rows       []rowCtx
	// N is how many Work the section holds, which for closed Work may be
	// more than the rows read.
	N int
}

// closedSet is the closed Work the Runs page reads by its query: how many
// closed Work there are and what they can be filtered on (Facets), how
// many the facets leave (Matching), and the rows read when the state filter
// shows them, at most Want.
type closedSet struct {
	Facets   client.WorkFacets
	Matching int
	Rows     []workView
	Want     int
}

// closedSorts are the list orders closest to each ledger sort for closed
// Work, whose disposition began when they closed (their last change).
// "Most execution time" has no list order; the rows read are the latest
// closed and sorted as read.
var closedSorts = map[string]string{"attention": "-updated", "updated": "-updated", "waiting": "updated", "active": "-updated", "created": "-created"}

// closedQuery is the list query for the closed Work the facets leave.
func (rq runsQuery) closedQuery(now time.Time) url.Values {
	q := url.Values{}
	for k, v := range map[string]string{"scope_id": rq.Scope, "source": rq.Source, "executor": rq.Executor, "q": rq.Q, "sort": closedSorts[rq.Sort]} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if q.Get("sort") == "" {
		q.Set("sort", "-updated")
	}
	for _, win := range windows {
		if rq.Updated == win.V {
			q.Set("updated_after", now.Add(-win.D).UTC().Format(time.RFC3339Nano))
		}
	}
	return q
}

// showsClosed says whether the state filter lists closed Work.
func (rq runsQuery) showsClosed() bool { return rq.G == "closed" || rq.G == "all" }

// closedStep is how many more closed rows "Show more" reads.
const closedStep = 50

// wantClosed is how many closed rows the page asks for: closedStep, and
// closedStep more for each "Show more" (the more query key).
func wantClosed(q url.Values) int {
	n, _ := strconv.Atoi(q.Get("more"))
	return closedStep * (1 + min(max(n, 0), 40))
}

type runsData struct {
	Query runsQuery
	// Total is every Work read; Scopes how many scopes they span;
	// Watching how many scopes there are.
	Total, Scopes, Watching int
	// Attention and Running count the faceted Work in those groups.
	Attention, Running                                               int
	Rail                                                             []railCell
	SubAny                                                           railCell
	Sub                                                              []railCell
	ScopeOpts, SourceOpts, ExecOpts, WindowOpts, GroupOpts, SortOpts []option
	// Facets filter and Orders group and sort, as the toolbar shows them.
	Facets, Orders []facetView
	Tokens         []token
	ClearHref      string
	Sections       []ledgerSection
	Rows           int
	// Faceted is how many Work the facets leave, before the state filter.
	Faceted int
	// Empty says which empty state shows: "nowork", "calm" (nothing needs
	// you) or "nomatch"; GroupLabel names the state filtered to.
	Empty, GroupLabel string
	AllHref           string
	Fresh             bool
	// MoreHref reads closedStep more closed rows, when more match than
	// are shown; Shown is how many closed rows are.
	MoreHref string
	Shown    int
	// Closed is how many closed Work the facets leave.
	Closed int
}

// href is the page's URL with keys set, and removed where the value is "".
func href(q url.Values, kv ...string) string {
	out := cloneQuery(q)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			out.Del(kv[i])
		} else {
			out.Set(kv[i], kv[i+1])
		}
	}
	if len(out) == 0 {
		return "/"
	}
	return "/?" + out.Encode()
}

// matches says whether w passes the facets (not the state filter).
func (rq runsQuery) matches(w workView, now time.Time) bool {
	if rq.Scope != "" && w.ScopeID != rq.Scope {
		return false
	}
	if rq.Source != "" && w.Source != rq.Source {
		return false
	}
	if rq.Executor != "" && w.Executor != rq.Executor {
		return false
	}
	for _, win := range windows {
		if rq.Updated == win.V && now.Sub(w.Updated) > win.D {
			return false
		}
	}
	if rq.Q != "" {
		hay := []string{w.Ref, w.Title, w.ScopeID, w.Summary, w.Disp.Label, w.Disp.Detail, w.Kind, w.ID}
		if d := w.Display; d != nil {
			hay = append(hay, d.Requester)
			for _, l := range d.Links {
				hay = append(hay, l.Label)
			}
		}
		text := strings.ToLower(strings.Join(hay, " "))
		for _, p := range strings.Fields(strings.ToLower(rq.Q)) {
			if !strings.Contains(text, p) {
				return false
			}
		}
	}
	return true
}

// inState says whether w passes the state filter and the waiting sub-rail.
func (rq runsQuery) inState(w workView) bool {
	switch rq.G {
	case "all":
		return true
	case "open":
		return w.Disp.Group != "closed"
	}
	if w.Disp.Group != rq.G {
		return false
	}
	return rq.G != "waiting" || rq.W == "" || w.Disp.Waiting == rq.W
}

func groupOrder(g string) int {
	for i, x := range dispGroups {
		if x == g {
			return i
		}
	}
	return len(dispGroups)
}

// sortWorks orders ws by the named sort, stably.
func sortWorks(ws []workView, by string, now time.Time) {
	less := map[string]func(a, b workView) bool{
		"attention": func(a, b workView) bool {
			if ga, gb := groupOrder(a.Disp.Group), groupOrder(b.Disp.Group); ga != gb {
				return ga < gb
			}
			if a.Disp.Group == "closed" {
				return a.Disp.Since.After(b.Disp.Since)
			}
			return a.Disp.Since.Before(b.Disp.Since)
		},
		"updated": func(a, b workView) bool { return a.Updated.After(b.Updated) },
		"waiting": func(a, b workView) bool { return a.Disp.Since.Before(b.Disp.Since) },
		"active":  func(a, b workView) bool { return a.Active(now) > b.Active(now) },
		"created": func(a, b workView) bool { return a.Created.After(b.Created) },
	}
	f, ok := less[by]
	if !ok {
		f = less["attention"]
	}
	sort.SliceStable(ws, func(i, j int) bool { return f(ws[i], ws[j]) })
}

// waitingLabel names a sub-rail key: the family's display name, "a time"
// for a timer alone.
func waitingLabel(k string, label func(string) string) string {
	switch k {
	case "time":
		return "a time"
	case "events":
		return "other events"
	}
	return label(k)
}

// holdable says whether a hold would be accepted now, as the runtime
// decides: open Work, not running, not held, with no pending or running
// attempt.
func holdable(w workView) bool {
	if !w.Open() || w.State == "running" || w.Hold != nil {
		return false
	}
	for _, t := range w.Turns {
		if t.Live() {
			return false
		}
	}
	return true
}

// runsView builds the page from the open Work and scopes and the closed
// Work read for it, labelling sources with label.
func runsView(wd world, cl closedSet, q url.Values, p *page, now time.Time, label func(string) string) runsData {
	rq := parseRunsQuery(q)
	d := runsData{Query: rq, Total: len(wd.Works) + cl.Facets.Total, Watching: len(wd.Scopes)}
	scopeName := map[string]string{}
	for _, s := range wd.Scopes {
		scopeName[s.ID] = s.Name
		d.ScopeOpts = append(d.ScopeOpts, option{V: s.ID, Label: s.Name, On: s.ID == rq.Scope})
	}
	spans, sources, execs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var faceted []workView
	for _, w := range wd.Works {
		spans[w.ScopeID] = true
		if w.Source != "" && !sources[w.Source] {
			sources[w.Source] = true
			d.SourceOpts = append(d.SourceOpts, option{V: w.Source, Label: label(w.Source), On: w.Source == rq.Source})
		}
		if w.Executor != "" && !execs[w.Executor] {
			execs[w.Executor] = true
			d.ExecOpts = append(d.ExecOpts, option{V: w.Executor, Label: w.Executor, On: w.Executor == rq.Executor})
		}
		if _, ok := scopeName[w.ScopeID]; !ok && w.ScopeID != "" {
			scopeName[w.ScopeID] = w.ScopeID
			d.ScopeOpts = append(d.ScopeOpts, option{V: w.ScopeID, Label: w.ScopeID, On: w.ScopeID == rq.Scope})
		}
		if rq.matches(w, now) {
			faceted = append(faceted, w)
		}
	}
	for _, id := range cl.Facets.ScopeIDs {
		spans[id] = true
		if _, ok := scopeName[id]; !ok && id != "" {
			scopeName[id] = id
			d.ScopeOpts = append(d.ScopeOpts, option{V: id, Label: id, On: id == rq.Scope})
		}
	}
	for _, src := range cl.Facets.Sources {
		if !sources[src] {
			sources[src] = true
			d.SourceOpts = append(d.SourceOpts, option{V: src, Label: label(src), On: src == rq.Source})
		}
	}
	for _, ex := range cl.Facets.Executors {
		if !execs[ex] {
			execs[ex] = true
			d.ExecOpts = append(d.ExecOpts, option{V: ex, Label: ex, On: ex == rq.Executor})
		}
	}
	d.Scopes, d.Faceted = len(spans), len(faceted)+cl.Matching
	for _, win := range windows {
		d.WindowOpts = append(d.WindowOpts, option{V: win.V, Label: win.Label, On: win.V == rq.Updated})
	}
	for _, g := range []option{{V: "state", Label: "state"}, {V: "scope", Label: "scope"}, {V: "none", Label: "none"}} {
		g.On = g.V == rq.By
		d.GroupOpts = append(d.GroupOpts, g)
	}
	for _, s := range sorts {
		d.SortOpts = append(d.SortOpts, option{V: s.V, Label: s.Label, On: s.V == rq.Sort})
	}

	d.Facets = []facetView{
		{Label: "Scope", Name: "scope", Any: "all", Value: rq.Scope, Opts: d.ScopeOpts},
		{Label: "Source", Name: "source", Any: "all", Value: rq.Source, Opts: d.SourceOpts},
		{Label: "Executor", Name: "executor", Any: "all", Value: rq.Executor, Opts: d.ExecOpts},
		{Label: "Changed", Name: "updated", Any: "any time", Value: rq.Updated, Opts: d.WindowOpts},
	}
	d.Orders = []facetView{{Label: "Group", Name: "by", Opts: d.GroupOpts}, {Label: "Sort", Name: "sort", Opts: d.SortOpts}}

	counts, waiting := map[string]int{}, map[string]int{}
	open := 0
	counts["closed"], d.Closed = cl.Matching, cl.Matching
	for _, w := range faceted {
		counts[w.Disp.Group]++
		if w.Disp.Group != "closed" {
			open++
		}
		if w.Disp.Group == "waiting" && w.Disp.Waiting != "" {
			waiting[w.Disp.Waiting]++
		}
	}
	d.Attention, d.Running = counts["attention"], counts["running"]
	d.Rail = append(d.Rail, railCell{Label: "Open", Blurb: "everything not closed", N: open, On: rq.G == "open", Href: href(q, "g", "", "w", "")})
	for _, g := range railGroups {
		d.Rail = append(d.Rail, railCell{Label: g.Label, Blurb: g.Blurb, N: counts[g.G], On: rq.G == g.G, Signal: g.G == "attention" && counts[g.G] > 0, Href: href(q, "g", g.G, "w", "")})
		if g.G == rq.G {
			d.GroupLabel = strings.ToLower(g.Label)
		}
	}
	if rq.G == "open" {
		d.GroupLabel = "open"
	}
	if rq.G == "waiting" {
		d.SubAny = railCell{Label: "Anything", N: counts["waiting"], On: rq.W == "", Href: href(q, "w", "")}
		for k, n := range waiting {
			d.Sub = append(d.Sub, railCell{Label: waitingLabel(k, label), N: n, On: rq.W == k, Href: href(q, "w", k)})
		}
		sort.Slice(d.Sub, func(i, j int) bool {
			if d.Sub[i].N != d.Sub[j].N {
				return d.Sub[i].N > d.Sub[j].N
			}
			return d.Sub[i].Label < d.Sub[j].Label
		})
	}

	labels := map[string]string{"scope": "Scope: " + rq.Scope, "source": "Source: " + label(rq.Source), "executor": "Executor: " + rq.Executor, "updated": "Changed in the last " + rq.Updated, "q": "“" + rq.Q + "”"}
	set := map[string]string{"scope": rq.Scope, "source": rq.Source, "executor": rq.Executor, "updated": rq.Updated, "q": rq.Q}
	var clear []string
	for _, k := range facetKeys {
		clear = append(clear, k, "")
		if set[k] != "" {
			d.Tokens = append(d.Tokens, token{Label: labels[k], Href: href(q, k, "")})
		}
	}
	d.ClearHref = href(q, clear...)
	d.AllHref = href(q, "g", "all", "w", "")

	var rows []workView
	for _, w := range faceted {
		if rq.inState(w) && w.Disp.Group != "closed" {
			rows = append(rows, w)
		}
	}
	d.Rows = len(rows)
	if rq.showsClosed() {
		rows = append(rows, cl.Rows...)
		d.Rows += cl.Matching
		d.Shown = len(cl.Rows)
		if cl.Matching > len(cl.Rows) && len(cl.Rows) >= cl.Want {
			d.MoreHref = href(q, "more", strconv.Itoa(cl.Want/closedStep))
		}
	}
	sortWorks(rows, rq.Sort, now)
	switch rq.By {
	case "none":
		if len(rows) > 0 {
			d.Sections = []ledgerSection{{Key: "all", Rows: rowCtxs(rows, p, now), N: d.Rows}}
		}
	case "scope":
		var order []string
		by := map[string][]workView{}
		for _, w := range rows {
			if _, ok := by[w.ScopeID]; !ok {
				order = append(order, w.ScopeID)
			}
			by[w.ScopeID] = append(by[w.ScopeID], w)
		}
		for _, id := range order {
			d.Sections = append(d.Sections, ledgerSection{Key: id, Label: scopeName[id], Rows: rowCtxs(by[id], p, now), N: len(by[id])})
		}
	default:
		for _, g := range railGroups {
			var in []workView
			for _, w := range rows {
				if w.Disp.Group == g.G {
					in = append(in, w)
				}
			}
			if len(in) > 0 {
				n := len(in)
				if g.G == "closed" {
					n = cl.Matching
				}
				d.Sections = append(d.Sections, ledgerSection{Key: g.G, Label: g.Label, Rows: rowCtxs(in, p, now), N: n})
			}
		}
	}
	n := 0
	for i := range d.Sections {
		for j := range d.Sections[i].Rows {
			d.Sections[i].Rows[j].Index = n
			n++
		}
	}
	if d.Rows == 0 {
		switch {
		case d.Total == 0:
			d.Empty = "nowork"
		case rq.G == "attention" && len(d.Tokens) == 0:
			d.Empty = "calm"
		default:
			d.Empty = "nomatch"
		}
	}
	return d
}

// Ratio is the row's executing share of its lifetime, out of 100.
func (c rowCtx) Ratio() float64 { return ratio(c.Active, c.Elapsed) }

// Title is the row's title with `code` spans, as the prototype's Inline.
func (c rowCtx) Title() template.HTML { return inlineCode(c.W.Title) }

// inlineCode escapes s and renders its backquoted spans as code.
func inlineCode(s string) template.HTML {
	parts := strings.Split(s, "`")
	var b strings.Builder
	for i, p := range parts {
		switch {
		case i%2 == 1 && i < len(parts)-1 && p != "":
			b.WriteString("<code>" + template.HTMLEscapeString(p) + "</code>")
		case i%2 == 1:
			b.WriteString("`" + template.HTMLEscapeString(p))
		default:
			b.WriteString(template.HTMLEscapeString(p))
		}
	}
	return template.HTML(b.String())
}

func rowCtxs(ws []workView, p *page, now time.Time) []rowCtx {
	out := make([]rowCtx, len(ws))
	for i, w := range ws {
		out[i] = rowCtx{W: w, Page: p, Life: lifelineOf(w, now, 22), Active: w.Active(now), Elapsed: w.Elapsed(now), Holdable: holdable(w), Held: w.Hold != nil}
		switch {
		case w.Disp.Since.IsZero():
		case w.Disp.Group == "closed":
			out[i].For = since(w.Disp.Since, now)
		default:
			out[i].For = "for " + dur(now.Sub(w.Disp.Since), false)
		}
	}
	return out
}

// runsPage is the Runs page.
func (s *Server) runsPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Runs", Nav: "runs"}
	wd := s.cachedWorld(ctx, true)
	q := r.URL.Query()
	cl, errs := s.closedFor(ctx, parseRunsQuery(q), wantClosed(q))
	p.Err = strings.Join(slices.Concat(wd.Errs, errs), "\n")
	d := runsView(wd, cl, q, p, s.opts.Now(), s.sourceLabel)
	if d.Total == 0 && len(wd.Errs) == 0 {
		if h, err := s.src.Health(ctx); err == nil {
			g, _ := strconv.Atoi(h.ConfigGeneration)
			d.Fresh = g == 0
		}
	}
	s.render(w, r, "runs", p, d)
}

// closedFor reads the closed Work the Runs page shows for rq: what every
// closed Work can be filtered on, how many the facets leave, and when the
// state filter lists closed Work, up to want of them with their runs. That
// is two or three requests whose cost follows want, however much has
// closed.
func (s *Server) closedFor(ctx context.Context, rq runsQuery, want int) (closedSet, []string) {
	cl := closedSet{Want: want}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []string
	)
	keep := func(err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, closedErr(err))
			mu.Unlock()
		}
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		var err error
		cl.Facets, err = s.src.WorkFacets(ctx, url.Values{"state": {closedStates}})
		keep(err)
	}()
	go func() {
		defer wg.Done()
		q := rq.closedQuery(s.opts.Now())
		if !rq.showsClosed() {
			q.Set("state", closedStates)
			q.Set("limit", "1")
			p, err := s.src.Works(ctx, q)
			if err == nil && p.Total != nil {
				cl.Matching = *p.Total
			}
			keep(err)
			return
		}
		var err error
		cl.Rows, cl.Matching, err = s.closedWork(ctx, q, want)
		keep(err)
	}()
	wg.Wait()
	return cl, errs
}

// closedErr is a failed read of closed Work, naming a daemon too old to
// answer it.
func closedErr(err error) string {
	if tooOldList(err) {
		return "this daemon does not list closed work by query; restart it on this version to see closed work here"
	}
	return errText(err)
}

// bulkControl holds or releases each selected Work that can take it, each
// with its own current revision and a fresh key, and says how many it
// skipped and why.
func (s *Server) bulkControl(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	name := r.PathValue("control")
	if name != "hold" && name != "release" {
		http.NotFound(w, r)
		return
	}
	why := strings.TrimSpace(r.PostFormValue("reason"))
	if why == "" {
		why = reason
	}
	ids := r.PostForm["work"]
	done, skipped, failed := s.bulk(ctx, name, ids, why)
	flashWith(w, bulkNote(name, len(ids), done, skipped, failed))
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// bulk applies the control to each Work in ids that can take it, counting
// those it skipped by why and the errors of those that failed.
func (s *Server) bulk(ctx context.Context, name string, ids []string, why string) (done int, skipped map[string]int, failed []string) {
	skipped = map[string]int{}
	for _, id := range ids {
		cw, err := s.src.Work(ctx, id)
		if err != nil {
			failed = append(failed, errText(err))
			continue
		}
		var runs []client.Run
		if name == "hold" && isOpen(cw.State) && cw.State != "running" && cw.Hold == nil {
			page, err := s.src.Runs(ctx, url.Values{"work_id": {id}, "limit": {"200"}})
			if err != nil {
				failed = append(failed, errText(err))
				continue
			}
			runs = page.Items
		}
		v := newWork(cw, runs, nil, s.opts.Now())
		var fn func(context.Context, string, string, string, string) (client.ControlResult, error)
		switch {
		case name == "release" && cw.Hold == nil:
			skipped["not held"]++
			continue
		case name == "release":
			fn = s.src.ReleaseWork
		case !v.Open():
			skipped["finished"]++
			continue
		case cw.Hold != nil:
			skipped["already held"]++
			continue
		case !holdable(v):
			skipped["running"]++
			continue
		default:
			fn = s.src.HoldWork
		}
		if _, err := fn(ctx, cw.ID, cw.Revision, why, s.opts.NewKey()); err != nil {
			failed = append(failed, errText(err))
			continue
		}
		done++
	}
	return done, skipped, failed
}

// bulkNote says what a bulk control did: "Held 3 of 5; 2 were running".
func bulkNote(name string, total, done int, skipped map[string]int, failed []string) flashNote {
	verb := map[string]string{"hold": "Held", "release": "Released"}[name]
	text := fmt.Sprintf("%s %d of %d", verb, done, total)
	var why []string
	for _, k := range []string{"running", "finished", "already held", "not held"} {
		if n := skipped[k]; n > 0 {
			was := "were"
			if n == 1 {
				was = "was"
			}
			if k == "finished" {
				was = "had"
			}
			why = append(why, fmt.Sprintf("%d %s %s", n, was, k))
		}
	}
	if len(failed) > 0 {
		why = append(why, fmt.Sprintf("%d failed: %s", len(failed), failed[0]))
	}
	if len(why) > 0 {
		text += "; " + strings.Join(why, ", ")
	}
	return flashNote{Text: text, Call: fmt.Sprintf("%d × POST /v1/work/…/%s", done, name), Err: len(failed) > 0}
}

// ago renders a timestamp relative to now: "just now", "4m ago", "3h ago",
// then the date.
func ago(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	case d < 7*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
	return t.Local().Format("2 Jan")
}

// summary is a run's outcome without its kind when the state already
// says it ("failed: the build broke" shows as "the build broke").
func summary(r client.Run) string {
	if o := r.Outcome; o != nil && o.Kind == r.State && o.Summary != "" {
		return o.Summary
	}
	return runview.Outcome(r)
}

// plural counts a noun: "1 run", "3 runs".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
