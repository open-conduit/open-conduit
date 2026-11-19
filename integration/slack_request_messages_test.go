package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

// directMessages are the app's messages in its DM with user.
func (e *slEnv) directMessages(user string) (string, []sl.Message) {
	e.t.Helper()
	dm, ok := e.fake.Direct(user)
	if !ok {
		return "", nil
	}
	var out []sl.Message
	for _, m := range e.fake.Thread(dm, "") {
		if m.BotID != "" {
			out = append(out, m)
		}
	}
	return dm, out
}

// requestFixture is a provider with one request on work, for the people
// responders names, in a state the test moves.
type requestFixture struct {
	state, by  string
	responders []string
}

func (e *slEnv) offerRequest(work domain.ID, r *requestFixture) {
	e.requests = func(_ context.Context, scope, w domain.ID) ([]extension.RequestOffer, error) {
		if scope != slScope || w != work {
			return nil, nil
		}
		action := ""
		if r.state == extension.RequestOpen {
			action = "answer"
		}
		return []extension.RequestOffer{{Provider: "decisions", Request: extension.Request{ID: "ask/1", Label: "Approve or refuse deploy.run", Action: action,
			Responders: r.responders, State: r.state, By: r.by}}}, nil
	}
	e.actions = func(_ context.Context, _, w domain.ID) ([]extension.ActionOffer, error) {
		if w != work || r.state != extension.RequestOpen {
			return nil, nil
		}
		return []extension.ActionOffer{{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Approve or refuse deploy.run",
			Detail: "Allow deploy.run?\n\nInput: {\"env\":\"prod\"}", Choices: []string{"approve", "refuse"}, Awaited: true}}}, nil
	}
}

// scheduled gives the scope Work with no thread, as a scheduled job's,
// waiting on binding.
func (e *slEnv) scheduled(id domain.ID, binding string) {
	e.t.Helper()
	// The attempt's trigger is any accepted Event: a message's.
	e.fake.Post(slChannel, slBob, "unrelated")
	e.sync()
	e.reconcile()
	e.handOff(id, "", binding, "Nightly deploy", domain.WorkRunning, domain.WorkWaiting)
}

func hasButtons(m sl.Message) bool {
	for _, b := range m.Blocks {
		if b["type"] == "actions" {
			return true
		}
	}
	return false
}

// With request_messages on, a request of Work with no thread at all (a
// scheduled job's, say) is sent as a direct message to each Slack person
// its responders name, once: who asks, what, and its buttons. A bare
// `slack` responder names nobody and is sent nothing. A press in the DM
// answers as the person who pressed it, and once the request settles, on
// any surface, the message says how it ended and loses its buttons.
func TestSlackSendsANamedResponderTheRequest(t *testing.T) {
	e := newSlackEnv(t, `,"request_messages":{"enabled":true},"identities":[{"binding":"deployer","username":"Deploy bot"}]`, sl.IngressOptions{})
	e.scheduled("job/nightly", "deployer")
	r := &requestFixture{state: extension.RequestOpen, responders: []string{"slack:" + slAlice, "slack", "operator:local"}}
	e.offerRequest("job/nightly", r)
	e.converge()
	dm, ms := e.directMessages(slAlice)
	if len(ms) != 1 || ms[0].Text != "Deploy bot asks: Approve or refuse deploy.run" || !hasButtons(ms[0]) {
		t.Fatalf("DM: %s %+v", dm, ms)
	}
	section := ms[0].Blocks[0]["text"].(map[string]any)["text"].(string)
	if !strings.Contains(section, `{"env":"prod"}`) || !strings.Contains(section, "Who can answer: slack:"+slAlice+", slack, operator:local") {
		t.Fatalf("DM text: %q", section)
	}
	if _, others := e.directMessages(slBob); len(others) != 0 {
		t.Fatalf("a bare slack responder was sent a DM: %+v", others)
	}
	if e.converge(); len(func() []sl.Message { _, m := e.directMessages(slAlice); return m }()) != 1 {
		t.Fatal("sent twice")
	}

	var got []extension.ActRequest
	e.act = func(_ context.Context, a extension.ActRequest) (extension.ActResult, error) {
		got = append(got, a)
		return extension.ActResult{Added: true, Message: "answered: " + a.Input}, nil
	}
	var approve map[string]any
	for _, b := range ms[0].Blocks {
		if b["type"] == "actions" {
			approve = b["elements"].([]any)[0].(map[string]any)
		}
	}
	if err := e.projector.Interact(context.Background(), e.fake.Press(dm, "", ms[0].TS, slAlice, approve["action_id"].(string), approve["value"].(string))); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Work != "job/nightly" || got[0].Provider != "decisions" || got[0].Input != "approve" || got[0].Caller != slAlice {
		t.Fatalf("act: %+v", got)
	}

	r.state, r.by = extension.RequestAnswered, "slack:"+slAlice
	e.converge()
	m, _ := e.fake.Message(dm, ms[0].TS)
	if hasButtons(m) || !strings.Contains(m.Text, "Deploy bot asks: Approve or refuse deploy.run") || !strings.Contains(m.Text, "Answered by slack:"+slAlice) {
		t.Fatalf("closed DM: %+v", m)
	}
	updates := e.fake.Updates()
	if e.converge(); e.fake.Updates() != updates {
		t.Fatal("closed twice")
	}
}

