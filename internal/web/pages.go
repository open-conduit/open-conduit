package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

// Filters are the state filters the list offers, as in the TUI.
var Filters = []string{"", "running", "parked", "failed", "completed", "waiting", "handoff", "cancelled"}

// LogTail bounds how much of an output the view fetches.
const LogTail = 64 << 10

// page is what every page template gets.
type page struct {
	Title    string
	Nav      string
	CSRF     string
	Back     string
	Flash    string
	FlashErr bool
	Err      string
}

func (s *Server) parse() error {
	funcs := template.FuncMap{
		"trigger":    runview.TriggerText,
		"outcome":    runview.Outcome,
		"time":       runview.ShortTime,
		"duration":   func(r client.Run) string { return runview.Duration(r, s.opts.Now()) },
		"live":       runview.Running,
		"ago":        func(ts string) string { return ago(ts, s.opts.Now()) },
		"plural":     plural,
		"summary":    summary,
		"stateGroup": stateGroup,
		"inc":        func(i int) int { return i + 1 },
	}
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"list", "run", "output", "scopes", "config"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/panels.html", "templates/"+name+".html")
		if err != nil {
			return err
		}
		s.pages[name] = t
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *page, data any) {
	p.CSRF, p.Back = s.csrf, r.URL.RequestURI()
	p.Flash, p.FlashErr = takeFlash(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].Execute(w, map[string]any{"Page": p, "Data": data}); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

func call(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), callTimeout)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// row is one list row: a run, its Work's name, its place among the
// Work's turns and those turns, oldest first.
type row struct {
	Run   client.Run
	Label string
	Turns string
	Trail []client.Run
}

// group is a run of rows that share what the operator does about them.
type group struct {
	Name string
	Rows []row
}

type listData struct {
	Filter  string
	Filters []string
	PerTurn bool
	Groups  []group
	Works   int
	Runs    int
}

// groups orders the states by what they ask of the operator: failures
// first, then what is running, then what waits, then what has finished.
var groups = []string{"Failed", "Running", "Waiting", "Finished"}

func stateGroup(state string) string {
	switch state {
	case "failed":
		return "Failed"
	case "running", "pending":
		return "Running"
	case "parked", "waiting", "handoff":
		return "Waiting"
	}
	return "Finished"
}

// grouped splits rows into the groups that have any, keeping their order.
func grouped(rs []row) []group {
	by := map[string][]row{}
	for _, r := range rs {
		g := stateGroup(r.Run.State)
		by[g] = append(by[g], r)
	}
	var out []group
	for _, name := range groups {
		if len(by[name]) > 0 {
			out = append(out, group{Name: name, Rows: by[name]})
		}
	}
	return out
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

// rows are the latest turn of each Work with its turn count, or with
// perTurn every turn as "k/n" of its Work (counting the runs listed).
func rows(runs []client.Run, perTurn bool) []row {
	total := map[string]int{}
	for _, r := range runs {
		total[r.WorkID]++
	}
	trail := map[string][]client.Run{}
	for i := len(runs) - 1; i >= 0; i-- {
		trail[runs[i].WorkID] = append(trail[runs[i].WorkID], runs[i])
	}
	seen := map[string]int{}
	var out []row
	for _, r := range runs {
		seen[r.WorkID]++
		n := total[r.WorkID]
		switch {
		case perTurn:
			out = append(out, row{Run: r, Turns: fmt.Sprintf("%d/%d", n-seen[r.WorkID]+1, n), Trail: trail[r.WorkID]})
		case seen[r.WorkID] == 1:
			out = append(out, row{Run: r, Turns: strconv.Itoa(n), Trail: trail[r.WorkID]})
		}
	}
	return out
}

// label names Work, looking each up once: labels don't change.
func (s *Server) label(ctx context.Context, id string) string {
	s.mu.Lock()
	l, ok := s.labels[id]
	s.mu.Unlock()
	if ok {
		return l
	}
	w, err := s.src.Work(ctx, id)
	if err != nil {
		return id
	}
	l = runview.WorkLabel(w)
	s.mu.Lock()
	s.labels[id] = l
	s.mu.Unlock()
	return l
}

func (s *Server) listPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	q := r.URL.Query()
	d := listData{Filter: q.Get("state"), Filters: Filters, PerTurn: q.Get("turns") == "1"}
	p := &page{Title: "Runs", Nav: "runs"}
	lq := url.Values{"limit": {"200"}}
	if d.Filter != "" {
		lq.Set("state", d.Filter)
	}
	runs, err := s.src.Runs(ctx, lq)
	if err != nil {
		p.Err = errText(err)
	}
	sort.SliceStable(runs.Items, func(i, j int) bool { return runs.Items[i].CreatedAt > runs.Items[j].CreatedAt })
	works := map[string]bool{}
	for _, run := range runs.Items {
		works[run.WorkID] = true
	}
	d.Works, d.Runs = len(works), len(runs.Items)
	rs := rows(runs.Items, d.PerTurn)
	for i := range rs {
		rs[i].Label = s.label(ctx, rs[i].Run.WorkID)
	}
	d.Groups = grouped(rs)
	s.render(w, r, "list", p, d)
}

