package bootstrap

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/storage"
)

var quietKey = domain.CorrelationKey{Source: "fixture/account", ExternalID: "quiet"}

const quietWork domain.ID = "work/quiet"

// quietDaemon is a daemon over f's store and clock, as a fresh start finds it.
func quietDaemon(f conformance.RuntimeFixture) *daemon {
	d := &daemon{o: Options{RetentionInterval: 10 * time.Minute, SweepInterval: 6 * time.Hour, BackupInterval: -1}, coord: f.Store, now: f.Clock.Now,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	d.openResources()
	return d
}

func quietFixture(t *testing.T) conformance.RuntimeFixture {
	t.Helper()
	return runtimetest.WaitFixture(t, runtimetest.Store(t, "direct"), runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "queue"))
}

// parkWork leaves a Work parked on a Wait with a timeout after d.
func parkWork(t *testing.T, f conformance.RuntimeFixture, after time.Duration) {
	t.Helper()
	runtimetest.RequireAccept(t, f, "trigger/quiet", &quietKey)
	runtimetest.RequireRoute(t, f.Driver, "trigger/quiet")
	f.Driver.(*runtimetest.WaitDriver).ExecutorWithResume.Caps.Resume = true
	ref, err := f.Driver.Start(quietWork)
	if err != nil {
		t.Fatal(err)
	}
	e := runtimetest.Event(f, "timeout/quiet", nil)
	e.Type, e.Source, e.WorkID, e.CausationID = "core.wait.timeout", "core.timer", quietWork, "trigger/quiet"
	if err = f.Store.Transact(context.Background(), storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		return tx.PutTimer(domain.Timer{ID: "timeout/quiet", FireAt: f.Clock.Now().Add(after), Event: e}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	var after0 uint64
	runtimetest.Read(t, f, func(tx storage.Tx) error {
		all, e := tx.Events(0, 0)
		if len(all) > 0 {
			after0 = all[len(all)-1].Sequence
		}
		return e
	})
	if err = f.Driver.Wait(ref, domain.Wait{ID: "wait/quiet", AfterSequence: after0, TimeoutTimerID: "timeout/quiet", Match: domain.EventMatch{Keys: []domain.CorrelationKey{quietKey}, Types: []string{"fixture.changed"}}}, true); err != nil {
		t.Fatal(err)
	}
}

// armJob arms a real schedule timer for a daily job at the given instant.
func armJob(t *testing.T, f conformance.RuntimeFixture, at time.Time) *runtime.Timers {
	t.Helper()
	timers, err := runtime.NewTimers(f.Store, f.Clock.Now, runtime.TimerOptions{Recurrences: map[string]runtime.RecurrenceFunc{sc.RecurrenceKind: sc.Recurrence}})
	if err != nil {
		t.Fatal(err)
	}
	at = at.UTC()
	job := sc.Job{Name: "report", Cron: fmt.Sprintf("%d %d * * *", at.Minute(), at.Hour())}
	timer, spec, changed := sc.Desired("schedule/demo", job, f.Clock.Now(), nil)
	if !changed || timer == nil {
		t.Fatal("no timer")
	}
	if !timer.FireAt.Equal(at) {
		t.Fatalf("job fires at %v, want %v", timer.FireAt, at)
	}
	if err = timers.ArmRecurrence(context.Background(), "routing-scope", *timer, 0, sc.RecurrenceKind, spec); err != nil {
		t.Fatal(err)
	}
	return timers
}

func slotEvents(t *testing.T, f conformance.RuntimeFixture) int {
	t.Helper()
	var n int
	runtimetest.Read(t, f, func(tx storage.Tx) error {
		n = 0 // the callback may run again
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if a.Event.Type == sc.TypeSlot {
				n++
			}
		}
		return err
	})
	return n
}

// A daemon with a job in two hours and Work parked for one is quiescent until
// the hour. Stopped and started then, the timeout fires exactly once.
func TestQuiescentUntilTheEarliestObligation(t *testing.T) {
	ctx := context.Background()
	f := quietFixture(t)
	start := f.Clock.Now().Truncate(time.Minute).Add(time.Minute)
	f.Clock.Advance(start.Sub(f.Clock.Now()))
	timers := armJob(t, f, start.Add(2*time.Hour))
	parkWork(t, f, time.Hour)
	d := quietDaemon(f)
	d.dutyRan(ctx, dutyRetention, f.Clock.Now().Add(d.o.RetentionInterval))
	s := f.Driver.(*runtimetest.WaitDriver).Scheduler
	if _, err := s.PollClaims(ctx, timers, passLimit); err != nil {
		t.Fatalf("poll: %+v", err)
	}
	q, err := d.quiescence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Quiescent || q.Until == nil || !q.Until.Equal(start.Add(time.Hour)) || q.Next != "timer" {
		t.Fatalf("answer %+v, want quiescent until %v", q, start.Add(time.Hour))
	}

	f.Clock.Advance(time.Hour)
	restarted := quietDaemon(f)
	for range 2 {
		if _, err = s.PollClaims(ctx, timers, passLimit); err != nil {
			t.Fatal(err)
		}
	}
	var fired int
	runtimetest.Read(t, f, func(tx storage.Tx) error {
		fired = 0 // the callback may run again
		all, e := tx.Events(0, 0)
		for _, a := range all {
			if a.Event.ID == "timeout/quiet" {
				fired++
			}
		}
		return e
	})
	if fired != 1 || slotEvents(t, f) != 0 {
		t.Fatalf("after the hour: %d timeouts, %d slots; want 1 and 0", fired, slotEvents(t, f))
	}
	if q, err = restarted.quiescence(ctx); err != nil {
		t.Fatal(err)
	}
	if q.Quiescent {
		t.Fatalf("woken Work with unread input reported quiescent: %+v", q)
	}
}

// Each thing in flight is a reason, and clears once it is done.
func TestNotQuiescentWhileAnEventIsUnrouted(t *testing.T) {
	ctx := context.Background()
	f := quietFixture(t)
	d := quietDaemon(f)
	d.dutyRan(ctx, dutyRetention, f.Clock.Now().Add(time.Hour))
	runtimetest.RequireAccept(t, f, "trigger/quiet", &quietKey)
	q, err := d.quiescence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q.Quiescent || !slices.ContainsFunc(q.Reasons, func(r runtime.QuiescenceReason) bool { return r.Code == runtime.QuiescentUnroutedEvent && r.Count == 1 }) {
		t.Fatalf("unrouted Event: %+v", q)
	}
	runtimetest.RequireRoute(t, f.Driver, "trigger/quiet")
	if q, err = d.quiescence(ctx); err != nil {
		t.Fatal(err)
	}
	if q.Quiescent || !slices.ContainsFunc(q.Reasons, func(r runtime.QuiescenceReason) bool { return r.Code == runtime.QuiescentUnreadInput }) {
		t.Fatalf("ready Work with unread input: %+v", q)
	}
}

// A slot 5h59m late still runs after a restart; one six hours late is skipped.
func TestScheduleCatchUpAcrossAStop(t *testing.T) {
	for _, c := range []struct {
		late  time.Duration
		slots int
	}{{5*time.Hour + 59*time.Minute, 1}, {6 * time.Hour, 0}} {
		t.Run(c.late.String(), func(t *testing.T) {
			f := quietFixture(t)
			at := f.Clock.Now().Truncate(time.Minute).Add(2 * time.Hour)
			armJob(t, f, at)
			f.Clock.Advance(at.Add(c.late).Sub(f.Clock.Now()))
			restarted, err := runtime.NewTimers(f.Store, f.Clock.Now, runtime.TimerOptions{Recurrences: map[string]runtime.RecurrenceFunc{sc.RecurrenceKind: sc.Recurrence}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = restarted.Due(context.Background(), passLimit); err != nil {
				t.Fatal(err)
			}
			if got := slotEvents(t, f); got != c.slots {
				t.Fatalf("%v late: %d slots, want %d", c.late, got, c.slots)
			}
		})
	}
}

// countingProvider is a resource provider that counts its sweeps.
type countingProvider struct {
	extension.ResourceProvider
	sweeps *atomic.Int32
}

func (p countingProvider) Sweep(context.Context, extension.ResourceStatusFunc) (extension.ResourceSweepReport, error) {
	p.sweeps.Add(1)
	return extension.ResourceSweepReport{}, nil
}

// Restarts more often than the interval don't put the workspace sweep off,
// and the sweep that runs moves the next one out across the next restart.
func TestWorkspaceSweepSurvivesRestarts(t *testing.T) {
	f := quietFixture(t)
	var sweeps atomic.Int32
	start := func() *daemon {
		d := quietDaemon(f)
		d.ctx = context.Background()
		d.resources.providers.set(map[string]extension.ResourceProvider{"p": countingProvider{sweeps: &sweeps}})
		d.sweepResources(nil)
		d.wg.Wait()
		return d
	}
	start()
	for range 5 {
		f.Clock.Advance(time.Hour)
		start()
	}
	if n := sweeps.Load(); n != 0 {
		t.Fatalf("%d sweeps before six hours", n)
	}
	f.Clock.Advance(time.Hour)
	start()
	if n := sweeps.Load(); n != 1 {
		t.Fatalf("%d sweeps after six hours of short runs, want 1", n)
	}
	f.Clock.Advance(time.Hour)
	start()
	if n := sweeps.Load(); n != 1 {
		t.Fatalf("%d sweeps an hour after the last, want 1", n)
	}
}

// A daemon off for two days waits for its first request before the bundle,
// and a run of short starts can't put the bundle off for ever.
func TestOverdueBackupWaitsForTheFirstRequest(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	now := start
	d := backupDaemon(t, now)
	d.now = func() time.Time { return now }
	d.coord = d.backups.db.(storage.Store)
	d.scheduleBackup(ctx)
	d.wg.Wait()
	if len(bundleNames(t, d.o.BackupDir)) != 1 {
		t.Fatal("first backup not taken")
	}
	restart := func() *daemon {
		r := &daemon{o: d.o, log: d.log, now: func() time.Time { return now }, artifacts: d.artifacts, reported: map[string]string{}, reportedAt: map[string]time.Time{}, coord: d.coord}
		if err := r.startBackups(d.backups.name, d.backups.db); err != nil {
			t.Fatal(err)
		}
		return r
	}
	now = start.Add(48 * time.Hour)
	r := restart()
	r.scheduleBackup(ctx)
	r.wg.Wait()
	if n := len(bundleNames(t, d.o.BackupDir)); n != 1 {
		t.Fatalf("%d bundles before the first request", n)
	}
	r.requestServed()
	r.scheduleBackup(ctx)
	r.wg.Wait()
	if n := len(bundleNames(t, d.o.BackupDir)); n != 2 {
		t.Fatalf("%d bundles after the first request, want 2", n)
	}

	// No request ever comes: the grace, spent across starts, ends the wait.
	now = now.Add(48 * time.Hour)
	restart()
	now = now.Add(backupStartGrace)
	r = restart()
	r.scheduleBackup(ctx)
	r.wg.Wait()
	want := bundlePrefix + now.UTC().Format(bundleLayout)
	if names := bundleNames(t, d.o.BackupDir); !slices.Contains(names, want) {
		t.Fatalf("bundles once the grace was spent: %v, want %s", names, want)
	}
}

// A failed upload's retry is in the answer and survives a restart, so a
// waker can't hold the off-site copy back until the next backup.
func TestShipmentRetryIsAnObligation(t *testing.T) {
	ctx := context.Background()
	f := quietFixture(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, bundlePrefix+f.Clock.Now().UTC().Format(bundleLayout)), 0o700); err != nil {
		t.Fatal(err)
	}
	start := func() *daemon {
		d := quietDaemon(f)
		d.o.BackupInterval, d.o.BackupDir = 24*time.Hour, dir
		if err := d.startBackups("test", fakeDatabase{}); err != nil {
			t.Fatal(err)
		}
		d.dutyRan(ctx, dutyRetention, f.Clock.Now().Add(time.Hour))
		return d
	}
	d := start()
	retry := f.Clock.Now().Add(backupRetry)
	d.backups.mu.Lock()
	d.backups.shipDue = retry
	d.backups.mu.Unlock()
	d.storeShipDue(ctx)
	check := func(d *daemon) {
		t.Helper()
		q, err := d.quiescence(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !q.Quiescent || q.Until == nil || !q.Until.Equal(retry) || q.Next != dutyShipment {
			t.Fatalf("answer %+v, want quiescent until the retry at %v", q, retry)
		}
	}
	check(d)
	f.Clock.Advance(time.Minute)
	check(start())

	d.backups.mu.Lock()
	d.backups.shipDue = time.Time{}
	d.backups.mu.Unlock()
	d.storeShipDue(ctx)
	q, err := d.quiescence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(q.Obligations, func(o management.QuiescenceObligation) bool { return o.Duty == dutyShipment }) {
		t.Fatalf("nothing is owed, yet: %+v", q.Obligations)
	}
}

// fakeDatabase stands for a store whose backups these tests don't take.
type fakeDatabase struct{ storage.Database }

// unlistedDuties are the pace sites in the Go source files of dir with no
// `//duty:<name>` comment naming a listed duty, and the listed duties no site
// names.
func unlistedDuties(t *testing.T, files map[string]string, listed []hostDuty) (unlisted, unused []string) {
	t.Helper()
	names := map[string]bool{}
	for _, duty := range listed {
		names[duty.Name] = true
	}
	used := map[string]bool{}
	for name, src := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		tags := map[int]string{}
		for _, group := range file.Comments {
			for _, c := range group.List {
				if tag, ok := strings.CutPrefix(c.Text, "//duty:"); ok {
					tags[fset.Position(c.Pos()).Line] = strings.TrimSpace(tag)
					used[strings.TrimSpace(tag)] = true
				}
			}
		}
		check := func(pos token.Pos) {
			line := fset.Position(pos).Line
			tag, ok := tags[line-1]
			if !ok {
				tag, ok = tags[line]
			}
			if !ok || !names[tag] {
				unlisted = append(unlisted, fmt.Sprintf("%s:%d", filepath.Base(name), line))
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if s, ok := x.Fun.(*ast.SelectorExpr); ok {
					if p, ok := s.X.(*ast.Ident); ok && p.Name == "time" && slices.Contains([]string{"NewTicker", "Tick", "NewTimer", "AfterFunc", "After"}, s.Sel.Name) {
						check(x.Pos())
					}
					// A deadline: the clock against a stored time, either way round.
					if (s.Sel.Name == "Before" || s.Sel.Name == "After") && len(x.Args) == 1 && (isClock(s.X) || isClock(x.Args[0])) {
						check(x.Pos())
					}
				}
			case *ast.BinaryExpr:
				if x.Op != token.LSS && x.Op != token.GEQ {
					return true
				}
				if call, ok := x.X.(*ast.CallExpr); ok {
					if s, ok := call.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Sub" {
						check(x.Pos())
					}
				}
			}
			return true
		})
	}
	for _, duty := range listed {
		if !used[duty.Name] && !duty.Durable {
			unused = append(unused, duty.Name)
		}
	}
	return unlisted, unused
}

