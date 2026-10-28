package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// workData is the Work page: the Work, the turn shown and everything the
// page around them says.
type workData struct {
	W    workView
	Ctx  workCtx
	Tab  string
	Turn *turnInfo
	// TV is the shown turn's transcript.
	TV turnView
	// TurnPanel is the shown turn's header: what started it and its stats.
	TurnPanel turnPanel
	Score     score
	// Prominent are the panels that offer an action; Side the others.
	Prominent []panelView
	Side      []panelView
	Repair    *repairNote
	Events    []eventRow
	Facts     facts
	// Since is how long the disposition has held, as the header says it.
	Since string
	// Missing is set instead of everything else when the Work could not
	// be read.
	Missing *workMissing
}

// score is the Work's lifecycle: its lifetime figures, its lifeline and
// every turn with the wait before it.
type score struct {
	Active, Elapsed time.Duration
	Ratio           float64
	Life            lifeline
	Turns           []scoreTurn
	Events          int
	// Now is the open cell after the last turn, for Work that is neither
	// closed nor running.
	Now *scoreNow
}

type scoreTurn struct {
	T        turnInfo
	Gap      time.Duration
	GapLabel string
	// GapShown says the gap is long enough to draw (over 90s).
	GapShown bool
	// Trigger is the triggering event's type without its family.
	Trigger  string
	Active   time.Duration
	Selected bool
	Href     string
}

type scoreNow struct {
	Gap   time.Duration
	Label string
	Kind  string
}

// turnPanel is the head of the turn shown.
type turnPanel struct {
	// Verb is how the turn began: Started by, Woken by, Retry of.
	Verb    string
	Trigger string
	Started time.Time
	// Outcome is the ended turn's outcome and what it means.
	Outcome     *client.Outcome
	OutcomeNote string
	Active      time.Duration
	Segments    int
	Model       string
	Usage       *client.Usage
	Calls, Tool int
	Deadline    time.Time
}

// repairNote says what failed in the runtime's words.
type repairNote struct {
	Label    string
	Text     string
	Retry    *client.RetryStatus
	Inspect  int
	Rebind   bool
	Turn     *turnInfo
	Blocked  string
	Failures int
}

// eventRow is one Event with how routing handled it.
type eventRow struct {
	client.Event
	At     time.Time
	Effect string
	Turn   int
	Open   bool
	Href   string
}

// facts are the sidebar's.
type facts struct {
	ScopeName  string
	Route      string
	Handler    string
	Policy     string
	PolicyText string
	Generation string
	Cost       float64
	Currency   string
	Reported   int
}

// workPage is the Work page: /work?id=…&turn=N, with ?tab=events for its
// Events.
func (s *Server) workPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	q := r.URL.Query()
	p := &page{Title: "Work", Nav: "runs"}
	id := q.Get("id")
	work, err := s.src.Work(ctx, id)
	if err != nil {
		p.Err = errText(err)
		status := statusOf(err)
		if status == http.StatusNotFound {
			p.Err = ""
		}
		w.WriteHeader(status)
		s.render(w, r, "work", p, workData{Missing: &workMissing{ID: id, NotFound: status == http.StatusNotFound}})
		return
	}
	d, errs := s.workData(ctx, p, work, q)
	p.Title = d.W.Label()
	p.Err = strings.Join(errs, "\n")
	s.render(w, r, "work", p, d)
}

// workMissing is the page for an ID the daemon does not know.
type workMissing struct {
	ID       string
	NotFound bool
}

