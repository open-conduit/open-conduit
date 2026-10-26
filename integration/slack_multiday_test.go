package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// mdTurn records one executor segment: a fresh Run or a Resume.
type mdTurn struct {
	Execution domain.ID
	Resumed   bool
	Trigger   string
	Inputs    []string // "type:ts" (ts empty for core events)
}

// mdExecutor is a toolbelt-capable executor fixture whose resume
// capability is chosen per case. Each segment hands its toolbelt token back
// to the test, which declares the wait as the agent would.
type mdExecutor struct {
	mu    sync.Mutex
	caps  domain.Capabilities
	token string
	turns []mdTurn
}

func (x *mdExecutor) Capabilities() domain.Capabilities { return x.caps }
func (x *mdExecutor) segment(r extension.RunRequest, p domain.Payload, resumed bool) (extension.RunHandle, error) {
	if err := r.PersistRecovery(p); err != nil {
		return nil, err
	}
	turn := mdTurn{Execution: r.Execution.ID, Resumed: resumed, Trigger: r.Trigger.Type}
	for _, in := range r.Inputs {
		var m sl.MessagePayload
		_ = json.Unmarshal(in.Data.Data, &m)
		turn.Inputs = append(turn.Inputs, in.Type+":"+m.TS)
	}
	x.mu.Lock()
	x.token = r.Context.ToolbeltToken
	x.turns = append(x.turns, turn)
	x.mu.Unlock()
	return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
}
func (x *mdExecutor) Run(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	return x.segment(r, domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}, false)
}
func (x *mdExecutor) Resume(_ context.Context, r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
	return x.segment(r, p, true)
}
func (x *mdExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrNotAdoptable
}
func (x *mdExecutor) Inject(context.Context, domain.ID, domain.Event) error { return nil }
func (x *mdExecutor) Interrupt(context.Context, domain.ID) error            { return nil }
func (x *mdExecutor) last() mdTurn {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.turns[len(x.turns)-1]
}
func (x *mdExecutor) count() int { x.mu.Lock(); defer x.mu.Unlock(); return len(x.turns) }

// mdHost is the daemon's host loop over the Slack env's store: scheduler,
// toolbelt and durable timers, rebuilt on every restart.
type mdHost struct {
	e      *slEnv
	x      *mdExecutor
	sched  *runtime.Scheduler
	belt   *runtime.Toolbelt
	timers *runtime.Timers
	leases map[domain.ID]domain.LeaseRef
}

func newMDHost(t *testing.T, e *slEnv, x *mdExecutor) *mdHost {
	t.Helper()
	h := &mdHost{e: e, x: x, leases: map[domain.ID]domain.LeaseRef{}}
	var err error
	if h.belt, err = runtime.NewToolbelt(e.store, e.clock.Now, e.router, runtime.ToolbeltOptions{
		Grant: func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			return runtime.ToolbeltGrant{Operations: []string{extension.Wait, extension.ReadContext}}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if h.sched, err = runtime.NewScheduler(e.store, e.clock.Now, e.router, runtime.SchedulerOptions{Owner: "multiday", LeaseDuration: time.Hour, Generation: "1",
		Executors: map[string]extension.Executor{"fixture": x}, Toolbelt: h.belt, ToolbeltEndpoint: "unix:///fixture/management.sock"}); err != nil {
		t.Fatal(err)
	}
	if h.timers, err = runtime.NewTimers(e.store, e.clock.Now, runtime.TimerOptions{}); err != nil {
		t.Fatal(err)
	}
	return h
}

// pass mirrors one daemon loop pass: route, poll timers and waits, route
// what timers accepted, then start every ready Work with pending input.
func (h *mdHost) pass() {
	t := h.e.t
	t.Helper()
	h.e.route()
	if _, err := h.sched.Poll(context.Background(), h.timers, 100); err != nil {
		t.Fatal(err)
	}
	h.e.route()
	var ready []domain.ID
	h.e.read(func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		for _, w := range works {
			if w.State != domain.WorkReady {
				continue
			}
			if in, e := runtime.PendingInputsInTx(tx, w.ID); e != nil || len(in) > 0 {
				ready = append(ready, w.ID)
				if e != nil {
					return e
				}
			}
		}
		return err
	})
	for _, id := range ready {
		a, err := h.sched.Start(context.Background(), id)
		if err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		h.leases[id] = a.Lease
	}
}

// park ends the running turn the way the support playbook does: a
// type-only wait on the thread with a seven-day timeout, then the segment
// finishes and the host registers the wait.
func (h *mdHost) park(work domain.ID) domain.Wait {
	t := h.e.t
	t.Helper()
	timeout := h.e.clock.Now().Add(7 * 24 * time.Hour)
	out, err := h.belt.Call(context.Background(), h.x.token, extension.ToolCall{Operation: extension.Wait,
		Wait: &extension.WaitRequest{Match: domain.EventMatch{Types: []string{sl.TypeMessageHuman}}, Timeout: &timeout}})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait declaration: %v", err)
	}
	if err = h.sched.Complete(context.Background(), h.leases[work], extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "replied"}}); err != nil {
		t.Fatal(err)
	}
	var w domain.Wait
	h.e.read(func(tx storage.Tx) error {
		var err error
		if w, err = tx.Wait(out.Wait.ID); err != nil {
			return err
		}
		timer, err := tx.Timer(w.TimeoutTimerID)
		if err != nil {
			return err
		}
		work, err := tx.Work(work)
		if err != nil {
			return err
		}
		if w.State != domain.WaitActive || work.State != domain.WorkWaiting || !timer.FireAt.Equal(timeout) {
			return fmt.Errorf("not parked: wait %s work %s timer %s", w.State, work.State, timer.FireAt)
		}
		return nil
	})
	return w
}

