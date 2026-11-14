package claudecode_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
)

// replyClaude is a claude whose first turn runs a skill and ends with only
// its status, writing no text (#638). Every later user message gets a text
// reply before the status. It keeps reading stdin until it closes.
const replyClaude = `#!/bin/sh
session=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-id|--resume) session=$2; shift ;;
  esac
  shift
done
f=${FAKE_DIR:?}
echo '{"type":"system","subtype":"init"}'
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '%s\n' "$line" >> "$f/stdin"
  if [ "$n" -eq 1 ]; then
    echo '{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"Let me run the skill."},{"type":"tool_use","name":"Skill"}]}}'
    echo '{"type":"assistant","parent_tool_use_id":"toolu_sub","message":{"content":[{"type":"text","text":"a subagent aside"}]}}'
    [ -e "$f/first_text" ] && echo '{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"Here it is."}]}}'
  elif [ -e "$f/follow_limit" ]; then
    echo '{"type":"result","subtype":"success","is_error":true,"api_error":"usage_limit_reached","num_turns":1,"total_cost_usd":0,"session_id":"'"$session"'","result":"You have hit the limit"}'
    exit 1
  else
    echo '{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"The release note line."}]}}'
  fi
  echo '{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","name":"StructuredOutput"}]}}'
  printf '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"%s","num_turns":%s,"total_cost_usd":0,"structured_output":{"state":"done","summary":"Wrote the release note line."}}\n' "$session" "$n"
done
`

type replyData struct {
	Text        string `json:"text"`
	ReplyAsked  bool   `json:"reply_asked"`
	ReplyFailed string `json:"reply_failed"`
	Status      string `json:"status"`
}

// A turn that ends with only its status, on a binding whose reply is its
// text, is asked for the reply once in the same process, and the outcome
// carries the reply and says it was asked. Without the option (the negative
// control) the turn ends as it was, with no text on its outcome.
func TestMissingReplyIsAskedForOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
		ask   bool
	}{{"reply text", map[string]any{"reply": "text"}, true}, {"no reply option", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if err := os.WriteFile(h.binary, []byte(replyClaude), 0o755); err != nil {
				t.Fatal(err)
			}
			r, _ := h.request("work/reply", "exec-reply", 1, tc.extra)
			res := h.run(h.executor(), r)
			var d replyData
			if err := json.Unmarshal(res.Outcome.Data.Data, &d); err != nil {
				t.Fatal(err)
			}
			prompts := h.prompts()
			if !tc.ask {
				if len(prompts) != 1 || d.Text != "" || d.ReplyAsked || d.Status != "done" {
					t.Fatalf("without the option: prompts %q data %+v", prompts, d)
				}
				return
			}
			if len(prompts) != 2 || !strings.HasPrefix(prompts[1], "## Your reply is missing") {
				t.Fatalf("prompts: %q", prompts)
			}
			if d.Text != "The release note line." || !d.ReplyAsked || d.Status != "done" {
				t.Fatalf("outcome data: %+v", d)
			}
			turns := h.reports.named("agent_turn")
			if len(turns) != 1 || turns[0]["reply_asked"] != true {
				t.Fatalf("telemetry: %v", turns)
			}
		})
	}
}

// A turn that already wrote its reply after its last tool call is not
// asked again, and that text is on the outcome.
func TestReplyWrittenIsNotAsked(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.binary, []byte(replyClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.fake+"/first_text", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := h.request("work/replied", "exec-replied", 1, map[string]any{"reply": "text"})
	res := h.run(h.executor(), r)
	var d replyData
	_ = json.Unmarshal(res.Outcome.Data.Data, &d)
	if len(h.prompts()) != 1 || d.Text != "Here it is." || d.ReplyAsked {
		t.Fatalf("prompts %q data %+v", h.prompts(), d)
	}
}

func TestReplyConfigIsChecked(t *testing.T) {
	if _, err := claudecode.ParseConfig([]byte(`{"reply":"summary"}`)); err == nil {
		t.Fatal("an unknown reply was accepted")
	}
	if c, err := claudecode.ParseConfig([]byte(`{"reply":"text"}`)); err != nil || c.Reply != claudecode.ReplyText {
		t.Fatalf("reply text: %+v %v", c, err)
	}
}

// A follow-up to the ask that fails (here, on the usage limit) does not undo
// the turn: it ends on the status it gave before the ask, and reply_failed
// says why. A turn that was never asked and fails the same way (the negative
// control) is failed.
func TestFailedReplyAskKeepsTheStatus(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.binary, []byte(replyClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.fake+"/follow_limit", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := h.request("work/limit", "exec-limit", 1, map[string]any{"reply": "text"})
	res := h.run(h.executor(), r)
	var d replyData
	_ = json.Unmarshal(res.Outcome.Data.Data, &d)
	if res.Outcome.Kind != domain.OutcomeHandoff || res.RetryHint != nil || d.Status != "done" || !d.ReplyAsked || !strings.Contains(d.ReplyFailed, "limit") {
		t.Fatalf("outcome %+v hint %v data %+v", res.Outcome, res.RetryHint, d)
	}
	turns := h.reports.named("agent_turn")
	if len(turns) != 1 || turns[0]["reply_failed"] == nil {
		t.Fatalf("telemetry: %v", turns)
	}

	h = newHarness(t)
	h.mode("limit")
	r, _ = h.request("work/limit2", "exec-limit2", 1, map[string]any{"reply": "text"})
	if res = h.run(h.executor(), r); res.Outcome.Kind != domain.OutcomeFailed {
		t.Fatalf("an unasked limit: %+v", res.Outcome)
	}
}