// A request of Work handed off by a conversation links to the
// conversation's thread, and one that expires says so.
func TestSlackRequestMessageLinksTheConversation(t *testing.T) {
	e := newSlackEnv(t, `,"request_messages":{"enabled":true}`, sl.IngressOptions{})
	root := e.fake.Post(slChannel, slBob, "Deploy the checkout fix")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	e.converge()
	e.moveWork(w.ID, domain.WorkRunning)
	e.handOff("child/deploy", w.ID, "deployer", "Deploy", domain.WorkRunning, domain.WorkWaiting)
	r := &requestFixture{state: extension.RequestOpen, responders: []string{"slack:" + slAlice}}
	e.offerRequest("child/deploy", r)
	e.converge()
	dm, ms := e.directMessages(slAlice)
	if len(ms) != 1 || !strings.Contains(ms[0].Text, "deployer asks:") {
		t.Fatalf("DM: %+v", ms)
	}
	link := slacktest.PermalinkOf(slChannel, root)
	if !strings.Contains(blockText(ms[0]), link) {
		t.Fatalf("no link to the thread %s: %+v", link, ms[0].Blocks)
	}
	r.state = extension.RequestExpired
	e.converge()
	if m, _ := e.fake.Message(dm, ms[0].TS); hasButtons(m) || !strings.Contains(m.Text, "expired") || !strings.Contains(blockText(m), link) {
		t.Fatalf("closed DM: %+v", m)
	}
}

// A post whose answer was lost is found again in the DM by its marker, not
// sent twice.
func TestSlackRequestMessageRecoversALostPost(t *testing.T) {
	e := newSlackEnv(t, `,"request_messages":{"enabled":true}`, sl.IngressOptions{})
	e.scheduled("job/nightly", "deployer")
	r := &requestFixture{state: extension.RequestOpen, responders: []string{"slack:" + slAlice}}
	e.offerRequest("job/nightly", r)
	e.fake.FailNextPost("lost")
	e.converge()
	if _, ms := e.directMessages(slAlice); len(ms) != 1 {
		t.Fatalf("lost post: %+v", ms)
	}
	e.converge()
	e.clock.Add(31 * time.Second)
	e.converge()
	e.converge()
	dm, ms := e.directMessages(slAlice)
	if len(ms) != 1 {
		t.Fatalf("sent again: %+v", ms)
	}
	r.state = extension.RequestRefused
	e.converge()
	if m, _ := e.fake.Message(dm, ms[0].TS); hasButtons(m) || !strings.Contains(m.Text, "Refused") {
		t.Fatalf("the recovered message was not closed: %+v", m)
	}
}

// Off by default: without request_messages nobody is sent a DM.
func TestSlackRequestMessagesAreOffByDefault(t *testing.T) {
	e := newSlackEnv(t, ``, sl.IngressOptions{})
	e.scheduled("job/nightly", "deployer")
	e.offerRequest("job/nightly", &requestFixture{state: extension.RequestOpen, responders: []string{"slack:" + slAlice}})
	e.converge()
	if _, ms := e.directMessages(slAlice); len(ms) != 0 {
		t.Fatalf("sent without request_messages: %+v", ms)
	}
}

func blockText(m sl.Message) string {
	var b strings.Builder
	for _, block := range m.Blocks {
		if els, ok := block["elements"].([]any); ok {
			for _, el := range els {
				if e, ok := el.(map[string]any); ok {
					if t, ok := e["text"].(string); ok {
						b.WriteString(t + "\n")
					}
				}
			}
		}
	}
	return b.String()
}

// A request whose Slack responder is a bare `slack` (anyone in the
// workspace) names nobody to send it to, so nothing is sent.
func TestSlackRequestMessageNeedsANamedPerson(t *testing.T) {
	e := newSlackEnv(t, `,"request_messages":{"enabled":true}`, sl.IngressOptions{})
	e.scheduled("job/nightly", "deployer")
	e.offerRequest("job/nightly", &requestFixture{state: extension.RequestOpen, responders: []string{"slack", "operator:local"}})
	posts := e.fake.Calls("chat.postMessage")
	e.converge()
	if e.fake.Calls("chat.postMessage") != posts {
		t.Fatal("sent a request that names no Slack person")
	}
}
