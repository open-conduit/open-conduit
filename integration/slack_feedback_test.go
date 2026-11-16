package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// feedbackButtons is the values of the feedback buttons on a message, by
// "positive" and "negative"; empty when it has none.
func feedbackButtons(t *testing.T, m sl.Message) map[string]string {
	t.Helper()
	raw, _ := json.Marshal(m.Blocks)
	var blocks []struct {
		Type     string `json:"type"`
		Elements []struct {
			Type     string `json:"type"`
			ActionID string `json:"action_id"`
			Positive struct {
				Value string `json:"value"`
			} `json:"positive_button"`
			Negative struct {
				Value string `json:"value"`
			} `json:"negative_button"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, b := range blocks {
		for _, el := range b.Elements {
			if b.Type == "context_actions" && el.Type == "feedback_buttons" && el.ActionID == "oc_feedback" {
				out["positive"], out["negative"] = el.Positive.Value, el.Negative.Value
			}
		}
	}
	return out
}

// reply is the thread's message from the app that says text.
func (e *slEnv) reply(root, text string) sl.Message {
	e.t.Helper()
	for _, m := range e.fake.Thread(slChannel, root) {
		if m.Text == text {
			return m
		}
	}
	e.t.Fatalf("no reply %q in the thread", text)
	return sl.Message{}
}

// feedbackHistory is the feedback Events in Work's history, in order.
func (e *slEnv) feedbackHistory(work domain.ID) []sl.Feedback {
	var out []sl.Feedback
	e.read(func(tx storage.Tx) error {
		all, err := tx.WorkEvents(work, 0, 0)
		for _, a := range all {
			if a.Event.Type != sl.TypeFeedback {
				continue
			}
			var f sl.Feedback
			if err := json.Unmarshal(a.Event.Data.Data, &f); err != nil {
				return err
			}
			out = append(out, f)
		}
		return err
	})
	return out
}

// With feedback on, a conversation's reply carries the feedback buttons. A
// press is recorded on the Work as slack.feedback, with who pressed which
// and on which reply, without becoming input: nothing is queued and the
// Work stays as it was. A second press is a second Event. Once the Work
// has finished, a press is still kept in its history and opens no
// successor. A press for Work that doesn't hold the thread records nothing.
func TestSlackFeedbackIsRecordedWithoutWaking(t *testing.T) {
	e := newSlackEnv(t, `,"feedback":{"enabled":true}`, sl.IngressOptions{})
	ctx := context.Background()
	root := e.fake.Post(slChannel, slAlice, "Why is checkout slow?")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	x := e.startAttempt(w.ID)
	e.finishAttempt(x, domain.WorkWaiting, "The cache is cold.")
	e.converge()
	m := e.reply(root, "The cache is cold.")
	buttons := feedbackButtons(t, m)
	if buttons["positive"] == "" || buttons["negative"] == "" {
		t.Fatalf("the reply has no feedback buttons: %+v", m.Blocks)
	}

	if err := e.projector.Interact(ctx, e.fake.Press(slChannel, root, m.TS, slAlice, "oc_feedback", buttons["positive"])); err != nil {
		t.Fatal(err)
	}
	if err := e.projector.Interact(ctx, e.fake.Press(slChannel, root, m.TS, slAlice, "oc_feedback", buttons["negative"])); err != nil {
		t.Fatal(err)
	}
	e.route()
	got := e.feedbackHistory(w.ID)
	if len(got) != 2 || got[0].Value != sl.FeedbackPositive || got[1].Value != sl.FeedbackNegative {
		t.Fatalf("feedback in the history: %+v", got)
	}
	if f := got[0]; f.User != slAlice || f.Message != m.TS || f.Thread != root || f.Channel != slChannel || f.Execution != string(x) || f.Intent == "" {
		t.Fatalf("feedback record: %+v", f)
	}
	if q := e.queued(w.ID); len(q) != 0 {
		t.Fatalf("feedback was queued as input: %v", q)
	}
	var routes []runtime.RouteRecord
	for _, a := range e.events(sl.TypeFeedback) {
		rec, err := e.router.Decision(ctx, a.Event.ID)
		if err != nil {
			t.Fatal(err)
		}
		routes = append(routes, rec)
	}
	if len(routes) != 2 || routes[0].Policy != "attach" || routes[0].WorkID != w.ID {
		t.Fatalf("feedback routing: %+v", routes)
	}
	if now, _ := e.workFor(root); now.State != domain.WorkWaiting {
		t.Fatalf("feedback moved the Work to %s", now.State)
	}

	// Finished Work keeps the press in its history and opens no successor.
	e.moveWork(w.ID, domain.WorkReady, domain.WorkRunning, domain.WorkCompleted)
	if err := e.projector.Interact(ctx, e.fake.Press(slChannel, root, m.TS, slBob, "oc_feedback", buttons["positive"])); err != nil {
		t.Fatal(err)
	}
	e.route()
	if got := e.feedbackHistory(w.ID); len(got) != 3 || got[2].User != slBob {
		t.Fatalf("feedback on finished Work: %+v", got)
	}
	if n := len(e.works()); n != 1 {
		t.Fatalf("feedback on finished Work opened Work: %d", n)
	}

	// A press naming Work that doesn't hold the thread records nothing.
	other := e.fake.Post(slChannel, slAlice, "Another question")
	if err := e.projector.Interact(ctx, e.fake.Press(slChannel, other, other, slAlice, "oc_feedback", buttons["positive"])); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events(sl.TypeFeedback)); n != 3 {
		t.Fatalf("a press in another thread was recorded: %d feedback events", n)
	}
}

// Feedback is off unless the entry turns it on, and on only for the
// bindings it names; a slack.reply call can leave it off its reply.
func TestSlackFeedbackFollowsTheEntryAndTheCall(t *testing.T) {
	cases := []struct {
		name, extra string
		input       map[string]any
		want        bool
	}{
		{"off by default", "", map[string]any{"text": "answer"}, false},
		{"on for every binding", `,"feedback":{"enabled":true}`, map[string]any{"text": "answer"}, true},
		{"on for this binding", `,"feedback":{"enabled":true,"bindings":["agent"]}`, map[string]any{"text": "answer"}, true},
		{"on for another binding", `,"feedback":{"enabled":true,"bindings":["triage"]}`, map[string]any{"text": "answer"}, false},
		{"left off by the call", `,"feedback":{"enabled":true}`, map[string]any{"text": "answer", "feedback": false}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSlackEnv(t, c.extra, sl.IngressOptions{})
			root := e.fake.Post(slChannel, slAlice, "Why is checkout slow?")
			e.sync()
			e.reconcile()
			e.route()
			w, _ := e.workFor(root)
			x := e.startAttempt(w.ID)
			tools, err := sl.NewTools(sl.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []sl.Source {
				return []sl.Source{{Settings: e.settings, Client: e.client, Projector: e.projector}}
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range tools.Registration().Operations {
				if op.Name != sl.OpReply {
					continue
				}
				in, _ := json.Marshal(c.input)
				if _, err := op.Handler(extension.WithCallRequestID(context.Background(), "r1"), extension.Authority{ScopeID: slScope, WorkID: w.ID, ExecutionID: x},
					domain.Payload{Schema: "json", Data: in}); err != nil {
					t.Fatal(err)
				}
			}
			if b := feedbackButtons(t, e.reply(root, "answer")); (len(b) != 0) != c.want {
				t.Fatalf("feedback buttons %v, want %v", b, c.want)
			}
		})
	}
}
