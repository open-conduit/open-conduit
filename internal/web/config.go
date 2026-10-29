package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/open-conduit/open-conduit/client"
)

// The Configuration page: an overview of every scope, and for each scope
// its routing as a flow chart, its bindings and what extensions offer on
// it. It is read-only: the only changes it makes are a scope reconcile and
// the scope actions extensions offer. Tracing is a dry run the daemon
// records nothing for, so it travels in the query string.

// flowScope is one scope's routing as the flow chart draws it: its sources
// feeding route 1, each route level with its binding and executor, and the
// bindings no route uses.
type flowScope struct {
	Scope   client.ConfigScope
	Sources []flowSource
	Routes  []flowRoute
	// Others are the scope's extensions that bring no events in.
	Others   []client.ConfigExtension
	Unrouted []client.ConfigBinding
	// Trace is the dry run shown, if any; Unmatched says no route matched
	// it, so the path ends at "No route matched".
	Trace     *client.RouteTrace
	Unmatched bool
}

// flowSource is where events come from: an ingress extension, or by hand.
type flowSource struct {
	Name, Type, Via string
	Entries         []client.ConfigEntry
	// Hand marks the operator's own events and actions.
	Hand bool
}

// flowRoute is one route's row: the route, the edge with its busy policy,
// its binding (nil when the scope has none by that name) and the binding's
// executor and workspace.
type flowRoute struct {
	N       int
	Route   client.ConfigRoute
	Binding *client.ConfigBinding
	// Shared says an earlier route already sends to this binding.
	Shared bool
	// Model and Checkout are the binding's "model" executor setting and its
	// resource's "checkout" setting, when its types show them.
	Model, Checkout string
	// Step is the trace's verdict on this route, when tracing.
	Step *client.RouteStep
}

// Verdict is the step's verdict as a class name ("not-reached").
func (r flowRoute) Verdict() string {
	if r.Step == nil {
		return ""
	}
	return strings.ReplaceAll(r.Step.Verdict, "_", "-")
}

// VerdictText is the step's verdict in words.
func (r flowRoute) VerdictText() string {
	if r.Step == nil {
		return ""
	}
	switch r.Step.Verdict {
	case "match":
		return "Matches"
	case "skip":
		return "Skipped"
	case "not_reached":
		return "Not reached"
	case "bypassed":
		return "Bypassed"
	}
	return r.Step.Verdict
}

// Caps are what the binding requires, in the order the chart lists them.
func caps(r client.Requires) []string {
	var out []string
	for _, c := range []struct {
		on   bool
		name string
	}{{r.Inject, "inject"}, {r.Resume, "resume"}, {r.Interrupt, "interrupt"}, {r.Workspace, "workspace"}} {
		if c.on {
			out = append(out, c.name)
		}
	}
	return out
}

// policyWords is a route's while-running policy in words.
func policyWords(p string) string {
	switch p {
	case "inject":
		return "new events are delivered into the live session"
	case "queue":
		return "new events wait for the next turn"
	case "interrupt":
		return "new events stop the turn and start another"
	case "drop":
		return "new events are ignored"
	}
	return p
}

// setting is a shown setting's value as text, or "" when it is hidden or
// absent.
func setting(ss []client.ConfigSetting, name string) string {
	for _, s := range ss {
		if s.Name == name && s.Value != nil {
			return settingText(s)
		}
	}
	return ""
}

// settingText is a setting's value as the page writes it: lists joined,
// paths under the home folder with "~".
func settingText(s client.ConfigSetting) string {
	if s.Value == nil {
		if s.Set {
			return "set (not shown)"
		}
		return ""
	}
	var v string
	switch x := s.Value.(type) {
	case []any:
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = fmt.Sprint(p)
		}
		v = strings.Join(parts, ", ")
	case float64:
		v = strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	default:
		v = fmt.Sprint(x)
	}
	return tilde(v)
}

