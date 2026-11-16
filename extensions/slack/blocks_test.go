package slack

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// rb is blocks as an agent sends them.
func rb(blocks ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(blocks))
	for i, b := range blocks {
		out[i] = json.RawMessage(b)
	}
	return out
}

// shape is each block's type, and the first block's text.
func shape(blocks []json.RawMessage) (types []string, text string) {
	for i, b := range blocks {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		t, _ := m["type"].(string)
		types = append(types, t)
		if i == 0 {
			text, _ = m["text"].(string)
		}
	}
	return types, text
}

// The text leads as a markdown block by default; mrkdwn keeps plain text
// unless blocks follow; markdown too long for one message goes as plain
// text when nothing follows it, and is refused when something does. The
// agent's blocks are passed on byte for byte.
func TestLayoutFollowsTheFormat(t *testing.T) {
	table := rb(`{"type":"table","rows":[[{"type":"raw_number","value":12345678901234567890,"text":"x"}]]}`)
	if types, text := shape(layout(FormatMarkdown, "**hi**", nil)); !slices.Equal(types, []string{"markdown"}) || text != "**hi**" {
		t.Fatalf("markdown: %v %q", types, text)
	}
	got := layout("", "hi", table)
	if types, _ := shape(got); !slices.Equal(types, []string{"markdown", "table"}) || string(got[1]) != string(table[0]) {
		t.Fatalf("default with blocks: %s", got)
	}
	if got := layout(FormatMrkdwn, "*hi*", nil); got != nil {
		t.Fatalf("mrkdwn alone: %s", got)
	}
	if types, _ := shape(layout(FormatMrkdwn, "*hi*", table)); !slices.Equal(types, []string{"section", "table"}) {
		t.Fatalf("mrkdwn with blocks: %v", types)
	}
	long := strings.Repeat("é", maxMarkdown+1)
	if got := layout(FormatMarkdown, long, nil); got != nil {
		t.Fatalf("long markdown alone: %d blocks", len(got))
	}
	if checkLayout(FormatMarkdown, long, nil) != nil || checkLayout(FormatMarkdown, long, table) == nil || checkLayout(FormatMrkdwn, strings.Repeat("a", maxSection+1), table) == nil {
		t.Fatal("checkLayout")
	}
}

// Display blocks pass; anything interactive, at any depth, and any block
// type not on the list is refused.
func TestCheckBlocksAdmitsDisplayOnly(t *testing.T) {
	ok := rb(
		`{"type":"markdown","text":"x"}`,
		`{"type":"section","text":{"type":"mrkdwn","text":"x"},"accessory":{"type":"image","image_url":"https://x","alt_text":"x"}}`,
		`{"type":"data_visualization","title":"x","chart":{"type":"pie","segments":[{"label":"a","value":1}]}}`,
		`{"type":"card","title":{"type":"plain_text","text":"x"}}`,
		`{"type":"data_table","caption":"x","rows":[[{"type":"raw_text","text":"a"}],[{"type":"raw_number","value":12345678901234567890,"text":"1"}]]}`,
	)
	if err := checkBlocks(ok); err != nil {
		t.Fatalf("display blocks refused: %v", err)
	}
	for name, b := range map[string]string{
		"alert":           `{"type":"alert"}`,
		"no type":         `{"text":"x"}`,
		"not an object":   `[1]`,
		"null":            `null`,
		"input":           `{"type":"input"}`,
		"context_actions": `{"type":"context_actions","elements":[]}`,
		"nested select":   `{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"static_select"}]}]}`,
		"action_id":       `{"type":"context","elements":[{"type":"image","action_id":"x"}]}`,
	} {
		var op *extension.OperationError
		if err := checkBlocks(rb(b)); !errors.As(err, &op) {
			t.Errorf("%s admitted: %v", name, err)
		}
	}
	many := make([]string, maxBlocks+1)
	for i := range many {
		many[i] = `{"type":"divider"}`
	}
	if checkBlocks(rb(many...)) == nil {
		t.Error("too many blocks admitted")
	}
}

// Slack's reasons are kept printable and bounded.
func TestDetailIsSanitized(t *testing.T) {
	got := detail([]string{"[ERROR] bad\x00 éfield", strings.Repeat("a", 400), "", "3", "4", "5", "6"})
	if strings.ContainsAny(got, "\x00é") || strings.Contains(got, "6") || strings.Contains(got, strings.Repeat("a", 301)) {
		t.Fatalf("detail: %q", got)
	}
}

// projection.format defaults to markdown and takes only the two formats.
func TestProjectionFormat(t *testing.T) {
	base := `{"namespace":"slack.test","scope":"support","team":"T0TEAM","channels":["C0CHAN"]`
	for cfg, want := range map[string]string{base + `}`: FormatMarkdown, base + `,"projection":{"format":"mrkdwn"}}`: FormatMrkdwn, base + `,"projection":{"format":"html"}}`: ""} {
		s, err := ParseExtension([]byte(cfg), nil)
		if want == "" {
			if err == nil {
				t.Errorf("%s accepted", cfg)
			}
			continue
		}
		if err != nil || s.Projection.Format != want {
			t.Errorf("%s: %q %v", cfg, s.Projection.Format, err)
		}
	}
}

// Neither whitespace nor the record's HTML escaping changes the intent a
// block belongs to, since the projector finishes it from the record.
func TestBlocksDigestMatchesTheRecord(t *testing.T) {
	link := `{"type":"section","text":{"type":"mrkdwn","text":"A & B <https://x|y>"}}`
	stored, _ := json.Marshal(struct {
		C []json.RawMessage `json:"c"`
	}{rb(link)})
	var back struct {
		C []json.RawMessage `json:"c"`
	}
	_ = json.Unmarshal(stored, &back)
	if blocksDigest(back.C) != blocksDigest(rb(link)) {
		t.Fatal("a block read back from its record is another intent")
	}
	if blocksDigest(rb(`{ "type": "divider" }`)) != blocksDigest(rb(`{"type":"divider"}`)) || blocksDigest(nil) != "" {
		t.Fatal("digest")
	}
}

// The feedback block follows the agent's blocks, and is left off a reply
// that can't take one more block rather than refusing the reply.
func TestWithFeedback(t *testing.T) {
	fb, ok := feedbackBlock("work/1", "work/1/attempt/1", "reply/1")
	if !ok {
		t.Fatal("no feedback block")
	}
	if types, _ := shape(withFeedback(FormatMarkdown, "hi", nil, fb)); !slices.Equal(types, []string{"markdown", "context_actions"}) {
		t.Fatalf("plain reply: %v", types)
	}
	table := rb(`{"type":"table","rows":[]}`)
	if types, _ := shape(withFeedback(FormatMrkdwn, "*hi*", table, fb)); !slices.Equal(types, []string{"section", "table", "context_actions"}) {
		t.Fatalf("mrkdwn with a table: %v", types)
	}
	long := strings.Repeat("a", maxMarkdown+1)
	if got := withFeedback(FormatMarkdown, long, nil, fb); got != nil {
		t.Fatalf("text past the markdown allowance took blocks: %d", len(got))
	}
	full := make([]json.RawMessage, maxBlocks)
	for i := range full {
		full[i] = json.RawMessage(`{"type":"divider"}`)
	}
	if got := withFeedback(FormatMarkdown, "hi", full, fb); len(got) != maxBlocks+1 {
		t.Fatalf("a full reply: %d blocks", len(got))
	}
	if _, ok := feedbackBlock("", "", "reply/1"); ok {
		t.Fatal("a feedback block for no Work")
	}
}
