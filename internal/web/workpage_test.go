package web

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

func TestProseAndFactsAreFoundWhateverTheOperation(t *testing.T) {
	for _, c := range []struct {
		name, in, prose string
	}{
		{"escaped newlines", `{"channel":"C1","text":"Thanks for waiting.\n\nThe refund went through this morning."}`, "Thanks for waiting.\n\nThe refund went through this morning."},
		{"nested", `{"issue":7,"comment":{"body":"I have opened a pull request that fixes the flaky test."}}`, "I have opened a pull request that fixes the flaky test."},
		{"array, longest wins", `{"parts":["a short line that is still over forty chars long","and a much longer line that is the message the call carried"]}`, "and a much longer line that is the message the call carried"},
		{"JSON in a string is not prose", `{"payload":"{\"a\": \"this is not a message, it is JSON in a string value\"}"}`, ""},
		{"no prose", `{"number":131,"draft":false,"title":"short"}`, ""},
	} {
		v, ok := decodeOrdered(c.in)
		if !ok {
			t.Fatalf("%s: not decoded", c.name)
		}
		if got := proseIn(v); got != c.prose {
			t.Errorf("%s: prose %q, want %q", c.name, got, c.prose)
		}
	}
	v, _ := decodeOrdered(`{"schema":"slack.reply/v1","data":{"state":"posted","ts":"1793039083.355829","url":"https://example.slack.com/archives/C1/p1","raw":"{x}"},"ok":true}`)
	if got, want := factsIn(v), []string{"state posted", "ts 1793039083.355829", "ok true"}; !reflect.DeepEqual(got, want) {
		t.Errorf("facts %q, want %q", got, want)
	}
	if l := linkIn(v); l == nil || l.Label != "C1/p1" {
		t.Errorf("link %+v", l)
	}
}

func TestScoreGapsAndActiveTimeAreTheRecordsSums(t *testing.T) {
	at := func(m int) string { return time.Date(2026, 10, 28, 0, m, 0, 0, time.UTC).Format(time.RFC3339) }
	now := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	r1 := run("w/attempt/1", "w", "handoff", at(1))
	r1.Attempt = 1
	r1.Segments = []client.RunSegment{{StartedAt: at(2), EndedAt: at(5)}}
	r1.ActiveMS = 3 * 60 * 1000
	r2 := run("w/attempt/2", "w", "failed", at(30))
	r2.Attempt = 2
	r2.Segments = []client.RunSegment{{StartedAt: at(30), EndedAt: at(31), Parked: true}, {StartedAt: at(40), EndedAt: at(42)}}
	r2.ActiveMS = 3 * 60 * 1000
	r3 := run("w/attempt/3", "w", "running", at(50))
	r3.Attempt = 3
	r3.Segments = []client.RunSegment{{StartedAt: at(50)}}
	w := client.Work{ID: "w", State: "running", CreatedAt: at(0), UpdatedAt: at(50)}
	waits := []client.Wait{{ExecutionID: "w/attempt/1", Description: "checks on PR #7"}}
	v := newWork(w, []client.Run{r3, r1, r2}, nil, now)
	sc := scoreOf(v, waits, nil, url.Values{}, now)
	if len(sc.Turns) != 3 {
		t.Fatalf("turns %d", len(sc.Turns))
	}
	gaps := []time.Duration{2 * time.Minute, 25 * time.Minute, 8 * time.Minute}
	labels := []string{"queued", "waiting · checks on PR #7", "retry backoff"}
	for i, st := range sc.Turns {
		if st.Gap != gaps[i] || st.GapLabel != labels[i] {
			t.Errorf("turn %d: gap %v %q, want %v %q", i+1, st.Gap, st.GapLabel, gaps[i], labels[i])
		}
	}
	// 3m + 3m (two segments) + 70m running.
	if want := 76 * time.Minute; sc.Active != want {
		t.Errorf("active %v, want %v", sc.Active, want)
	}
	if want := 2 * time.Hour; sc.Elapsed != want {
		t.Errorf("elapsed %v, want %v", sc.Elapsed, want)
	}
	if sc.Now != nil {
		t.Error("running Work has no open now cell")
	}
	if got := pickTurn(v.Turns, ""); got.Number != 3 {
		t.Errorf("default turn %d, want the running one", got.Number)
	}
	if got := pickTurn(v.Turns, "1"); got.Number != 1 {
		t.Errorf("asked turn %d", got.Number)
	}
}

