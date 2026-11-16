package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/storage"
)

// moveWork walks Work through states, each a legal transition.
func (e *slEnv) moveWork(id domain.ID, states ...domain.WorkState) {
	e.t.Helper()
	e.read(func(tx storage.Tx) error {
		for _, st := range states {
			w, err := tx.Work(id)
			if err != nil {
				return err
			}
			w.State = st
			if err := tx.PutWork(w, w.Revision); err != nil {
				return err
			}
		}
		return nil
	})
}

// progress is the conversation's progress messages in the thread: the
// app's messages laid out as a plan.
func (e *slEnv) progress(root string) []sl.Message {
	var out []sl.Message
	for _, m := range e.fake.Thread(slChannel, root) {
		if len(m.Blocks) > 0 && m.Blocks[0]["type"] == "plan" {
			out = append(out, m)
		}
	}
	return out
}

type shownTask struct{ Title, Status string }

func planOf(t *testing.T, m sl.Message) (string, []shownTask, bool) {
	t.Helper()
	raw, _ := json.Marshal(m.Blocks)
	var blocks []struct {
		Type     string      `json:"type"`
		Title    string      `json:"title"`
		Tasks    []shownTask `json:"tasks"`
		Elements []struct {
			ActionID string `json:"action_id"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatal(err)
	}
	stop := false
	for _, b := range blocks {
		for _, el := range b.Elements {
			stop = stop || el.ActionID == "oc_stop"
		}
	}
	return blocks[0].Title, blocks[0].Tasks, stop
}

// With progress on, a run a person's message starts gets one message in the
// thread that follows the run from durable state: an eyes reaction on the
// message, a task per child Work and per approval it waits on, updated in
// place only when something changes, and its last state with a done
// reaction once it hands back. The next message starts the next run. Stop
// cancels the Work for a person the entry lists, and nobody else.
func TestSlackProgressMessageFollowsTheRun(t *testing.T) {
	e := newSlackEnv(t, `,"progress":{"enabled":true,"stop":["slack:UALICE"]}`, sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Why is checkout slow?")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)

	// Start: the queued run is shown, and the message gets eyes.
	if res := e.converge(); res.Progress == 0 {
		t.Fatalf("no progress message: %+v", res)
	}
	shown := e.progress(root)
	if len(shown) != 1 {
		t.Fatalf("progress messages: %+v", shown)
	}
	if title, _, stop := planOf(t, shown[0]); title != "Starting" || !stop {
		t.Fatalf("first plan: %q stop %v", title, stop)
	}
	if got := e.fake.Reactions(slChannel, root); !slices.Equal(got, []string{"eyes"}) {
		t.Fatalf("reactions: %v", got)
	}

	// Steps: a child Work and an approval it waits on.
	e.moveWork(w.ID, domain.WorkRunning)
	child := domain.Work{ID: "child/1", Kind: "fixture.investigation", ScopeID: slScope, Context: domain.Payload{Schema: "fixture.v1", Data: []byte(`{}`)}, State: domain.WorkReady, ParentID: w.ID, Display: &domain.WorkDisplay{Title: "Read the checkout traces"}}
	e.read(func(tx storage.Tx) error { return tx.PutWork(child, 0) })
	e.moveWork(child.ID, domain.WorkRunning)
	e.converge()
	title, tasks, _ := planOf(t, e.progress(root)[0])
	if title != "Working" || len(tasks) != 1 || tasks[0] != (shownTask{"Read the checkout traces", "in_progress"}) {
		t.Fatalf("plan with a child: %q %+v", title, tasks)
	}
	updates := e.fake.Updates()
	e.converge()
	if e.fake.Updates() != updates {
		t.Fatal("an unchanged plan was sent again")
	}
	e.moveWork(child.ID, domain.WorkCompleted)
	approval := true
	e.actions = func(context.Context, domain.ID, domain.ID) ([]extension.ActionOffer, error) {
		if !approval {
			return nil, nil
		}
		return []extension.ActionOffer{{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Approve or refuse fixture.deploy", Choices: []string{"approve", "refuse"}, Awaited: true}}}, nil
	}
	e.park(w.ID, "wait/approval")
	e.converge()
	title, tasks, _ = planOf(t, e.progress(root)[0])
	if title != "Waiting" || len(tasks) != 2 || tasks[0].Status != "complete" || tasks[1] != (shownTask{"Approve or refuse fixture.deploy", "in_progress"}) {
		t.Fatalf("plan waiting for approval: %q %+v", title, tasks)
	}

	// End: the approval is answered and the run hands back.
	approval = false
	e.read(func(tx storage.Tx) error {
		wt, err := tx.Wait("wait/approval")
		if err != nil {
			return err
		}
		wt.State = domain.WaitCancelled
		if err := tx.PutWait(wt, wt.Revision); err != nil {
			return err
		}
		cur, err := tx.Work(w.ID)
		if err != nil {
			return err
		}
		return tx.PutWait(domain.Wait{ID: "wait/thread", WorkID: w.ID, ExecutionID: wt.ExecutionID, State: domain.WaitActive,
			Match: domain.EventMatch{Keys: cur.CorrelationKeys, Types: []string{sl.TypeMessageHuman}}, CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}, 0)
	})
	e.converge()
	title, tasks, stop := planOf(t, e.progress(root)[0])
	if title != "Waiting for your reply" || stop || tasks[1].Status != "complete" {
		t.Fatalf("plan once handed back: %q %+v stop %v", title, tasks, stop)
	}
	if got := e.fake.Reactions(slChannel, root); !slices.Equal(got, []string{"white_check_mark"}) {
		t.Fatalf("reactions once handed back: %v", got)
	}

	// The next message is the next run, with its own message. (The
	// runtime would wake the thread's Wait with it; here the Wait is
	// released first and the message queued for the ready Work.)
	e.read(func(tx storage.Tx) error {
		wt, err := tx.Wait("wait/thread")
		if err != nil {
			return err
		}
		wt.State = domain.WaitCancelled
		return tx.PutWait(wt, wt.Revision)
	})
	e.moveWork(w.ID, domain.WorkReady)
	e.fake.Reply(slChannel, root, slAlice, "Any news?")
	e.sync()
	e.reconcile()
	e.route()
	e.converge()
	shown = e.progress(root)
	if len(shown) != 2 {
		t.Fatalf("the second run has no message of its own: %d", len(shown))
	}
	if title, tasks, _ := planOf(t, shown[1]); title != "Starting" || len(tasks) != 1 {
		t.Fatalf("second run's plan: %q %+v", title, tasks)
	}

	// Stop: refused for someone the entry doesn't list, taken for Alice.
	var cancelled []string
	e.cancel = func(_ context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error) {
		cur, _ := e.workFor(root)
		if work != w.ID || expected != cur.Revision {
			return "", storage.ErrConflict
		}
		cancelled = append(cancelled, reason+" "+key)
		e.moveWork(work, domain.WorkCancelled)
		return "cancel/1", nil
	}
	value := `{"w":"` + string(w.ID) + `"}`
	ctx := context.Background()
	if err := e.projector.Interact(ctx, e.fake.Press(slChannel, root, shown[1].TS, "UMALLORY", "oc_stop", value)); err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 0 {
		t.Fatal("someone the entry doesn't list stopped the run")
	}
	if err := e.projector.Interact(ctx, e.fake.Press(slChannel, root, shown[1].TS, slAlice, "oc_stop", value)); err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 1 || !strings.HasPrefix(cancelled[0], "stopped from Slack by slack:UALICE slack-stop/UALICE/") {
		t.Fatalf("cancellations: %v", cancelled)
	}
	eph := e.fake.Ephemerals()
	if len(eph) != 2 || !strings.Contains(eph[0].Text, "not one of the people") || eph[1].Text != "Stopped." {
		t.Fatalf("ephemerals: %+v", eph)
	}
	e.converge()
	shown = e.progress(root)
	if title, _, stop := planOf(t, shown[1]); title != "Stopped" || stop {
		t.Fatalf("stopped run's plan: %q stop %v", title, stop)
	}
	if title, _, _ := planOf(t, shown[0]); title != "Done" {
		t.Fatalf("the run the next message ended: %q", title)
	}
	replies := e.fake.Thread(slChannel, root)
	reply := replies[len(replies)-1]
	for _, m := range replies {
		if m.Text == "Any news?" {
			reply = m
		}
	}
	if got := e.fake.Reactions(slChannel, reply.TS); !slices.Equal(got, []string{"octagonal_sign"}) {
		t.Fatalf("stopped run's reactions: %v", got)
	}
	updates = e.fake.Updates()
	e.clock.t = e.clock.t.Add(time.Minute)
	e.converge()
	if e.fake.Updates() != updates {
		t.Fatal("a finished run's message was changed again")
	}
}

// Off by default: a conversation without progress on gets no progress
// message and no reaction, and a Stop press is not taken.
func TestSlackProgressIsOffByDefault(t *testing.T) {
	e := newSlackEnv(t, "", sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Why is checkout slow?")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	e.moveWork(w.ID, domain.WorkRunning)
	if res := e.converge(); res.Progress != 0 || len(e.progress(root)) != 0 || len(e.fake.Reactions(slChannel, root)) != 0 {
		t.Fatalf("progress shown while off: %+v", res)
	}
	called := false
	e.cancel = func(context.Context, domain.ID, domain.Revision, string, string) (domain.ID, error) {
		called = true
		return "", nil
	}
	if err := e.projector.Interact(context.Background(), e.fake.Press(slChannel, root, root, slAlice, "oc_stop", `{"w":"`+string(w.ID)+`"}`)); err != nil || called {
		t.Fatalf("a Stop press was taken while progress is off: %v %v", err, called)
	}
}

// In a dry run nothing is written to Slack, and a run that has ended is
// retired all the same, so later passes don't read it again.
func TestSlackProgressDryRunRetiresEndedRuns(t *testing.T) {
	e := newSlackEnv(t, `,"projection":{"dry_run":true},"progress":{"enabled":true}`, sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Why is checkout slow?")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	e.moveWork(w.ID, domain.WorkRunning)
	e.converge()
	e.moveWork(w.ID, domain.WorkCompleted)
	e.converge()
	if len(e.progress(root)) != 0 || len(e.fake.Reactions(slChannel, root)) != 0 {
		t.Fatal("a dry run wrote to Slack")
	}
	e.read(func(tx storage.Tx) error {
		rows, err := tx.List("slack.projection."+slProjectionNS(), "progress/")
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Fatalf("progress records: %d", len(rows))
		}
		var rec struct{ Done bool }
		if err := json.Unmarshal(rows[0].Value, &rec); err != nil || !rec.Done {
			t.Fatalf("the ended run was not retired: %s", rows[0].Value)
		}
		return nil
	})
}

// slProjectionNS is the projector's state namespace suffix for slNS, as
// the extension derives it.
func slProjectionNS() string {
	h := sha256.Sum256([]byte(strconv.Itoa(len(slNS)) + ":" + slNS + "|"))
	return hex.EncodeToString(h[:])[:16]
}
