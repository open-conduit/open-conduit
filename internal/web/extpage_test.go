package web

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// The page under test is a test package's, so nothing here leans on a
// first-party extension: "notes" declares a page whose sessions are
// notes.session Work, started by its entry's start action.
var notesPage = client.ExtensionPage{Extension: "notes", ID: "notes", Title: "Notes", Summary: "Take notes with an agent.", Entry: "notes", WorkKind: "notes.session", Start: "start", Compose: []string{"answer", "message"}, Route: "notes"}

const (
	notesHref = "/pages/notes"
	sessionID = "notes/desk/abc"
)

func pageFake(t *testing.T) (*fake, *Server) {
	t.Helper()
	f, s, _ := setup(t)
	f.work = map[string]client.Work{}
	f.panels = map[string][]client.Panel{}
	f.pages = []client.ExtensionPage{notesPage}
	f.config = &client.Config{Generation: "1", Scopes: []client.ConfigScope{
		{ID: "desk", Name: "Desk", Enabled: true, Extensions: []client.ConfigExtension{{Name: "jot", Type: "notes"}}},
		{ID: "repo", Name: "Repo", Enabled: true, Extensions: []client.ConfigExtension{{Name: "github", Type: "github"}}},
	}}
	return f, s
}

func get(t *testing.T, s *Server, target string) string {
	t.Helper()
	w := do(s.Handler(addr), "GET", target, nil, s)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d\n%s", target, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestPageIsInTheNavigationAndUnknownPagesAreNotFound(t *testing.T) {
	_, s := pageFake(t)
	body := get(t, s, "/")
	if !strings.Contains(body, `<a href="/pages/notes" class="nav-item">Notes</a>`) {
		t.Fatalf("no nav item:\n%s", body)
	}
	if body := get(t, s, notesHref); !strings.Contains(body, `<a href="/pages/notes" class="nav-item is-on" aria-current="page">Notes</a>`) {
		t.Fatalf("the page's nav item is not current:\n%s", body)
	}
	if w := do(s.Handler(addr), "GET", "/pages/other", nil, s); w.Code != http.StatusNotFound {
		t.Fatalf("unknown page: %d", w.Code)
	}
}

func TestPageOffersTheStartActionWithChoicesAndGrants(t *testing.T) {
	f, s := pageFake(t)
	f.config.Scopes[0].Routes = []client.ConfigRoute{{Route: client.Route{Name: "notes", Handler: "notes", Binding: "agent"}}}
	f.config.Scopes[0].Bindings = []client.ConfigBinding{{Name: "agent", Executor: "claude-code", Grants: []client.ConfigGrant{{Name: "shell", Label: "A shell on this machine"}, {Name: "connectors"}}}}
	f.actions["desk"] = []client.Action{
		{Provider: "desk/other", ID: "start", Label: "Not this one"},
		{Provider: "desk/jot", ID: "start", Label: "Start a notes session", Prompt: "the model", Choices: []string{"opus", "sonnet"}},
	}
	body := get(t, s, notesHref)
	for _, want := range []string{"Take notes with an agent.", `name="scope" value="desk"`, `name="provider" value="desk/jot"`, `name="action" value="start"`, `value="opus" required checked`, `value="sonnet" required>`,
		"Start a notes session", `name="back" value="/pages/notes?starting=1"`,
		`<input type="checkbox" name="input" value="+shell"><span>A shell on this machine</span>`, `value="+connectors"><span>connectors</span>`, "for this session only"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Not this one") {
		t.Fatal("another provider's start action is offered")
	}
	form := url.Values{"csrf": {s.csrf}, "scope": {"desk"}, "provider": {"desk/jot"}, "action": {"start"}, "input": {"sonnet", "+shell"}, "back": {"/pages/notes?starting=1"}}
	do(s.Handler(addr), "POST", "/actions", form, s)
	if len(f.invoked) != 1 || f.invoked[0].in != "sonnet +shell" {
		t.Fatalf("invoked %+v", f.invoked)
	}
	if body := get(t, s, notesHref+"?starting=1"); !strings.Contains(body, "Starting the session") || strings.Contains(body, `value="start"`) {
		t.Fatalf("a session just started is offered again:\n%s", body)
	}
}

func TestPageWithoutAScopeSaysWhatItNeeds(t *testing.T) {
	f, s := pageFake(t)
	f.config.Scopes = f.config.Scopes[1:]
	if body := get(t, s, notesHref); !strings.Contains(body, "a scope with a <code>notes</code> entry, and this configuration has none") {
		t.Fatalf("no explanation:\n%s", body)
	}
}