type runData struct {
	Run      client.Run
	Detail   string
	Attempts []client.Run
	Work     *client.Work
	Waits    []client.Wait
	Panels   []panelView
	// Actions are those the panels do not already show.
	Actions []client.Action
}

// panelView is a panel with what its buttons need: the page and the target.
type panelView struct {
	client.Panel
	Page            *page
	ScopeID, WorkID string
}

// panels fetches a target's panels for p. A daemon too old to serve them
// shows none: its actions say it needs upgrading.
func (s *Server) panels(ctx context.Context, p *page, t client.ActionTarget) ([]panelView, error) {
	ps, err := s.src.Panels(ctx, t)
	if tooOld(err) {
		return nil, nil
	}
	out := make([]panelView, len(ps))
	for i, x := range ps {
		out[i] = panelView{Panel: x, Page: p, ScopeID: t.ScopeID, WorkID: t.WorkID}
	}
	return out, err
}

// unshown are the actions no panel offers.
func unshown(actions []client.Action, panels []panelView) []client.Action {
	in := map[[2]string]bool{}
	for _, p := range panels {
		for _, sec := range p.Sections {
			for _, a := range sec.Actions {
				in[[2]string{a.Provider, a.ID}] = true
			}
		}
	}
	var out []client.Action
	for _, a := range actions {
		if !in[[2]string{a.Provider, a.ID}] {
			out = append(out, a)
		}
	}
	return out
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Run", Nav: "runs"}
	run, err := s.src.Run(ctx, r.URL.Query().Get("id"))
	if err != nil {
		p.Err = errText(err)
		w.WriteHeader(statusOf(err))
		s.render(w, r, "run", p, nil)
		return
	}
	d := runData{Run: run, Detail: runview.Detail(run, nil, s.opts.Now())}
	p.Title = s.label(ctx, run.WorkID)
	var errs []string
	if page, err := s.src.Runs(ctx, url.Values{"work_id": {run.WorkID}, "limit": {"200"}}); err != nil {
		errs = append(errs, errText(err))
	} else {
		d.Attempts = page.Items
		sort.SliceStable(d.Attempts, func(i, j int) bool { return d.Attempts[i].Attempt < d.Attempts[j].Attempt })
	}
	if work, err := s.src.Work(ctx, run.WorkID); err != nil {
		errs = append(errs, errText(err))
	} else {
		d.Work = &work
	}
	if page, err := s.src.Waits(ctx, url.Values{"work_id": {run.WorkID}, "limit": {"200"}}); err != nil {
		errs = append(errs, errText(err))
	} else {
		for _, wt := range page.Items {
			// As in the TUI: the open Waits, and those that woke the Work.
			if wt.State == "active" || wt.WakeEventID != "" {
				d.Waits = append(d.Waits, wt)
			}
		}
		sort.SliceStable(d.Waits, func(i, j int) bool { return d.Waits[i].CreatedAt < d.Waits[j].CreatedAt })
	}
	target := client.ActionTarget{WorkID: run.WorkID}
	if d.Panels, err = s.panels(ctx, p, target); err != nil {
		errs = append(errs, errText(err))
	}
	if actions, err := s.src.Actions(ctx, target); err != nil {
		errs = append(errs, actionsErr(err))
	} else {
		d.Actions = unshown(actions, d.Panels)
	}
	p.Err = strings.Join(errs, "\n")
	s.render(w, r, "run", p, d)
}

// actionsErr explains a daemon too old to list actions.
func actionsErr(err error) string {
	if tooOld(err) {
		return "this daemon does not offer actions; upgrade it to answer and claim from here"
	}
	return errText(err)
}

// tooOld reports a daemon without the route asked for.
func tooOld(err error) bool {
	var e *client.Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == "not_found"
}

func statusOf(err error) int {
	var e *client.Error
	if errors.As(err, &e) && e.Status >= 400 {
		return e.Status
	}
	return http.StatusBadGateway
}

// Output picks: the log, the transcript, or the output at an index.
const (
	pickLog        = -1
	pickTranscript = -2
)