func TestWorkPageShowsPanelsWithActionsFirst(t *testing.T) {
	f, s, h := setup(t)
	f.panels = map[string][]client.Panel{"wrk_1": {
		{Provider: "approvals", ID: "ask", Title: "Approve the refund", Sections: []client.PanelSection{{Text: "Refund £40?", Actions: []client.Action{{Provider: "approvals", ID: "answer", Label: "Answer"}}}}},
		{Provider: "crm", ID: "customer", Title: "Customer record", Sections: []client.PanelSection{{Fields: []client.PanelField{{Name: "plan", Value: "pro"}}}}},
	}}
	w := do(h, "GET", "/work?id=wrk_1", nil, s)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d\n%s", w.Code, w.Body)
	}
	body := w.Body.String()
	ask, score, side, rec := strings.Index(body, "Approve the refund"), strings.Index(body, `class="score"`), strings.Index(body, "From extensions"), strings.Index(body, "Customer record")
	if ask < 0 || score < 0 || side < 0 || rec < 0 || !(ask < score && score < side && side < rec) {
		t.Fatalf("panel order: ask %d score %d side %d record %d", ask, score, side, rec)
	}
	// The parked turn is the one shown, and both turns are in the score.
	if !strings.Contains(body, "Turn 2") || !strings.Contains(body, "turn=1") {
		t.Fatal("turns missing from the score")
	}
}

func TestWorkPageEventsAndMissingWork(t *testing.T) {
	f, s, h := setup(t)
	f.events = []client.Event{{ID: "evt_1", Type: "github.issue.open", Source: "github/example", WorkID: "wrk_1", Sequence: "4", OccurredAt: "2026-10-28T00:00:00Z", Routed: true,
		Route: &client.EventRoute{Route: "issues", Binding: "agent", Policy: "inject", Disposition: "started_work", Reason: "matched"}}}
	body := do(h, "GET", "/work?id=wrk_1&tab=events&event=evt_1", nil, s).Body.String()
	for _, want := range []string{"github.issue.open", "started work", "route <code>issues</code>", "matched", "open-conduit events chain evt_1"} {
		if !strings.Contains(body, want) {
			t.Errorf("events tab lacks %q", want)
		}
	}
	w := do(h, "GET", "/work?id=nope", nil, s)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "No work with ID nope") {
		t.Fatalf("missing: %d", w.Code)
	}
}

func TestRunRedirectsToItsWorkAndTurn(t *testing.T) {
	_, s, h := setup(t)
	w := do(h, "GET", "/run?id=exe_2", nil, s)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/work?id=wrk_1&turn=2" {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Location"))
	}
}

// TestTurnShowsItsReport: a turn whose outcome carries a report shows it
// under the summary; one without shows none.
func TestTurnShowsItsReport(t *testing.T) {
	f, s, h := setup(t)
	f.runs[0].Outcome = &client.Outcome{Kind: "completed", Summary: "Triaged 4 reports into 2 causes",
		Data: &client.Payload{Schema: "fixture.v1", Data: []byte(`{"status":"done","report":"## Skills still call reply.sh\n\nThree sessions hit it."}`)}}
	body := do(h, "GET", "/work?id=wrk_1&turn=1", nil, s).Body.String()
	if !strings.Contains(body, `class="turn-report"`) || !strings.Contains(body, "Three sessions hit it.") || !strings.Contains(body, "Skills still call reply.sh") {
		t.Fatalf("report missing:\n%s", body)
	}
	f.runs[0].Outcome.Data = &client.Payload{Schema: "fixture.v1", Data: []byte(`{"status":"done"}`)}
	if body = do(h, "GET", "/work?id=wrk_1&turn=1", nil, s).Body.String(); strings.Contains(body, `class="turn-report"`) {
		t.Fatal("a turn without a report shows one")
	}
}
