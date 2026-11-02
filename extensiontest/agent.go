package extensiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// Deadline is how long Agent.Played, WaitWork and Events wait before they
// fail the test.
const Deadline = 30 * time.Second

// Agent is the scripted participant of the scope Daemon generated: it
// plays its turns in order, one each time the daemon launches it, of any
// Work. By default it is a ScriptedShell on the shell executor, making each
// call with the `os` CLI and ending each turn with an outcome; with
// UseFakeClaude it is a FakeClaude on the claude-code executor. Script it
// before starting the Work it serves:
//
//	agent := d.Agent().
//		Call("weather.forecast", map[string]any{"city": "Leeds"}).
//		Call("wait", map[string]any{"match": map[string]any{"types": []string{"weather.alert"}}}).Ends("waiting").
//		Turn().Ends("completed")
//
// Each call goes through the toolbelt as any participant's would. A launch
// past the last turn fails that turn.
type Agent struct {
	d     *Instance
	dir   string
	turns []Turn
	open  bool
}

// Agent is the scope's scripted agent. It fails the test when the test gave
// its own config, which has no agent of the harness's.
func (d *Instance) Agent() *Agent {
	d.t.Helper()
	if d.scope == nil {
		d.t.Fatalf("extensiontest: Agent needs the scope Daemon generates from extensiontest.Package; with Options.Config, name a ScriptedShell or FakeClaude in your own binding; %s", docs)
	}
	return d.scope.agent
}

// Call adds a toolbelt call to the turn being scripted, starting a turn
// when the last one has ended. Operation is one the package declares or a
// core one (read_context, wait, declare_outcome); input is its input
// object.
func (a *Agent) Call(operation string, input any) *Agent {
	a.d.t.Helper()
	if !a.open {
		a.turns = append(a.turns, Turn{})
		a.open = true
	}
	n := len(a.turns)
	t := &a.turns[n-1]
	t.Calls = append(t.Calls, Call{Operation: operation, Input: input, RequestID: fmt.Sprintf("turn%d-call%d", n, len(t.Calls)+1)})
	return a.save()
}

// Ends ends the turn being scripted with state: for the default shell
// participant the attempt's outcome (completed, failed or handoff, or
// waiting after a wait call); with UseFakeClaude the structured status the
// turn ends with. With no turn open it scripts a turn that makes no calls.
func (a *Agent) Ends(state string) *Agent {
	a.d.t.Helper()
	if !a.open {
		a.turns = append(a.turns, Turn{})
	}
	a.turns[len(a.turns)-1].State = state
	a.open = false
	return a.save()
}

// Turn starts scripting the next turn. The one before must have ended.
func (a *Agent) Turn() *Agent {
	a.d.t.Helper()
	if a.open {
		a.d.t.Fatalf("extensiontest: turn %d has no end: call Ends before Turn", len(a.turns))
	}
	a.turns = append(a.turns, Turn{})
	a.open = true
	return a.save()
}

func (a *Agent) save() *Agent {
	a.d.t.Helper()
	b, err := json.Marshal(a.turns)
	if err == nil {
		err = os.WriteFile(filepath.Join(a.dir, "turns.json"), b, 0o600)
	}
	if err != nil {
		a.d.t.Fatal(err)
	}
	return a
}

// Played waits until turn n (from 1) has made its calls and returns what
// it saw: the message it was started or resumed with and each call's
// result.
func (a *Agent) Played(n int) TurnRecord {
	a.d.t.Helper()
	var r TurnRecord
	a.d.Until(fmt.Sprintf("the agent to play turn %d", n), Deadline, func() bool {
		var ok bool
		r, ok = readTurn(a.dir, n)
		return ok
	})
	return r
}

// Result is what operation returned in turn n, waiting for the turn to be
// played; the last such call's when it made several. It fails the test
// when the turn made no such call.
func (a *Agent) Result(n int, operation string) Result {
	a.d.t.Helper()
	r := a.Played(n)
	for i := len(r.Results) - 1; i >= 0; i-- {
		if r.Results[i].Operation == operation {
			return r.Results[i]
		}
	}
	a.d.t.Fatalf("extensiontest: turn %d made no %s call", n, operation)
	return Result{}
}

// Start creates Work of kind in the generated scope, run by the agent,
// with input (JSON-marshalable, or nil) as its context.
func (d *Instance) Start(kind string, input any) client.Work {
	d.t.Helper()
	d.Agent()
	r := client.WorkRequest{ScopeID: ScopeID, Kind: kind, Binding: AgentBinding}
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			d.t.Fatal(err)
		}
		r.Context = &client.Payload{Data: b}
	}
	d.seq++
	out, err := d.c.CreateWork(context.Background(), r, fmt.Sprintf("extensiontest-start-%d", d.seq))
	if err != nil {
		d.t.Fatalf("extensiontest: start %s: %v", kind, err)
	}
	return out.Work
}

// Submit submits an operator Event to the generated scope, as `open-conduit
// events emit` does. ScopeID and DeliveryID are filled in when empty.
func (d *Instance) Submit(e client.EventRequest) client.Event {
	d.t.Helper()
	d.Agent()
	d.seq++
	if e.ScopeID == "" {
		e.ScopeID = ScopeID
	}
	if e.DeliveryID == "" {
		e.DeliveryID = fmt.Sprintf("extensiontest-%d", d.seq)
	}
	out, err := d.c.SubmitEvent(context.Background(), e, "extensiontest-submit-"+e.DeliveryID)
	if err != nil {
		d.t.Fatalf("extensiontest: submit %s: %v", e.Type, err)
	}
	return out
}

// WaitWork waits until Work id is in state (ready, running, waiting,
// completed, failed, cancelled) and returns it.
func (d *Instance) WaitWork(id, state string) client.Work {
	d.t.Helper()
	var w client.Work
	d.Until(fmt.Sprintf("Work %s to be %s", id, state), Deadline, func() bool {
		var err error
		w, err = d.c.Work(context.Background(), id)
		return err == nil && w.State == state
	})
	return w
}

// Events waits until the generated scope has at least one Event of type
// typ and returns them all, oldest first.
func (d *Instance) Events(typ string) []client.Event {
	d.t.Helper()
	var items []client.Event
	d.Until("an Event of type "+typ, Deadline, func() bool {
		page, err := d.c.Events(context.Background(), url.Values{"scope_id": {ScopeID}, "type": {typ}})
		items = page.Items
		return err == nil && len(items) > 0
	})
	return items
}
