package web

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// transcript is a turn in the stream-json shape: a prompt, a reply, a tool
// that ran an agent of its own, a failed call, an empty thinking block and
// the turn's end. It starts part-way through a line, as a tail does.
const transcript = `"model":"cut off"}
{"type":"system","subtype":"init","model":"test-model"}
{"type":"user","message":{"content":"Work issue 12"},"timestamp":"2026-10-28T02:00:00Z"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"Reading the **issue** first."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__open-conduit__call","input":{"operation":"github.issue.view","input":{"number":12}}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"the issue"}]}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Task","input":{"description":"Find the page","prompt":"Look for it"}}]}}
{"type":"assistant","parent_tool_use_id":"t2","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"grep -r page .","description":"Search"}}]}}
{"type":"user","parent_tool_use_id":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"found it","is_error":false}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"It is in pages.go"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"make check"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t4","content":"exit 2","is_error":true}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t5","name":"Read","input":{"file_path":"a.go"}}]}}
{"type":"rate_limit_event"}
{"type":"result","subtype":"success","num_turns":4,"duration_ms":61000,"total_cost_usd":0.5}
`

func TestParseConversation(t *testing.T) {
	c := parseConversation(transcript, 0, "")
	var kinds []string
	for _, e := range c.Entries {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, " "); got != "note user assistant tool tool tool tool note" {
		t.Fatalf("entries: %s", got)
	}
	if c.Entries[0].Text != "Session started on test-model" || c.Entries[7].Text != "Turn ended: 4 model calls, 1m1s, $0.50 session cost" {
		t.Errorf("notes: %q, %q", c.Entries[0].Text, c.Entries[7].Text)
	}
	view := c.Entries[3].Tool
	if view.Server != "open-conduit" || view.Name != "call" || view.Headline != "github.issue.view" || view.Result != "the issue" || !view.Done {
		t.Errorf("MCP call: %+v", view)
	}
	task := c.Entries[4].Tool
	if task.Headline != "Find the page" || len(task.Steps) != 1 || task.Steps[0].Tool.Headline != "grep -r page ." || task.Steps[0].Tool.Note != "Search" || task.Steps[0].Tool.Result != "found it" {
		t.Errorf("an agent's steps nest in its call: %+v", task)
	}
	if failed := c.Entries[5].Tool; !failed.Error || failed.Result != "exit 2" {
		t.Errorf("failed call: %+v", failed)
	}
	if running := c.Entries[6].Tool; running.Done {
		t.Error("a call with no result yet is still running")
	}
}

func TestParseConversationClipsAndKeepsOrphans(t *testing.T) {
	long := strings.Repeat("x", clipResult+50)
	c := parseConversation(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"gone","content":"`+long+`"}]}}`, 0, "")
	if len(c.Entries) != 1 || c.Entries[0].Tool.Name != "an earlier call" || c.Entries[0].Tool.Clipped != 50 {
		t.Fatalf("orphan result: %+v", c.Entries)
	}
	prompt := strings.Repeat("y", clipMessage+10)
	c = parseConversation(`{"type":"user","message":{"content":"`+prompt+`"}}`, 0, "")
	if e := c.Entries[0]; e.Full != prompt || len(e.Text) > clipMessage+5 {
		t.Fatalf("long prompt: %d %d", len(e.Text), len(e.Full))
	}
}

func TestParseConversationAcrossAGap(t *testing.T) {
	lines := strings.SplitAfter(transcript, "\n")
	head, tail := strings.Join(lines[:4], "")+`{"type":"assi`, `nt"}`+"\n"+strings.Join(lines[12:], "")
	c := parseConversation(head, 2048, tail)
	var got []string
	for _, e := range c.Entries {
		got = append(got, e.Kind+":"+e.Text)
	}
	want := "note:Session started on test-model|user:Work issue 12|assistant:Reading the **issue** first.|note:2 KB of the transcript are not shown here|tool:|note:Turn ended: 4 model calls, 1m1s, $0.50 session cost"
	if strings.Join(got, "|") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, "|"), want)
	}
	settle(c.Entries)
	if r := c.Entries[4].Tool; !r.Done || !r.Lost {
		t.Errorf("an ended turn's call with no result: %+v", r)
	}
}

func TestMarkdownEscapes(t *testing.T) {
	got := string(markdown("# Done\n\nRan `go <test>` and **passed** <script>x</script>.\n\n- one\n  more\n- [PR](https://example.test/1)\n\n```\n<b>code</b>\n```\n| a | b |\n| - | - |"))
	for _, want := range []string{
		`<p class="h">Done</p>`, "<code>go &lt;test&gt;</code>", "<strong>passed</strong>", "&lt;script&gt;x&lt;/script&gt;",
		"<ul><li>one<br>more</li><li><a href=\"https://example.test/1\" rel=\"noreferrer\">PR</a></li></ul>",
		"<pre><code>&lt;b&gt;code&lt;/b&gt;</code></pre>", `<pre class="table">| a | b |`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "<script>") || strings.Contains(string(markdown("[x](javascript:alert(1))")), "<a") {
		t.Error("markup must not pass through")
	}
}

func TestRunPageShowsTheTimelineAsAConversation(t *testing.T) {
	f, s, h := setup(t)
	for i := range f.runs {
		if f.runs[i].ID == "exe_1" {
			f.runs[i].Artifacts = append(f.runs[i].Artifacts, client.Artifact{ID: "art_transcript", MediaType: "application/x-ndjson", Size: int64(len(transcript))})
		}
	}
	f.content["art_transcript"] = transcript
	body := do(h, "GET", "/run?id=exe_1", nil, s).Body.String()
	for _, want := range []string{
		`id="turn-1" data-part="/run/conversation?run=exe_1" open aria-current="step" data-loaded>`,
		`id="turn-2" data-part="/run/conversation?run=exe_2">`, `<a href="/run?id=exe_2">Go to turn</a>`,
		"Reading the <strong>issue</strong> first.", `<span class="tname">call</span><span class="from">open-conduit</span><code class="thead">github.issue.view</code>`,
		`<details class="tool failed" data-key="t4">`, "No result was recorded",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}

	// A turn opened later loads alone; a running one says it is live.
	w := do(h, "GET", "/run/conversation?run=exe_1", nil, s)
	if w.Header().Get("X-Live") != "0" || !strings.Contains(w.Body.String(), "Reading the <strong>issue</strong> first.") || strings.Contains(w.Body.String(), "<html") {
		t.Errorf("finished part: %s\n%s", w.Header().Get("X-Live"), w.Body.String())
	}
	f.live["exe_3"] = client.LiveRun{Outputs: []client.LiveOutput{{Name: "transcript", MediaType: "application/x-ndjson", Content: transcript}}}
	if w := do(h, "GET", "/run/conversation?run=exe_3", nil, s); w.Header().Get("X-Live") != "1" || !strings.Contains(w.Body.String(), "Working") || !strings.Contains(w.Body.String(), "running</span>") {
		t.Errorf("live part: %s\n%s", w.Header().Get("X-Live"), w.Body.String())
	}
	if w := do(h, "GET", "/run/conversation?run=exe_2", nil, s); !strings.Contains(w.Body.String(), "No conversation to show: run has no artifacts") {
		t.Errorf("turn without a transcript:\n%s", w.Body.String())
	}
	if w := do(h, "GET", "/run/conversation?run=nope", nil, s); w.Code != 404 {
		t.Errorf("missing run: %d", w.Code)
	}
}
