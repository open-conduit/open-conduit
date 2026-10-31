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
	if c.Entries[0].Text != "Session started on test-model" || c.Entries[7].Text != "Turn ended: 4 model calls, 1m1s, $0.50 for the session so far" {
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
	want := "note:Session started on test-model|user:Work issue 12|assistant:Reading the **issue** first.|note:2 KB of the transcript are not shown here|tool:|note:Turn ended: 4 model calls, 1m1s, $0.50 for the session so far"
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

func chatOf(t *testing.T, lines ...string) chat {
	t.Helper()
	c := parseConversation(strings.Join(lines, "\n"), 0, "")
	settle(c.Entries)
	return buildChat(c, client.Run{})
}

func TestChatSendsToolbeltProseAsAMessage(t *testing.T) {
	ch := chatOf(t,
		`{"type":"user","message":{"content":"You are working issue #7."}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__open-conduit__call","input":{"operation":"tracker.comment","input":{"issue":7,"body":"I have opened a pull request that fixes the flaky test."}}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"{\"state\":\"posted\",\"number\":12}"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"mcp__open-conduit__wait","input":{"match":{"types":["x.y"]}}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"{\"state\":\"active\"}"}]}}`,
		`{"type":"user","message":{"content":"A comment arrived while you were working."}}`,
	)
	kinds := []string{}
	for _, b := range ch.Blocks {
		kinds = append(kinds, b.Kind)
	}
	if strings.Join(kinds, " ") != "prompt sent call person" {
		t.Fatalf("blocks %v", kinds)
	}
	sent := ch.Blocks[1].Tool
	if sent.Name != "tracker.comment" || sent.Owner != "tracker" || sent.Prose != "I have opened a pull request that fixes the flaky test." || strings.Join(sent.Facts, " · ") != "state posted · number 12" {
		t.Fatalf("sent %+v", sent)
	}
	if c := ch.Blocks[2].Tool; c.Name != "wait" || c.Owner != "" || strings.Join(c.Facts, "") != "state active" {
		t.Fatalf("call %+v", c)
	}
	if p := ch.Blocks[3]; !p.Injected || p.Text != "A comment arrived while you were working." {
		t.Fatalf("injected %+v", p)
	}
	if ch.Counts[fToolbelt] != 2 || ch.Counts[fMessages] != 2 {
		t.Fatalf("counts %v", ch.Counts)
	}
}

func TestChatGroupsToolsAndHighlightsOnlyRealFailures(t *testing.T) {
	use := func(id, name, cmd string) string {
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{"command":"` + cmd + `"}}]}}`
	}
	res := func(id, out string, failed bool) string {
		e := "false"
		if failed {
			e = "true"
		}
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","is_error":` + e + `,"content":"` + out + `"}]}}`
	}
	ch := chatOf(t,
		`{"type":"user","message":{"content":"go"}}`,
		use("a", "Bash", "go test ./..."), res("a", `--- FAIL: TestX\nok  \tother\nexit status 1`, true),
		use("b", "Read", "x.go"), res("b", "package x", false),
		use("c", "Bash", "ls"), res("c", "a b", false),
		`{"type":"assistant","message":{"content":[{"type":"text","text":"The error: was a typo; fixed."}]}}`,
	)
	g := ch.Blocks[1]
	if g.Kind != "tools" || len(g.Group) != 3 || g.Summary != "Bash ×2 · Read ×1" || g.Failed != 1 || !g.First || ch.Errors != 1 {
		t.Fatalf("group %+v", g)
	}
	c := codeOf("Result", g.Group[0].Result, 14, true)
	if c.Shown[0].Class != "ln-bad" || c.Shown[1].Class != "" || c.Shown[2].Class != "ln-bad" {
		t.Fatalf("failure lines %+v", c.Shown)
	}
	// A result that did not fail, and prose, are never highlighted.
	if c := codeOf("Result", "error: not really", 14, false); c.Shown[0].Class != "" {
		t.Fatal("highlighted a call that did not fail")
	}
}

