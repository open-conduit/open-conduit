package web

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

var testNow = time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)

func boundRun(work string, n uint32, state, binding string) client.Run {
	r := run("work/"+work+"/attempt/"+itoa(int(n)), work, state, "2026-10-28T01:00:00Z")
	r.Attempt, r.ExecutorBinding = n, binding
	return r
}

func view(state string, hold bool, runs ...client.Run) workView {
	w := client.Work{ID: "w1", State: state, Revision: "4", ScopeID: "sc", CreatedAt: "2026-10-28T00:00:00Z"}
	if hold {
		w.Hold = &client.Hold{Reason: "busy", At: "2026-10-28T02:00:00Z"}
	}
	return newWork(w, runs, nil, testNow)
}

func enabled(cs []control) map[string]bool {
	out := map[string]bool{}
	for _, c := range cs {
		out[c.ID] = c.Enabled
		if !c.Enabled && c.Why == "" {
			out[c.ID+"!nowhy"] = true
		}
	}
	return out
}

func TestControlsFollowTheRuntimeRules(t *testing.T) {
	failed := boundRun("w1", 1, "failed", "agent")
	failed.Retry = &client.RetryStatus{FailureCount: 3, Exhausted: true}
	cases := []struct {
		name string
		w    workView
		on   []string
		off  []string
	}{
		// HoldWork refuses running Work; a running turn can be cancelled.
		{"running", view("running", false, boundRun("w1", 1, "running", "agent")), []string{"cancel-turn", "cancel-work", "attach"}, []string{"hold", "retry", "wake"}},
		// RetryWork needs ready Work with retry state and every attempt ended.
		{"failed, retry exhausted", view("ready", false, failed), []string{"retry", "hold", "cancel-work"}, []string{"cancel-turn", "wake"}},
		{"operator-cancelled turn", view("ready", false, boundRun("w1", 1, "cancelled", "agent")), []string{"retry"}, nil},
		{"queued after a completed turn", view("ready", false, boundRun("w1", 1, "completed", "agent")), []string{"hold"}, []string{"retry"}},
		// A parked attempt may be held, not retried, and can be cancelled.
		{"parked", view("waiting", false, boundRun("w1", 1, "parked", "agent")), []string{"hold", "wake", "cancel-turn"}, []string{"retry"}},
		// Wake only while waiting, and not while held.
		{"waiting and held", view("waiting", true, boundRun("w1", 1, "completed", "agent")), []string{"release"}, []string{"wake"}},
		{"closed", view("completed", false, boundRun("w1", 1, "completed", "agent")), nil, []string{"retry", "hold", "wake", "attach", "cancel-turn", "cancel-work"}},
		{"no turn yet", view("ready", false), []string{"hold"}, []string{"attach", "retry"}},
	}
	for _, c := range cases {
		got := enabled(controls(c.w, ""))
		for _, id := range c.on {
			if !got[id] {
				t.Errorf("%s: %s should be offered: %v", c.name, id, got)
			}
		}
		for _, id := range c.off {
			if got[id] {
				t.Errorf("%s: %s should not be offered", c.name, id)
			}
		}
		for k := range got {
			if strings.HasSuffix(k, "!nowhy") {
				t.Errorf("%s: %s is unavailable without saying why", c.name, k)
			}
		}
	}
}

func TestRebindOnlyForReadyWorkWhoseBindingIsGone(t *testing.T) {
	bindings := []client.Binding{{Name: "agent-opus", Executor: "claude-code"}}
	ready := view("ready", false, boundRun("w1", 1, "failed", "agent-sonnet"))
	if got := pinnedBinding(ready, bindings); got != "agent-sonnet" {
		t.Fatalf("pinned: %q", got)
	}
	cs := enabled(controls(ready, "agent-sonnet"))
	if !cs["rebind"] || cs["retry"] {
		t.Fatalf("rebind before retry: %v", cs)
	}
	if got := pinnedBinding(view("ready", false, boundRun("w1", 1, "failed", "agent-opus")), bindings); got != "" {
		t.Fatalf("binding still there: %q", got)
	}
	if got := pinnedBinding(view("waiting", false, boundRun("w1", 1, "parked", "agent-sonnet")), bindings); got != "" {
		t.Fatalf("parked Work cannot be rebound: %q", got)
	}
	if got := pinnedBinding(ready, nil); got != "" {
		t.Fatalf("unknown bindings: %q", got)
	}
}