// session is an open session waiting on a question, drawn as the
// extension's conversation panel and one more panel.
func session(f *fake) {
	f.work[sessionID] = client.Work{ID: sessionID, Kind: "notes.session", ScopeID: "desk", State: "waiting", Revision: "3", CreatedAt: at0(0), UpdatedAt: at0(0)}
	f.panels[sessionID] = []client.Panel{
		{Provider: "desk/jot", ID: "session", Title: "Model and access", Sections: []client.PanelSection{{Fields: []client.PanelField{{Name: "Model", Value: "sonnet"}}}}},
		{Provider: "desk/jot", ID: "conversation", Title: "Conversation", Sections: []client.PanelSection{{Text: "It only talks here.", Messages: []client.PanelMessage{
			{From: "agent", Label: "Assistant", Text: "Claude Code is **signed in**."},
			{From: "operator", Text: "<b>hello</b>"},
			{From: "note", Label: "Probe", Text: "The agent ran gh auth status"},
			{From: "agent", Label: "Assistant asks", Text: "Which repository?", Choices: []string{"open-conduit", "dotfiles"}},
		}}}},
		{Provider: "desk/other", ID: "foreign", Title: "Someone else's", Sections: []client.PanelSection{{Text: "not on this page"}}},
	}
	f.actions[sessionID] = []client.Action{{Provider: "desk/jot", ID: "message", Label: "Send the agent a message", Prompt: "your message"}, {Provider: "desk/jot", ID: "answer", Label: "Answer the agent's question", Prompt: "a choice", Choices: []string{"open-conduit", "dotfiles"}}}
}

func TestPageDrawsTheSessionFromItsPanels(t *testing.T) {
	f, s := pageFake(t)
	session(f)
	body := get(t, s, notesHref)
	signed, hello, probe, asked := strings.Index(body, "<strong>signed in</strong>"), strings.Index(body, "&lt;b&gt;hello&lt;/b&gt;"), strings.Index(body, "Probe · "), strings.Index(body, "Which repository?")
	if signed < 0 || hello < signed || probe < hello || asked < probe {
		t.Fatalf("conversation out of order or unescaped (%d %d %d %d):\n%s", signed, hello, probe, asked, body)
	}
	for _, want := range []string{"It only talks here.", `name="work" value="notes/desk/abc"`, `name="action" value="answer"`, `<button class="choice" name="input" value="dotfiles">`, "Pick a choice above or write your own", "Desk · started", "End session",
		"Model and access", "<dt>Model</dt><dd>sonnet</dd>", `<textarea name="input" rows="1"`, `data-grow`, `/static/page.js`, `data-live="page-convo"`, `data-live="page-bench"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="start"`) || strings.Contains(body, "not on this page") {
		t.Fatalf("start or another provider's panel beside an open session:\n%s", body)
	}
	// End session opens a confirmation the confirm page knows.
	end := regexp.MustCompile(`href="([^"]+)"[^>]*>End session<`).FindStringSubmatch(body)
	if end == nil {
		t.Fatalf("no End session link:\n%s", body)
	}
	confirm := do(s.Handler(addr), "GET", html.UnescapeString(end[1])+"&part=1", nil, s)
	if confirm.Code != http.StatusOK || !strings.Contains(confirm.Body.String(), "for good") {
		t.Fatalf("End session confirmation: %d\n%s", confirm.Code, confirm.Body.String())
	}
	// The first compose action offered wins: one box, and it answers.
	if n := strings.Count(body, "<textarea"); n != 1 || strings.Contains(body, `name="action" value="message"`) || !strings.Contains(body, `data-live="page-send"`) {
		t.Fatalf("want one box that answers, got %d boxes:\n%s", n, body)
	}
	// The message box is outside every live region.
	if strings.Contains(body, `data-live="page"`) {
		t.Fatal("the whole page is one live region beside an open session")
	}

	// Answered, the question shows the choice taken and the box messages.
	msgs := f.panels[sessionID][1].Sections[0].Messages
	msgs[3].Chosen = "dotfiles"
	f.panels[sessionID][1].Sections[0].Messages = append(msgs, client.PanelMessage{From: "operator", Text: "dotfiles"})
	f.actions[sessionID] = f.actions[sessionID][:1]
	w := f.work[sessionID]
	w.State = "running"
	f.work[sessionID] = w
	s.cachedWorld(t.Context(), true)
	body = get(t, s, notesHref)
	if !strings.Contains(body, `class="choice is-on" disabled>dotfiles`) || strings.Contains(body, `value="answer"`) || !strings.Contains(body, `name="action" value="message"`) {
		t.Fatalf("answered question:\n%s", body)
	}
}

