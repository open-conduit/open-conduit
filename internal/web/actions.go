package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// workCtx is what the Work page hands the "work-actions" template: the
// Work, the page (for the form token and where a change returns to) and
// the turn shown, which may be nil. workpage.go builds it.
type workCtx struct {
	W    workView
	Page *page
	Turn *turnInfo
}

// control is one operator control on a Work: available now, or why not.
// Eligibility mirrors the runtime's own rules (internal/runtime/operator.go
// and manual.go), so a control offered here is one the daemon accepts; the
// daemon still decides, with the Work's revision.
type control struct {
	ID          string
	Label       string
	Enabled     bool
	Why         string
	Destructive bool
}

// liveTurn is the Work's pending or running turn, parkedTurn its parked
// one.
func liveTurn(w workView) *turnInfo {
	for i := range w.Turns {
		if w.Turns[i].Live() {
			return &w.Turns[i]
		}
	}
	return nil
}

func parkedTurn(w workView) *turnInfo {
	for i := range w.Turns {
		if w.Turns[i].State == "parked" {
			return &w.Turns[i]
		}
	}
	return nil
}

// lastFailed is the Work's latest failed turn.
func lastFailed(w workView) *turnInfo {
	for i := len(w.Turns) - 1; i >= 0; i-- {
		if w.Turns[i].State == "failed" {
			return &w.Turns[i]
		}
	}
	return nil
}

// controls are the operator controls on w in the menu's order. pinned is
// the binding ready Work's input is stuck on (see pinnedBinding), or "".
func controls(w workView, pinned string) []control {
	live, parked := liveTurn(w), parkedTurn(w)
	terminal := !w.Open()
	last := w.Last()
	// RetryWork needs ready Work with retry state (a failure, or an
	// operator's cancellation of a turn) and every attempt ended.
	retryable := w.State == "ready" && live == nil && parked == nil && last != nil && (last.Retry != nil || last.State == "failed" || last.State == "cancelled")
	retry := control{ID: "retry", Label: "Run again", Enabled: retryable && pinned == ""}
	switch {
	case terminal:
		retry.Why = "Finished Work cannot be retried"
	case live != nil:
		retry.Why = "A turn is running"
	case pinned != "":
		retry.Why = "Rebind first: the pinned binding no longer exists"
	case !retryable:
		retry.Why = "Only Work that failed or was cancelled by an operator can be retried"
	}
	out := []control{retry}
	if pinned != "" {
		out = append(out, control{ID: "rebind", Label: "Rebind queued input", Enabled: w.State == "ready" && live == nil && parked == nil})
	}
	if w.Hold != nil {
		out = append(out, control{ID: "release", Label: "Release hold", Enabled: true})
	} else {
		h := control{ID: "hold", Label: "Hold", Enabled: !terminal && live == nil && w.State != "running"}
		switch {
		case terminal:
			h.Why = "Finished Work cannot be held"
		case !h.Enabled:
			h.Why = "Holds apply between turns; this one is running. Cancel the turn or wait."
		}
		out = append(out, h)
	}
	wake := control{ID: "wake", Label: "Send a wake event…", Enabled: w.State == "waiting" && w.Hold == nil}
	if w.State != "waiting" {
		wake.Why = "Only waiting Work has a wait to wake"
	} else if w.Hold != nil {
		wake.Why = "Release the hold first"
	}
	out = append(out, wake)
	attach := control{ID: "attach", Label: "Attach to session", Enabled: !terminal && len(w.Turns) > 0}
	if terminal {
		attach.Why = "Finished Work has no session to attach to"
	} else if len(w.Turns) == 0 {
		attach.Why = "No turn has run yet"
	}
	out = append(out, attach)
	ct := control{ID: "cancel-turn", Label: "Cancel this turn", Enabled: live != nil || parked != nil, Destructive: true}
	if !ct.Enabled {
		ct.Why = "No turn is running or parked"
	}
	out = append(out, ct)
	cw := control{ID: "cancel-work", Label: "Cancel Work", Enabled: !terminal, Destructive: true}
	if terminal {
		cw.Why = "Already finished"
	}
	return append(out, cw)
}

// controlOf is the control id among cs.
func controlOf(cs []control, id string) (control, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return control{}, false
}

