package web

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

func TestNeedsYouShowsWhatIsBlockedOnAPerson(t *testing.T) {
	f, s, h := setup(t)
	failed := run("work/rep/attempt/2", "rep", "failed", "2026-10-28T01:00:00Z")
	failed.Attempt, failed.FinishedAt = 2, "2026-10-28T01:30:00Z"
	failed.Outcome = &client.Outcome{Kind: "failed", Summary: "go test failed"}
	failed.Retry = &client.RetryStatus{FailureCount: 3, Exhausted: true}
	f.runs = []client.Run{failed}
	f.work = map[string]client.Work{
		// An extension nobody here has heard of offers a decision.
		"ask": {ID: "ask", Kind: "acme.ticket", ScopeID: "sc", State: "waiting", Revision: "2", UpdatedAt: "2026-10-28T02:00:00Z", Display: &client.Display{Ref: "T-9", Title: "Refund for order 4411", Requester: "dana"}},
		"rep": {ID: "rep", ScopeID: "sc", State: "ready", Revision: "5", Display: &client.Display{Ref: "#117", Title: "Flaky DNS"}},
		"hld": {ID: "hld", ScopeID: "sc", State: "ready", Revision: "1", Hold: &client.Hold{Reason: "Waiting on a product decision", At: "2026-10-28T00:00:00Z"}, Display: &client.Display{Title: "Rename the CLI"}},
		"old": {ID: "old", ScopeID: "sc", State: "waiting", Revision: "1", UpdatedAt: "2026-10-25T00:00:00Z", Display: &client.Display{Title: "Nightly report"}},
		"ok":  {ID: "ok", ScopeID: "sc", State: "completed", Revision: "1"},
	}
	f.waits = []client.Wait{{ID: "wait/o", WorkID: "old", State: "active", CreatedAt: "2026-10-25T00:00:00Z", Description: "until Mon 09:00"}}
	approve := client.Action{Provider: "acme/approvals", ID: "approve", Label: "Approve or decline", Prompt: "decision", Choices: []string{"Approve", "Decline"}}
	note := client.Action{Provider: "acme/approvals", ID: "note", Label: "Add a note", Prompt: "note"}
	f.actions = map[string][]client.Action{"ask": {approve, note}}
	f.panels = map[string][]client.Panel{"ask": {{Provider: "acme/approvals", ID: "refund", Title: "Refund needs approval", Sections: []client.PanelSection{{Text: "Customer asks for €40 back.\n\nOrder shipped late."}, {Actions: []client.Action{approve}}}}}}
	s.receipts.add(receipt{Summary: "Hold · “busy”", Call: "POST /v1/work/hld/hold", Event: "evt_9", WorkID: "hld"})

	body := do(h, "GET", "/attention", nil, s).Body.String()
	for _, want := range []string{
		"1 waiting on a person, 1 stopped and needing a repair.",
		"Refund needs approval", "Customer asks for €40 back.</p><p>Order shipped late.", "From approvals", `value="Decline"`,
		"Add a note — note", // the action no panel shows
		"<b>Failed turn.</b> go test failed", "Run again…", "/work?id=rep&amp;turn=2",
		"“Waiting on a product decision”", "action=release",
		"Nightly report", "unchanged for 3d 3h",
		"Hold · “busy”", "POST /v1/work/hld/hold · event evt_9", "Rename the CLI</a>",
		`<span class="nav-count" aria-label="2 items">2</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Needs you lacks %q", want)
		}
	}
	if strings.Contains(body, "Simulated") {
		t.Error("shows a simulation tag")
	}
	// The nav count is the attention group, as the Runs page counts it.
	n := 0
	for _, w := range s.cachedWorld(t.Context(), false).Works {
		if w.Disp.Group == "attention" {
			n++
		}
	}
	if n != 2 || s.needsYou(t.Context()) != n {
		t.Fatalf("attention %d, nav %d", n, s.needsYou(t.Context()))
	}
}

func TestNeedsYouAllClear(t *testing.T) {
	f, s, h := setup(t)
	f.work = map[string]client.Work{"ok": {ID: "ok", State: "completed", Revision: "1"}}
	body := do(h, "GET", "/attention", nil, s).Body.String()
	if !strings.Contains(body, "Nothing is blocked on you.") || strings.Contains(body, "nav-count") {
		t.Fatalf("all clear:\n%s", body)
	}
}
