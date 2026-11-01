package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/open-conduit/open-conduit/client"
)

// Extension pages (issue #242): a page an extension declares
// (client.ExtensionPage) is drawn here from what every extension already
// has, so nothing in this file names one. With no session open, the page
// offers its Start action on a scope with its entry, and the grants of its
// route's binding to turn on. With one open, the session Work's panels are
// the page: a panel with messages is the conversation and the rest stand
// beside it, with one message box sending the first of its Compose actions
// offered. When the session proposes configuration, core reviews it: each
// change's before and after with the actions that decide it, and Apply,
// which only the operator does.

const (
	pagePastSessions = 5
	// Change states the review draws differently from the rest.
	changeProposed = "proposed"
	changeApproved = "approved"
	changeApplied  = "applied"
	codeConflict   = "generation_conflict"
	applyCall      = "POST /v1/config/apply"
)

type extPageData struct {
	P client.ExtensionPage
	// Scopes are the scopes with the page's entry. Start is the start
	// action one of them offers when no session is open, in StartScope.
	Scopes     []entryScope
	Start      *actionView
	StartScope string
	// StartGrants are what the start scope's route binding offers a
	// session, for the operator to turn on.
	StartGrants []client.ConfigGrant
	// Starting says a session was just started and its Work is not there
	// yet: the route creates it when it takes the start Event.
	Starting bool
	Session  *extSession
	// Past are recently ended sessions.
	Past []workView
}

// entryScope is a scope with the page's entry, by the entry's name.
type entryScope struct {
	client.ConfigScope
	Entry string
}

// extSession is an open (or asked for) session: its Work, its panels and
// what the operator can send it.
type extSession struct {
	W     workView
	Scope string
	// Lead and Msgs are the conversation panel's; Panels are the others.
	Lead   string
	Msgs   []client.PanelMessage
	Panels []panelView
	// Compose is the action the message box sends.
	Compose *actionView
	Changes *changeReview
	// Doctor is the daemon's checks once a change was applied.
	Doctor *client.Doctor
}

// CheckMark is the mark for a doctor check's status.
func (*extSession) CheckMark(status string) string { return configData{}.CheckMark(status) }

// Busy is what the session is doing while the operator waits for it, or "".
func (s *extSession) Busy() string {
	switch s.W.Disp.Kind {
	case "running":
		return "The agent is working"
	case "queued":
		return "Waiting for an agent slot"
	}
	return ""
}

// changeReview is a session's change set: every change, the ones still
// to decide or apply, and the controls that act on all of them.
type changeReview struct {
	BaseGeneration string
	All            []client.CandidateChange
	Open           []changeView
	Approved       int
	// ApproveAll and Validate are offered when the session lists them;
	// Apply when the approved changes make a candidate to apply, and
	// Invalid says why not when some are approved.
	ApproveAll, Validate *actionView
	Apply, Invalid       bool
	Applied              bool
}

// changeView is a change waiting on the operator, or approved and not yet
// applied: its before and after side by side, with its controls.
type changeView struct {
	client.CandidateChange
	Before, After       string
	HasBefore, HasAfter bool
	Approve, Reject     *actionView
}

// Approved says the change is approved and waiting to be applied.
func (c changeView) Approved() bool { return c.State == changeApproved }

// pageHref is where a page lives.
func pageHref(id string) string { return "/pages/" + url.PathEscape(id) }

// sessionHref is a page showing one session.
func sessionHref(id, work string) string {
	return pageHref(id) + "?" + url.Values{"session": {work}}.Encode()
}

// extensionPages are the pages extensions contribute; a daemon too old to
// list them has none.
func (s *Server) extensionPages(ctx context.Context) []client.ExtensionPage {
	ps, err := s.src.Pages(ctx)
	if err != nil {
		return nil
	}
	return ps
}

// extPage is an extension's page.
func (s *Server) extPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	id := r.PathValue("id")
	var pg *client.ExtensionPage
	pages, err := s.src.Pages(ctx)
	for i := range pages {
		if pages[i].ID == id {
			pg = &pages[i]
		}
	}
	if pg == nil && err == nil {
		http.NotFound(w, r)
		return
	}
	p := &page{Title: id, Nav: "page:" + id}
	d := extPageData{}
	if err != nil {
		p.Err = errText(err)
		s.render(w, r, "page", p, d)
		return
	}
	p.Title, d.P = pg.Title, *pg
	cfg, err := s.src.Config(ctx)
	if err != nil && !tooOld(err) {
		p.Err = errText(err)
	}
	for _, sc := range cfg.Scopes {
		for _, x := range sc.Extensions {
			if x.Type == pg.Entry {
				d.Scopes = append(d.Scopes, entryScope{ConfigScope: sc, Entry: x.Name})
				break
			}
		}
	}
	if len(d.Scopes) > 0 {
		starting := r.URL.Query().Get("starting") != ""
		err := s.pageSessions(ctx, p, r.URL.Query().Get("session"), starting, &d)
		if d.Session == nil && starting {
			d.Start, d.Starting = nil, true
		}
		if err != nil && p.Err == "" {
			p.Err = errText(err)
		}
	}
	s.render(w, r, "page", p, d)
}