// tilde writes a path under the home folder from "~".
func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// flow lays out one scope. trace, when set, lights the path an event
// would take.
func flow(sc client.ConfigScope, trace *client.RouteTrace) flowScope {
	f := flowScope{Scope: sc, Trace: trace}
	for _, x := range sc.Extensions {
		if x.Ingress {
			f.Sources = append(f.Sources, flowSource{Name: x.Name, Type: x.Type, Via: x.Connection, Entries: x.Entries})
		} else {
			f.Others = append(f.Others, x)
		}
	}
	f.Sources = append(f.Sources, flowSource{Name: "By hand", Type: "actions and events from the web view, TUI or CLI", Hand: true})
	used := map[string]bool{}
	for i, r := range sc.Routes {
		row := flowRoute{N: i + 1, Route: r, Shared: used[r.Binding]}
		used[r.Binding] = true
		for j := range sc.Bindings {
			if b := &sc.Bindings[j]; b.Name == r.Binding {
				row.Binding = b
				row.Model = setting(b.Settings, "model")
				row.Checkout = setting(b.ResourceSettings, "checkout")
			}
		}
		if trace != nil {
			for k := range trace.Routes {
				if trace.Routes[k].Route == r.Name {
					row.Step = &trace.Routes[k]
				}
			}
		}
		f.Routes = append(f.Routes, row)
	}
	for _, b := range sc.Bindings {
		if !used[b.Name] {
			f.Unrouted = append(f.Unrouted, b)
		}
	}
	if trace != nil {
		f.Unmatched = trace.Effect.Kind == "unmatched"
	}
	return f
}

// scopeOffers is what extensions offer on a scope now: its panels and the
// actions no panel shows.
type scopeOffers struct {
	Panels  []panelView
	Actions []actionView
	// All are every action offered, for the overview's chips.
	All []client.Action
	Err string
}

// configScope is one scope as the page shows it.
type configScope struct {
	Flow     flowScope
	Open     int
	Revision string
	Offers   scopeOffers
}

// Kinds are the types of the scope's sources, for the scope nav.
func (c configScope) Kinds() string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Flow.Sources {
		if !s.Hand && !seen[s.Type] {
			seen[s.Type] = true
			out = append(out, sourceLabel(s.Type))
		}
	}
	if len(out) == 0 {
		return "by hand"
	}
	return strings.Join(out, ", ")
}

// Targets are the distinct bindings the scope's routes send to, in order,
// with their executors.
func (c configScope) Targets() []flowRoute {
	seen := map[string]bool{}
	var out []flowRoute
	for _, r := range c.Flow.Routes {
		if !seen[r.Route.Binding] {
			seen[r.Route.Binding] = true
			out = append(out, r)
		}
	}
	return out
}

type configData struct {
	Page   *page
	Config client.Config
	Scopes []configScope
	// Scope is the scope shown, or nil for the overview.
	Scope *configScope
	// Samples are the scope's trace samples; Tracing is false when the
	// daemon cannot trace, and the controls are absent.
	Samples []client.RouteSample
	Tracing bool
	Sample  *client.RouteSample
	// TraceErr is why the chosen trace failed.
	TraceErr string
	Pools    []client.CapacityPool
	Doctor   *client.Doctor
}