// candidateFake is an open session with four changes: c1 and c3 open, c2
// approved, c4 rejected.
func candidateFake(t *testing.T) (*fake, *Server) {
	t.Helper()
	f, s := pageFake(t)
	f.work[sessionID] = client.Work{ID: sessionID, Kind: "notes.session", ScopeID: "desk", State: "waiting", Revision: "3", CreatedAt: at0(0), UpdatedAt: at0(0)}
	act := func(id, label, prompt string) client.Action {
		return client.Action{Provider: "desk/jot", ID: id, Label: label, Prompt: prompt}
	}
	f.actions[sessionID] = []client.Action{act("message", "Send the agent a message", "your message"), act("approve_all_ab12cd34", "Approve all 2", ""),
		act("approve_c1", "Approve c1", ""), act("reject_c1", "Reject c1", "why (optional)"), act("reject_c2", "Reject c2", "why (optional)"),
		act("approve_c3", "Approve c3", ""), act("reject_c3", "Reject c3", "why (optional)"), act("validate", "Validate the approved changes", "")}
	f.candidate = &client.ConfigCandidate{BaseGeneration: "1", ApproveAll: "approve_all_ab12cd34", Validate: "validate", Changes: []client.CandidateChange{
		{ID: "c1", Op: "replace", Pointer: "/scopes/desk/name", State: "proposed", Why: "a clearer name", Before: json.RawMessage(`"Desk"`), After: json.RawMessage(`"Front desk"`), Approve: "approve_c1", Reject: "reject_c1"},
		{ID: "c2", Op: "add", Pointer: "/settings/reconcile_seconds", State: "approved", Why: "every ten minutes", After: json.RawMessage(`600`), Reject: "reject_c2"},
		{ID: "c3", Op: "add", Pointer: "/scopes/desk/secret_refs/token", State: "proposed", Why: "the token", After: json.RawMessage(`"keyring:github-token"`),
			Problems: []string{"/scopes/desk/secret_refs/token: unused_secret: no entry selects it"}, Approve: "approve_c3", Reject: "reject_c3"},
		{ID: "c4", Op: "remove", Pointer: "/scopes/old", State: "rejected"},
	}}
	return f, s
}

// changesOf is the Changes part of the session's bench.
func changesOf(t *testing.T, body string) string {
	t.Helper()
	start, end := strings.Index(body, `<h2 class="section-rule">Changes</h2>`), strings.Index(body, `<div class="row-gap"><a class="btn btn-quiet"`)
	if start < 0 || end < start {
		t.Fatalf("no Changes section:\n%s", body)
	}
	return body[start:end]
}

