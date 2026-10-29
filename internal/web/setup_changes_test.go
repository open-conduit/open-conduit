package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

const sessionID = "setup/setup/abc"

// changeSetFake is an open session with three changes, laid out as the
// setup extension's `changes` panel lays them out: c1 and c3 open, c2
// approved, c4 rejected.
func changeSetFake(t *testing.T) (*fake, *Server) {
	t.Helper()
	f, s := setupFake(t)
	f.panels = map[string][]client.Panel{}
	f.work[sessionID] = client.Work{ID: sessionID, Kind: "setup.session", ScopeID: "setup", State: "waiting", Revision: "3", CreatedAt: at0(0), UpdatedAt: at0(0)}
	act := func(id, label, prompt string) client.Action {
		return client.Action{Provider: "setup", ID: id, Label: label, Prompt: prompt}
	}
	all, validate := act("approve_all_ab12cd34", "Approve all 2", ""), act("validate", "Validate the approved changes", "")
	approve1, reject1 := act("approve_c1", "Approve c1", ""), act("reject_c1", "Reject c1", "why (optional)")
	reject2 := act("reject_c2", "Reject c2", "why (optional)")
	approve3, reject3 := act("approve_c3", "Approve c3", ""), act("reject_c3", "Reject c3", "why (optional)")
	f.actions[sessionID] = []client.Action{act("message", "Send the agent a message", "your message"), all, approve1, reject1, reject2, approve3, reject3, validate}
	f.panels[sessionID] = []client.Panel{{Provider: "setup", ID: "changes", Title: "Proposed changes", Sections: []client.PanelSection{
		{Text: "Based on generation 1. Nothing changes until you apply the approved changes.", Actions: []client.Action{all, validate}},
		{Title: "All changes", Table: &client.PanelTable{Columns: []string{"Change", "Op", "Pointer", "State"}, Rows: [][]string{
			{"c1", "replace", "/scopes/setup/name", "proposed"},
			{"c2", "add", "/settings/reconcile_seconds", "approved"},
			{"c3", "add", "/scopes/setup/secret_refs/token", "proposed"},
			{"c4", "remove", "/scopes/old", "rejected"},
		}}},
		{Title: "c1: replace /scopes/setup/name", Fields: []client.PanelField{{Name: "State", Value: "proposed"}, {Name: "Why", Value: "a clearer name"}, {Name: "Before", Value: `"Setup"`}, {Name: "After", Value: `"Set up"`}}, Actions: []client.Action{approve1, reject1}},
		{Title: "c2: add /settings/reconcile_seconds", Fields: []client.PanelField{{Name: "State", Value: "approved"}, {Name: "Why", Value: "every ten minutes"}, {Name: "After", Value: "600"}}, Actions: []client.Action{reject2}},
		{Title: "c3: add /scopes/setup/secret_refs/token", Fields: []client.PanelField{{Name: "State", Value: "proposed"}, {Name: "Why", Value: "the token"}, {Name: "After", Value: `"keyring:github-token"`},
			{Name: "Problem", Value: "/scopes/setup/secret_refs/token: unused_secret: no entry selects it"}}, Actions: []client.Action{approve3, reject3}},
	}}}
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

func TestSetupShowsTheChangeSet(t *testing.T) {
	_, s := changeSetFake(t)
	body := changesOf(t, do(s.Handler(addr), "GET", "/setup", nil, s).Body.String())
	for _, want := range []string{
		"Based on generation 1",
		`id="change-c1"`, "replace <code>/scopes/setup/name</code>", "a clearer name",
		`<span class="diff-label">Before</span><pre>&#34;Setup&#34;</pre>`, `<span class="diff-label">After</span><pre>&#34;Set up&#34;</pre>`,
		`id="change-c2"`, `<pre>(nothing)</pre></div>`, `class="change cs-approved"`,
		"unused_secret: no entry selects it", "keyring:github-token",
		`name="action" value="approve_all_ab12cd34"`, ">Approve all 2</button>",
		`name="action" value="validate"`, `name="action" value="approve_c1"`, `name="action" value="reject_c1"`,
		`action="/setup/apply"`, "Apply 1 approved change", `name="work" value="setup/setup/abc"`,
		`<td>c4</td><td>remove</td><td><code>/scopes/old</code></td><td>rejected</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="approve_c2"`) || strings.Contains(body, `id="change-c4"`) {
		t.Fatalf("a decided change is offered for a decision:\n%s", body)
	}
	// A reason for rejecting is optional.
	i := strings.Index(body, `value="reject_c1"`)
	form := body[i : i+strings.Index(body[i:], "</form>")]
	if !strings.Contains(form, `placeholder="Why (optional)"`) || strings.Contains(form, "required") {
		t.Fatalf("reject form: %s", form)
	}
}

// The page holds no state: two loads of the same session draw the same
// proposals and controls.
func TestSetupChangeSetSurvivesAReload(t *testing.T) {
	_, s := changeSetFake(t)
	first := changesOf(t, do(s.Handler(addr), "GET", "/setup", nil, s).Body.String())
	second := changesOf(t, do(s.Handler(addr), "GET", "/setup?session="+url.QueryEscape(sessionID), nil, s).Body.String())
	if first != second {
		t.Fatalf("a reload drew a different change set:\n%s\n---\n%s", first, second)
	}
}

func TestSetupWithoutProposalsSaysSo(t *testing.T) {
	f, s := changeSetFake(t)
	f.panels[sessionID] = nil
	body := changesOf(t, do(s.Handler(addr), "GET", "/setup", nil, s).Body.String())
	if !strings.Contains(body, "hasn't proposed a change yet") || strings.Contains(body, "/setup/apply") {
		t.Fatalf("no proposals:\n%s", body)
	}
}

// The change set's Events are short lines of the conversation, and once a
// generation is applied the daemon's checks show beside it.
func TestSetupDrawsChangeSetEventsAndChecksAfterApply(t *testing.T) {
	f, s := changeSetFake(t)
	f.doctor = &client.Doctor{State: "warn", Checks: []client.Check{{Name: "config", Status: "ok", Code: "current", Detail: "generation 2 is active"}, {Name: "claude_code", Status: "warn", Detail: "not signed in"}}}
	f.events = []client.Event{
		{ID: "e0", Sequence: "1", WorkID: sessionID, Type: "setup.probe", OccurredAt: at0(0), Data: payloadOf("setup.probe.v1", map[string]any{"probe": "github.accounts", "command": "gh auth status", "why": "which logins you have"})},
		{ID: "e1", Sequence: "1", WorkID: sessionID, Type: "setup.proposal", OccurredAt: at0(0), Data: payloadOf("setup.proposal.v1", map[string]any{"change": "c1", "op": "replace", "pointer": "/scopes/setup/name", "why": "x", "diagnostics": []any{map[string]string{"path": "/x"}}})},
		{ID: "e2", Sequence: "2", WorkID: sessionID, Type: "setup.decision", OccurredAt: at0(0), Data: payloadOf("setup.decision.v1", map[string]any{"changes": []string{"c4"}, "decision": "rejected", "reason": "not yet"})},
		{ID: "e3", Sequence: "3", WorkID: sessionID, Type: "setup.decision", OccurredAt: at0(0), Data: payloadOf("setup.decision.v1", map[string]any{"changes": []string{"c3"}, "decision": "withdrawn"})},
		{ID: "e4", Sequence: "4", WorkID: sessionID, Type: "setup.checks", OccurredAt: at0(0), Data: payloadOf("setup.checks.v1", map[string]any{"source": "operator", "valid": true, "approved": 2})},
		{ID: "e5", Sequence: "5", RoutedWorkID: sessionID, Type: "setup.applied", OccurredAt: at0(0), Data: payloadOf("setup.applied.v1", map[string]any{"generation": "2", "hash": "h", "changes": []string{"c1", "c2"}})},
	}
	body := do(s.Handler(addr), "GET", "/setup", nil, s).Body.String()
	lines := []string{"The assistant ran gh auth status: which logins you have", "Proposed c1: replace /scopes/setup/name (1 problem)", "You rejected c4: not yet", "The assistant withdrew c3", "You validated the approved changes: 2 changes, valid", "Applied c1, c2 as generation 2"}
	last := -1
	for _, want := range lines {
		i := strings.Index(body, want)
		if i < last {
			t.Fatalf("missing or out of order %q:\n%s", want, body)
		}
		last = i
	}
	for _, want := range []string{"Checks after generation 2", "<b>claude_code</b><span>not signed in</span>", "generation 2 is active"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	f.events, f.doctorCalled = f.events[:4], 0
	do(s.Handler(addr), "GET", "/setup", nil, s)
	if f.doctorCalled != 0 {
		t.Fatal("doctor ran for a session with nothing applied")
	}
}

func TestSetupApply(t *testing.T) {
	f, s := changeSetFake(t)
	h := s.Handler(addr)
	form := url.Values{"csrf": {s.csrf}, "work": {sessionID}, "back": {"/setup?session=" + url.QueryEscape(sessionID)}}
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
		w := do(h, "POST", "/setup/apply", form, s)
		return &httpResult{code: w.Code, location: w.Header().Get("Location"), cookies: w.Result().Cookies()}
	}

	if r := post(); r.code != http.StatusSeeOther || !strings.Contains(flashOf(r), "nothing to apply") || len(f.applied) != 0 {
		t.Fatalf("no candidate: %+v %q", r, flashOf(r))
	}
	f.candidate = &client.ConfigCandidate{ConfigText: "{\n  \"version\": 1\n}\n", BaseGeneration: "1", CandidateHash: "abc"}
	r := post()
	if r.location != "/setup?session="+url.QueryEscape(sessionID) || !strings.Contains(flashOf(r), "generation 2 is active") {
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
	if w := do(h, "POST", "/setup/apply", url.Values{"work": {sessionID}}, s); w.Code != http.StatusForbidden {
		t.Fatalf("apply without the form token: %d", w.Code)
	}
}

type httpResult struct {
	code     int
	location string
	cookies  []*http.Cookie
}