// offers reports whether an action is the page's entry's in scope: listed
// by the entry, or by the package that declares the page.
func (d *extPageData) offers(a client.Action, scope string) bool {
	name := providerName(a.Provider)
	if name == d.P.Extension {
		return true
	}
	for _, sc := range d.Scopes {
		if sc.ID == scope && sc.Entry == name {
			return true
		}
	}
	return false
}

// action is the entry's action id among actions, if offered.
func (d *extPageData) action(actions []client.Action, scope, id string) (client.Action, bool) {
	i := slices.IndexFunc(actions, func(a client.Action) bool { return a.ID == id && d.offers(a, scope) })
	if i < 0 {
		return client.Action{}, false
	}
	return actions[i], true
}

// pageSessions finds the open session (or the one asked for) and reads
// it, and otherwise the scope offering a new one. Fresh reads the Work
// again, as just after a start.
func (s *Server) pageSessions(ctx context.Context, p *page, want string, fresh bool, d *extPageData) error {
	in := map[string]bool{}
	for _, sc := range d.Scopes {
		in[sc.ID] = true
	}
	var open *workView
	for _, v := range s.cachedWorld(ctx, fresh).Works {
		if v.Kind != d.P.WorkKind || !in[v.ScopeID] {
			continue
		}
		if v.ID == want || want == "" && v.Disp.Group != "closed" && (open == nil || v.Updated.After(open.Updated)) {
			open = &v
		}
		if v.Disp.Group == "closed" && v.ID != want {
			d.Past = append(d.Past, v)
		}
	}
	slices.SortFunc(d.Past, func(a, b workView) int { return b.Updated.Compare(a.Updated) })
	d.Past = d.Past[:min(len(d.Past), pagePastSessions)]
	if open != nil {
		return s.pageSession(ctx, p, *open, d)
	}
	for _, sc := range d.Scopes {
		actions, err := s.src.Actions(ctx, client.ActionTarget{ScopeID: sc.ID})
		if err != nil {
			if tooOld(err) {
				continue
			}
			return err
		}
		if a, ok := d.action(actions, sc.ID, d.P.Start); ok {
			back := pageHref(d.P.ID) + "?starting=1"
			d.Start, d.StartScope = &actionView{Action: a, Page: p, ScopeID: sc.ID, Back: back}, sc.Name
			d.StartGrants = routeGrants(sc.ConfigScope, d.P.Route)
			return nil
		}
	}
	return nil
}

func (s *Server) pageSession(ctx context.Context, p *page, v workView, d *extPageData) error {
	ss := &extSession{W: v, Scope: v.ScopeID}
	for _, sc := range d.Scopes {
		if sc.ID == v.ScopeID && sc.Name != "" {
			ss.Scope = sc.Name
		}
	}
	d.Session = ss
	back := sessionHref(d.P.ID, v.ID)
	av := func(a client.Action) *actionView { return &actionView{Action: a, Page: p, WorkID: v.ID, Back: back} }
	var actions []client.Action
	if v.Disp.Group != "closed" {
		var err error
		if actions, err = s.src.Actions(ctx, client.ActionTarget{WorkID: v.ID}); err != nil && !tooOld(err) {
			return err
		}
	}
	panels, err := s.src.Panels(ctx, client.ActionTarget{WorkID: v.ID})
	if err != nil && !tooOld(err) {
		return err
	}
	for _, x := range panels {
		if !d.offers(client.Action{Provider: x.Provider}, v.ScopeID) {
			continue
		}
		if i := slices.IndexFunc(x.Sections, func(sec client.PanelSection) bool { return len(sec.Messages) > 0 }); i >= 0 && ss.Msgs == nil {
			ss.Lead, ss.Msgs = x.Sections[i].Text, x.Sections[i].Messages
			continue
		}
		ss.Panels = append(ss.Panels, panelView{Panel: x, Page: p, WorkID: v.ID})
	}
	for _, id := range d.P.Compose {
		if a, ok := d.action(actions, v.ScopeID, id); ok {
			ss.Compose = av(a)
			break
		}
	}
	cand, err := s.src.WorkConfigCandidate(ctx, v.ID)
	var ce *client.Error
	switch {
	case err == nil:
		ss.Changes = review(cand, func(id string) *actionView {
			if id == "" {
				return nil
			}
			if a, ok := d.action(actions, v.ScopeID, id); ok {
				return av(a)
			}
			return nil
		})
	case errors.As(err, &ce) && ce.Status == http.StatusNotFound, tooOld(err):
	default:
		return err
	}
	if ss.Changes != nil && ss.Changes.Applied {
		if doc, err := s.src.Doctor(ctx, false); err == nil {
			ss.Doctor = &doc
		}
	}
	return nil
}

