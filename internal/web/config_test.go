package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

func cfgRoute(name, handler, binding, policy string) client.ConfigRoute {
	return client.ConfigRoute{Route: client.Route{Name: name, Handler: handler, Binding: binding, ActivePolicy: policy}}
}

// ghScope is a GitHub scope shaped like the daemon's: hold-then-issues
// routes, an agent binding with a git resource, and a hold binding.
func ghScope() client.ConfigScope {
	return client.ConfigScope{
		ID: "open-conduit", Name: "open-conduit/open-conduit", Enabled: true,
		Authorization: client.ConfigAuthorization{Connections: []string{"github"}, Operations: []string{"github.pr.create", "wait"}},
		Extensions: []client.ConfigExtension{
			{Name: "open-conduit", Type: "github", Connection: "github", Ingress: true, Entries: []client.ConfigEntry{{Name: "issues", Detail: "labelled agent"}}},
			{Name: "decisions", Type: "human"},
		},
		Routes: []client.ConfigRoute{cfgRoute("held", "github-hold", "hold", "queue"), cfgRoute("issues", "github", "agent", "inject")},
		Bindings: []client.ConfigBinding{
			{Name: "agent", Executor: "claude-code", Connection: "github", Requires: client.Requires{Inject: true, Resume: true, Workspace: true, Toolbelt: "v1"}, Operations: []string{"github.pr.create", "wait"},
				Settings: []client.ConfigSetting{{Name: "model", Value: "opus"}, {Name: "token_file", Set: true}}, Resource: "git", ResourceSettings: []client.ConfigSetting{{Name: "checkout", Value: "/srv/checkout"}}},
			{Name: "hold", Executor: "github-hold", Connection: "github"},
		},
	}
}

func TestFlowRows(t *testing.T) {
	shared := ghScope()
	shared.Routes = append(shared.Routes, cfgRoute("comments", "github", "agent", "queue"))
	missing := ghScope()
	missing.Routes = []client.ConfigRoute{cfgRoute("lost", "github", "gone", "queue")}
	noIngress := ghScope()
	noIngress.Extensions = nil
	one := ghScope()
	one.Routes = one.Routes[1:]
	for _, c := range []struct {
		name     string
		sc       client.ConfigScope
		routes   int
		sources  int
		shared   []bool
		missing  []bool
		unrouted []string
		model    string
	}{
		{"one route", one, 1, 2, []bool{false}, []bool{false}, []string{"hold"}, "opus"},
		{"two routes", ghScope(), 2, 2, []bool{false, false}, []bool{false, false}, nil, ""},
		{"routes sharing a binding", shared, 3, 2, []bool{false, false, true}, []bool{false, false, false}, nil, ""},
		{"no ingress source", noIngress, 2, 1, []bool{false, false}, []bool{false, false}, nil, ""},
		{"a missing binding", missing, 1, 2, []bool{false}, []bool{true}, []string{"agent", "hold"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := flow(c.sc, nil)
			if len(f.Routes) != c.routes || len(f.Sources) != c.sources || !f.Sources[len(f.Sources)-1].Hand {
				t.Fatalf("routes %d sources %+v", len(f.Routes), f.Sources)
			}
			for i, r := range f.Routes {
				if r.N != i+1 || r.Shared != c.shared[i] || (r.Binding == nil) != c.missing[i] {
					t.Errorf("route %d: %+v", i, r)
				}
			}
			var un []string
			for _, b := range f.Unrouted {
				un = append(un, b.Name)
			}
			if strings.Join(un, ",") != strings.Join(c.unrouted, ",") {
				t.Errorf("unrouted %v", un)
			}
			if c.model != "" && (f.Routes[0].Model != c.model || f.Routes[0].Checkout != "/srv/checkout") {
				t.Errorf("model %q checkout %q", f.Routes[0].Model, f.Routes[0].Checkout)
			}
			if len(c.sc.Extensions) > 0 && (len(f.Others) != 1 || f.Others[0].Name != "decisions") {
				t.Errorf("others %+v", f.Others)
			}
		})
	}
}

func configFake(t *testing.T) (*fake, *Server, http.Handler) {
	f, s, h := setup(t)
	f.config = &client.Config{Generation: "4", Hash: "sha256:abc", Connections: []client.ConfigConnection{{ID: "github", Type: "github"}}, Scopes: []client.ConfigScope{ghScope()}}
	f.scopes = []client.Scope{{ID: "open-conduit", Name: "open-conduit/open-conduit", Revision: "9"}}
	return f, s, h
}

// The flow chart is emitted in grid order with nothing positioned inline.
func TestConfigPagesRenderTheFlowInGridOrder(t *testing.T) {
	_, s, h := configFake(t)
	w := do(h, "GET", "/config", nil, s)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "How work flows through this installation") || !strings.Contains(body, `href="/config?scope=open-conduit"`) {
		t.Fatalf("overview %d:\n%s", w.Code, body)
	}
	w = do(h, "GET", "/config?scope=open-conduit", nil, s)
	body = w.Body.String()
	if w.Code != 200 {
		t.Fatalf("scope %d:\n%s", w.Code, body)
	}
	for _, want := range []string{"handler github-hold", "if busy: inject", "model opus", "worktree of /srv/checkout", "No route matched", `id="binding-agent"`, "token_file: set (not shown)", "github.pr.create"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "style=") {
		t.Error("inline style in the page")
	}
	// No samples on this daemon: tracing is absent, not disabled.
	if strings.Contains(body, "Trace an event") {
		t.Error("trace offered without samples endpoint")
	}
	// Each route row holds seven cells and its fall-through row seven more.
	flowHTML := body[strings.Index(body, `<div class="flow`):]
	rows := regexp.MustCompile(`(?s)<div class="flow-row[^"]*" role="listitem">(.*?)\n  </div>`).FindAllStringSubmatch(flowHTML, -1)
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
}