// pinnedBinding is the binding ready Work's queued input is pinned to when
// the scope no longer has it (a reload removed it), so the Work cannot
// start until it is rebound. The API does not expose the queue's binding,
// so this reads the latest turn's: retry state and queued input keep the
// binding they were accepted under.
func pinnedBinding(w workView, bindings []client.Binding) string {
	last := w.Last()
	if w.State != "ready" || last == nil || last.ExecutorBinding == "" || bindings == nil || liveTurn(w) != nil || parkedTurn(w) != nil {
		return ""
	}
	for _, b := range bindings {
		if b.Name == last.ExecutorBinding {
			return ""
		}
	}
	return last.ExecutorBinding
}

// scopeBindings are the bindings of the active generation's scope id, from
// the last read of every Work when there was one, else read now. nil means
// they are not known.
func (s *Server) scopeBindings(ctx context.Context, id string) []client.Binding {
	s.wmu.Lock()
	scopes, read := s.wcache.Scopes, !s.wat.IsZero()
	s.wmu.Unlock()
	if !read {
		var err error
		if scopes, err = listAll(ctx, url.Values{"limit": {"200"}}, s.src.Scopes); err != nil {
			return nil
		}
	}
	for _, sc := range scopes {
		if sc.ID == id {
			if sc.Bindings == nil {
				return []client.Binding{}
			}
			return sc.Bindings
		}
	}
	return nil
}

// workName is how a dialog names the Work: its ref, else its title.
func workName(w workView) string {
	if w.Ref != "" {
		return w.Ref
	}
	return w.Title
}

// shortPath is an escaped ID shortened for the endpoint line.
func shortPath(id string) string {
	e := url.PathEscape(id)
	if len(e) > 18 {
		return e[:16] + "…"
	}
	return e
}

// workHref links to a Work, at a turn when turn > 0.
func workHref(id string, turn int) string {
	q := url.Values{"id": {id}}
	if turn > 0 {
		q.Set("turn", itoa(turn))
	}
	return "/work?" + q.Encode()
}

// confirmHref opens a control's confirmation: a dialog with script, the
// confirmation page without.
func confirmHref(action string, w workView, back string) string {
	return "/work/confirm?" + url.Values{"action": {action}, "id": {w.ID}, "back": {back}}.Encode()
}

// extHref opens an extension action offered on w from a menu.
func extHref(a client.Action, w workView, back string) string {
	return "/work/confirm?" + url.Values{"action": {"ext"}, "id": {w.ID}, "provider": {a.Provider}, "ext": {a.ID}, "back": {back}}.Encode()
}

// providerName is a provider's display name: its entry name, the last
// path segment.
func providerName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

// primaryControl is the Work page's primary action: the repair for Work
// that needs one, the release for held Work.
func primaryControl(w workView, pinned string) *control {
	cs := controls(w, pinned)
	if w.Disp.Kind == "repair" {
		for _, id := range []string{"retry", "rebind"} {
			if c, ok := controlOf(cs, id); ok && c.Enabled {
				return &c
			}
		}
	}
	if w.Hold != nil {
		c, _ := controlOf(cs, "release")
		return &c
	}
	return nil
}

// rowPrimary is a Runs row's inline action: Run again for a repair, Rebind
// for pinned input. Decisions link to the Work instead (the template).
func rowPrimary(w workView, pinned string) *control {
	cs := controls(w, pinned)
	if c, ok := controlOf(cs, "retry"); ok && c.Enabled && w.Disp.Kind == "repair" {
		c.Label = "Run again"
		return &c
	}
	if c, ok := controlOf(cs, "rebind"); ok && c.Enabled {
		c.Label = "Rebind"
		return &c
	}
	return nil
}

// actionFuncs are the template functions the action templates use.
func (s *Server) actionFuncs() template.FuncMap {
	bindings := func(scope string) []client.Binding {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		return s.scopeBindings(ctx, scope)
	}
	pinned := func(w workView) string { return pinnedBinding(w, bindings(w.ScopeID)) }
	return template.FuncMap{
		"controls":     func(w workView) []control { return controls(w, pinned(w)) },
		"primary":      func(w workView) *control { return primaryControl(w, pinned(w)) },
		"rowPrimary":   func(w workView) *control { return rowPrimary(w, pinned(w)) },
		"confirmHref":  confirmHref,
		"extHref":      extHref,
		"workHref":     workHref,
		"workName":     workName,
		"providerName": providerName,
		"workPanels":   workPanels,
		"actionIn":     actionIn,
		"unshownOn":    unshownOn,
		"paras":        paras,
		"parseTime":    parseTime,
		"dict":         dict,
		"failedTurn": func(w workView) int {
			if t := lastFailed(w); t != nil {
				return t.Number
			}
			return 0
		},
		"unchanged": func(w workView) time.Duration { return s.opts.Now().Sub(w.Disp.Since) },
	}
}