// output is one of a run's outputs as shown: its live output while it runs,
// else its artifact.
type output struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	Index int    `json:"index"`
	Count int    `json:"count"`
	Live  bool   `json:"live"`
	Err   string `json:"error,omitempty"`
}

func pick(n, want, log, transcript int, hasTranscript bool) (int, error) {
	switch {
	case want == pickTranscript && !hasTranscript:
		return 0, fmt.Errorf("run has no transcript")
	case want == pickTranscript:
		return transcript, nil
	case want == pickLog:
		return log, nil
	}
	return want % n, nil
}

func parsePick(v string) int {
	switch v {
	case "", "log":
		return pickLog
	case "transcript":
		return pickTranscript
	}
	if i, err := strconv.Atoi(v); err == nil && i >= 0 {
		return i
	}
	return pickLog
}

// fetchOutput reads the output want of run id: the live output while it
// runs, else its artifact, settling on the artifacts once it ends.
func (s *Server) fetchOutput(ctx context.Context, id string, want int) (output, error) {
	live, err := s.src.RunLive(ctx, id, LogTail)
	var e *client.Error
	switch {
	case errors.As(err, &e) && (e.Code == "not_live" || e.Code == "live_unsupported"):
	case err != nil:
		return output{}, err
	case len(live.Outputs) == 0:
		return output{Title: id + " (running)", Live: true}, nil
	default:
		log, _ := runview.LiveLog(live.Outputs)
		transcript, has := runview.LiveTranscript(live.Outputs)
		i, err := pick(len(live.Outputs), want, log, transcript, has)
		if err != nil {
			return output{}, err
		}
		o := live.Outputs[i]
		return output{Title: o.Name + " (running)", Text: o.Content, Index: i, Count: len(live.Outputs), Live: true}, nil
	}
	run, err := s.src.Run(ctx, id)
	if err != nil {
		return output{}, err
	}
	if len(run.Artifacts) == 0 {
		return output{}, fmt.Errorf("run has no artifacts")
	}
	logIdx, transcript, has := 0, 0, false
	for i, a := range run.Artifacts {
		if l, ok := runview.LogArtifact(run); ok && l.ID == a.ID {
			logIdx = i
		}
		if t, ok := runview.TranscriptArtifact(run); ok && t.ID == a.ID {
			transcript, has = i, true
		}
	}
	i, err := pick(len(run.Artifacts), want, logIdx, transcript, has)
	if err != nil {
		return output{}, err
	}
	a := run.Artifacts[i]
	if run.Retention.State == "expired" {
		return output{}, fmt.Errorf("artifact content expired under the retention policy")
	}
	out := output{Title: a.ID + " " + a.MediaType, Index: i, Count: len(run.Artifacts)}
	if a.Size == 0 {
		return out, nil
	}
	var b strings.Builder
	byteRange := ""
	if a.Size > LogTail {
		byteRange = "bytes=-" + strconv.Itoa(LogTail)
	}
	if err := s.src.ArtifactContent(ctx, a.ID, byteRange, &b); err != nil {
		return output{}, err
	}
	out.Text = b.String()
	return out, nil
}

type outputData struct {
	RunID string
	Pick  string
	Out   output
	Next  int
}

func (s *Server) outputPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	id, want := r.URL.Query().Get("run"), r.URL.Query().Get("pick")
	out, err := s.fetchOutput(ctx, id, parsePick(want))
	if err != nil {
		out.Err = errText(err)
	}
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	if want == "" {
		want = "log"
	}
	next := 0
	if out.Count > 0 {
		next = (out.Index + 1) % out.Count
	}
	s.render(w, r, "output", &page{Title: "Output " + id, Nav: "runs", Err: out.Err}, outputData{RunID: id, Pick: want, Out: out, Next: next})
}

type scopeActions struct {
	Scope   client.Scope
	Panels  []panelView
	Actions []client.Action
	Err     string
}

func (s *Server) scopesPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Scopes", Nav: "scopes"}
	scopes, err := s.src.Scopes(ctx, url.Values{"limit": {"200"}})
	if err != nil {
		p.Err = errText(err)
	}
	var d []scopeActions
	for _, sc := range scopes.Items {
		t := client.ActionTarget{ScopeID: sc.ID}
		panels, perr := s.panels(ctx, p, t)
		actions, err := s.src.Actions(ctx, t)
		errs := actionsErr(err)
		if perr != nil {
			errs = strings.TrimSpace(errs + "\n" + errText(perr))
		}
		d = append(d, scopeActions{Scope: sc, Panels: panels, Actions: unshown(actions, panels), Err: errs})
	}
	s.render(w, r, "scopes", p, d)
}

