package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

var runsNow = time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return runsNow.Add(-d).Format(time.RFC3339) }

// runsWorld is a Work in every group, across two scopes and sources.
func runsWorld() world {
	mk := func(id, scope, src, state string, created, updated time.Duration, runs []client.Run, wait *client.Wait, actions []client.Action, hold *client.Hold) workView {
		w := client.Work{ID: id, ScopeID: scope, State: state, Kind: src + ".issue", Revision: "1", CreatedAt: ts(created), UpdatedAt: ts(updated), CorrelationKeys: []client.Key{{Source: src, ExternalID: id}}, Display: &client.Display{Title: "Title " + id, Ref: "#" + id}, Actions: actions, Hold: hold}
		for i := range runs {
			runs[i].WorkID, runs[i].Executor = id, "claude-code"
		}
		return newWork(w, runs, wait, runsNow)
	}
	turn := func(n uint32, state string, start, end time.Duration) client.Run {
		r := client.Run{}
		r.Attempt, r.State, r.StartedAt = n, state, ts(start)
		if end > 0 {
			r.FinishedAt = ts(end)
		}
		return r
	}
	failed := turn(1, "failed", 3*time.Hour, 2*time.Hour)
	failed.Retry = &client.RetryStatus{Exhausted: true}
	return world{
		Scopes: []client.Scope{{ID: "a", Name: "Alpha"}, {ID: "b", Name: "Beta"}},
		Works: []workView{
			mk("1", "a", "github", "waiting", 5*time.Hour, time.Hour, []client.Run{turn(1, "waiting", 5*time.Hour, 4*time.Hour)}, &client.Wait{CreatedAt: ts(4 * time.Hour), Match: client.Match{Types: []string{"human.answer"}}}, []client.Action{{Provider: "human", ID: "answer", Label: "Answer", Awaited: true}}, nil),
			mk("2", "a", "github", "running", 2*time.Hour, 10*time.Minute, []client.Run{turn(1, "running", 10*time.Minute, 0)}, nil, nil, nil),
			mk("3", "b", "slack", "waiting", 30*time.Hour, 20*time.Hour, []client.Run{turn(1, "waiting", 30*time.Hour, 29*time.Hour)}, &client.Wait{CreatedAt: ts(29 * time.Hour), Description: "a reply in the thread", Match: client.Match{Types: []string{"slack.message"}}}, nil, nil),
			mk("4", "b", "schedule", "waiting", 3*time.Hour, 3*time.Hour, []client.Run{turn(1, "waiting", 3*time.Hour, 3*time.Hour-time.Minute)}, &client.Wait{CreatedAt: ts(3 * time.Hour), TimeoutTimerID: "t", Description: "until Mon 09:00"}, nil, nil),
			mk("5", "a", "github", "ready", 4*time.Hour, 2*time.Hour, []client.Run{failed}, nil, nil, nil),
			mk("6", "b", "slack", "ready", time.Hour, 30*time.Minute, nil, nil, nil, &client.Hold{Reason: "freeze", At: ts(30 * time.Minute)}),
			mk("7", "a", "github", "completed", 48*time.Hour, 40*time.Hour, []client.Run{turn(1, "completed", 48*time.Hour, 47*time.Hour)}, nil, nil, nil),
		},
	}
}

// viewOf is runsView over a world holding closed Work too, as the page
// reads it: the open Work from the world, and the closed Work the facets
// leave (as the daemon's query would) with every closed Work's facets.
func viewOf(wd world, q url.Values) runsData {
	rq := parseRunsQuery(q)
	open := world{Scopes: wd.Scopes, Errs: wd.Errs}
	cl := closedSet{Want: closedStep, Facets: client.WorkFacets{}}
	for _, w := range wd.Works {
		if w.Open() {
			open.Works = append(open.Works, w)
			continue
		}
		cl.Facets.Total++
		cl.Facets.ScopeIDs = append(cl.Facets.ScopeIDs, w.ScopeID)
		cl.Facets.Sources = append(cl.Facets.Sources, w.Source)
		if rq.matches(w, runsNow) {
			cl.Matching++
			if rq.showsClosed() {
				cl.Rows = append(cl.Rows, w)
			}
		}
	}
	return runsView(open, cl, q, &page{}, runsNow, testLabel)
}

func ids(d runsData) string {
	var out []string
	for _, s := range d.Sections {
		for _, r := range s.Rows {
			out = append(out, r.W.ID)
		}
	}
	return strings.Join(out, ",")
}