func TestControlsSendRevisionKeyReasonAndChoices(t *testing.T) {
	f, s, h := setup(t)
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "waiting", Revision: "7", ScopeID: "scope_a", CorrelationKeys: []client.Key{{Source: "github", ExternalID: "issue/12"}}}
	post := func(path string, form url.Values) *http.Response {
		form.Set("csrf", s.csrf)
		form.Set("work", "wrk_1")
		w := do(h, "POST", path, form, s)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Result()
	}
	post("/work/hold", url.Values{"reason": {"Waiting on a product decision"}})
	post("/work/cancel", url.Values{"reason": {"  "}})
	post("/work/wake", url.Values{"type": {"github.check_suite.completed"}})
	post("/work/rebind", url.Values{"binding": {"agent-opus"}})
	want := "hold wrk_1@7,cancel wrk_1@7,wake:github.check_suite.completed wrk_1@7 github/issue/12,rebind:agent-opus wrk_1@7"
	if strings.Join(f.controls, ",") != want {
		t.Fatalf("controls: %v", f.controls)
	}
	if strings.Join(f.reasons, "|") != "Waiting on a product decision|Cancelled from the web view" {
		t.Fatalf("reasons: %q", f.reasons)
	}
	// A wake without a type is refused before it reaches the daemon.
	f.controls = nil
	w := do(h, "POST", "/work/wake", url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}}, s)
	if w.Code != http.StatusSeeOther || len(f.controls) != 0 {
		t.Fatalf("typeless wake: %d %v", w.Code, f.controls)
	}
	// Each change is this session's receipt, newest first, with its call.
	rs := s.receipts.list()
	if len(rs) != 4 || rs[0].Call != "POST /v1/work/wrk_1/rebind" || rs[0].Event != "evt_rebind:agent-opus" || rs[3].Summary != "Hold · “Waiting on a product decision”" {
		t.Fatalf("receipts: %+v", rs)
	}
}

func TestReceiptsAreBounded(t *testing.T) {
	var l receiptLog
	for i := range maxReceipts + 5 {
		l.add(receipt{Summary: itoa(i)})
	}
	if got := l.list(); len(got) != maxReceipts || got[0].Summary != itoa(maxReceipts+4) {
		t.Fatalf("receipts: %d, newest %q", len(got), got[0].Summary)
	}
}

func TestConfirmationPageAndDialog(t *testing.T) {
	f, s, h := setup(t)
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "ready", Revision: "7", ScopeID: "scope_a", Display: &client.Display{Ref: "#88", Title: "Fix the flaky test"}}
	failed := run("exe_9", "wrk_1", "failed", "2026-10-28T01:00:00Z")
	failed.Attempt, failed.Outcome = 3, &client.Outcome{Kind: "failed", Summary: "dial tcp: lookup api.github.com: no such host"}
	failed.Retry = &client.RetryStatus{FailureCount: 3, Exhausted: true}
	f.runs = []client.Run{failed}

	// Without script: a page with the form, returning where it came from.
	body := do(h, "GET", "/work/confirm?action=retry&id=wrk_1&back=%2Fattention", nil, s).Body.String()
	for _, want := range []string{"Run #88 again", "Last failure · turn 3", "no such host", `action="/work/retry"`, `name="back" value="/attention"`, "POST /v1/work/wrk_1/retry", "expected_revision 7", "The failure count (3) resets"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "Simulated") || strings.Contains(body, "<dialog class=\"dialog\" aria") {
		t.Error("page shows a simulation tag or a dialog")
	}
	// With script: just the dialog.
	part := do(h, "GET", "/work/confirm?action=cancel-work&id=wrk_1&back=%2F&part=1", nil, s).Body.String()
	if !strings.HasPrefix(strings.TrimSpace(part), `<dialog class="dialog tone-danger"`) || !strings.Contains(part, "Cancel #88 for good") || strings.Contains(part, "<html") {
		t.Fatalf("dialog:\n%s", part)
	}
	if strings.Contains(part, "type to confirm") || strings.Contains(part, "Type ") {
		t.Fatal("cancel asks to type to confirm")
	}
	// A control the runtime would refuse says why and offers no submit.
	part = do(h, "GET", "/work/confirm?action=wake&id=wrk_1&back=%2F&part=1", nil, s).Body.String()
	if !strings.Contains(part, "Only waiting Work has a wait to wake") || strings.Contains(part, "/work/wake") {
		t.Fatalf("refused wake:\n%s", part)
	}
	// A back that would leave the view goes home.
	if body := do(h, "GET", "/work/confirm?action=hold&id=wrk_1&back=%2F%2Fevil.example", nil, s).Body.String(); !strings.Contains(body, `name="back" value="/"`) {
		t.Fatal("back not confined to the view")
	}
	if w := do(h, "GET", "/work/confirm?action=delete&id=wrk_1", nil, s); w.Code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", w.Code)
	}
}