func TestConfigTraceLightsThePath(t *testing.T) {
	f, s, h := configFake(t)
	f.samples = map[string][]client.RouteSample{"open-conduit": {
		{ID: "new-issue", Title: "A new issue is opened", Route: "issues", Handler: "github", TraceEvent: client.TraceEvent{Type: "github.issue.open", Source: "github"}},
		{ID: "merge", Title: "A blocker merges", Route: "held", Handler: "github-hold", TraceEvent: client.TraceEvent{Type: "github.pull_request.merged"}},
	}}
	f.trace = &client.RouteTrace{ScopeID: "open-conduit", Generation: "4",
		Routes: []client.RouteStep{{Route: "held", Verdict: "skip", Reason: "no hold names it"}, {Route: "issues", Verdict: "match", Reason: "issue labelled agent"}},
		Effect: client.RouteEffect{Kind: "start", Binding: "agent", Detail: "starts Work on agent"}}
	body := do(h, "GET", "/config?scope=open-conduit", nil, s).Body.String()
	if !strings.Contains(body, "Trace an event") || strings.Contains(body, "is-tracing") {
		t.Fatal("trace control missing, or tracing without a choice")
	}
	body = do(h, "GET", "/config?scope=open-conduit&trace=new-issue", nil, s).Body.String()
	for _, want := range []string{`class="flow is-tracing"`, `flow-row v-skip`, `flow-row v-match`, "Skipped — no hold names it", "Matches — issue labelled agent", `<div class="fall is-lit"`, "Starts new Work", "starts Work on agent", "Stop tracing", `<option value="new-issue" selected>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `node-none is-lit`) {
		t.Error("unmatched lit for a match")
	}
	if len(f.traced) != 1 || f.traced[0] != "open-conduit github.issue.open" {
		t.Errorf("traced %v", f.traced)
	}
	f.trace = &client.RouteTrace{Routes: []client.RouteStep{{Route: "held", Verdict: "skip"}, {Route: "issues", Verdict: "skip"}}, Effect: client.RouteEffect{Kind: "unmatched", Detail: "recorded as unmatched"}}
	body = do(h, "GET", "/config?scope=open-conduit&trace=merge", nil, s).Body.String()
	if !strings.Contains(body, `node-none is-lit`) || !strings.Contains(body, "No route matched</b>") {
		t.Error("unmatched not lit")
	}
}

func TestConfigScopeActionsPostWithTheScope(t *testing.T) {
	f, s, h := configFake(t)
	f.actions["open-conduit"] = []client.Action{{Provider: "inventor", ID: "rescan", Label: "Rescan everything"}}
	body := do(h, "GET", "/config?scope=open-conduit", nil, s).Body.String()
	if !strings.Contains(body, `name="scope" value="open-conduit"`) || !strings.Contains(body, "Rescan everything") {
		t.Fatalf("scope action not offered:\n%s", body)
	}
	w := do(h, "POST", "/actions", url.Values{"csrf": {s.csrf}, "scope": {"open-conduit"}, "provider": {"inventor"}, "action": {"rescan"}, "back": {"/config?scope=open-conduit"}}, s)
	if w.Code != http.StatusSeeOther || len(f.invoked) != 1 || f.invoked[0].target != (client.ActionTarget{ScopeID: "open-conduit"}) {
		t.Fatalf("invoke: %d %+v", w.Code, f.invoked)
	}
}

func TestReconcileSendsTheScopeRevision(t *testing.T) {
	f, s, h := configFake(t)
	w := do(h, "POST", "/scopes/reconcile", url.Values{"csrf": {s.csrf}, "scope": {"open-conduit"}, "back": {"/config?scope=open-conduit"}}, s)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/config?scope=open-conduit" {
		t.Fatalf("reconcile: %d %s", w.Code, w.Header().Get("Location"))
	}
	if len(f.controls) != 1 || f.controls[0] != "reconcile open-conduit@9" {
		t.Fatalf("controls %v", f.controls)
	}
	if w := do(h, "POST", "/scopes/reconcile", url.Values{"scope": {"open-conduit"}}, s); w.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", w.Code)
	}
}

func TestConfigWithoutAGeneration(t *testing.T) {
	f, s, h := configFake(t)
	f.config.Generation = ""
	if body := do(h, "GET", "/config", nil, s).Body.String(); !strings.Contains(body, "No configuration yet") || !strings.Contains(body, `href="/setup"`) {
		t.Fatal("no fresh state")
	}
}

// The doctor list shows what warns or fails; the rest are summed up.
func TestDoctorSummary(t *testing.T) {
	checks := func(statuses ...string) configData {
		d := &client.Doctor{}
		for i, s := range statuses {
			d.Checks = append(d.Checks, client.Check{Name: itoa(i), Status: s})
		}
		return configData{Doctor: d}
	}
	for _, c := range []struct {
		d       configData
		flagged int
		text    string
	}{
		{checks("ok", "info", "ok"), 0, "All 3 checks pass"},
		{checks("ok"), 0, "The one check passes"},
		{checks("warn", "ok"), 1, "1 other passes"},
		{checks("fail", "warn", "ok", "info"), 2, "2 others pass"},
	} {
		if got := len(c.d.Flagged()); got != c.flagged || c.d.PassedText() != c.text {
			t.Errorf("%d flagged, %q; want %d, %q", got, c.d.PassedText(), c.flagged, c.text)
		}
	}
}
