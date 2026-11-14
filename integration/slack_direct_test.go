package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/storage"
)

// TestSlackDirectMessageConversation: with direct_messages on, a person
// the limit admits starts Work by DM on the route that selects DMs, and
// the agent's reply goes to the DM; a second message in that DM thread
// reaches the same Work; a DM from someone the limit does not admit
// starts nothing and its route trace says why; and reconcile recovers a
// DM push missed.
func TestSlackDirectMessageConversation(t *testing.T) {
	const aliceDM, bobDM = "D0ALICE", "D0BOB"
	bindings := []domain.Binding{{Name: "private", Executor: "fixture"}, {Name: "agent", Executor: "fixture"}}
	routes := []domain.Route{
		{Name: "dms", Handler: "slack", Binding: "private", ActivePolicy: "queue", Config: domain.Payload{Data: json.RawMessage(`{"direct_messages":true}`)}},
		{Name: "threads", Handler: "slack", Binding: "agent", ActivePolicy: "queue"},
	}
	e := newSlackEnvWith(t, `,"direct_messages":{"users":["`+slAlice+`"]}`, sl.IngressOptions{}, []string{slChannel}, bindings, routes)
	e.fake.AddMember(aliceDM, slAlice)
	e.fake.AddMember(bobDM, slBob)
	workIn := func(channel, thread string) (domain.Work, bool) {
		var w domain.Work
		found := false
		e.read(func(tx storage.Tx) error {
			var err error
			w, err = tx.WorkByCorrelation(domain.CorrelationKey{Source: slNS, ExternalID: sl.ThreadKey(channel, thread)})
			if errors.Is(err, storage.ErrNotFound) {
				return nil
			}
			found = err == nil
			return err
		})
		return w, found
	}

	question := e.fake.Post(aliceDM, slAlice, "Quick private question about my invoice")
	refused := e.fake.Post(bobDM, slBob, "Can you look at this for me?")
	e.sync()
	for _, m := range []struct{ channel, ts string }{{aliceDM, question}, {bobDM, refused}} {
		if code := e.deliver(e.fake.MessageEvent(m.channel, m.ts)); code != http.StatusOK {
			t.Fatalf("delivery %s: %d", m.channel, code)
		}
	}
	e.route()
	w, ok := workIn(aliceDM, question)
	if !ok || w.Kind != sl.WorkKind {
		t.Fatalf("an admitted DM started no conversation: %+v", e.works())
	}
	if _, ok := workIn(bobDM, refused); ok {
		t.Fatal("a DM from someone direct_messages does not admit started Work")
	}
	traced := 0
	for _, a := range e.events("slack.message.human") {
		d, err := e.router.Decision(context.Background(), a.Event.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch a.Event.CorrelationKey.ExternalID {
		case sl.ThreadKey(aliceDM, question):
			traced++
			if d.Route != "dms" || d.Binding != "private" {
				t.Fatalf("the DM ran on %s/%s, want dms/private", d.Route, d.Binding)
			}
		case sl.ThreadKey(bobDM, refused):
			traced++
			found := false
			for _, r := range d.Refusals {
				found = found || strings.Contains(r.Reason, slBob) && strings.Contains(r.Reason, "direct_messages")
			}
			if d.Policy != "unmatched" || !found {
				t.Fatalf("the refused DM's trace: %+v", d)
			}
		}
	}

	if traced != 2 {
		t.Fatalf("traced %d of the two DMs", traced)
	}

	// The agent replies in the DM.
	attempt := e.startAttempt(w.ID)
	tools, err := sl.NewTools(sl.ToolsOptions{Store: e.store, Now: e.clock.Now, Sources: func() []sl.Source {
		return []sl.Source{{Settings: e.settings, Client: e.client, Projector: e.projector}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	var reply extension.Operation
	for _, op := range tools.Registration().Operations {
		if op.Name == sl.OpReply {
			reply = op
		}
	}
	auth := extension.Authority{ScopeID: slScope, WorkID: w.ID, ExecutionID: attempt}
	if _, err := reply.Handler(extension.WithCallRequestID(context.Background(), "r1"), auth, domain.Payload{Data: json.RawMessage(`{"text":"Happy to help. Which invoice?"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.AgentReplies(aliceDM, question); len(got) != 1 {
		t.Fatalf("the reply did not reach the DM thread: %+v", got)
	}
	e.finishAttempt(attempt, domain.WorkReady, "")

	// A second message in the DM thread wakes the same Work.
	more := e.fake.Reply(aliceDM, question, slAlice, "Invoice 1042")
	e.sync()
	if code := e.deliver(e.fake.MessageEvent(aliceDM, more)); code != http.StatusOK {
		t.Fatalf("reply delivery: %d", code)
	}
	e.route()
	if q := e.queued(w.ID); len(q) != 1 || q[0] != sl.TypeMessageHuman+":"+more {
		t.Fatalf("the DM thread's second message is not the Work's input: %v", q)
	}

	// Reconcile recovers a DM push missed.
	missed := e.fake.Post(aliceDM, slAlice, "A new question, never pushed")
	e.sync()
	e.reconcile()
	e.route()
	if _, ok := workIn(aliceDM, missed); !ok {
		t.Fatal("reconcile did not recover a missed DM")
	}
	if n := len(e.works()); n != 2 {
		t.Fatalf("works = %d, want the two admitted DMs", n)
	}
}