// Every block that opens carries a key that stays put as a live turn grows,
// so a redraw reopens what the operator opened (issue 225).
func TestChatKeysBlocksThatOpen(t *testing.T) {
	use := func(id string) string {
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"ls"}}]}}`
	}
	think := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`
	lines := []string{`{"type":"user","message":{"content":"go"}}`, think, use("a"), use("b"), use("c")}
	before := chatOf(t, lines...)
	after := chatOf(t, append(lines, use("d"), think)...)
	keys := func(ch chat) []string {
		var ks []string
		for _, b := range ch.Blocks {
			if b.Kind == "thinking" || b.Kind == "tools" {
				if b.Key == "" {
					t.Fatalf("%s block has no key", b.Kind)
				}
				ks = append(ks, b.Key)
			}
		}
		return ks
	}
	b, a := keys(before), keys(after)
	if len(b) != 2 || len(a) != 3 || b[0] != a[0] || b[1] != a[1] || b[1] != "group-a" || a[2] == a[0] {
		t.Fatalf("keys moved: before %v, after %v", b, a)
	}
}

func TestTurnTranscriptRendersAsAChat(t *testing.T) {
	f, s, h := setup(t)
	f.runs[1].Artifacts = []client.Artifact{{ID: "art_tx", MediaType: "application/x-ndjson", Size: 1}}
	f.content["art_tx"] = strings.Join([]string{
		`{"type":"system","subtype":"init","model":"m-1"}`,
		`{"type":"user","message":{"content":"Work issue #88."}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"On it, **now**."}]}}`,
		`{"type":"result","subtype":"success","num_turns":4}`,
	}, "\n")
	body := do(h, "GET", "/work?id=wrk_1&turn=2", nil, s).Body.String()
	for _, want := range []string{"Input to the executor", `class="c-msg from-agent"`, "<strong>now</strong>", "m-1", "4 model"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
	// Filters are links that keep the view, and a filter off hides its rows.
	if !strings.Contains(body, "show=toolbelt%2Ctools%2Csystem") {
		t.Error("Messages filter link missing")
	}
	off := do(h, "GET", "/work?id=wrk_1&turn=2&show=system", nil, s).Body.String()
	if strings.Contains(off, `class="c-msg from-agent"`) || !strings.Contains(off, "Session started") {
		t.Error("show=system still shows messages")
	}
}

// Claude Code's own notes in a session read as notes and commands, not as
// a person's message with its tags.
func TestClaudeCodeMarkupInUserMessages(t *testing.T) {
	note := message("user", "<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n<summary>Background command \"sleep\" completed (exit code 0)</summary>\n</task-notification>", "")
	if note.Kind != "note" || note.Text != `Background task completed: Background command "sleep" completed (exit code 0)` {
		t.Fatalf("task notification: %+v", note)
	}
	cmd := message("user", "<command-message>open-conduit:work-issue</command-message>\n<command-name>/open-conduit:work-issue</command-name>\n<command-args>issue/12\n\nThe issue.</command-args>", "")
	if cmd.Kind != "user" || cmd.Text != "/open-conduit:work-issue\n\nissue/12\n\nThe issue." {
		t.Fatalf("command: %+v", cmd)
	}
	reordered := message("user", "<command-name>/open-conduit:work-issue</command-name>\n<command-message>open-conduit:work-issue</command-message>", "")
	if reordered.Text != "/open-conduit:work-issue" {
		t.Fatalf("tags in another order: %+v", reordered)
	}
	if said := message("user", "<task-notification>x</task-notification> and also this", ""); said.Kind != "user" {
		t.Fatalf("a notification with more text is the person's message: %+v", said)
	}
	if plain := message("user", "hello", ""); plain.Kind != "user" || plain.Text != "hello" {
		t.Fatalf("plain: %+v", plain)
	}
}

// Only web links are links: a message can't carry a script or data URL.
func TestMarkdownLinksAreWebOnly(t *testing.T) {
	out := string(markdown("[docs](https://example.com/a) and [x](javascript:void)"))
	if !strings.Contains(out, `<a href="https://example.com/a" rel="noreferrer">docs</a>`) || strings.Contains(out, `href="javascript`) {
		t.Fatalf("links: %s", out)
	}
}

// A message sent elsewhere reads like any other: a list after a line of
// text is a list.
func TestProseLists(t *testing.T) {
	if out := string(prose("*Built so far:*\n#142 fixes this\n# Heading")); !strings.Contains(out, "<strong>Built so far:</strong>") || !strings.Contains(out, "#142 fixes this") || !strings.Contains(out, `<p class="h">Heading</p>`) {
		t.Fatalf("chat bold and headings: %s", out)
	}
	out := string(prose("**Built so far:**\n- one\n- two\n\n1. first\n2. second"))
	if !strings.Contains(out, "<ul><li>one</li><li>two</li></ul>") || !strings.Contains(out, "<ol><li>first</li><li>second</li></ol>") || !strings.Contains(out, "<strong>Built so far:</strong>") {
		t.Fatalf("prose: %s", out)
	}
}