func (s *Server) workData(ctx context.Context, p *page, work client.Work, q url.Values) (workData, []string) {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []string
		runs   []client.Run
		waits  []client.Wait
		events []client.Event
		panels []panelView
		acts   []client.Action
		cfg    client.Config
		cfgOK  bool
	)
	keep := func(err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, errText(err))
			mu.Unlock()
		}
	}
	target := client.ActionTarget{WorkID: work.ID}
	wg.Add(6)
	go func() {
		defer wg.Done()
		var err error
		runs, err = listAll(ctx, url.Values{"work_id": {work.ID}, "limit": {"200"}}, s.src.Runs)
		keep(err)
	}()
	go func() {
		defer wg.Done()
		var err error
		waits, err = listAll(ctx, url.Values{"work_id": {work.ID}, "limit": {"200"}}, s.src.Waits)
		keep(err)
	}()
	go func() {
		defer wg.Done()
		var err error
		events, err = listAll(ctx, url.Values{"work_id": {work.ID}, "limit": {"200"}}, s.src.EventsWithRoutes)
		if tooOld(err) || badFilter(err) {
			// A daemon older than the list form: routes are read one at
			// a time, for the event opened.
			events, err = listAll(ctx, url.Values{"work_id": {work.ID}, "limit": {"200"}}, s.src.Events)
			if err == nil && len(events) > 0 {
				// The first event's route names the Work's route.
				first := events[0].ID
				if rt, err := s.src.EventRoute(ctx, first); err == nil {
					events[0].Route = &rt
				}
			}
			if open := q.Get("event"); open != "" && err == nil {
				for i := range events {
					if events[i].ID == open && events[i].Route == nil {
						if rt, err := s.src.EventRoute(ctx, open); err == nil {
							events[i].Route = &rt
						}
					}
				}
			}
		}
		keep(err)
	}()
	go func() {
		defer wg.Done()
		var err error
		panels, err = s.panels(ctx, p, target)
		keep(err)
	}()
	go func() {
		defer wg.Done()
		a, err := s.src.Actions(ctx, target)
		if err != nil && !tooOld(err) {
			mu.Lock()
			errs = append(errs, actionsErr(err))
			mu.Unlock()
		}
		acts = a
	}()
	go func() {
		defer wg.Done()
		c, err := s.src.Config(ctx)
		cfg, cfgOK = c, err == nil
	}()
	wg.Wait()
	now := s.opts.Now()

	var active *client.Wait
	for i := range waits {
		if waits[i].State == "active" {
			active = &waits[i]
		}
	}
	work.Actions = acts
	work.Panels = nil
	for _, pv := range panels {
		work.Panels = append(work.Panels, pv.Panel)
	}
	sort.SliceStable(events, func(i, j int) bool { return seq(events[i].Sequence) < seq(events[j].Sequence) })
	v := newWork(work, runs, active, now)
	v.Events = events
	d := workData{W: v, Tab: q.Get("tab")}
	if d.Tab != "events" {
		d.Tab = ""
	}

	// The turn shown: the one asked for, else the running one, else a
	// parked one, else the latest.
	d.Turn = pickTurn(v.Turns, q.Get("turn"))
	d.Ctx = workCtx{W: v, Page: p, Turn: d.Turn}
	for _, pv := range panels {
		if panelActs(pv.Panel) {
			d.Prominent = append(d.Prominent, pv)
		} else {
			d.Side = append(d.Side, pv)
		}
	}
	d.Since = sinceText(v.Disp, now)
	d.Score = scoreOf(v, waits, d.Turn, q, now)
	d.Repair = repairOf(v)
	d.Facts = factsOf(v, cfg, cfgOK)
	if d.Tab == "events" {
		d.Events = eventRows(v, q)
	}
	if d.Turn != nil && d.Tab == "" {
		d.TV = s.turn(ctx, d.Turn.Run)
		d.TV.filter(txViewOf(q))
		d.TurnPanel = turnPanelOf(v, *d.Turn, d.TV.Chat, now)
	}
	return d, errs
}

// badFilter reports a daemon refusing a query parameter it does not know.
func badFilter(err error) bool {
	var e *client.Error
	return errors.As(err, &e) && e.Status == http.StatusBadRequest && e.Code == "invalid_filter"
}