// dict builds a map for a template call from key, value pairs.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("dict takes key, value pairs")
	}
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, errors.New("dict keys are strings")
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// receipt is one intervention made from this view: what was done, the call
// made and the event the daemon accepted.
type receipt struct {
	At      time.Time
	Summary string
	Call    string
	Event   string
	WorkID  string
	Work    string
}

// receiptLog keeps this session's receipts in the process, newest first,
// at most maxReceipts; nothing is persisted. The view signs in one browser
// session per process, so the process's log is the session's.
type receiptLog struct {
	mu    sync.Mutex
	items []receipt
}

const maxReceipts = 50

func (l *receiptLog) add(r receipt) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append([]receipt{r}, l.items...)
	if len(l.items) > maxReceipts {
		l.items = l.items[:maxReceipts]
	}
}

func (l *receiptLog) list() []receipt {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]receipt(nil), l.items...)
}

// record finishes a change: on success it logs the receipt and shows the
// outcome with the call and event; on failure it shows the daemon's error.
// Either way it returns to the page the change was made on.
func (s *Server) record(w http.ResponseWriter, r *http.Request, text string, rc receipt, err error) {
	if err != nil {
		flashWith(w, flashNote{Text: errText(err), Call: rc.Call, Err: true})
	} else {
		rc.At = s.opts.Now()
		s.receipts.add(rc)
		flashWith(w, flashNote{Text: text, Call: rc.Call, Event: rc.Event})
	}
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// formReason is the reason the operator typed, else def.
func formReason(r *http.Request, def string) string {
	if v := strings.TrimSpace(r.PostFormValue("reason")); v != "" {
		return v
	}
	return def
}

// activeWait is waiting Work's active Wait, or nil.
func (s *Server) activeWait(ctx context.Context, work client.Work) *client.Wait {
	if work.State != "waiting" {
		return nil
	}
	ws, err := s.src.Waits(ctx, url.Values{"work_id": {work.ID}, "state": {"active"}})
	if err != nil {
		return nil
	}
	for i := range ws.Items {
		if ws.Items[i].State == "active" && ws.Items[i].WorkID == work.ID {
			return &ws.Items[i]
		}
	}
	return nil
}

// wakeKeys are the correlation keys a wake event can carry: the active
// Wait's, which are what it matches on, else the Work's own. Whoever set
// the Wait chose them, so the view names no source of its own.
func wakeKeys(work client.Work, wait *client.Wait) []client.Key {
	if wait != nil && len(wait.Match.Keys) > 0 {
		return wait.Match.Keys
	}
	return work.CorrelationKeys
}

// wakeKey is the key a wake event carries: the one at the form's index, or
// the first.
func wakeKey(work client.Work, wait *client.Wait, index string) (client.Key, error) {
	keys := wakeKeys(work, wait)
	i := 0
	if index != "" {
		n, err := strconv.Atoi(index)
		if err != nil || n < 0 || n >= len(keys) {
			return client.Key{}, errors.New("that correlation key is not one the Wait matches")
		}
		i = n
	}
	if len(keys) == 0 {
		return client.Key{}, errors.New("this Work has no correlation key for a wake event to carry")
	}
	return keys[i], nil
}

// workControl runs one control against the Work's current revision, read
// now, with a fresh Idempotency-Key.
func (s *Server) workControl(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	name := r.PathValue("control")
	simple := map[string]func(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error){
		"retry":   s.src.RetryWork,
		"hold":    s.src.HoldWork,
		"release": s.src.ReleaseWork,
		"cancel":  s.src.CancelWork,
	}
	if _, ok := simple[name]; !ok && name != "wake" && name != "rebind" {
		http.NotFound(w, r)
		return
	}
	work, err := s.src.Work(ctx, r.PostFormValue("work"))
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	v := newWork(work, nil, nil, s.opts.Now())
	label := workName(v)
	rc := receipt{WorkID: work.ID, Work: label, Call: "POST /v1/work/" + shortPath(work.ID) + "/" + name}
	var text string
	switch name {
	case "wake":
		typ := strings.TrimSpace(r.PostFormValue("type"))
		if typ == "" {
			s.done(w, r, "", errors.New("a wake event needs a type"))
			return
		}
		corr, err := wakeKey(work, s.activeWait(ctx, work), r.PostFormValue("key"))
		if err != nil {
			s.done(w, r, "", err)
			return
		}
		key := s.opts.NewKey()
		res, err := s.src.Wake(ctx, work.ID, client.WakeRequest{ExpectedRevision: work.Revision, Type: typ, DeliveryID: "web/" + key, CorrelationKey: &corr}, key)
		rc.Summary, rc.Event = "Wake event "+typ, res.Event.ID
		text = "Sent " + typ + " to " + label + "; it matched the active Wait, so the next turn is queued."
		if !res.Matched {
			text = "Sent " + typ + " to " + label + "; it did not match the active Wait, so it was recorded and nothing else happened."
		}
		s.record(w, r, text, rc, err)
		return
	case "rebind":
		binding := r.PostFormValue("binding")
		res, err := s.src.RebindWork(ctx, work.ID, work.Revision, binding, formReason(r, "binding removed by reload"), s.opts.NewKey())
		rc.Summary, rc.Event = "Rebind to "+binding, res.EventID
		s.record(w, r, "Rebound "+label+" to "+binding+".", rc, err)
		return
	}
	defaults := map[string]string{"hold": "Held from the web view", "cancel": "Cancelled from the web view"}
	reasonText := formReason(r, defaults[name])
	if reasonText == "" {
		reasonText = reason
	}
	res, err := simple[name](ctx, work.ID, work.Revision, reasonText, s.opts.NewKey())
	rc.Event = res.EventID
	switch name {
	case "retry":
		rc.Summary, text = "Run again", "Run again accepted for "+label+"; retry policy queues a new attempt."
	case "hold":
		rc.Summary, text = "Hold · “"+reasonText+"”", "Held "+label+". No turn starts until you release it."
	case "release":
		rc.Summary, text = "Release hold", "Released "+label+"."
	case "cancel":
		rc.Summary, text = "Cancel Work · “"+reasonText+"”", "Cancelled "+label+"."
	}
	s.record(w, r, text, rc, err)
}

// cancelRun cancels one turn at its current revision.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	run, err := s.src.Run(ctx, r.PostFormValue("run"))
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	res, err := s.src.CancelExecution(ctx, run.ID, run.Revision, formReason(r, reason), s.opts.NewKey())
	rc := receipt{Summary: "Cancel turn " + itoa(int(run.Attempt)), Call: "POST /v1/executions/" + shortPath(run.ID) + "/cancel", Event: res.EventID, WorkID: run.WorkID, Work: "turn " + itoa(int(run.Attempt))}
	s.record(w, r, "Cancelled turn "+itoa(int(run.Attempt))+". It is not retried automatically; run it again when you are ready.", rc, err)
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
	label, provider, id := r.PostFormValue("label"), r.PostFormValue("provider"), r.PostFormValue("action")
	// A form may send several input fields (a radio and checkboxes); the
	// action gets them in order, separated by spaces.
	_ = r.ParseForm()
	input := strings.TrimSpace(strings.Join(r.PostForm["input"], " "))
	res, err := s.src.InvokeAction(ctx, t, provider, id, input, s.opts.NewKey())
	path := "/v1/scopes/" + shortPath(t.ScopeID)
	if t.WorkID != "" {
		path = "/v1/work/" + shortPath(t.WorkID)
	}
	summary := label
	if input != "" {
		summary += " · “" + input + "”"
	}
	rc := receipt{Summary: summary, Call: "POST " + path + "/actions/" + url.PathEscape(provider) + "/" + url.PathEscape(id), Event: res.Event.ID, WorkID: t.WorkID}
	text := label + " accepted"
	if res.Message != "" {
		text = res.Message
	}
	s.record(w, r, text, rc, err)
}

