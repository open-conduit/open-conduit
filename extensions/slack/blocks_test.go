package slack

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// The text leads as a markdown block by default; mrkdwn keeps plain text
// unless blocks follow; markdown too long for one message goes as plain
// text when nothing follows it, and is refused when something does.
func TestLayoutFollowsTheFormat(t *testing.T) {
	table := []map[string]any{{"type": "table"}}
	if got := layout(FormatMarkdown, "**hi**", nil); len(got) != 1 || got[0]["type"] != "markdown" || got[0]["text"] != "**hi**" {
		t.Fatalf("markdown: %v", got)
	}
	if got := layout("", "hi", table); len(got) != 2 || got[0]["type"] != "markdown" || got[1]["type"] != "table" {
		t.Fatalf("default with blocks: %v", got)
	}
	if got := layout(FormatMrkdwn, "*hi*", nil); got != nil {
		t.Fatalf("mrkdwn alone: %v", got)
	}
	if got := layout(FormatMrkdwn, "*hi*", table); len(got) != 2 || got[0]["type"] != "section" {
		t.Fatalf("mrkdwn with blocks: %v", got)
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
	ok := []map[string]any{
		{"type": "markdown", "text": "x"},
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "x"}, "accessory": map[string]any{"type": "image", "image_url": "https://x", "alt_text": "x"}},
		{"type": "data_visualization", "title": "x", "chart": map[string]any{"type": "pie", "segments": []any{map[string]any{"label": "a", "value": 1.0}}}},
		{"type": "card", "title": map[string]any{"type": "plain_text", "text": "x"}},
		{"type": "data_table", "caption": "x", "rows": []any{[]any{map[string]any{"type": "raw_text", "text": "a"}}, []any{map[string]any{"type": "raw_number", "value": 1.0, "text": "1"}}}},
	}
	if err := checkBlocks(ok); err != nil {
		t.Fatalf("display blocks refused: %v", err)
	}
	for name, b := range map[string]map[string]any{
		"alert":           {"type": "alert"},
		"no type":         {"text": "x"},
		"input":           {"type": "input"},
		"context_actions": {"type": "context_actions", "elements": []any{}},
		"nested select":   {"type": "rich_text", "elements": []any{map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "static_select"}}}}},
		"action_id":       {"type": "context", "elements": []any{map[string]any{"type": "image", "action_id": "x"}}},
	} {
		var op *extension.OperationError
		if err := checkBlocks([]map[string]any{b}); !errors.As(err, &op) {
			t.Errorf("%s admitted: %v", name, err)
		}
	}
	many := make([]map[string]any, maxBlocks+1)
	for i := range many {
		many[i] = map[string]any{"type": "divider"}
	}
	if checkBlocks(many) == nil {
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
