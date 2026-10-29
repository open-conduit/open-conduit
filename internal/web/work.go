package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// workView is a Work as the pages show it: what it is, its turns oldest
// first, and what it is doing or waiting for now (its disposition), all
// derived from the management API's records. Nothing here knows a Work
// kind, source, extension or operation: what a Work is comes from the
// display its source gave, what it waits for from the Wait's description,
// and what a person can do from the actions extensions offer.
type workView struct {
	client.Work
	// Title and Ref are the source's display, else the label the CLI and
	// TUI show and no ref.
	Title, Ref string
	// Source is the family of the Work's first correlation key ("github"
	// for "github.issue"), for the source filter and its label.
	Source string
	Turns  []turnInfo
	Events []client.Event
	// Wait is the active Wait, if any.
	Wait    *client.Wait
	Actions []client.Action
	Panels  []client.Panel
	Disp    disposition
	// Binding and Executor are the latest turn's.
	Binding, Executor string
	// Summary is the latest outcome's summary.
	Summary string
	Created time.Time
	Updated time.Time
}

// turnInfo is one turn (an execution attempt) with its times.
type turnInfo struct {
	client.Run
	Number   int
	Segments []span
}

// span is a stretch of active time; End is zero while it runs.
type span struct {
	Start, End time.Time
}

// disposition is what the Work is doing or waiting for from the operator's
// point of view. Group drives the state rail, Kind the mark; Since is when
// the condition began.
type disposition struct {
	Group  string
	Kind   string
	Label  string
	Detail string
	Since  time.Time
	// Waiting is the sub-rail key for waiting Work: what it waits on.
	Waiting string
}

// Groups in the order the pages list them.
var dispGroups = []string{"attention", "running", "queued", "waiting", "held", "closed"}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func isOpen(state string) bool {
	return state == "ready" || state == "running" || state == "waiting"
}

func itoa(i int) string { return strconv.Itoa(i) }

// sourceFamily is the part of a source or type name before its first dot
// or slash: "github" for "github.issue" and for the source "github/main".
func sourceFamily(s string) string {
	if i := strings.IndexAny(s, "./"); i >= 0 {
		return s[:i]
	}
	return s
}

// newTurn builds a turn's spans from its segments, or from its start and
// finish when the run predates segments.
func newTurn(r client.Run) turnInfo {
	t := turnInfo{Run: r, Number: int(r.Attempt)}
	for _, s := range r.Segments {
		t.Segments = append(t.Segments, span{parseTime(s.StartedAt), parseTime(s.EndedAt)})
	}
	if len(t.Segments) == 0 && r.StartedAt != "" {
		end := parseTime(r.FinishedAt)
		if end.IsZero() && !runview.Running(r) {
			end = parseTime(r.UpdatedAt)
		}
		t.Segments = []span{{parseTime(r.StartedAt), end}}
	}
	return t
}

// Active is the turn's active time as of now.
func (t turnInfo) Active(now time.Time) time.Duration {
	if len(t.Run.Segments) > 0 || t.ActiveMS > 0 {
		return t.Run.Active(now)
	}
	var d time.Duration
	for _, s := range t.Segments {
		end := s.End
		if end.IsZero() {
			end = now
		}
		if end.After(s.Start) {
			d += end.Sub(s.Start)
		}
	}
	return d
}

// Live says whether the turn is pending or running.
func (t turnInfo) Live() bool { return runview.Running(t.Run) }

// newWork builds the view of w from its runs (any order), its active Wait
// and the actions and panels offered on it.
func newWork(w client.Work, runs []client.Run, wait *client.Wait, now time.Time) workView {
	v := workView{Work: w, Wait: wait, Actions: w.Actions, Panels: w.Panels, Created: parseTime(w.CreatedAt), Updated: parseTime(w.UpdatedAt)}
	v.Title, v.Ref = runview.WorkLabel(w), ""
	// Without a display or an annotation the label is the first key; the
	// row names its source beside the title, so the title is the key's ID.
	if k := w.CorrelationKeys; len(k) > 0 && v.Title == k[0].Source+" "+k[0].ExternalID {
		v.Title = k[0].ExternalID
	}
	if d := w.Display; d != nil {
		if d.Title != "" {
			v.Title = d.Title
		}
		v.Ref = d.Ref
	}
	if len(w.CorrelationKeys) > 0 {
		v.Source = sourceFamily(w.CorrelationKeys[0].Source)
	} else {
		v.Source = sourceFamily(w.Kind)
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Attempt < runs[j].Attempt })
	for _, r := range runs {
		v.Turns = append(v.Turns, newTurn(r))
	}
	if last := v.Last(); last != nil {
		v.Binding, v.Executor = last.ExecutorBinding, last.Executor
		if last.Outcome != nil {
			v.Summary = last.Outcome.Summary
		}
	}
	v.Disp = dispositionOf(v, now)
	return v
}