func seq(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

func pickTurn(turns []turnInfo, want string) *turnInfo {
	if n, err := strconv.Atoi(want); err == nil {
		for i := range turns {
			if turns[i].Number == n {
				return &turns[i]
			}
		}
	}
	for _, state := range []string{"running", "pending", "parked"} {
		for i := range turns {
			if turns[i].State == state {
				return &turns[i]
			}
		}
	}
	if len(turns) == 0 {
		return nil
	}
	return &turns[len(turns)-1]
}

// sinceText is the header's "for 2h", or "2h ago" for closed Work.
func sinceText(d disposition, now time.Time) string {
	if d.Since.IsZero() {
		return ""
	}
	if d.Group == "closed" {
		return since(d.Since, now)
	}
	return "for " + dur(now.Sub(d.Since), false)
}

// scoreOf lays out the score: every turn with the wait before it, and the
// open now cell.
func scoreOf(v workView, waits []client.Wait, shown *turnInfo, q url.Values, now time.Time) score {
	sc := score{Active: v.Active(now), Elapsed: v.Elapsed(now), Life: lifelineOf(v, now, 20), Events: len(v.Events)}
	sc.Ratio = ratio(sc.Active, sc.Elapsed)
	for i, t := range v.Turns {
		st := scoreTurn{T: t, Active: t.Active(now), Selected: shown != nil && shown.ID == t.ID, Trigger: triggerWord(t)}
		st.Href = workLink(v.ID, q, "turn", strconv.Itoa(t.Number))
		from := v.Created
		if i > 0 {
			prev := v.Turns[i-1]
			if n := len(prev.Segments); n > 0 && !prev.Segments[n-1].End.IsZero() {
				from = prev.Segments[n-1].End
			} else {
				from = time.Time{}
			}
			st.GapLabel = waitedFor(prev, t, waits)
		} else {
			st.GapLabel = "queued"
		}
		if len(t.Segments) > 0 && !from.IsZero() {
			st.Gap = t.Segments[0].Start.Sub(from)
			st.GapShown = st.Gap > 90*time.Second
		}
		sc.Turns = append(sc.Turns, st)
	}
	if v.Open() && v.Disp.Group != "running" && !v.Disp.Since.IsZero() {
		sc.Now = &scoreNow{Gap: now.Sub(v.Disp.Since), Label: strings.ToLower(v.Disp.Label), Kind: v.Disp.Kind}
	}
	return sc
}

// waitedFor says what the Work waited for between prev and t: retry
// backoff after a failure, else what the Wait prev declared said it was
// for, else the prev turn's outcome kind.
func waitedFor(prev, t turnInfo, waits []client.Wait) string {
	if prev.State == "failed" {
		return "retry backoff"
	}
	for _, w := range waits {
		if w.ExecutionID == prev.ID && w.Description != "" {
			return "waiting · " + w.Description
		}
	}
	if prev.Outcome != nil && prev.Outcome.Kind != "" && prev.Outcome.Kind != "completed" {
		if prev.Outcome.Kind == "handoff" {
			return "waiting"
		}
		return prev.Outcome.Kind
	}
	return "waiting"
}

// triggerWord is a turn's trigger type without its family prefix:
// github.checks.passed reads "checks passed".
func triggerWord(t turnInfo) string {
	if t.Trigger == nil || t.Trigger.Type == "" {
		if t.Number <= 1 {
			return "start"
		}
		return "retry"
	}
	return shortType(t.Trigger.Type)
}

func shortType(typ string) string {
	_, rest, ok := strings.Cut(typ, ".")
	if !ok || rest == "" {
		return typ
	}
	return strings.ReplaceAll(strings.ReplaceAll(rest, ".", " "), "_", " ")
}

// workLink is the Work page's URL with one query parameter changed,
// keeping the others the page uses.
func workLink(id string, q url.Values, key, value string) string {
	out := url.Values{"id": {id}}
	for _, k := range []string{"turn", "tab", "show", "q", "expand"} {
		if v := q.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	if value == "" {
		out.Del(key)
	} else {
		out.Set(key, value)
	}
	if key == "turn" {
		out.Del("tab")
	}
	return "/work?" + out.Encode()
}

// repairOf is the note a repair shows: what failed in the runtime's words.
func repairOf(v workView) *repairNote {
	if v.Disp.Kind != "repair" {
		return nil
	}
	n := &repairNote{Label: v.Disp.Label, Text: v.Disp.Detail}
	for i := len(v.Turns) - 1; i >= 0; i-- {
		t := v.Turns[i]
		if t.State == "failed" || t.State == "cancelled" {
			n.Turn, n.Inspect = &v.Turns[i], t.Number
			n.Retry = t.Retry
			break
		}
	}
	if r := n.Retry; r != nil {
		n.Failures = r.FailureCount
		n.Blocked = r.BlockedReason
	}
	return n
}

// factsOf fills the sidebar from the configuration: the scope's name and
// the route the Work's events took, with its handler and policy.
func factsOf(v workView, cfg client.Config, ok bool) facts {
	f := facts{ScopeName: v.ScopeID, Generation: cfg.Generation}
	if t := v.Last(); t != nil && t.ConfigGeneration != "" {
		f.Generation = t.ConfigGeneration
	}
	for _, e := range v.Events {
		if e.Route != nil && e.Route.Route != "" {
			f.Route = e.Route.Route
			f.Policy = e.Route.Policy
			break
		}
	}
	if ok {
		for _, sc := range cfg.Scopes {
			if sc.ID != v.ScopeID {
				continue
			}
			if sc.Name != "" {
				f.ScopeName = sc.Name
			}
			for _, r := range sc.Routes {
				if r.Name == f.Route {
					f.Handler, f.Policy = r.Handler, r.ActivePolicy
				}
			}
		}
	}
	f.PolicyText = policyWords(f.Policy)
	for _, t := range v.Turns {
		if u := t.Usage; u != nil && u.Cost != nil {
			f.Cost += *u.Cost
			f.Reported++
			f.Currency = u.Currency
		}
	}
	return f
}

// eventRows are the Work's Events with what routing did with each; the one
// named by ?event= is open.
func eventRows(v workView, q url.Values) []eventRow {
	turnOf := map[string]int{}
	for _, t := range v.Turns {
		if t.Trigger != nil {
			turnOf[t.Trigger.ID] = t.Number
		}
		for _, s := range t.Run.Segments {
			if s.WakeEventID != "" {
				turnOf[s.WakeEventID] = t.Number
			}
		}
	}
	var out []eventRow
	for _, e := range v.Events {
		r := eventRow{Event: e, At: parseTime(e.OccurredAt), Turn: turnOf[e.ID], Open: q.Get("event") == e.ID}
		r.Effect = effectWord(e)
		if r.Open {
			r.Href = workLink(v.ID, q, "event", "")
		} else {
			r.Href = workLink(v.ID, q, "event", e.ID)
		}
		out = append(out, r)
	}
	return out
}

// effectWord is how routing affected the Work, in its own words.
func effectWord(e client.Event) string {
	d := e.Disposition
	if e.Route != nil && e.Route.Disposition != "" {
		d = e.Route.Disposition
	}
	if d == "" && e.Route != nil {
		d = e.Route.Policy
	}
	if d == "" {
		if e.Routed {
			return "routed"
		}
		return "not routed"
	}
	return strings.ReplaceAll(d, "_", " ")
}

// turnPanelOf is the head of the turn shown.
func turnPanelOf(v workView, t turnInfo, c chat, now time.Time) turnPanel {
	p := turnPanel{Active: t.Active(now), Segments: len(t.Run.Segments), Model: c.Model, Usage: t.Usage, Calls: c.Calls, Tool: c.Tools}
	if len(t.Segments) > 0 {
		p.Started = t.Segments[0].Start
	} else {
		p.Started = parseTime(t.CreatedAt)
	}
	if t.Trigger != nil {
		p.Trigger = t.Trigger.Type
	}
	switch {
	case t.Number <= 1:
		p.Verb = "Started by"
	case t.Number >= 2 && len(v.Turns) >= t.Number-1 && v.Turns[t.Number-2].State == "failed":
		p.Verb = "Retry of"
	default:
		p.Verb = "Woken by"
	}
	if t.Outcome != nil && !t.Live() {
		p.Outcome = t.Outcome
		switch t.Outcome.Kind {
		case "handoff":
			p.OutcomeNote = "Turn ended; the next event continues the work"
		case "waiting":
			p.OutcomeNote = "Parked; resumes in this session"
		case "completed":
			p.OutcomeNote = "Completed"
		default:
			p.OutcomeNote = "Failed"
		}
	}
	if t.Live() && t.Deadline != "" {
		p.Deadline = parseTime(t.Deadline)
	}
	return p
}

// workFuncs are the template functions the Work page and its transcript
// use.
var workFuncs = template.FuncMap{
	"prose":      prose,
	"code":       codeOf,
	"inputText":  inputText,
	"money":      money,
	"kTok":       kTok,
	"filterName": func(f string) string { return filterNames[f] },
	"filters":    func() []string { return chatFilters },
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("15:04:05")
	},
	"policyWords": policyWords,
	"initial": func(s string) string {
		for _, r := range strings.TrimPrefix(s, "operator:") {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"filterHref": filterHref,
	"parseTime":  parseTime,
	"cut": func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	},
	"blockCtx": func(b chatBlock, x bool) blockCtx { return blockCtx{b, x} },
	"toolCtx":  func(t chatTool, x bool) toolCtx { return toolCtx{t, x} },
	"orchMark": func(tone string) string {
		switch tone {
		case "park":
			return "parked"
		case "wake":
			return "running"
		case "cancel":
			return "cancelled"
		case "inject":
			return "decision"
		}
		return "queued"
	},
	"deref": func(n *uint64) uint64 {
		if n == nil {
			return 0
		}
		return *n
	},
	"derefCost": func(n *float64) float64 {
		if n == nil {
			return 0
		}
		return *n
	},
}

// filterHref is the transcript toolbar's link that turns one filter on or
// off, keeping the rest of the view.
func filterHref(id string, turn int, v txView, f string) string {
	q := url.Values{"id": {id}, "turn": {strconv.Itoa(turn)}}
	var on []string
	for _, x := range chatFilters {
		if v.On[x] != (x == f) {
			on = append(on, x)
		}
	}
	if len(on) != len(chatFilters) {
		q.Set("show", strings.Join(on, ","))
		if len(on) == 0 {
			q.Set("show", "none")
		}
	}
	if v.Query != "" {
		q.Set("q", v.Query)
	}
	if v.Expand {
		q.Set("expand", "1")
	}
	return "/work?" + q.Encode()
}

// blockCtx and toolCtx carry a chat block or call into its template with
// whether every row is open.
type blockCtx struct {
	B      chatBlock
	Expand bool
}

type toolCtx struct {
	T      chatTool
	Expand bool
}