func TestPageReviewsTheCandidate(t *testing.T) {
	f, s := candidateFake(t)
	body := changesOf(t, get(t, s, notesHref))
	for _, want := range []string{
		"Based on generation 1",
		`id="change-c1"`, "replace <code>/scopes/desk/name</code>", "a clearer name",
		`<span class="diff-label">Before</span><pre>&#34;Desk&#34;</pre>`, `<span class="diff-label">After</span><pre>&#34;Front desk&#34;</pre>`,
		`id="change-c2"`, `<pre>(nothing)</pre></div>`, `class="change cs-approved"`,
		"unused_secret: no entry selects it", "keyring:github-token",
		`name="action" value="approve_all_ab12cd34"`, ">Approve all 2</button>",
		`name="action" value="validate"`, `name="action" value="approve_c1"`, `name="action" value="reject_c1"`,
		"don't validate, so there is nothing to apply yet",
		`<td>c4</td><td>remove</td><td><code>/scopes/old</code></td><td>rejected</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="approve_c2"`) || strings.Contains(body, `id="change-c4"`) || strings.Contains(body, "/pages/apply") {
		t.Fatalf("a decided change is offered for a decision, or apply without a candidate:\n%s", body)
	}
	// A reason for rejecting is optional.
	i := strings.Index(body, `value="reject_c1"`)
	form := body[i : i+strings.Index(body[i:], "</form>")]
	if !strings.Contains(form, `placeholder="Why (optional)"`) || strings.Contains(form, "required") {
		t.Fatalf("reject form: %s", form)
	}
	// With a candidate to apply, Apply is core's button.
	f.candidate.ConfigText, f.candidate.CandidateHash = "{}\n", "abc"
	body = changesOf(t, get(t, s, notesHref))
	for _, want := range []string{`action="/pages/apply"`, "Apply 1 approved change", `name="work" value="notes/desk/abc"`, `name="back" value="/pages/notes?session=notes%2Fdesk%2Fabc"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	// A control the session no longer lists is not drawn.
	f.actions[sessionID] = f.actions[sessionID][:1]
	if body := changesOf(t, get(t, s, notesHref)); strings.Contains(body, `value="approve_c1"`) || strings.Contains(body, `value="validate"`) {
		t.Fatalf("unlisted controls drawn:\n%s", body)
	}
}

// The page holds no state: two loads of the same session draw the same
// review.
func TestPageReviewSurvivesAReload(t *testing.T) {
	_, s := candidateFake(t)
	first := changesOf(t, get(t, s, notesHref))
	second := changesOf(t, get(t, s, notesHref+"?session="+url.QueryEscape(sessionID)))
	if first != second {
		t.Fatalf("a reload drew a different review:\n%s\n---\n%s", first, second)
	}
}

func TestPageWithoutACandidateHasNoReview(t *testing.T) {
	f, s := candidateFake(t)
	f.candidate = nil
	if body := get(t, s, notesHref); strings.Contains(body, `<h2 class="section-rule">Changes</h2>`) {
		t.Fatalf("a review with nothing proposed:\n%s", body)
	}
}

// Once a change is applied the daemon's checks show beside the review.
func TestPageShowsChecksOnceAChangeIsApplied(t *testing.T) {
	f, s := candidateFake(t)
	f.doctor = &client.Doctor{State: "warn", Checks: []client.Check{{Name: "config", Status: "ok", Detail: "generation 2 is active"}, {Name: "claude_code", Status: "warn", Detail: "not signed in"}}}
	get(t, s, notesHref)
	if f.doctorCalled != 0 {
		t.Fatal("doctor ran for a session with nothing applied")
	}
	f.candidate.Changes[1].State = "applied"
	body := get(t, s, notesHref)
	for _, want := range []string{"Checks on generation", "<b>claude_code</b><span>not signed in</span>", "generation 2 is active"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
}

func TestPageApply(t *testing.T) {
	f, s := candidateFake(t)
	h := s.Handler(addr)
	back := notesHref + "?session=" + url.QueryEscape(sessionID)
	form := url.Values{"csrf": {s.csrf}, "work": {sessionID}, "back": {back}}
	flashOf := func(w *httpResult) string {
		for _, c := range w.cookies {
			if c.Name == flashCookie {
				v, _ := url.QueryUnescape(c.Value)
				return v
			}
		}
		return ""
	}
	post := func() *httpResult {
		w := do(h, "POST", "/pages/apply", form, s)
		return &httpResult{code: w.Code, location: w.Header().Get("Location"), cookies: w.Result().Cookies()}
	}

	if r := post(); r.code != http.StatusSeeOther || !strings.Contains(flashOf(r), "nothing to apply") || len(f.applied) != 0 {
		t.Fatalf("nothing approved: %+v %q", r, flashOf(r))
	}
	f.candidate.ConfigText, f.candidate.CandidateHash = "{\n  \"version\": 1\n}\n", "abc"
	r := post()
	if r.location != back || !strings.Contains(flashOf(r), "generation 2 is active") {
		t.Fatalf("apply: %+v %q", r, flashOf(r))
	}
	if len(f.applied) != 1 || f.applied[0] != "{\n  \"version\": 1\n}\n|abc|1" {
		t.Fatalf("applied: %q", f.applied)
	}
	f.applyErr = &client.Error{Status: 409, Code: "generation_conflict", Message: "config generation conflict"}
	if r := post(); !strings.Contains(flashOf(r), "configuration changed since these changes were based on it") {
		t.Fatalf("conflict: %q", flashOf(r))
	}
	f.applyErr = &client.Error{Status: 422, Code: "invalid_candidate", Message: "the approved changes don't validate"}
	if r := post(); !strings.Contains(flashOf(r), "Not applied: the approved changes don't validate (invalid_candidate)") {
		t.Fatalf("invalid: %q", flashOf(r))
	}
	if w := do(h, "POST", "/pages/apply", url.Values{"work": {sessionID}}, s); w.Code != http.StatusForbidden {
		t.Fatalf("apply without the form token: %d", w.Code)
	}
}

type httpResult struct {
	code     int
	location string
	cookies  []*http.Cookie
}