// Detailed says whether the daemon shows settings and ceilings, which an
// older daemon leaves out.
func (d configData) Detailed() bool {
	for _, sc := range d.Config.Scopes {
		a := sc.Authorization
		if len(a.Connections)+len(a.Operations)+len(a.ReadScopes) > 0 {
			return true
		}
		for _, b := range sc.Bindings {
			if b.Settings != nil || b.Operations != nil {
				return true
			}
		}
	}
	return false
}

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	q := r.URL.Query()
	p := &page{Title: "Configuration", Nav: "config"}
	cfg, err := s.src.Config(ctx)
	switch {
	case tooOld(err):
		p.Err = "this daemon does not show its config; upgrade it to see it here"
	case err != nil:
		p.Err = errText(err)
	}
	d := configData{Page: p, Config: cfg}
	open := map[string]int{}
	for _, w := range s.cachedWorld(ctx, false).Works {
		if w.Open() {
			open[w.ScopeID]++
		}
	}
	revisions := map[string]string{}
	if scopes, err := listAll(ctx, url.Values{"limit": {"200"}}, s.src.Scopes); err == nil {
		for _, sc := range scopes {
			revisions[sc.ID] = sc.Revision
		}
	}
	want := q.Get("scope")
	var trace *client.RouteTrace
	if want != "" {
		d.Samples, err = s.src.RouteSamples(ctx, want)
		d.Tracing = err == nil
		if id := q.Get("trace"); d.Tracing && id != "" && len(d.Samples) > 0 {
			d.Sample = &d.Samples[0]
			for i := range d.Samples {
				if d.Samples[i].ID == id {
					d.Sample = &d.Samples[i]
				}
			}
			t, err := s.src.TraceRoute(ctx, want, d.Sample.TraceEvent)
			if err != nil {
				d.TraceErr = errText(err)
			} else {
				trace = &t
			}
		}
	}
	for _, sc := range cfg.Scopes {
		var t *client.RouteTrace
		if sc.ID == want {
			t = trace
		}
		d.Scopes = append(d.Scopes, configScope{Flow: flow(sc, t), Open: open[sc.ID], Revision: revisions[sc.ID]})
	}
	s.offers(ctx, p, d.Scopes)
	for i := range d.Scopes {
		if d.Scopes[i].Flow.Scope.ID == want {
			d.Scope = &d.Scopes[i]
			p.Title = d.Scope.Flow.Scope.Name + " · Configuration"
		}
	}
	if want != "" && d.Scope == nil && p.Err == "" {
		p.Err = "no scope " + want + " in the active generation"
	}
	if d.Scope == nil {
		if c, err := s.src.Capacity(ctx); err == nil {
			d.Pools = c.Pools
		}
		if doc, err := s.src.Doctor(ctx, false); err == nil {
			d.Doctor = &doc
		}
	}
	s.render(w, r, "config", p, d)
}

// offers reads what extensions offer on each scope, concurrently.
func (s *Server) offers(ctx context.Context, p *page, scopes []configScope) {
	var wg sync.WaitGroup
	for i := range scopes {
		wg.Add(1)
		go func(c *configScope) {
			defer wg.Done()
			t := client.ActionTarget{ScopeID: c.Flow.Scope.ID}
			panels, perr := s.panels(ctx, p, t)
			actions, aerr := s.src.Actions(ctx, t)
			if tooOld(aerr) {
				aerr = nil
			}
			var errs []string
			for _, e := range []error{perr, aerr} {
				if e != nil {
					errs = append(errs, errText(e))
				}
			}
			c.Offers = scopeOffers{Panels: panels, All: actions, Err: strings.Join(errs, "\n")}
			for _, a := range unshown(actions, panels) {
				c.Offers.Actions = append(c.Offers.Actions, actionView{Action: a, Page: p, ScopeID: t.ScopeID})
			}
		}(&scopes[i])
	}
	wg.Wait()
}