func TestRunsFiltersSortsAndGroups(t *testing.T) {
	wd := runsWorld()
	for _, c := range []struct {
		name, query, want string
	}{
		{"open, by what needs me first", "", "1,5,2,3,4,6"},
		{"needs you", "g=attention", "1,5"},
		{"closed", "g=closed", "7"},
		{"every state", "g=all", "1,5,2,3,4,6,7"},
		{"waiting on slack", "g=waiting&w=slack", "3"},
		{"waiting on a time", "g=waiting&w=time", "4"},
		{"scope facet", "scope=b", "3,4,6"},
		{"source facet", "source=slack&g=all", "3,6"},
		{"changed in the last hour", "updated=1h", "1,2,6"},
		{"search matches every word", "q=reply+thread", "3"},
		{"search by ref", "q=%235", "5"},
		{"recently changed", "sort=updated&by=none", "2,6,1,5,4,3"},
		{"newest", "sort=created&by=none", "6,2,4,5,1,3"},
		{"longest in this state", "sort=waiting&by=none", "3,1,4,5,6,2"},
		{"by scope keeps the sort within", "by=scope", "1,5,2,3,4,6"},
	} {
		q, _ := url.ParseQuery(c.query)
		if got := ids(viewOf(wd, q)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	d := viewOf(wd, url.Values{"g": {"waiting"}})
	if d.Rail[0].N != 6 || d.Rail[1].N != 2 || !d.Rail[1].Signal || d.Rail[4].N != 2 || !d.Rail[4].On {
		t.Errorf("rail: %+v", d.Rail)
	}
	var sub []string
	for _, c := range d.Sub {
		sub = append(sub, c.Label)
	}
	// Equal counts sort by label.
	if strings.Join(sub, ",") != "Slack,a time" {
		t.Errorf("sub-rail: %v", sub)
	}
	d = viewOf(wd, url.Values{"by": {"scope"}})
	if len(d.Sections) != 2 || d.Sections[0].Label != "Alpha" || d.Sections[1].Label != "Beta" {
		t.Errorf("scope sections: %+v", d.Sections)
	}
	d = viewOf(wd, url.Values{"scope": {"a"}, "q": {"zzz"}})
	if d.Empty != "nomatch" || len(d.Tokens) != 2 || d.ClearHref != "/" {
		t.Errorf("filtered empty: %q %+v %s", d.Empty, d.Tokens, d.ClearHref)
	}
	if d := viewOf(world{}, nil); d.Empty != "nowork" {
		t.Errorf("no work: %q", d.Empty)
	}
}

func TestHoldable(t *testing.T) {
	wd := runsWorld()
	want := map[string]bool{"1": true, "2": false, "3": true, "4": true, "5": true, "6": false, "7": false}
	for _, w := range wd.Works {
		if holdable(w) != want[w.ID] {
			t.Errorf("%s: holdable %v", w.ID, holdable(w))
		}
	}
}

// runsFake is a fake daemon with open Work in several states.
func runsFake(t *testing.T) (*fake, *Server, http.Handler) {
	f, s, h := setup(t)
	f.work = map[string]client.Work{
		"w_ready":   {ID: "w_ready", State: "ready", Revision: "4", ScopeID: "scope_a", CreatedAt: ts(time.Hour), UpdatedAt: ts(time.Hour), Display: &client.Display{Title: "Ready one", Ref: "#1"}},
		"w_wait":    {ID: "w_wait", State: "waiting", Revision: "9", ScopeID: "scope_a", CreatedAt: ts(time.Hour), UpdatedAt: ts(time.Hour)},
		"w_running": {ID: "w_running", State: "running", Revision: "2", ScopeID: "scope_a", CreatedAt: ts(time.Hour), UpdatedAt: ts(time.Hour)},
		"w_held":    {ID: "w_held", State: "ready", Revision: "3", ScopeID: "scope_a", Hold: &client.Hold{Reason: "x"}, CreatedAt: ts(time.Hour), UpdatedAt: ts(time.Hour)},
		"w_done":    {ID: "w_done", State: "completed", Revision: "5", ScopeID: "scope_a", CreatedAt: ts(time.Hour), UpdatedAt: ts(time.Hour)},
	}
	r := run("exe_r", "w_running", "running", ts(time.Minute))
	r.StartedAt = ts(time.Minute)
	f.runs = []client.Run{r}
	return f, s, h
}

func TestRunsPage(t *testing.T) {
	f, s, h := runsFake(t)
	f.actions["w_wait"] = []client.Action{{Provider: "anything", ID: "go", Label: "Pick one", Awaited: true}}
	body := do(h, "GET", "/", nil, s).Body.String()
	for _, want := range []string{
		"5 pieces of work across 1 scope.",
		`<a href="/attention" class="sub-signal">1 need you</a>, 1 running now.`,
		`href="/work?id=w_ready"`,
		"Ready one",
		`<a class="btn btn-signal btn-xs" href="/work?id=w_wait">Decide</a>`,
		`data-row="0"`,
		`name="work" value="w_ready" form="bulk"`,
		`class="lifeline"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runs page lacks %q", want)
		}
	}
	if strings.Contains(body, "w_done") {
		t.Error("closed Work listed under Open")
	}
	if strings.Contains(body, "style=") {
		t.Error("inline style breaks the CSP")
	}
	if !strings.Contains(do(h, "GET", "/?g=closed", nil, s).Body.String(), "w_done") {
		t.Error("closed Work missing under Closed")
	}
	// The open list asks for runs, actions and panels in the list form.
	var with bool
	for _, c := range f.calls {
		with = with || c == "works+runs ready,running,waiting"
	}
	if !with {
		t.Errorf("calls: %v", f.calls)
	}
}

func TestRunsFreshInstallation(t *testing.T) {
	f, s, h := setup(t)
	f.work = map[string]client.Work{}
	f.runs = nil
	f.health = &client.Health{State: "ready", Version: "1", ConfigGeneration: "0"}
	f.pages = []client.ExtensionPage{{ID: "setup", Title: "Setup"}}
	if body := do(h, "GET", "/", nil, s).Body.String(); !strings.Contains(body, `<a class="btn btn-primary btn-lg" href="/pages/setup">Setup</a>`) {
		t.Fatal("fresh installation page not shown")
	}
	f.health = &client.Health{State: "ready", Version: "1", ConfigGeneration: "3"}
	if body := do(h, "GET", "/", nil, s).Body.String(); !strings.Contains(body, "No work yet") {
		t.Fatal("configured but empty should say no work yet")
	}
}

func TestBulkHoldAndRelease(t *testing.T) {
	f, s, h := runsFake(t)
	form := url.Values{"csrf": {s.csrf}, "back": {"/?g=all"}, "reason": {"freeze"}, "work": {"w_ready", "w_wait", "w_running", "w_held", "w_done"}}
	w := do(h, "POST", "/work/bulk/hold", form, s)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?g=all" {
		t.Fatalf("bulk hold: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := strings.Join(f.controls, ";"); got != "hold w_ready@4;hold w_wait@9" {
		t.Fatalf("holds: %s", got)
	}
	var note string
	for _, c := range w.Result().Cookies() {
		if c.Name == flashCookie {
			note, _ = url.QueryUnescape(c.Value)
		}
	}
	if !strings.Contains(note, "Held 2 of 5; 1 was running, 1 had finished, 1 was already held") {
		t.Fatalf("flash: %s", note)
	}
	f.controls = nil
	form.Set("work", "w_held")
	form.Add("work", "w_ready")
	do(h, "POST", "/work/bulk/release", form, s)
	if got := strings.Join(f.controls, ";"); got != "release w_held@3" {
		t.Fatalf("releases: %s", got)
	}
	if w := do(h, "POST", "/work/bulk/cancel", form, s); w.Code != http.StatusNotFound {
		t.Fatalf("bulk cancel: %d", w.Code)
	}
	if w := do(h, "POST", "/work/bulk/hold", url.Values{"work": {"w_ready"}}, s); w.Code != http.StatusForbidden {
		t.Fatalf("bulk without CSRF: %d", w.Code)
	}
}

// testLabel labels sources as an extension titled "Slack" would.
func testLabel(s string) string { return titleOf(map[string]string{"slack": "Slack"}, s) }

func TestSourceLabelFromTitles(t *testing.T) {
	f := newFake()
	f.titles = []client.ExtensionTitle{{Name: "acme", Title: "Acme Tickets"}}
	s := &Server{src: f, opts: Options{Now: func() time.Time { return runsNow }}}
	for in, want := range map[string]string{"acme": "Acme Tickets", "core": "Core", "other": "other"} {
		if got := s.sourceLabel(in); got != want {
			t.Errorf("sourceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Closed Work is read by the page's query: the default view counts it with
// one list of one item, and the Closed view reads a page of it with its
// runs and offers more, however much has closed (#544).
func TestRunsPageReadsClosedWorkByQuery(t *testing.T) {
	f, s, h := runsFake(t)
	for i := range 120 {
		id := fmt.Sprintf("w_old_%03d", i)
		f.work[id] = client.Work{ID: id, State: "completed", Revision: "1", ScopeID: "scope_b", Kind: "github.issue", CreatedAt: ts(time.Duration(200-i) * time.Hour), UpdatedAt: ts(time.Duration(200-i) * time.Hour)}
	}
	f.calls = nil
	body := do(h, "GET", "/", nil, s).Body.String()
	if !strings.Contains(body, `rail-n">121<`) || strings.Contains(body, "w_old_") {
		t.Errorf("default view: closed count or rows wrong")
	}
	if len(f.runReads) != 0 {
		t.Errorf("runs read one Work at a time: %v", f.runReads)
	}
	f.calls = nil
	body = do(h, "GET", "/?g=closed", nil, s).Body.String()
	if !strings.Contains(body, "w_old_119") || strings.Contains(body, "w_old_069") || !strings.Contains(body, "Show more") {
		t.Errorf("closed view should show the 50 latest closed and offer more")
	}
	var pages int
	for _, c := range f.calls {
		if c == "works+runs completed,failed,cancelled" {
			pages++
		}
	}
	if pages != 1 {
		t.Errorf("closed pages read %d, want 1: %v", pages, f.calls)
	}
	body = do(h, "GET", "/?g=closed&more=2", nil, s).Body.String()
	if !strings.Contains(body, "w_old_000") || strings.Contains(body, "Show more") {
		t.Errorf("more=2 should show every closed Work")
	}
	body = do(h, "GET", "/?g=closed&scope=scope_a", nil, s).Body.String()
	if !strings.Contains(body, "w_done") || strings.Contains(body, "w_old_") {
		t.Errorf("scope facet should reach the closed query")
	}
}