// Last is the latest turn, or nil.
func (v *workView) Last() *turnInfo {
	if len(v.Turns) == 0 {
		return nil
	}
	return &v.Turns[len(v.Turns)-1]
}

// Open says whether the Work has not closed.
func (v workView) Open() bool { return isOpen(v.State) }

// Label is the Work's ref and title as one line.
func (v workView) Label() string {
	if v.Ref == "" {
		return v.Title
	}
	return v.Ref + " " + v.Title
}

// dispositionOf derives what the Work is doing or waiting for, in order of
// precedence: closed, held, a person's decision, running, a repair,
// waiting, queued.
func dispositionOf(v workView, now time.Time) disposition {
	last := v.Last()
	updated := v.Updated
	summary := v.Summary
	switch v.State {
	case "completed":
		return disposition{Group: "closed", Kind: "completed", Label: "Completed", Detail: summary, Since: updated}
	case "failed":
		return disposition{Group: "closed", Kind: "failed", Label: "Failed", Detail: summary, Since: updated}
	case "cancelled":
		return disposition{Group: "closed", Kind: "cancelled", Label: "Cancelled", Detail: summary, Since: updated}
	}
	if h := v.Hold; h != nil {
		return disposition{Group: "held", Kind: "held", Label: "Held", Detail: h.Reason, Since: parseTime(h.At)}
	}
	if v.State == "waiting" && len(v.Actions) > 0 {
		d := disposition{Group: "attention", Kind: "decision", Label: v.Actions[0].Label, Detail: v.Actions[0].Label, Since: updated}
		if v.Wait != nil {
			d.Since = parseTime(v.Wait.CreatedAt)
		}
		for _, p := range v.Panels {
			if !panelActs(p) {
				continue
			}
			d.Label = p.Title
			for _, s := range p.Sections {
				if s.Text != "" {
					d.Detail = s.Text
					break
				}
			}
			break
		}
		return d
	}
	if last != nil && last.Live() {
		d := disposition{Group: "running", Kind: "running", Label: "Running", Detail: "Turn " + itoa(last.Number), Since: parseTime(last.StartedAt)}
		if last.State == "pending" || last.StartedAt == "" {
			d.Since = parseTime(last.CreatedAt)
		}
		if len(last.Segments) > 0 {
			d.Since = last.Segments[len(last.Segments)-1].Start
		}
		return d
	}
	if v.State == "ready" && last != nil {
		if r := last.Retry; last.State == "failed" && r != nil && (r.Exhausted || r.Blocked) {
			detail := last.outcomeText()
			if r.Blocked && r.BlockedReason != "" {
				detail = r.BlockedReason
			}
			return disposition{Group: "attention", Kind: "repair", Label: "Failed turn", Detail: detail, Since: parseTime(last.FinishedAt)}
		}
		if last.State == "cancelled" {
			return disposition{Group: "attention", Kind: "repair", Label: "Turn cancelled", Detail: "An operator cancelled turn " + itoa(last.Number) + "; run it again or cancel the Work", Since: parseTime(last.FinishedAt)}
		}
	}
	if v.State == "waiting" || v.Wait != nil {
		d := disposition{Group: "waiting", Kind: "waiting", Label: "Waiting", Detail: summary, Since: updated}
		if wt := v.Wait; wt != nil {
			d.Since = parseTime(wt.CreatedAt)
			if wt.Description != "" {
				d.Detail = wt.Description
			}
			if wt.Progress != "" {
				d.Detail = joinDot(wt.Description, wt.Progress)
			}
			d.Waiting = waitingOn(*wt)
		}
		return d
	}
	d := disposition{Group: "queued", Kind: "queued", Label: "Queued", Detail: "Ready for the next turn", Since: updated}
	if last != nil && last.State == "failed" && last.Retry != nil && last.Retry.EligibleAt != "" {
		d.Detail = "Retrying after turn " + itoa(last.Number) + " failed"
		d.Since = parseTime(last.FinishedAt)
	}
	return d
}

// waitingOn is what a Wait waits on for the sub-rail: the one family its
// event types share ("github"), "time" for a timer alone, else "events".
func waitingOn(w client.Wait) string {
	fam := ""
	for _, t := range w.Match.Types {
		f := sourceFamily(t)
		if fam != "" && f != fam {
			return "events"
		}
		fam = f
	}
	switch {
	case fam != "":
		return fam
	case w.TimeoutTimerID != "" || w.DueAt != "":
		return "time"
	}
	return "events"
}