// flowScope is one scope's wiring as the config page draws it, in rows
// so a route and its binding sit level. Sources are its ingress extensions
// and "by hand"; each feeds every route, tried in order. Other Work names
// its binding itself, so it gets its own row, beside the bindings no
// route uses.
type flowScope struct {
	Scope    client.ConfigScope
	Rows     []flowRow
	Others   []client.ConfigExtension
	Unrouted []client.ConfigBinding
}

// flowRow is one row of the diagram: a source, a route, or both. First
// and Last mark where the line joining the sources to the routes ends.
type flowRow struct {
	Source      *flowSource
	Route       *client.Route
	Binding     *client.ConfigBinding
	First, Last bool
}

type flowSource struct {
	Name, Kind, Via string
}

type configData struct {
	Config client.Config
	Scopes []flowScope
}

func flow(sc client.ConfigScope) flowScope {
	f := flowScope{Scope: sc}
	var sources []flowSource
	for _, x := range sc.Extensions {
		if x.Ingress {
			sources = append(sources, flowSource{Name: x.Name, Kind: x.Type, Via: x.Connection})
		} else {
			f.Others = append(f.Others, x)
		}
	}
	sources = append(sources, flowSource{Name: "By hand", Kind: "actions and events from the web view, TUI or CLI"})
	used := map[string]bool{}
	for _, r := range sc.Routes {
		used[r.Binding] = true
	}
	for _, b := range sc.Bindings {
		if !used[b.Name] {
			f.Unrouted = append(f.Unrouted, b)
		}
	}
	n := max(len(sources), len(sc.Routes))
	for i := range n {
		row := flowRow{First: i == 0, Last: i == n-1}
		if i < len(sources) {
			row.Source = &sources[i]
		}
		if i < len(sc.Routes) {
			r := &sc.Routes[i]
			row.Route = r
			for j := range sc.Bindings {
				if sc.Bindings[j].Name == r.Binding {
					row.Binding = &sc.Bindings[j]
				}
			}
		}
		f.Rows = append(f.Rows, row)
	}
	return f
}

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Config", Nav: "config"}
	cfg, err := s.src.Config(ctx)
	switch {
	case tooOld(err):
		p.Err = "this daemon does not show its config; upgrade it to see it here"
	case err != nil:
		p.Err = errText(err)
	}
	d := configData{Config: cfg}
	for _, sc := range cfg.Scopes {
		d.Scopes = append(d.Scopes, flow(sc))
	}
	s.render(w, r, "config", p, d)
}

// back is where a change returns to: the page it was made on.
func back(r *http.Request) string {
	b := r.PostFormValue("back")
	if !strings.HasPrefix(b, "/") || strings.HasPrefix(b, "//") || strings.HasPrefix(b, "/\\") {
		return "/"
	}
	return b
}

func (s *Server) done(w http.ResponseWriter, r *http.Request, text string, err error) {
	if err != nil {
		flash(w, errText(err), true)
	} else {
		flash(w, text, false)
	}
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

const reason = "operator (web)"

// workControl runs one control against the Work's current revision.
func (s *Server) workControl(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	controls := map[string]func(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error){
		"retry":   s.src.RetryWork,
		"hold":    s.src.HoldWork,
		"release": s.src.ReleaseWork,
		"cancel":  s.src.CancelWork,
	}
	name := r.PathValue("control")
	fn, ok := controls[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	work, err := s.src.Work(ctx, r.PostFormValue("work"))
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	res, err := fn(ctx, work.ID, work.Revision, reason, s.opts.NewKey())
	s.done(w, r, name+" accepted (event "+res.EventID+")", err)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	run, err := s.src.Run(ctx, r.PostFormValue("run"))
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	res, err := s.src.CancelExecution(ctx, run.ID, run.Revision, reason, s.opts.NewKey())
	s.done(w, r, "execution cancel accepted (event "+res.EventID+")", err)
}

// invokeAction invokes an action an extension offers on a scope or Work,
// as the local operator.
func (s *Server) invokeAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	t := client.ActionTarget{ScopeID: r.PostFormValue("scope"), WorkID: r.PostFormValue("work")}
	if (t.ScopeID == "") == (t.WorkID == "") {
		http.Error(w, "an action needs a scope or Work", http.StatusBadRequest)
		return
	}
	label := r.PostFormValue("label")
	res, err := s.src.InvokeAction(ctx, t, r.PostFormValue("provider"), r.PostFormValue("action"), strings.TrimSpace(r.PostFormValue("input")), s.opts.NewKey())
	text := label + " accepted"
	if res.Message != "" {
		text = res.Message
	}
	s.done(w, r, text+" (event "+res.Event.ID+")", err)
}