// review lays out a candidate's changes, with the controls the session
// offers now.
func review(c client.ConfigCandidate, act func(id string) *actionView) *changeReview {
	if len(c.Changes) == 0 {
		return nil
	}
	cr := &changeReview{BaseGeneration: c.BaseGeneration, All: c.Changes, ApproveAll: act(c.ApproveAll), Validate: act(c.Validate), Apply: c.CandidateHash != ""}
	for _, x := range c.Changes {
		switch x.State {
		case changeApplied:
			cr.Applied = true
			continue
		case changeApproved:
			cr.Approved++
		case changeProposed:
		default:
			continue
		}
		cv := changeView{CandidateChange: x, Approve: act(x.Approve), Reject: act(x.Reject)}
		cv.Before, cv.HasBefore = jsonText(x.Before)
		cv.After, cv.HasAfter = jsonText(x.After)
		cr.Open = append(cr.Open, cv)
	}
	cr.Invalid = cr.Approved > 0 && !cr.Apply
	return cr
}

// jsonText is a JSON value indented to read, or false when there is none.
func jsonText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return string(raw), true
	}
	return b.String(), true
}

// routeGrants are the grants the binding of the scope's first route with
// the handler offers.
func routeGrants(sc client.ConfigScope, handler string) []client.ConfigGrant {
	if handler == "" {
		return nil
	}
	for _, r := range sc.Routes {
		if r.Handler != handler {
			continue
		}
		for _, b := range sc.Bindings {
			if b.Name == r.Binding {
				return b.Grants
			}
		}
		return nil
	}
	return nil
}

// applyChanges applies a session's approved changes as the operator: it
// reads the session's candidate (the base text plus the approved changes,
// with the generation it is based on and its hash) and applies it with that
// expected generation, so a configuration that moved meanwhile is refused
// rather than overwritten.
func (s *Server) applyChanges(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	work := r.PostFormValue("work")
	if work == "" {
		http.Error(w, "apply needs a session", http.StatusBadRequest)
		return
	}
	rc := receipt{Summary: "Apply the approved changes", Call: applyCall, WorkID: work}
	applied, err := s.applyCandidate(ctx, work)
	if err != nil {
		s.record(w, r, "", rc, errors.New(applyErr(err)))
		return
	}
	s.record(w, r, "Applied: generation "+applied.Generation+" is active. The agent hears it next.", rc, nil)
}

// errNothingToApply refuses apply while nothing approved makes a candidate.
var errNothingToApply = &client.Error{Status: http.StatusNotFound, Code: "not_found"}

func (s *Server) applyCandidate(ctx context.Context, work string) (client.ConfigApplied, error) {
	c, err := s.src.WorkConfigCandidate(ctx, work)
	if err != nil {
		return client.ConfigApplied{}, err
	}
	if c.CandidateHash == "" {
		return client.ConfigApplied{}, errNothingToApply
	}
	return s.src.ApplyConfigText(ctx, []byte(c.ConfigText), c.CandidateHash, c.BaseGeneration, s.opts.NewKey())
}

// applyErr says why apply was refused.
func applyErr(err error) string {
	var e *client.Error
	switch {
	case errors.As(err, &e) && e.Code == codeConflict:
		return "The configuration changed since these changes were based on it, so nothing was applied. Reload, review the changes again and apply."
	case errors.As(err, &e) && e.Status == http.StatusNotFound:
		return "This session has nothing to apply: approve a change, and validate the approved changes to see what stops them."
	case errors.As(err, &e) && e.Message != "":
		return "Not applied: " + e.Message + " (" + e.Code + ")"
	}
	return "Not applied: " + errText(err)
}

// navPage is a page in the navigation.
type navPage struct{ ID, Title, Href string }

// navPages are the extension pages for the navigation, cached briefly:
// every page draws them.
func (s *Server) navPages(ctx context.Context) []navPage {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	now := s.opts.Now()
	if s.pat.IsZero() || now.Sub(s.pat) > worldTTL || now.Before(s.pat) {
		s.pcache = nil
		for _, p := range s.extensionPages(ctx) {
			s.pcache = append(s.pcache, navPage{ID: p.ID, Title: p.Title, Href: pageHref(p.ID)})
		}
		s.pat = now
	}
	return s.pcache
}