// panelActs says whether a panel offers an action.
func panelActs(p client.Panel) bool {
	for _, s := range p.Sections {
		if len(s.Actions) > 0 {
			return true
		}
	}
	return false
}

// outcomeText is the turn's outcome summary, else its state.
func (t turnInfo) outcomeText() string {
	if t.Outcome != nil && t.Outcome.Summary != "" {
		return t.Outcome.Summary
	}
	return "Turn " + itoa(t.Number) + " " + t.State
}

// Active is the sum of the Work's turns' active time.
func (v workView) Active(now time.Time) time.Duration {
	var d time.Duration
	for _, t := range v.Turns {
		d += t.Active(now)
	}
	return d
}

// End is when the Work's lifetime ends for its figures: when it closed, or
// now.
func (v workView) End(now time.Time) time.Time {
	if !v.Open() && !v.Disp.Since.IsZero() {
		return v.Disp.Since
	}
	return now
}

// Elapsed is the Work's lifetime: created to closed, or to now.
func (v workView) Elapsed(now time.Time) time.Duration {
	return v.End(now).Sub(v.Created)
}

// lifeline is the geometry of a Work's lifetime on one line, in a 0–1000
// wide viewBox: a bar per active segment, a tick per Event.
type lifeline struct {
	Height int
	Mid    float64
	EndX   float64
	Bars   []lifeBar
	Ticks  []lifeTick
	// Mark is the disposition kind drawn at the end while the Work is open.
	Mark string
}

type lifeBar struct {
	X, Width float64
	// Tone is the turn's colour: run, fail, muted or ink.
	Tone string
}

type lifeTick struct {
	X float64
	// Person says whether a person rather than a bot raised the event.
	Person bool
}

const (
	lifeWidth  = 1000
	lifeMinBar = 22
)

func barTone(state string) string {
	switch state {
	case "running", "pending":
		return "run"
	case "failed":
		return "fail"
	case "cancelled":
		return "muted"
	}
	return "ink"
}

// lifelineOf lays out v's turns and events from its creation to its end
// (at least a minute), clamping anything outside.
func lifelineOf(v workView, now time.Time, height int) lifeline {
	start := v.Created
	end := v.End(now)
	if end.Before(start.Add(time.Minute)) {
		end = start.Add(time.Minute)
	}
	total := float64(end.Sub(start))
	x := func(t time.Time) float64 {
		if t.Before(start) {
			t = start
		}
		if t.After(end) {
			t = end
		}
		return float64(t.Sub(start))/total*(lifeWidth-8) + 4
	}
	l := lifeline{Height: height, Mid: float64(height)/2 + 3, EndX: x(end)}
	if v.Open() {
		l.Mark = v.Disp.Kind
	}
	for _, e := range v.Events {
		l.Ticks = append(l.Ticks, lifeTick{X: x(parseTime(e.OccurredAt)), Person: !strings.Contains(e.Source, "bot")})
	}
	for _, t := range v.Turns {
		for _, s := range t.Segments {
			a := x(s.Start)
			e := s.End
			if e.IsZero() {
				e = now
			}
			l.Bars = append(l.Bars, lifeBar{X: a, Width: max(x(e)-a, lifeMinBar), Tone: barTone(t.State)})
		}
	}
	return l
}

// ratio is the share of elapsed time spent executing, as a width out of
// 100, at least 3 when there was any.
func ratio(active, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	p := min(1, float64(active)/float64(elapsed)) * 100
	if active > 0 {
		p = max(p, 3)
	}
	return p
}

// world is everything the Runs, Needs you and palette pages show: every
// Work with its turns, active Wait and offers.
type world struct {
	Works  []workView
	Scopes []client.Scope
	Errs   []string
}

// fetchCap bounds how many records of each list the view reads.
const fetchCap = 1000

// listAll reads up to fetchCap items of a paginated list.
func listAll[T any](ctx context.Context, q url.Values, fn func(context.Context, url.Values) (client.Page[T], error)) ([]T, error) {
	var out []T
	q = cloneQuery(q)
	for len(out) < fetchCap {
		p, err := fn(ctx, q)
		out = append(out, p.Items...)
		if err != nil || p.NextCursor == "" {
			return out, err
		}
		q.Set("cursor", p.NextCursor)
	}
	return out, nil
}