// isClock is an expression that reads the current time: now, now(), d.now()
// or time.Now().
func isClock(e ast.Expr) bool {
	if call, ok := e.(*ast.CallExpr); ok {
		e = call.Fun
	}
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "now"
	case *ast.SelectorExpr:
		return x.Sel.Name == "now" || x.Sel.Name == "Now"
	}
	return false
}

// Every ticker, timer, elapsed-time pace and deadline in this package names a listed duty, so
// a new in-memory clock can't hide from the quiescence answer.
func TestEveryPeriodicDutyIsListed(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, p := range paths {
		// duties.go is the mechanism: it compares every duty's time.
		if strings.HasSuffix(p, "_test.go") || p == "duties.go" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = string(b)
	}
	unlisted, unused := unlistedDuties(t, files, hostDuties)
	if len(unlisted) > 0 {
		t.Fatalf("periodic code with no listed duty (add it to hostDuties and tag it //duty:<name>): %v", unlisted)
	}
	if len(unused) > 0 {
		t.Fatalf("listed duties no code names: %v", unused)
	}
	for _, duty := range hostDuties {
		if !duty.Durable && duty.Why == "" {
			t.Fatalf("%s is process-bound and doesn't say why", duty.Name)
		}
	}

	// The negative control: each shape of in-memory pace.
	for _, body := range []string{
		"_ = time.NewTicker(time.Second)",
		"_ = time.NewTimer(time.Second)",
		"_ = time.AfterFunc(time.Second, func() {})",
		"<-time.After(time.Second)",
		"if time.Now().Sub(last) < time.Minute {\n}",
		"if time.Now().Before(last) {\n}",
		"if last.After(time.Now()) {\n}",
	} {
		src := "package p\nimport \"time\"\nvar last time.Time\nfunc f() {\n" + body + "\n}\n"
		if got, _ := unlistedDuties(t, map[string]string{"new.go": src}, hostDuties); len(got) != 1 {
			t.Fatalf("an unlisted duty went unnoticed: %q gave %v", body, got)
		}
		tagged := strings.Replace(src, "func f() {\n", "func f() {\n//duty:host_loop\n", 1)
		if got, _ := unlistedDuties(t, map[string]string{"new.go": tagged}, hostDuties); len(got) != 0 {
			t.Fatalf("a listed duty was refused: %v", got)
		}
	}
}