// advance moves the host and the fake Slack clocks together.
func mdAdvance(e *slEnv, d time.Duration) {
	e.clock.Add(d)
	e.fake.SetClock(e.clock.Now())
}

// TestSlackMultiDayWaitAcrossRestart is the fake-clock multi-day scenario
// of workflow B (04-03), for a resume-capable and a non-resumable executor.
// A support thread's turn parks on the playbook's seven-day wait. Three days
// later, after a restart, a reply missed by push (its thread's parent is now
// older than history_hours) is recovered by reconcile's owned-thread scan
// and wakes the same Work: the resume-capable executor resumes the same
// execution, the non-resumable one starts attempt N+1 with only the new
// message. The next wait then times out after seven quiet days, a
// core.wait.timeout continues the conversation, the turn hands back, and a
// message twelve days in still reaches the same Work. One Work throughout.
func TestSlackMultiDayWaitAcrossRestart(t *testing.T) {
	for _, resume := range []bool{true, false} {
		t.Run(map[bool]string{true: "resume", false: "non_resumable"}[resume], func(t *testing.T) {
			e := newSlackEnv(t, `,"history_hours":24`, sl.IngressOptions{})
			x := &mdExecutor{caps: domain.Capabilities{Resume: resume, Toolbelt: extension.ToolbeltCLI}}
			e.binding = runtime.RouteBinding{Executor: x, Capabilities: x.Capabilities()}
			e.reopen()
			h := newMDHost(t, e, x)

			// Day 0: a question starts the conversation; the first turn parks.
			root := e.fake.Post(slChannel, slAlice, "CSV export fails with a 500")
			e.sync()
			e.reconcile()
			h.pass()
			w, ok := e.workFor(root)
			if !ok || x.count() != 1 {
				t.Fatalf("conversation not started: %v turns %d", ok, x.count())
			}
			first := x.last()
			wait1 := h.park(w.ID)

			// Day 3: the daemon restarts; the reply was never pushed and its
			// parent has left the 24-hour reconcile window.
			mdAdvance(e, 3*24*time.Hour)
			e.reopen()
			h = newMDHost(t, e, x)
			reply := e.fake.Reply(slChannel, root, slAlice, "Still failing on day 3")
			e.sync()
			e.reconcile()
			h.pass()
			e.read(func(tx storage.Tx) error {
				got, err := tx.Wait(wait1.ID)
				if err != nil {
					return fmt.Errorf("wait %s: %w", wait1.ID, err)
				}
				woke, err := tx.Event(got.WakeEventID)
				if err != nil {
					return fmt.Errorf("wake event of %+v: %w", got, err)
				}
				var m sl.MessagePayload
				_ = json.Unmarshal(woke.Event.Data.Data, &m)
				if got.State != domain.WaitWoken || woke.Event.Type != sl.TypeMessageHuman || m.TS != reply {
					return fmt.Errorf("day-3 reply did not wake the wait: %+v %s %s", got, woke.Event.Type, m.TS)
				}
				return nil
			})
			second := x.last()
			if x.count() != 2 {
				t.Fatalf("turns after day-3 reply: %d", x.count())
			}
			if resume && (!second.Resumed || second.Execution != first.Execution) {
				t.Fatalf("resume-capable executor did not resume: %+v then %+v", first, second)
			}
			if !resume && (second.Resumed || second.Execution == first.Execution || fmt.Sprint(second.Inputs) != fmt.Sprint([]string{sl.TypeMessageHuman + ":" + reply})) {
				t.Fatalf("non-resumable executor: want attempt N+1 with only the reply, got %+v (first %+v)", second, first)
			}
			h.park(w.ID)

			// Day 10: seven quiet days later the wait times out (after
			// another restart); the conversation continues on the timeout.
			mdAdvance(e, 7*24*time.Hour+time.Minute)
			e.reopen()
			h = newMDHost(t, e, x)
			e.reconcile()
			h.pass()
			third := x.last()
			if x.count() != 3 || third.Trigger != "core.wait.timeout" {
				t.Fatalf("timeout did not continue the conversation: %d %+v", x.count(), third)
			}
			if resume != third.Resumed {
				t.Fatalf("timeout continuation resumed=%v for resume=%v", third.Resumed, resume)
			}
			// The timeout turn hands back (the playbook's done): Work stays
			// open and owns its thread.
			if err := h.sched.Complete(context.Background(), h.leases[w.ID], extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Summary: "done"}}); err != nil {
				t.Fatal(err)
			}

			// Day 12: a new message, again only through reconcile, starts the
			// next attempt on the same Work.
			mdAdvance(e, 2*24*time.Hour)
			late := e.fake.Reply(slChannel, root, slAlice, "Found it, it was the proxy")
			e.sync()
			e.reconcile()
			h.pass()
			fourth := x.last()
			if x.count() != 4 || fourth.Resumed || fourth.Execution == third.Execution || fourth.Inputs[len(fourth.Inputs)-1] != sl.TypeMessageHuman+":"+late {
				t.Fatalf("day-12 message did not start attempt N+1: %+v", fourth)
			}
			again, _ := e.workFor(root)
			if again.ID != w.ID || len(e.works()) != 1 {
				t.Fatalf("thread correlation changed: %s vs %s (%d works)", again.ID, w.ID, len(e.works()))
			}
			var attempts int
			e.read(func(tx storage.Tx) error {
				xs, err := tx.ListExecutions(w.ID)
				attempts = len(xs)
				if errors.Is(err, storage.ErrNotFound) {
					return nil
				}
				return err
			})
			if want := map[bool]int{true: 2, false: 4}[resume]; attempts != want {
				t.Fatalf("attempts: got %d want %d", attempts, want)
			}
		})
	}
}
