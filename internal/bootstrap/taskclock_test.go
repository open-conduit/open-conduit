package bootstrap

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	ghpkg "github.com/open-conduit/open-conduit/extensions/github/pkg"
	scpkg "github.com/open-conduit/open-conduit/extensions/schedule/pkg"
	slpkg "github.com/open-conduit/open-conduit/extensions/slack/pkg"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
	"github.com/open-conduit/open-conduit/storage"
)

const clockTask = "acme/acme-sync"

// clockPackage is a package using the fixture's scope, with one task.
func clockPackage() *installed {
	return &installed{m: protocol.Manifest{Name: "acme", Provides: protocol.Provides{
		Extensions: []protocol.ExtensionDecl{{Type: "acme"}},
		Tasks:      []protocol.TaskDecl{{Name: "acme-sync", IntervalSeconds: 5}},
	}}}
}

// clockDaemon is a daemon over f, as a fresh start finds it, whose generation
// gives the package the fixture's scopes.
func clockDaemon(t *testing.T, f conformance.RuntimeFixture) *daemon {
	t.Helper()
	d := quietDaemon(f)
	s := &config.Snapshot{Scopes: map[domain.ID]*config.EffectiveScope{}}
	runtimetest.Read(t, f, func(tx storage.Tx) error {
		scopes, err := tx.ListScopes()
		for _, sc := range scopes {
			s.Order = append(s.Order, sc.ID)
			s.Scopes[sc.ID] = &config.EffectiveScope{ID: sc.ID, Enabled: true, Extensions: []config.Extension{{Type: "acme"}}}
		}
		return err
	})
	d.external.current.Store(&generationSnapshot{s: s, gen: 1})
	return d
}

// taskPass is one host pass over the tasks, waited for.
func taskPass(d *daemon, x *installed, tasks map[string]task) {
	ctx := context.Background()
	d.loadTaskSleeps(ctx)
	d.wakeSleepingTasks(ctx, []*installed{x})
	for key, t := range tasks {
		d.runTask(ctx, x, key, t)
	}
	d.wg.Wait()
}

func countingTask(calls *atomic.Int64, result func() protocol.TaskRunResult) map[string]task {
	return map[string]task{clockTask: {Name: "acme-sync", Pace: 5 * time.Second, Run: func(context.Context) (protocol.TaskRunResult, error) {
		calls.Add(1)
		return result(), nil
	}}}
}

func TestTaskUntilTHoldsAcrossARestart(t *testing.T) {
	f := quietFixture(t)
	x := clockPackage()
	d := clockDaemon(t, f)
	at := f.Clock.Now().Add(20 * time.Minute)
	var calls atomic.Int64
	tasks := countingTask(&calls, func() protocol.TaskRunResult { return protocol.TaskRunResult{NextRunAt: &at} })
	taskPass(d, x, tasks)
	if calls.Load() != 1 {
		t.Fatalf("first pass made %d calls", calls.Load())
	}
	f.Clock.Advance(19 * time.Minute)
	taskPass(d, x, tasks)

	restarted := clockDaemon(t, f)
	taskPass(restarted, x, tasks)
	if calls.Load() != 1 {
		t.Fatalf("called %d times before T, across a restart", calls.Load())
	}
	f.Clock.Advance(time.Minute)
	taskPass(restarted, x, tasks)
	if calls.Load() != 2 {
		t.Fatalf("called %d times at T, want 2", calls.Load())
	}
}

func TestTaskCannotSleepPastTheBound(t *testing.T) {
	f := quietFixture(t)
	x := clockPackage()
	d := clockDaemon(t, f)
	far := f.Clock.Now().Add(24 * 365 * time.Hour)
	var calls atomic.Int64
	tasks := countingTask(&calls, func() protocol.TaskRunResult { return protocol.TaskRunResult{NextRunAt: &far} })
	taskPass(d, x, tasks)
	f.Clock.Advance(protocol.MaxTaskSleep - time.Second)
	taskPass(d, x, tasks)
	if calls.Load() != 1 {
		t.Fatalf("called %d times inside the bound", calls.Load())
	}
	f.Clock.Advance(time.Second)
	taskPass(d, x, tasks)
	if calls.Load() != 2 {
		t.Fatalf("a task asking for a year wasn't run at the bound: %d calls", calls.Load())
	}
}

