package integration_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/observe"
	"github.com/open-conduit/open-conduit/storage"
)

// Observed sources (#318) through the production router, scheduler and
// toolbelt over SQLite. A source's identity is the source, never the binding
// that observed it: two reporters of one source give one Event per item
// revision and one thread Work, and a thread's Work parks between items and
// wakes on the next one.

const obsScope domain.ID = "help"

func newObsEnv(t *testing.T) *repEnv {
	t.Helper()
	e := &repEnv{t: t, clock: &ghClock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "conduit.db"),
		tokens: map[domain.ID]string{}, handlers: map[string]extension.RouteHandler{observe.Type: observe.Route{}},
		register: func(s storage.Store) extension.Registration {
			tools, err := observe.NewTools(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			return tools.Registration()
		},
		// watcher reads the channel through an agent's connector; poller is
		// the script that replaces it; stranger is granted the operation but
		// is no reporter of the source; plain is not granted it.
		grants: map[string][]string{"watcher": {observe.OpRecord}, "poller": {observe.OpRecord}, "stranger": {observe.OpRecord},
			"responder": {extension.Wait}, "plain": {}}}
	e.open()
	e.putObsScope(map[string]any{"name": "slack-help", "reporters": []string{"watcher", "poller"}})
	return e
}

// putObsScope stores the scope with an `observe` entry declaring sources and
// the slack-help route, as an applied config generation would.
func (e *repEnv) putObsScope(sources ...any) {
	var bindings []domain.Binding
	for _, n := range []string{"watcher", "poller", "stranger", "responder", "plain"} {
		bindings = append(bindings, domain.Binding{Name: n, Executor: "fixture", Requires: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}})
	}
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []any{map[string]any{"name": "help", "type": observe.Type, "secret_names": []string{},
		"config": map[string]any{"sources": sources}}}})
	e.read(func(tx storage.Tx) error {
		var revision domain.Revision
		if old, err := tx.Scope(obsScope); err == nil {
			revision = old.Revision
		}
		return tx.PutScope(domain.Scope{ID: obsScope, Name: "help", Bindings: bindings,
			Routes: []domain.Route{{Name: "slack-help", Handler: observe.Type, Binding: "responder", ActivePolicy: "queue",
				Config: domain.Payload{Schema: "core.config.route.v1", Data: []byte(`{"source":"slack-help"}`)}}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, revision)
	})
}

func (e *repEnv) threads() map[string]domain.Work {
	out := map[string]domain.Work{}
	e.read(func(tx storage.Tx) error {
		ws, err := tx.ListWorks()
		for _, w := range ws {
			if w.Kind == observe.WorkKind {
				out[w.CorrelationKeys[0].ExternalID] = w
			}
		}
		return err
	})
	return out
}

func obsItem(item, thread, text string, revision ...string) map[string]any {
	m := map[string]any{"item": item, "thread": thread, "text": text}
	if len(revision) > 0 {
		m["revision"] = revision[0]
	}
	return m
}

// TestObservedSourceSurvivesReplacingTheObserver: an agent watcher and the
// script that replaces it report one source. Repeats from either are the
// first Event; each thread has one Work throughout.
func TestObservedSourceSurvivesReplacingTheObserver(t *testing.T) {
	e := newObsEnv(t)

	run1 := e.manual(obsScope, "schedule.job", "watcher")
	first := e.must(run1, observe.OpRecord, "", obsItem("1.001", "1.001", "how do I reset my VPN?"))
	if first["duplicate"] != false || first["source"] != "slack-help" {
		t.Fatalf("first record: %v", first)
	}
	e.must(run1, observe.OpRecord, "", obsItem("2.001", "2.001", "printer on fire"))
	e.finish(run1)
	before := e.threads()
	if len(before) != 2 || before["1.001"].Display == nil || before["1.001"].Display.Title != "how do I reset my VPN?" {
		t.Fatalf("threads after the watcher: %+v", before)
	}

	// The script replaces the agent and reads the whole channel again.
	run2 := e.manual(obsScope, "schedule.job", "poller")
	if again := e.must(run2, observe.OpRecord, "", obsItem("1.001", "1.001", "how do I reset my VPN?")); again["duplicate"] != true {
		t.Fatalf("the poller's repeat: %v", again)
	}
	e.must(run2, observe.OpRecord, "", obsItem("2.001", "2.001", "printer on fire"))
	e.must(run2, observe.OpRecord, "", obsItem("1.002", "1.001", "also, which app?"))
	e.finish(run2)
	after := e.threads()
	if len(after) != 2 || after["1.001"].ID != before["1.001"].ID || after["2.001"].ID != before["2.001"].ID {
		t.Fatalf("threads after the poller: %+v", after)
	}
	if n := e.countType(observe.TypeRecorded); n != 3 {
		t.Fatalf("accepted %d items, want 3", n)
	}

	// Reporter authority: the entry names who may report for the source.
	stranger := e.manual(obsScope, "manual.work", "stranger")
	_, err := e.call(stranger, observe.OpRecord, "", obsItem("9.001", "9.001", "spoofed"))
	refusedWith(t, err, "not_a_reporter")
	_, err = e.call(stranger, observe.OpRecord, "", map[string]any{"source": "slack-help", "item": "9.001", "thread": "9.001", "text": "spoofed"})
	refusedWith(t, err, "not_a_reporter")
	e.finish(stranger)
	plain := e.manual(obsScope, "manual.work", "plain")
	if _, err := e.call(plain, observe.OpRecord, "", obsItem("9.001", "9.001", "x")); err == nil {
		t.Fatal("a binding without the grant recorded an item")
	}
	e.finish(plain)
	if n := e.countType(observe.TypeRecorded); n != 3 {
		t.Fatalf("a refused record was accepted: %d", n)
	}
}

// TestObservedRevisions: an item's identity includes its revision, so an
// edit back to earlier text (A, B, A) is three items, and changed text
// without a new revision is refused rather than lost.
func TestObservedRevisions(t *testing.T) {
	e := newObsEnv(t)
	run := e.manual(obsScope, "schedule.job", "watcher")
	e.must(run, observe.OpRecord, "", obsItem("1.001", "1.001", "A"))
	if b := e.must(run, observe.OpRecord, "", obsItem("1.001", "1.001", "B", "1.050")); b["duplicate"] != false {
		t.Fatalf("edit to B: %v", b)
	}
	if a := e.must(run, observe.OpRecord, "", obsItem("1.001", "1.001", "A", "1.090")); a["duplicate"] != false {
		t.Fatalf("edit back to A: %v", a)
	}
	_, err := e.call(run, observe.OpRecord, "", obsItem("1.001", "1.001", "C"))
	refusedWith(t, err, "once_conflict")
	_, err = e.call(run, observe.OpRecord, "", obsItem("1.001", "1.999", "A"))
	refusedWith(t, err, "once_conflict")
	e.finish(run)
	if n := e.countType(observe.TypeRecorded); n != 3 {
		t.Fatalf("accepted %d, want 3", n)
	}
}

// TestObservedThreadParksAndWakes: the responder handles a thread, parks
// on a wait for the thread's next item, and wakes on a follow-up a later
// observer run records, with only that item as input.
func TestObservedThreadParksAndWakes(t *testing.T) {
	e := newObsEnv(t)
	var inputs []domain.Event
	e.exec.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		e.mu.Lock()
		e.tokens[r.Execution.ID] = r.Context.ToolbeltToken
		inputs = r.Inputs
		e.mu.Unlock()
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}

	run1 := e.manual(obsScope, "schedule.job", "watcher")
	e.must(run1, observe.OpRecord, "", obsItem("1.001", "1.001", "how do I reset my VPN?"))
	e.finish(run1)
	w := e.threads()["1.001"]

	// The responder's turn: it answers, then parks until the thread moves.
	turn := e.start(w.ID)
	if len(inputs) != 1 {
		t.Fatalf("first turn inputs: %d", len(inputs))
	}
	timeout := e.clock.Now().Add(7 * 24 * time.Hour)
	e.mu.Lock()
	token := e.tokens[turn.Lease.ExecutionID]
	e.mu.Unlock()
	out, err := e.belt.Call(context.Background(), token, extension.ToolCall{Operation: extension.Wait,
		Wait: &extension.WaitRequest{Match: domain.EventMatch{Types: []string{observe.TypeRecorded}}, Timeout: &timeout}})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait: %v", err)
	}
	e.finish(turn)
	if got, _ := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("not parked: %s", got.State)
	}

	// An hour later the next observer run sees the opener again and a
	// follow-up; only the follow-up is new, and it wakes the thread.
	e.clock.Add(time.Hour)
	run2 := e.manual(obsScope, "schedule.job", "poller")
	e.must(run2, observe.OpRecord, "", obsItem("1.001", "1.001", "how do I reset my VPN?"))
	follow := e.must(run2, observe.OpRecord, "", obsItem("1.002", "1.001", "still stuck, it says token expired"))
	if follow["duplicate"] != false {
		t.Fatalf("follow-up: %v", follow)
	}
	e.finish(run2)
	e.read(func(tx storage.Tx) error {
		got, err := tx.Wait(out.Wait.ID)
		if err != nil {
			return err
		}
		if got.State == domain.WaitActive {
			t.Fatalf("the follow-up did not satisfy the wait: %+v", got)
		}
		return nil
	})
	if got, _ := e.work(w.ID); got.State == domain.WorkWaiting {
		t.Fatalf("still parked: %s", got.State)
	}
	e.finish(e.start(w.ID))
	if len(inputs) != 1 {
		t.Fatalf("second turn inputs: %d, want only the follow-up", len(inputs))
	}
	var item observe.Item
	if err := json.Unmarshal(inputs[0].Data.Data, &item); err != nil || item.Item != "1.002" {
		t.Fatalf("second turn input: %s %v", inputs[0].Data.Data, err)
	}
	if len(e.threads()) != 1 {
		t.Fatalf("threads: %v", e.threads())
	}
}

// TestObservedSourceRemovedBeforeRouting: an item accepted while its source
// was declared, but routed after the source was removed from the entry
// (the route kept), starts no Work.
func TestObservedSourceRemovedBeforeRouting(t *testing.T) {
	e := newObsEnv(t)
	run := e.manual(obsScope, "schedule.job", "watcher")
	e.must(run, observe.OpRecord, "", obsItem("1.001", "1.001", "how do I reset my VPN?"))
	e.putObsScope(map[string]any{"name": "forms", "reporters": []string{"watcher"}})
	e.finish(run)
	if n := e.countType(observe.TypeRecorded); n != 1 {
		t.Fatalf("accepted %d, want the 1 recorded before removal", n)
	}
	if th := e.threads(); len(th) != 0 {
		t.Fatalf("a removed source started Work: %v", th)
	}
}
