package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ActivityMediaType is the rendered activity artifact (v1's transcript and
// activity view): the turn's stream-json output as readable lines, so an
// operator view shows what the agent said and did without parsing the
// transcript itself. The executor owns the transcript format, so it renders
// it; the TUI and `runs log` only display artifacts.
const ActivityMediaType = "text/plain; charset=utf-8"

const (
	activityText   = 2000
	activityInput  = 200
	activityResult = 300
)

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + " …"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// RenderActivity renders stream-json output (one JSON message per line) as
// the turn's activity: the agent's text, each tool call with a short input,
// each tool result's start, and the turn's end. Lines it cannot read are
// skipped; the transcript artifact keeps everything.
func RenderActivity(stdout string) string {
	var b strings.Builder
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var m struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			IsError      bool     `json:"is_error"`
			NumTurns     int      `json:"num_turns"`
			TotalCostUSD *float64 `json:"total_cost_usd"`
			Result       any      `json:"result"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m.Type {
		case "system":
			if m.Subtype == "init" {
				fmt.Fprintf(&b, "session started%s\n", map[bool]string{true: " (" + m.Model + ")", false: ""}[m.Model != ""])
			}
		case "assistant", "user":
			var parts []struct {
				Type    string          `json:"type"`
				Text    string          `json:"text"`
				Name    string          `json:"name"`
				Input   json.RawMessage `json:"input"`
				Content json.RawMessage `json:"content"`
				IsError bool            `json:"is_error"`
			}
			if json.Unmarshal(m.Message.Content, &parts) != nil {
				var text string
				if json.Unmarshal(m.Message.Content, &text) == nil && text != "" {
					fmt.Fprintf(&b, "%s: %s\n", m.Type, clip(text, activityText))
				}
				continue
			}
			for _, p := range parts {
				switch p.Type {
				case "text":
					if strings.TrimSpace(p.Text) != "" {
						fmt.Fprintf(&b, "%s: %s\n", m.Type, clip(p.Text, activityText))
					}
				case "tool_use":
					fmt.Fprintf(&b, "  tool %s %s\n", p.Name, clip(oneLine(string(p.Input)), activityInput))
				case "tool_result":
					text := ""
					var s string
					var blocks []struct {
						Text string `json:"text"`
					}
					switch {
					case json.Unmarshal(p.Content, &s) == nil:
						text = s
					case json.Unmarshal(p.Content, &blocks) == nil:
						for _, bl := range blocks {
							text += bl.Text + " "
						}
					}
					label := "  result"
					if p.IsError {
						label = "  error"
					}
					fmt.Fprintf(&b, "%s %s\n", label, clip(oneLine(text), activityResult))
				}
			}
		case "result":
			end := "turn ended (" + m.Subtype
			if m.IsError {
				end += ", error"
			}
			end += fmt.Sprintf(", %d model turns", m.NumTurns)
			if m.TotalCostUSD != nil {
				end += fmt.Sprintf(", $%.4f session cost", *m.TotalCostUSD)
			}
			fmt.Fprintln(&b, end+")")
		}
	}
	return b.String()
}