func TestTaskNamingAFamilyRunsOnceAfterItsEvent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family string
		want   int64
	}{{"its family", "fixture.changed", 1}, {"another family", "fixture.other", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			f := quietFixture(t)
			x := clockPackage()
			d := clockDaemon(t, f)
			var calls atomic.Int64
			tasks := countingTask(&calls, func() protocol.TaskRunResult {
				return protocol.TaskRunResult{Wake: &protocol.TaskWake{Families: []string{tc.family}}}
			})
			// The first run has no baseline, so a second takes one.
			taskPass(d, x, tasks)
			taskPass(d, x, tasks)
			settled := calls.Load()
			f.Clock.Advance(time.Minute)
			taskPass(d, x, tasks)
			if calls.Load() != settled {
				t.Fatalf("ran with nothing accepted: %d calls after %d", calls.Load(), settled)
			}
			runtimetest.RequireAccept(t, f, "trigger/clock", &quietKey)
			if err := f.Store.Transact(context.Background(), storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
				scopes, err := tx.ListScopes()
				if err != nil || len(scopes) == 0 {
					return err
				}
				return tx.SetEventScope("trigger/clock", scopes[0].ID)
			}); err != nil {
				t.Fatal(err)
			}
			taskPass(d, x, tasks)
			taskPass(d, x, tasks)
			if got := calls.Load() - settled; got != tc.want {
				t.Fatalf("ran %d times after the Event, want %d", got, tc.want)
			}
		})
	}
}

func TestTaskOnlyOnItsIntervalBehavesAsBefore(t *testing.T) {
	f := quietFixture(t)
	x := clockPackage()
	d := clockDaemon(t, f)
	var calls atomic.Int64
	tasks := countingTask(&calls, func() protocol.TaskRunResult { return protocol.TaskRunResult{} })
	taskPass(d, x, tasks)
	f.Clock.Advance(4 * time.Second)
	taskPass(d, x, tasks)
	if calls.Load() != 1 {
		t.Fatalf("called %d times inside the interval", calls.Load())
	}
	f.Clock.Advance(time.Second)
	taskPass(d, x, tasks)
	if calls.Load() != 2 {
		t.Fatalf("called %d times at the interval", calls.Load())
	}
	runtimetest.Read(t, f, func(tx storage.Tx) error {
		rows, err := tx.List(taskNamespace, "")
		if len(rows) != 0 {
			t.Fatalf("an interval task stored a wait: %v", rows)
		}
		return err
	})
}

func TestTaskWakeRunsASleepingTask(t *testing.T) {
	f := quietFixture(t)
	x := clockPackage()
	d := clockDaemon(t, f)
	d.nudge = make(chan struct{}, 1)
	at := f.Clock.Now().Add(30 * time.Minute)
	var calls atomic.Int64
	tasks := countingTask(&calls, func() protocol.TaskRunResult { return protocol.TaskRunResult{NextRunAt: &at} })
	taskPass(d, x, tasks)
	d.wakeTask(clockTask)
	taskPass(d, x, tasks)
	taskPass(d, x, tasks)
	if calls.Load() != 2 {
		t.Fatalf("called %d times, want one more after the wake", calls.Load())
	}
}