func TestWakeOffersTheWaitsTypes(t *testing.T) {
	f, s, h := setup(t)
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "waiting", Revision: "7"}
	f.waits = []client.Wait{{ID: "wait/1", WorkID: "wrk_1", State: "active", Match: client.Match{Types: []string{"slack.message.reply", "slack.reaction"}}}}
	part := do(h, "GET", "/work/confirm?action=wake&id=wrk_1&part=1", nil, s).Body.String()
	if !strings.Contains(part, "<option>slack.message.reply</option>") || !strings.Contains(part, "from the active Wait") {
		t.Fatalf("wake:\n%s", part)
	}
}

func TestWakeCarriesAKeyTheWaitMatches(t *testing.T) {
	f, s, h := setup(t)
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "waiting", Revision: "7", CorrelationKeys: []client.Key{{Source: "slack", ExternalID: "T1/C1/1"}}}
	f.waits = []client.Wait{{ID: "wait/1", WorkID: "wrk_1", State: "active", Match: client.Match{Keys: []client.Key{{Source: "slack", ExternalID: "T1/C1/1"}, {Source: "github", ExternalID: "pr/9"}}, Types: []string{"slack.reaction"}}}}
	part := do(h, "GET", "/work/confirm?action=wake&id=wrk_1&part=1", nil, s).Body.String()
	if !strings.Contains(part, `<option value="1">github · pr/9</option>`) {
		t.Fatalf("wake keys:\n%s", part)
	}
	send := func(key string) int {
		form := url.Values{"csrf": {s.csrf}, "work": {"wrk_1"}, "type": {"slack.reaction"}}
		if key != "" {
			form.Set("key", key)
		}
		return do(h, "POST", "/work/wake", form, s).Code
	}
	if c := send(""); c != http.StatusSeeOther {
		t.Fatalf("default key: %d", c)
	}
	if c := send("1"); c != http.StatusSeeOther {
		t.Fatalf("chosen key: %d", c)
	}
	send("5") // not one the Wait matches: refused before the daemon
	want := "wake:slack.reaction wrk_1@7 slack/T1/C1/1,wake:slack.reaction wrk_1@7 github/pr/9"
	if got := strings.Join(f.controls, ","); got != want {
		t.Fatalf("controls: %s", got)
	}
}

func TestAnExtensionActionFromTheMenu(t *testing.T) {
	f, s, h := setup(t)
	f.work["wrk_1"] = client.Work{ID: "wrk_1", State: "waiting", Revision: "7"}
	f.actions["wrk_1"] = []client.Action{{Provider: "acme/approvals", ID: "approve", Label: "Approve the refund", Prompt: "decision", Choices: []string{"Approve", "Decline"}}}
	part := do(h, "GET", "/work/confirm?action=ext&id=wrk_1&provider=acme%2Fapprovals&ext=approve&back=%2Fwork&part=1", nil, s).Body.String()
	for _, want := range []string{"Approve the refund", "Offered by approvals", `value="Decline"`, `name="back" value="/work"`, "POST /v1/work/wrk_1/actions/acme%2Fapprovals/approve"} {
		if !strings.Contains(part, want) {
			t.Errorf("ext dialog lacks %q:\n%s", want, part)
		}
	}
}

func TestMenusRenderFromTheirContexts(t *testing.T) {
	f, s, _ := setup(t)
	_ = f
	p := &page{CSRF: "c", Back: "/"}
	failed := boundRun("w1", 2, "failed", "agent")
	failed.Retry = &client.RetryStatus{Exhausted: true}
	w := view("ready", false, failed)
	w.Actions = []client.Action{{Provider: "x/y", ID: "go", Label: "Do the thing"}}
	var b bytes.Buffer
	if err := s.pages["confirm"].ExecuteTemplate(&b, "work-actions", workCtx{W: w, Page: p}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`class="btn btn-primary btn-sm"`, "Run again", "Do the thing", "from y", "Actions ▾", `aria-disabled="true"`, "Cancel Work"} {
		if !strings.Contains(out, want) {
			t.Errorf("work-actions lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	w.State, w.Disp = "waiting", disposition{Kind: "decision", Group: "attention"}
	if err := s.pages["confirm"].ExecuteTemplate(&b, "row-actions", rowCtx{W: w, Page: p}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ">Decide</a>") || !strings.Contains(b.String(), "kebab") {
		t.Fatalf("row-actions:\n%s", b.String())
	}
}