func cloneQuery(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// world reads every Work with its runs, active Waits, scopes, and for open
// Work the actions and panels on it, concurrently. Holds are on the single
// Work record only, so ready Work is read once more for its hold.
func (s *Server) world(ctx context.Context) world {
	var (
		wg             sync.WaitGroup
		mu             sync.Mutex
		openW, closedW []client.Work
		runs           []client.Run
		waits          []client.Wait
		scopes         []client.Scope
		errs           []string
	)
	keep := func(err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, errText(err))
			mu.Unlock()
		}
	}
	wg.Add(4)
	go func() {
		defer wg.Done()
		var err error
		openW, closedW, err = s.works(ctx)
		keep(err)
	}()
	go func() {
		defer wg.Done()
		var err error
		runs, err = listAll(ctx, url.Values{"limit": {"200"}}, s.src.Runs)
		keep(err)
	}()
	go func() {
		defer wg.Done()
		var err error
		waits, err = listAll(ctx, url.Values{"state": {"active"}, "limit": {"200"}}, s.src.Waits)
		keep(err)
	}()
	go func() {
		defer wg.Done()
		var err error
		scopes, err = listAll(ctx, url.Values{"limit": {"200"}}, s.src.Scopes)
		keep(err)
	}()
	wg.Wait()
	s.holds(ctx, openW)
	byWork := map[string][]client.Run{}
	for _, r := range runs {
		byWork[r.WorkID] = append(byWork[r.WorkID], r)
	}
	waitOf := map[string]*client.Wait{}
	for i := range waits {
		waitOf[waits[i].WorkID] = &waits[i]
	}
	now := s.opts.Now()
	out := world{Scopes: scopes, Errs: errs}
	for _, w := range append(openW, closedW...) {
		out.Works = append(out.Works, newWork(w, byWork[w.ID], waitOf[w.ID], now))
	}
	return out
}

// works reads open Work with the actions and panels on it, and closed Work.
// A daemon older than the list forms (#124) refuses with= and several
// states at once; then every Work is read in one list and the offers on
// waiting Work one Work at a time, eight at a time.
func (s *Server) works(ctx context.Context) (openW, closedW []client.Work, err error) {
	var cerr error
	openW, err = listAll(ctx, url.Values{"state": {"ready,running,waiting"}, "limit": {"50"}}, s.src.WorksWithActions)
	if err == nil {
		closedW, cerr = listAll(ctx, url.Values{"state": {"completed,failed,cancelled"}, "limit": {"200"}}, s.src.Works)
	}
	var e *client.Error
	if !errors.As(err, &e) && !errors.As(cerr, &e) || e.Status != http.StatusBadRequest {
		if err == nil {
			err = cerr
		}
		return openW, closedW, err
	}
	all, err := listAll(ctx, url.Values{"limit": {"200"}}, s.src.Works)
	openW, closedW = nil, nil
	for _, w := range all {
		if isOpen(w.State) {
			openW = append(openW, w)
		} else {
			closedW = append(closedW, w)
		}
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range openW {
		if openW[i].State != "waiting" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(w *client.Work) {
			defer wg.Done()
			defer func() { <-sem }()
			t := client.ActionTarget{WorkID: w.ID}
			w.Actions, _ = s.src.Actions(ctx, t)
			w.Panels, _ = s.src.Panels(ctx, t)
		}(&openW[i])
	}
	wg.Wait()
	return openW, closedW, err
}

// holds fills in the hold of each ready Work, eight at a time: a held Work
// is ready, and lists do not carry holds.
func (s *Server) holds(ctx context.Context, works []client.Work) {
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range works {
		if works[i].State != "ready" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(w *client.Work) {
			defer wg.Done()
			defer func() { <-sem }()
			if full, err := s.src.Work(ctx, w.ID); err == nil {
				w.Hold = full.Hold
			}
		}(&works[i])
	}
	wg.Wait()
}

// worldTTL is how long a read of every Work serves the nav count and the
// palette before it is read again.
const worldTTL = 3 * time.Second

// cachedWorld is the world read at most worldTTL ago; fresh reads it now.
func (s *Server) cachedWorld(ctx context.Context, fresh bool) world {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if fresh || s.wat.IsZero() || s.opts.Now().Sub(s.wat) > worldTTL || s.opts.Now().Before(s.wat) {
		s.wcache, s.wat = s.world(ctx), s.opts.Now()
	}
	return s.wcache
}

// needsYou counts the Work in the attention group.
func (s *Server) needsYou(ctx context.Context) int {
	n := 0
	for _, w := range s.cachedWorld(ctx, false).Works {
		if w.Disp.Group == "attention" {
			n++
		}
	}
	return n
}

// joinDot joins the non-empty parts with " · ".
func joinDot(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}