// dialogView is a control's or extension action's confirmation: as a
// <dialog> fragment the page opens with script (Part), or as its own page.
type dialogView struct {
	Action string
	Title  string
	Tone   string
	W      workView
	Page   *page
	Back   string
	Part   bool
	// Control is the control confirmed; when it is not enabled the dialog
	// says why and offers nothing to submit.
	Control control
	// Call is the endpoint line: the call the confirmation makes.
	Call       string
	Live       *turnInfo
	Parked     *turnInfo
	Turn       *turnInfo
	LastFail   *turnInfo
	Latest     *turnInfo
	WaitTypes  []string
	WakeKeys   []client.Key
	Bindings   []client.Binding
	Pinned     string
	Generation string
	Ext        *actionView
	Err        string
}

// confirmPage confirms a control or an extension action: the whole page
// without script, or with part=1 just the dialog for the page to open.
func (s *Server) confirmPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	q := r.URL.Query()
	p := &page{Title: "Confirm", Nav: "runs"}
	d := dialogView{Action: q.Get("action"), Page: p, Back: q.Get("back"), Part: q.Get("part") == "1"}
	if !strings.HasPrefix(d.Back, "/") || strings.HasPrefix(d.Back, "//") || strings.HasPrefix(d.Back, "/\\") {
		d.Back = "/"
	}
	p.CSRF = s.csrf
	work, err := s.src.Work(ctx, q.Get("id"))
	if err != nil {
		if d.Part {
			http.Error(w, errText(err), statusOf(err))
			return
		}
		p.Err = errText(err)
		w.WriteHeader(statusOf(err))
		s.render(w, r, "confirm", p, d)
		return
	}
	runs, _ := listAll(ctx, url.Values{"work_id": {work.ID}, "limit": {"200"}}, s.src.Runs)
	wait := s.activeWait(ctx, work)
	if acts, err := s.src.Actions(ctx, client.ActionTarget{WorkID: work.ID}); err == nil {
		work.Actions = acts
	}
	v := newWork(work, runs, wait, s.opts.Now())
	d.W = v
	d.Live, d.Parked, d.LastFail, d.Latest = liveTurn(v), parkedTurn(v), lastFailed(v), v.Last()
	d.Turn = d.Live
	if d.Turn == nil {
		d.Turn = d.Parked
	}
	if wait != nil {
		d.WaitTypes = wait.Match.Types
	}
	d.WakeKeys = wakeKeys(work, wait)
	bindings := s.scopeBindings(ctx, v.ScopeID)
	d.Pinned = pinnedBinding(v, bindings)
	for _, b := range bindings {
		if b.Name != d.Pinned {
			d.Bindings = append(d.Bindings, b)
		}
	}
	if h, err := s.src.Health(ctx); err == nil {
		d.Generation = h.ConfigGeneration
	}
	name := workName(v)
	base := "POST /v1/work/" + shortPath(v.ID)
	switch d.Action {
	case "ext":
		for _, a := range v.Actions {
			if a.Provider == q.Get("provider") && a.ID == q.Get("ext") {
				av := actionView{Action: a, Page: p, WorkID: v.ID, Back: d.Back}
				d.Ext = &av
			}
		}
		if d.Ext == nil {
			d.Title, d.Err = "Action no longer offered", "The extension does not offer this action on "+name+" now."
		} else {
			d.Title, d.Call = d.Ext.Label, base+"/actions/"+url.PathEscape(d.Ext.Provider)+"/"+url.PathEscape(d.Ext.ID)
			d.Control = control{ID: "ext", Enabled: true}
		}
	default:
		titles := map[string]string{
			"hold":        "Hold " + name,
			"release":     "Release " + name,
			"retry":       "Run " + name + " again",
			"wake":        "Send a wake event to " + name,
			"rebind":      "Rebind " + name,
			"cancel-turn": "Cancel this turn of " + name,
			"cancel-work": "Cancel " + name + " for good",
			"attach":      "Attach to " + name + "'s session",
		}
		title, ok := titles[d.Action]
		if !ok {
			http.NotFound(w, r)
			return
		}
		d.Title = title
		d.Control, _ = controlOf(controls(v, d.Pinned), d.Action)
		switch d.Action {
		case "cancel-turn":
			if d.Turn != nil {
				d.Title = "Cancel turn " + itoa(d.Turn.Number) + " of " + name
				d.Call = "POST /v1/executions/" + shortPath(d.Turn.ID) + "/cancel"
			}
			d.Tone = "danger"
		case "cancel-work":
			d.Call, d.Tone = base+"/cancel", "danger"
		case "attach":
		default:
			d.Call = base + "/" + d.Action
		}
		if !d.Control.Enabled {
			d.Err = d.Control.Why
		}
	}
	if d.Part {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.pages["confirm"].ExecuteTemplate(w, "action-dialog", d); err != nil {
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
		return
	}
	p.Title = d.Title
	s.render(w, r, "confirm", p, d)
}
