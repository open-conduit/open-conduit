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
		"trigger":  runview.TriggerText,
		"outcome":  runview.Outcome,
		"time":     runview.ShortTime,
		"duration": func(r client.Run) string { return runview.Duration(r, s.opts.Now()) },
		"live":     runview.Running,
	}
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"list", "run", "output", "scopes"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+name+".html")
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

// row is one list row: a run, its Work's name and its place among the
// Work's turns.
type row struct {
	Run   client.Run
	Label string
	Turns string
}

type listData struct {
	Filter  string
	Filters []string
	PerTurn bool
	Rows    []row
	Works   int
	Runs    int
}

// rows are the latest turn of each Work with its turn count, or with
// perTurn every turn as "k/n" of its Work (counting the runs listed).
func rows(runs []client.Run, perTurn bool) []row {
	total := map[string]int{}
	for _, r := range runs {
		total[r.WorkID]++
	}
	seen := map[string]int{}
	var out []row
	for _, r := range runs {
		seen[r.WorkID]++
		n := total[r.WorkID]
		switch {
		case perTurn:
			out = append(out, row{Run: r, Turns: fmt.Sprintf("%d/%d", n-seen[r.WorkID]+1, n)})
		case seen[r.WorkID] == 1:
			out = append(out, row{Run: r, Turns: strconv.Itoa(n)})
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
	d.Rows = rows(runs.Items, d.PerTurn)
	for i := range d.Rows {
		d.Rows[i].Label = s.label(ctx, d.Rows[i].Run.WorkID)
	}
	s.render(w, r, "list", p, d)
}

type runData struct {
	Run      client.Run
	Detail   string
	Attempts []client.Run
	Work     *client.Work
	Waits    []client.Wait
	Actions  []client.Action
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
	if actions, err := s.src.Actions(ctx, client.ActionTarget{WorkID: run.WorkID}); err != nil {
		errs = append(errs, actionsErr(err))
	} else {
		d.Actions = actions
	}
	p.Err = strings.Join(errs, "\n")
	s.render(w, r, "run", p, d)
}

// actionsErr explains a daemon too old to list actions.
func actionsErr(err error) string {
	var e *client.Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == "not_found" {
		return "this daemon does not offer actions; upgrade it to answer and claim from here"
	}
	return errText(err)
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
		actions, err := s.src.Actions(ctx, client.ActionTarget{ScopeID: sc.ID})
		d = append(d, scopeActions{Scope: sc, Actions: actions, Err: actionsErr(err)})
	}
	s.render(w, r, "scopes", p, d)
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