// reconcileScope asks for an immediate reconcile of a scope's sources,
// against the scope's current revision.
func (s *Server) reconcileScope(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	id := r.PostFormValue("scope")
	scopes, err := listAll(ctx, url.Values{"limit": {"200"}}, s.src.Scopes)
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	rev := ""
	for _, sc := range scopes {
		if sc.ID == id {
			rev = sc.Revision
		}
	}
	if rev == "" {
		s.done(w, r, "", fmt.Errorf("no scope %s", id))
		return
	}
	res, err := s.src.ReconcileScope(ctx, id, rev, s.opts.NewKey())
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	text := "Reconcile requested"
	if len(res.Sources) > 0 {
		text += " for " + strings.Join(res.Sources, ", ")
	}
	flashWith(w, flashNote{Text: text, Call: "POST /v1/scopes/" + url.PathEscape(id) + "/reconcile · expected revision " + rev, Event: res.EventID})
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// Helpers the template calls.

// Caps are a binding's required capabilities.
func (configData) Caps(r client.Requires) []string { return caps(r) }

// Setting is a setting's value as text.
func (configData) Setting(s client.ConfigSetting) string { return settingText(s) }

// Policy is the route's busy policy in words.
func (r flowRoute) Policy() string { return policyWords(r.Route.ActivePolicy) }

// Caps are the route's binding's required capabilities.
func (r flowRoute) Caps() []string {
	if r.Binding == nil {
		return nil
	}
	return caps(r.Binding.Requires)
}

// EffectText is a trace's effect kind in words.
func (configData) EffectText(kind string) string {
	switch kind {
	case "start":
		return "Starts new Work"
	case "queue":
		return "Queued for the Work's next turn"
	case "inject":
		return "Delivered into the running turn"
	case "interrupt":
		return "Interrupts the running turn"
	case "wake":
		return "Wakes the waiting Work"
	case "drop":
		return "Dropped"
	case "unmatched":
		return "No route matched"
	}
	return kind
}

// Executors are the executors the scopes' bindings use, each with the
// bindings that use it, in first-use order.
func (d configData) Executors() []executorUse {
	var out []executorUse
	at := map[string]int{}
	for _, sc := range d.Config.Scopes {
		for _, b := range sc.Bindings {
			i, ok := at[b.Executor]
			if !ok {
				i = len(out)
				at[b.Executor] = i
				out = append(out, executorUse{Name: b.Executor})
			}
			out[i].Bindings = append(out[i].Bindings, sc.ID+"/"+b.Name)
		}
	}
	return out
}

type executorUse struct {
	Name     string
	Bindings []string
}

// CheckMark is the mark for a doctor check's status.
func (configData) CheckMark(status string) string {
	switch status {
	case "ok":
		return "completed"
	case "info":
		return "queued"
	case "warn", "warning":
		return "repair"
	}
	return "failed"
}

// SettingOf is the named setting's shown value.
func (configData) SettingOf(ss []client.ConfigSetting, name string) string { return setting(ss, name) }

// Tilde writes a path under the home folder from "~".
func (configData) Tilde(p string) string { return tilde(p) }

// Tracing says whether a trace is shown.
func (f flowScope) Tracing() bool { return f.Trace != nil }

// flowEdge is an edge between two nodes: lit on the traced path, with a
// label above and below and a title.
type flowEdge struct {
	Lit                bool
	Top, Bottom, Title string
}

// Edge makes an edge for the template.
func (flowScope) Edge(lit bool, top, bottom, title string) flowEdge {
	return flowEdge{lit, top, bottom, title}
}

// Provider is an action provider's display name: its entry name, the last
// segment of its path.
func (configData) Provider(p string) string { return p[strings.LastIndex(p, "/")+1:] }

// Flagged are the doctor checks that warn or fail; Passed counts the rest,
// which pass or only report.
func (d configData) Flagged() []client.Check {
	var out []client.Check
	if d.Doctor != nil {
		for _, c := range d.Doctor.Checks {
			if c.Status != "ok" && c.Status != "info" {
				out = append(out, c)
			}
		}
	}
	return out
}

// Passed counts the doctor checks that pass or only report.
func (d configData) Passed() int {
	if d.Doctor == nil {
		return 0
	}
	return len(d.Doctor.Checks) - len(d.Flagged())
}

// PassedText sums up the checks that pass: "All 24 checks pass", or with
// some flagged, "22 others pass" ("1 other passes").
func (d configData) PassedText() string {
	n := d.Passed()
	switch {
	case len(d.Flagged()) == 0 && n == 1:
		return "The one check passes"
	case len(d.Flagged()) == 0:
		return "All " + itoa(n) + " checks pass"
	case n == 1:
		return "1 other passes"
	}
	return itoa(n) + " others pass"
}
