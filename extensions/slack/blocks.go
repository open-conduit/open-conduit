package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Message layout (issue #674). A message's text is sent as a markdown block
// by default, so an agent writes standard markdown rather than Slack's
// mrkdwn, with the text itself as the notification and accessibility
// fallback. An agent may add display blocks (a table, a chart, a card)
// after it. The host admits only display blocks with no interactive
// element anywhere in them: a button or select would send presses that
// nothing here handles. Everything else about a block (its fields, sizes and
// counts) is Slack's to judge, and its refusal is passed back.

// displayBlocks are the top-level block types an agent may send.
var displayBlocks = map[string]bool{
	"markdown": true, "section": true, "header": true, "divider": true, "context": true, "image": true,
	"rich_text": true, "table": true, "data_table": true, "data_visualization": true, "card": true,
}

// interactiveTypes are element types that send presses or input back.
var interactiveTypes = map[string]bool{
	"button": true, "workflow_button": true, "icon_button": true, "feedback_buttons": true, "action_cell": true,
	"overflow": true, "checkboxes": true, "radio_buttons": true, "datepicker": true, "datetimepicker": true, "timepicker": true,
	"static_select": true, "external_select": true, "users_select": true, "conversations_select": true, "channels_select": true,
	"multi_static_select": true, "multi_external_select": true, "multi_users_select": true, "multi_conversations_select": true, "multi_channels_select": true,
	"plain_text_input": true, "email_text_input": true, "url_text_input": true, "number_input": true, "rich_text_input": true, "file_input": true,
}

// interactiveKeys are fields only interactive content carries (a card's
// actions, an element's action_id).
var interactiveKeys = map[string]bool{"action_id": true, "actions": true, "dispatch_action": true, "dispatch_action_config": true}

// Bounds the host sets on what it stores and sends; Slack's own limits are
// Slack's to enforce.
const (
	maxBlocks     = 49
	maxBlockBytes = 100_000
	// maxMarkdown is Slack's cumulative limit on markdown block text in
	// one message; longer text is sent as plain message text instead.
	maxMarkdown = 12000
	// maxSection is Slack's limit on a section block's text.
	maxSection = 3000
)

// checkBlocks admits an agent's blocks: display blocks only, nothing
// interactive anywhere in them. The blocks stay as the agent sent them
// (json.RawMessage), so their numbers reach Slack exactly; only a copy is
// decoded to check them.
func checkBlocks(blocks []json.RawMessage) error {
	if len(blocks) == 0 {
		return nil
	}
	if len(blocks) > maxBlocks {
		return refuse("too_many_blocks", "a message takes at most %d blocks after its text", maxBlocks)
	}
	size := 0
	for _, b := range blocks {
		size += len(b)
	}
	if size > maxBlockBytes {
		return refuse("blocks_too_large", "the blocks are larger than %d bytes", maxBlockBytes)
	}
	for i, raw := range blocks {
		var b map[string]any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&b) != nil || b == nil {
			return refuse("block_not_allowed", "block %d is not a JSON object", i+1)
		}
		t, _ := b["type"].(string)
		if !displayBlocks[t] {
			return refuse("block_not_allowed", "block %d has type %q; only display blocks may be sent: %s", i+1, t, displayList())
		}
		if where, ok := interactive(b, t); ok {
			return refuse("block_interactive", "block %d (%s) has an interactive part (%s); only display content may be sent, since nothing handles presses on it", i+1, t, where)
		}
	}
	return nil
}

func displayList() string {
	return "markdown, section, header, divider, context, image, rich_text, table, data_table, data_visualization, card"
}

// interactive finds an interactive element or field anywhere in v.
func interactive(v any, path string) (string, bool) {
	switch v := v.(type) {
	case map[string]any:
		if t, _ := v["type"].(string); interactiveTypes[t] {
			return path + ": " + t, true
		}
		for k, c := range v {
			if interactiveKeys[k] {
				return path + "." + k, true
			}
			if where, ok := interactive(c, path+"."+k); ok {
				return where, true
			}
		}
	case []any:
		for i, c := range v {
			if where, ok := interactive(c, fmt.Sprintf("%s[%d]", path, i)); ok {
				return where, true
			}
		}
	}
	return "", false
}

// checkLayout refuses text that can't lead a message with blocks in the
// entry's format: it must fit one markdown block, or in mrkdwn one section.
func checkLayout(format, text string, blocks []json.RawMessage) error {
	if len(blocks) == 0 {
		return nil
	}
	n := utf8.RuneCountInString(text)
	if format == FormatMrkdwn && n > maxSection {
		return refuse("text_too_long", "with blocks, the text is at most %d characters; put the rest in a markdown or section block", maxSection)
	}
	if format != FormatMrkdwn && n > maxMarkdown {
		return refuse("text_too_long", "with blocks, the text is at most %d characters; put the rest in another block", maxMarkdown)
	}
	return nil
}

// layout is the blocks a message is posted with: the text first, as a
// markdown block (or in mrkdwn, a section, only when blocks follow), then
// the agent's blocks. nil posts the text alone. Markdown text longer than
// one message's markdown allowance, with nothing after it, is posted as
// plain text rather than refused (checkLayout refuses it with blocks).
func layout(format, text string, blocks []json.RawMessage) []json.RawMessage {
	var lead map[string]any
	switch {
	case format == FormatMrkdwn && len(blocks) == 0:
		return nil
	case format == FormatMrkdwn:
		lead = map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
	case len(blocks) == 0 && utf8.RuneCountInString(text) > maxMarkdown:
		return nil
	default:
		lead = map[string]any{"type": "markdown", "text": text}
	}
	return append([]json.RawMessage{raw(lead)}, blocks...)
}

// raw encodes blocks the host lays out itself (the text's, a prompt's).
func raw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// raws encodes a prompt's blocks; nil stays nil.
func raws(blocks []map[string]any) []json.RawMessage {
	if blocks == nil {
		return nil
	}
	out := make([]json.RawMessage, len(blocks))
	for i, b := range blocks {
		out[i] = raw(b)
	}
	return out
}

// blocksDigest identifies blocks in an intent record by their bytes ("" for none, so a record written before blocks existed still
// matches).
func blocksDigest(blocks []json.RawMessage) string {
	if len(blocks) == 0 {
		return ""
	}
	// In the record's own encoding (json.Marshal compacts a raw value and
	// escapes <, >, & and U+2028/2029), so a block is the same intent when
	// the projector finishes it from the record.
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		data, err := json.Marshal(b)
		if err != nil {
			data = b
		}
		parts[i] = string(data)
	}
	return digest(parts...)
}