// TestIdleComposedTasksMakeNoCalls serves the real GitHub, Slack and schedule
// packages over the protocol: once settled, an idle minute on the host's fake
// clock calls none of their tasks, and each has a next due time. A package
// reads its own clock, so its answer is carried across as an offset.
func TestIdleComposedTasksMakeNoCalls(t *testing.T) {
	f := quietFixture(t)
	d := clockDaemon(t, f)
	ctx := context.Background()
	const scope = "eng"
	github, err := ghpkg.New()
	if err != nil {
		t.Fatal(err)
	}
	slack, err := slpkg.New()
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := scpkg.New()
	if err != nil {
		t.Fatal(err)
	}
	engaged := sdktest.WithScopes(domain.Scope{ID: scope, Name: scope})
	serve := func(b *inprocsdk.Bridge, entry protocol.ConfigEntry) *sdktest.Host {
		h := sdktest.New(t, b.Extension(), engaged, sdktest.WithSecrets(scope, map[string]string{"hook": "secret"}))
		b.Attach(h.Server())
		if _, err := h.Configure(ctx, protocol.ConfigureParams{Entries: []protocol.ConfigEntry{entry}}); err != nil {
			t.Fatal(err)
		}
		return h
	}
	hosts := []*sdktest.Host{
		serve(github, protocol.ConfigEntry{ScopeID: scope, Type: "github", Name: "main",
			Config:     json.RawMessage(`{"namespace":"github/octo/widgets","scope":"eng","repo":"octo/widgets"}`),
			Connection: &protocol.ConnectionEntry{ID: "github", Type: "github", Config: json.RawMessage(`{"api_url":"http://127.0.0.1:1"}`)}}),
		serve(slack, protocol.ConfigEntry{ScopeID: scope, Type: "slack", Name: "help",
			Config:     json.RawMessage(`{"namespace":"slack/T1","scope":"eng","team":"T1","channels":["C1"],"bot_user_id":"UBOT","receiver":"none","claim_from":"all"}`),
			Connection: &protocol.ConnectionEntry{ID: "slack", Type: "slack", Config: json.RawMessage(`{"api_url":"http://127.0.0.1:1"}`)}}),
		serve(schedule, protocol.ConfigEntry{ScopeID: scope, Type: "schedule", Name: "jobs",
			Config: json.RawMessage(`{"namespace":"schedule/eng","scope":"eng","jobs":[{"name":"report","cron":"0 3 * * *"}]}`)}),
	}
	var calls atomic.Int64
	x := &installed{m: protocol.Manifest{Name: "composed", Provides: protocol.Provides{Extensions: []protocol.ExtensionDecl{{Type: "acme"}}}}}
	tasks := map[string]task{}
	for _, h := range hosts {
		for _, decl := range h.Manifest().Provides.Tasks {
			x.m.Provides.Tasks = append(x.m.Provides.Tasks, protocol.TaskDecl{Name: decl.Name, IntervalSeconds: decl.IntervalSeconds})
			tasks["composed/"+decl.Name] = task{Name: decl.Name, Pace: time.Duration(decl.IntervalSeconds) * time.Second, Run: func(ctx context.Context) (protocol.TaskRunResult, error) {
				calls.Add(1)
				res, err := h.RunPlannedTask(ctx, decl.Name)
				if res.NextRunAt != nil {
					at := f.Clock.Now().Add(time.Until(*res.NextRunAt))
					res.NextRunAt = &at
				}
				return res, err
			}}
		}
	}
	if len(tasks) != 8 {
		t.Fatalf("composed %d tasks, want GitHub's 3, Slack's 2 and schedule's 3", len(tasks))
	}
	// Settle: the first runs take their baselines.
	for range 3 {
		f.Clock.Advance(time.Second)
		taskPass(d, x, tasks)
	}
	settled := calls.Load()
	for range 60 {
		f.Clock.Advance(time.Second)
		taskPass(d, x, tasks)
	}
	if got := calls.Load() - settled; got != 0 {
		t.Fatalf("an idle minute made %d task calls", got)
	}
	d.external.clock.mu.Lock()
	defer d.external.clock.mu.Unlock()
	for key := range tasks {
		r, ok := d.external.clock.sleeps[key]
		if !ok {
			t.Errorf("%s isn't sleeping", key)
		} else if !r.NextDue.After(f.Clock.Now()) {
			t.Errorf("%s next due %v isn't after now %v", key, r.NextDue, f.Clock.Now())
		}
	}
}
