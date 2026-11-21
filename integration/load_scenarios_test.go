//go:build loadtest

package integration_test

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const loadScope = "load"

// loadShellConfig is one scope whose manual route runs a shell binding.
func loadShellConfig(script string, settings map[string]any) []byte {
	ops := []string{"read_context", "declare_outcome", "wait"}
	b, _ := json.Marshal(map[string]any{
		"version": 1, "settings": settings, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": loadScope, "name": "Load",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
				"config": map[string]any{"command": script}}},
			"routes":      []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	})
	return b
}

const loadDone = `os outcome completed --summary done --request-id done >/dev/null`

// newLoadDrill is a drill on config, with the daemon's goroutines published.
func newLoadDrill(t *testing.T, config []byte) (*drill, string) {
	t.Helper()
	d := newDrill(t)
	if err := os.WriteFile(d.config, config, 0o600); err != nil {
		t.Fatal(err)
	}
	stats := filepath.Join(d.root, "stats")
	t.Setenv("OPEN_CONDUIT_STATS_FILE", stats)
	return d, stats
}

// stopDaemon ends the daemon gracefully, releasing the state's lock.
func stopDaemon(d *drill) {
	d.t.Helper()
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.exited:
	case <-time.After(60 * time.Second):
		d.t.Fatal("daemon did not stop")
	}
}

// seedRetained writes n routed Events of about 1 KiB and their finished
// Work into the stopped daemon's store, in threads of 25, as
// history_bench_test does.
func seedRetained(t *testing.T, d *drill, n int) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(d.state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	guard := storage.Guard{Now: now}
	data := domain.Payload{Schema: "load.v1", Data: []byte(`{"body":"` + strings.Repeat("x", 1000) + `"}`)}
	const batch, thread = 1000, 25
	for start := 0; start < n; start += batch {
		if err := store.Transact(ctx, guard, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, n); i++ {
				name := "retained-" + strconv.Itoa(i/thread)
				key := domain.CorrelationKey{Source: "load", ExternalID: name}
				work, xid := domain.ID("work/"+name), domain.ID("execution/"+name)
				if i%thread == 0 {
					if e := tx.PutWork(domain.Work{ID: work, Kind: "load.work", ScopeID: loadScope, State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{key}}, 0); e != nil {
						return e
					}
				}
				id := domain.ID("retained-event-" + strconv.Itoa(i))
				if _, _, e := runtime.AcceptInTx(tx, loadScope, domain.Event{ID: id, Type: "load.retained", Source: "load", DeliveryID: string(id), OccurredAt: now, ReceivedAt: now, CorrelationKey: &key, Data: data}); e != nil {
					return e
				}
				if e := tx.MarkRouted(id, work, "seeded"); e != nil {
					return e
				}
				if i%thread == thread-1 || i == n-1 {
					trigger := domain.ID("retained-event-" + strconv.Itoa(i/thread*thread))
					if e := tx.PutExecution(domain.Execution{ID: xid, WorkID: work, ExecutorBinding: "run", Attempt: 1, State: domain.ExecutionPending, TriggerID: trigger}, 0); e != nil {
						return e
					}
					if _, e := tx.Claim(xid, "seed", time.Minute); e != nil {
						return e
					}
					x, e := tx.Execution(xid)
					if e != nil {
						return e
					}
					x.State, x.Outcome = domain.ExecutionCompleted, &domain.Outcome{Kind: domain.OutcomeCompleted}
					if e = tx.PutExecution(x, x.Revision); e != nil {
						return e
					}
					w, e := tx.Work(work)
					if e != nil {
						return e
					}
					w.State = domain.WorkCompleted
					if e = tx.PutWork(w, w.Revision); e != nil {
						return e
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// doctorLines is the named check's detail and items.
func doctorLines(t *testing.T, d *drill, name string) string {
	t.Helper()
	r, err := d.c.Doctor(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	c := check(r, name)
	return strings.Join(append([]string{c.Detail}, c.Items...), "; ")
}

// TestLoadScheduler is scenario 2: Events through the management API at
// fixed rates over retained history, with each lifecycle step's latency.
func TestLoadScheduler(t *testing.T) {
	report := newLoadReport(t, "Scheduler latency")
	rates := loadEnv(t, "OC_LOAD_RATES", 5, 20)
	count := loadEnv(t, "OC_LOAD_EVENTS", 200)[0]
	var rows, passes [][]string
	for _, retained := range loadEnv(t, "OC_LOAD_HISTORY", 1_000, 50_000) {
		d, _ := newLoadDrill(t, loadShellConfig(loadDone, map[string]any{}))
		d.start("")
		stopDaemon(d)
		seedRetained(t, d, retained)
		d.start("")
		for _, rate := range rates {
			ctx := context.Background()
			prefix := fmt.Sprintf("r%d-%d-", retained, rate)
			var failed atomic.Int64
			var mu sync.Mutex
			ids := map[string]bool{}
			atRate(count, rate, func(i int) {
				id := prefix + strconv.Itoa(i)
				e, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: loadScope, Type: "load.event", DeliveryID: id, CorrelationKey: &client.Key{Source: "load", ExternalID: id}}, id)
				if err != nil {
					failed.Add(1)
					return
				}
				mu.Lock()
				ids[e.ID] = true
				mu.Unlock()
			})
			if failed.Load() != 0 {
				t.Fatalf("%d submissions failed", failed.Load())
			}
			d.until("every Work finished", 10*time.Minute, func() bool {
				page, err := d.c.Works(ctx, url.Values{"scope_id": {loadScope}, "state": {"ready,running,waiting"}, "limit": {"1"}})
				return err == nil && len(page.Items) == 0
			})
			// Telemetry reaches the log on the pipeline's flush.
			d.settle()
			steps := stepLatencies(lifecycleLog(t, d.state), ids)
			for _, name := range lifecycleSteps {
				// A table of nothing is a broken harness, not a result.
				if len(steps[name]) == 0 {
					t.Fatalf("no lifecycle records for step %q at %d retained, %d/s", name, retained, rate)
				}
				rows = append(rows, append([]string{strconv.Itoa(retained), strconv.Itoa(rate), name}, spreadOf(steps[name])...))
			}
			passes = append(passes, []string{strconv.Itoa(retained), strconv.Itoa(rate), doctorLines(t, d, "loop")})
		}
		stopDaemon(d)
	}
	report.table([]string{"Retained Events", "Events/s", "Step", "Seen", "p50", "p95", "p99"}, rows)
	report.table([]string{"Retained Events", "Events/s", "Whole pass and phases (doctor, latest 600)"}, passes)
}

var lifecycleSteps = []string{"accepted → routed", "routed → queued", "queued → started", "started → launched"}

// stepLatencies follows the Events named by ids through the lifecycle log. Telemetry may drop under load, so a step counts only the
// Events both of whose records were seen.
func stepLatencies(records []lifecycle, ids map[string]bool) map[string][]time.Duration {
	accepted, routed, queued := map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}
	execTrigger, started := map[string]string{}, map[string]time.Time{}
	out := map[string][]time.Duration{}
	for _, r := range records {
		switch r.Msg {
		case "event.accepted":
			if ids[r.EventID] {
				accepted[r.EventID] = r.At
			}
		case "route.decided":
			if at, ok := accepted[r.EventID]; ok {
				routed[r.EventID] = r.At
				out[lifecycleSteps[0]] = append(out[lifecycleSteps[0]], r.At.Sub(at))
			}
		case "execution.queued":
			trigger := r.Attributes["trigger.id"]
			if at, ok := routed[trigger]; ok {
				queued[trigger], execTrigger[r.ExecutionID] = r.At, trigger
				out[lifecycleSteps[1]] = append(out[lifecycleSteps[1]], r.At.Sub(at))
			}
		case "execution.started":
			if at, ok := queued[execTrigger[r.ExecutionID]]; ok {
				if _, seen := started[r.ExecutionID]; !seen {
					started[r.ExecutionID] = r.At
					out[lifecycleSteps[2]] = append(out[lifecycleSteps[2]], r.At.Sub(at))
				}
			}
		case "execution.launched":
			if at, ok := started[r.ExecutionID]; ok {
				out[lifecycleSteps[3]] = append(out[lifecycleSteps[3]], r.At.Sub(at))
				delete(started, r.ExecutionID)
			}
		}
	}
	return out
}

// loadWebhooks is a daemon taking signed GitHub and Slack deliveries.
type loadWebhooks struct {
	d      *drill
	addr   string
	github *githubtest.Server
	slack  *slacktest.Server
}

func newLoadWebhooks(t *testing.T, settings map[string]any) (*loadWebhooks, string) {
	t.Helper()
	gh := githubtest.New("octo/widgets", "tok")
	t.Cleanup(gh.Close)
	sk := slacktest.New("T0TEAM", "xoxb-test")
	t.Cleanup(sk.Close)
	sk.SetClock(time.Now().Add(-time.Minute))
	sk.Join("C0SUPPORT", "UBOB")
	for k, v := range map[string]string{"OC_TEST_GITHUB_TOKEN": "tok", "OC_TEST_HOOK_SECRET": "hook-secret", "OC_TEST_SLACK_BOT_TOKEN": "xoxb-test", "OC_TEST_SLACK_SIGNING": "signing-secret"} {
		t.Setenv(k, v)
	}
	var cfg, slackCfg map[string]any
	_ = json.Unmarshal(webhookConfig(gh.URL), &cfg)
	_ = json.Unmarshal(slackDaemonConfig(sk.URL), &slackCfg)
	eng := cfg["scopes"].([]any)[0].(map[string]any)
	eng["extensions"].([]any)[0].(map[string]any)["config"].(map[string]any)["manage_webhook"] = false
	support := slackCfg["scopes"].([]any)[0].(map[string]any)
	support["secret_refs"] = map[string]any{"signing": "env:OC_TEST_SLACK_SIGNING"}
	x := support["extensions"].([]any)[0].(map[string]any)
	x["secret_names"] = []string{"signing"}
	xc := x["config"].(map[string]any)
	xc["receiver"], xc["signing_secret"] = "http", "signing"
	cfg["scopes"] = []any{eng, support}
	cfg["connections"] = append(cfg["connections"].([]any), slackCfg["connections"].([]any)...)
	merged := map[string]any{"reconcile_seconds": 3600}
	for k, v := range settings {
		merged[k] = v
	}
	cfg["settings"] = merged
	data, _ := json.Marshal(cfg)
	d, stats := newLoadDrill(t, data)
	d.extra = []string{"--webhook-addr", "127.0.0.1:0"}
	d.start("")
	w := &loadWebhooks{d: d, github: gh, slack: sk}
	d.until("webhook routes", 60*time.Second, func() bool {
		r, err := d.c.Doctor(context.Background(), false)
		if err != nil {
			return false
		}
		m := listenerAddr.FindStringSubmatch(check(r, "webhooks").Detail)
		if m == nil {
			return false
		}
		w.addr = m[1]
		lag := strings.Join(check(r, "reconcile_lag").Items, " ")
		return strings.Contains(lag, "slack/support/helpdesk: reconciled") && strings.Contains(lag, "github/eng/widgets: reconciled")
	})
	sk.SetClock(time.Now())
	return w, stats
}

var loadHTTP = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

func (w *loadWebhooks) post(path string, h http.Header, body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+w.addr+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header = h
	res, err := loadHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, nil
}

// delivery prepares the i'th signed delivery: GitHub on even i, Slack on
// odd. It returns the delivery's key as it appears in its Event's delivery
// ID, and a function that sends it.
func (w *loadWebhooks) delivery(run string, i int) (string, func() (int, error)) {
	if i%2 == 0 {
		guid := fmt.Sprintf("%s-gh-%d", run, i)
		n := w.github.OpenIssue("Load "+guid, "", githubtest.User{Login: "alice", Type: "User"}, "bug")
		body := w.github.IssuesHook("opened", n)
		return guid, func() (int, error) {
			return w.post("/github/eng/widgets", githubtest.Sign("hook-secret", "issues", guid, body), body)
		}
	}
	ts := w.slack.Post("C0SUPPORT", "UALICE", "<@UBOT> load "+strconv.Itoa(i))
	body := w.slack.MessageEvent("C0SUPPORT", ts)
	// A Slack Event's delivery ID names the message by its timestamp.
	return ts, func() (int, error) {
		return w.post("/slack/support/helpdesk", slacktest.Headers("signing-secret", body, time.Now()), body)
	}
}

// TestLoadIngress is scenario 1: signed GitHub and Slack deliveries at fixed
// rates, with acknowledged deliveries per second and 2xx to route.decided.
func TestLoadIngress(t *testing.T) {
	report := newLoadReport(t, "Webhook ingress")
	count := loadEnv(t, "OC_LOAD_DELIVERIES", 400)[0]
	var rows [][]string
	for _, rate := range loadEnv(t, "OC_LOAD_RATES", 10, 50, 200) {
		w, _ := newLoadWebhooks(t, map[string]any{})
		run := "rate" + strconv.Itoa(rate)
		keys, sends := make([]string, count), make([]func() (int, error), count)
		for i := range count {
			keys[i], sends[i] = w.delivery(run, i)
		}
		var mu sync.Mutex
		acked := map[string]time.Time{}
		var ack latencies
		var refused atomic.Int64
		took := atRate(count, rate, func(i int) {
			start := time.Now()
			code, err := sends[i]()
			if err != nil || code < 200 || code >= 300 {
				refused.Add(1)
				return
			}
			at := time.Now()
			ack.add(at.Sub(start))
			mu.Lock()
			acked[keys[i]] = at
			mu.Unlock()
		})
		ctx := context.Background()
		byID := map[string]time.Time{}
		w.d.until("every delivery routed", 10*time.Minute, func() bool {
			clear(byID)
			pending := 0
			for _, scope := range []string{"eng", "support"} {
				q := url.Values{"scope_id": {scope}, "limit": {"200"}}
				for {
					page, err := w.d.c.Events(ctx, q)
					if err != nil {
						return false
					}
					for _, e := range page.Items {
						for key, at := range acked {
							if strings.Contains(e.DeliveryID, key) {
								byID[e.ID] = at
								if !e.Routed {
									pending++
								}
							}
						}
					}
					if page.NextCursor == "" {
						break
					}
					q.Set("cursor", page.NextCursor)
				}
			}
			return pending == 0 && len(byID) >= len(acked)
		})
		w.d.settle()
		var routed []time.Duration
		for _, r := range lifecycleLog(t, w.d.state) {
			if at, ok := byID[r.EventID]; ok && r.Msg == "route.decided" {
				// A route can commit before the 2xx reaches the sender.
				routed = append(routed, max(r.At.Sub(at), 0))
				delete(byID, r.EventID)
			}
		}
		if len(routed) == 0 {
			t.Fatalf("no route.decided records matched the deliveries at %d/s", rate)
		}
		perSecond := float64(len(acked)) / took.Seconds()
		s, a := spreadOf(routed), spreadOf(ack.d)
		rows = append(rows, []string{strconv.Itoa(rate), fmt.Sprintf("%.1f", perSecond), strconv.FormatInt(refused.Load(), 10), a[1], a[3], s[0], s[1], s[2], s[3]})
		stopDaemon(w.d)
	}
	report.table([]string{"Offered/s", "Acked/s", "Refused", "Ack p50", "Ack p99", "Routes seen", "2xx→routed p50", "p95", "p99"}, rows)
}

// TestLoadContention is scenario 3's daemon half: the TUI's 2 s refresh, a
// management list loop and webhook pushes together, with how long reads took
// and how long write transactions waited for and held the writer.
func TestLoadContention(t *testing.T) {
	report := newLoadReport(t, "SQLite contention under mixed load")
	w, _ := newLoadWebhooks(t, map[string]any{})
	d := w.d
	ctx, cancel := context.WithTimeout(context.Background(), loadDuration(t, "OC_LOAD_DURATION", time.Minute))
	defer cancel()
	var tui, lists latencies
	var wg sync.WaitGroup
	read := func(into *latencies) {
		start := time.Now()
		_, err1 := d.c.Works(ctx, url.Values{"limit": {"50"}})
		_, err2 := d.c.Events(ctx, url.Values{"limit": {"50"}})
		if err1 == nil && err2 == nil {
			into.add(time.Since(start))
		}
	}
	wg.Add(3)
	go func() { // The TUI's refresh.
		defer wg.Done()
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			read(&tui)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	go func() { // A management list loop.
		defer wg.Done()
		for ctx.Err() == nil {
			read(&lists)
		}
	}()
	var pushed atomic.Int64
	go func() { // Webhook pushes, 20 a second.
		defer wg.Done()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			_, send := w.delivery("mixed", i)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if code, err := send(); err == nil && code < 300 {
					pushed.Add(1)
				}
			}()
		}
	}()
	wg.Wait()
	rows := [][]string{append([]string{"TUI refresh (Work and Events)"}, spreadOf(tui.d)...), append([]string{"Management list loop"}, spreadOf(lists.d)...)}
	report.table([]string{"Reader", "Reads", "p50", "p95", "p99"}, rows)
	report.note("Webhook deliveries acknowledged: %d. Write transactions (doctor): %s.", pushed.Load(), doctorLines(t, d, "transactions"))
	stopDaemon(d)
}

// TestLoadRecovery is scenario 4: a daemon SIGKILLed mid-transaction by the
// fault injector with 100 attempts running, then an outage's backlog of
// 10,000 unrouted Events over 1,000 Work and 1,000 due timers.
func TestLoadRecovery(t *testing.T) {
	report := newLoadReport(t, "Large-backlog recovery")
	events := loadEnv(t, "OC_RECOVERY_EVENTS", 10_000)[0]
	threads := loadEnv(t, "OC_RECOVERY_WORK", 1_000)[0]
	timers := loadEnv(t, "OC_RECOVERY_TIMERS", 1_000)[0]
	running := loadEnv(t, "OC_RECOVERY_RUNNING", 100)[0]
	// A held attempt blocks on a FIFO, costing no CPU, until it is released.
	d, stats := newLoadDrill(t, nil)
	gate := filepath.Join(d.root, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(gate, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	script := `case "$OS_TRIGGER_TYPE" in load.held) [ -p "` + gate + `" ] && cat "` + gate + `" >/dev/null ;; esac; ` + loadDone
	config := loadShellConfig(script, map[string]any{"concurrency": running + 8, "job_concurrency": running + 8, "retry_delays_seconds": []int{1}})
	if err = os.WriteFile(d.config, config, 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("route:before:load.crash")
	ctx := context.Background()
	for i := range running {
		id := "held-" + strconv.Itoa(i)
		if _, err = d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: loadScope, Type: "load.held", DeliveryID: id, CorrelationKey: &client.Key{Source: "load", ExternalID: id}}, id); err != nil {
			t.Fatal(err)
		}
	}
	d.until("every held attempt running", 3*time.Minute, func() bool {
		page, e := d.c.Executions(ctx, url.Values{"scope_id": {loadScope}, "state": {"running"}, "limit": {"200"}})
		return e == nil && len(page.Items) >= running
	})
	// The next route kills the daemon inside its transaction.
	// Its answer may not arrive: the route can kill the daemon first, and
	// d.killed is what shows the fault fired.
	_, _ = d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: loadScope, Type: "load.crash", DeliveryID: "crash", CorrelationKey: &client.Key{Source: "load", ExternalID: "crash"}}, "crash")
	d.killed()
	seedOutage(t, d, events, threads, timers)
	leaseEnd := time.Now().Add(drillLease)
	d.until("the dead daemon's leases expired", time.Minute, func() bool { return time.Now().After(leaseEnd) })
	logBefore := len(lifecycleLog(t, d.state))
	start := time.Now()
	d.start("")
	ready := time.Since(start)
	var peakRSS, peakTree int64
	sample := func() {
		s := sampleDaemon(d, stats)
		peakRSS, peakTree = max(peakRSS, s.rssKB), max(peakTree, s.treeRSSKB)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(d.state, "conduit.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var firstStart, drained, fired time.Duration
	d.until("the backlog drained and every timer fired", 30*time.Minute, func() bool {
		sample()
		if firstStart == 0 {
			for _, r := range lifecycleLog(t, d.state)[logBefore:] {
				if r.Msg == "execution.started" {
					firstStart = r.At.Sub(start)
					break
				}
			}
		}
		unrouted, left, e := backlogLeft(db)
		if e != nil {
			return false
		}
		if drained == 0 && unrouted == 0 {
			drained = time.Since(start)
		}
		if fired == 0 && left == 0 {
			fired = time.Since(start)
		}
		return firstStart != 0 && drained != 0 && fired != 0
	})
	report.note("%d unrouted Events over %d Work, %d due timers and %d running attempts whose leases had expired, left by a SIGKILL inside a route's transaction.", events, threads, timers, running)
	report.table([]string{"Measure", "Value"}, [][]string{
		{"Start to management API answering", roundLatency(ready)},
		{"Start to first `execution.started`", roundLatency(firstStart)},
		{"Start to no unrouted Event", roundLatency(drained)},
		{"Start to every due timer fired", roundLatency(fired)},
		{"Peak daemon RSS", fmt.Sprintf("%.0f MiB", float64(peakRSS)/1024)},
		{"Peak RSS, daemon and packages", fmt.Sprintf("%.0f MiB", float64(peakTree)/1024)},
	})
	report.note("Pass and phases after the drain (doctor): %s.", doctorLines(t, d, "loop"))
	// Release the held attempts so the drill's cleanup finds none blocked.
	holder.Close()
	_ = os.Remove(gate)
}

// backlogLeft counts, on a read-only connection beside the daemon's, the
// Events still unrouted and the outage's timers not yet fired. A fired
// one-shot timer leaves the table.
func backlogLeft(db *sql.DB) (unrouted, timers int, err error) {
	if err = db.QueryRow(`SELECT count(*) FROM events WHERE json_extract(CAST(record AS TEXT),'$.Routed')=0`).Scan(&unrouted); err != nil {
		return 0, 0, err
	}
	err = db.QueryRow(`SELECT count(*) FROM timers WHERE id LIKE 'outage-timer-%'`).Scan(&timers)
	return unrouted, timers, err
}

// seedOutage writes what an outage leaves into the dead daemon's store:
// Events accepted and never routed, and timers come due.
func seedOutage(t *testing.T, d *drill, events, threads, timers int) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(d.state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	guard := storage.Guard{Now: now}
	data := domain.Payload{Schema: "load.v1", Data: []byte(`{"body":"` + strings.Repeat("x", 1000) + `"}`)}
	const batch = 1000
	for start := 0; start < events; start += batch {
		if err := store.Transact(ctx, guard, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, events); i++ {
				key := domain.CorrelationKey{Source: "load", ExternalID: "outage-" + strconv.Itoa(i%threads)}
				id := domain.ID("outage-event-" + strconv.Itoa(i))
				if _, _, e := runtime.AcceptInTx(tx, loadScope, domain.Event{ID: id, Type: "load.backlog", Source: "load", DeliveryID: string(id), OccurredAt: now, ReceivedAt: now, CorrelationKey: &key, Data: data}); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The timers' Events go to one Work, which gives them their scope.
	key := domain.CorrelationKey{Source: "load", ExternalID: "timer-sink"}
	if err := store.Transact(ctx, guard, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "work/timer-sink", Kind: "load.work", ScopeID: loadScope, State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{key}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < timers; start += batch {
		if err := store.Transact(ctx, guard, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, timers); i++ {
				id := "outage-timer-" + strconv.Itoa(i)
				timer := domain.Timer{ID: domain.ID(id), FireAt: now.Add(-time.Minute), Event: domain.Event{ID: domain.ID(id + "-event"), Type: "load.timer", Source: "load", DeliveryID: id, OccurredAt: now, ReceivedAt: now, CorrelationKey: &key}}
				if e := tx.PutTimer(timer, 0); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// loadClaude is a fake claude that streams a line of output every 100 ms for
// FAKE_TURN_SECONDS, then ends its turn.
const loadClaude = `#!/bin/sh
session=""
while [ $# -gt 0 ]; do
  case "$1" in --session-id|--resume) session=$2; shift ;; esac
  shift
done
echo '{"type":"system","subtype":"init"}'
IFS= read -r line
i=0; end=$(( ${FAKE_TURN_SECONDS:-5} * 10 ))
while [ "$i" -lt "$end" ]; do
  printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working %s"}]}}\n' "$i"
  i=$((i+1)); sleep 0.1
done
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"%s","num_turns":1,"total_cost_usd":0,"structured_output":{"state":"done","summary":"turn","handoff":"note"},"modelUsage":{"claude-x":{"inputTokens":1,"outputTokens":1,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}\n' "$session"
`

// soakConfig is the default capacity (4 agent turns, 4 jobs): an agents
// scope on the fake claude and a jobs scope whose shell runs for a moment.
func soakConfig(root, binary string) []byte {
	ops := []string{"read_context", "declare_outcome", "wait"}
	scope := func(id string, binding map[string]any) map[string]any {
		return map[string]any{"id": id, "name": id, "authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{binding}, "routes": []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": map[string]any{}}}, "secret_refs": map[string]any{}}
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "settings": map[string]any{}, "connections": []any{}, "scopes": []any{
		scope("agents", map[string]any{"name": "run", "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp"}, "operations": ops, "secret_names": []any{},
			"config": map[string]any{"binary": binary, "env": map[string]string{"FAKE_TURN_SECONDS": "5", "CLAUDE_CONFIG_DIR": filepath.Join(root, "claude-config")}}}),
		scope("jobs", map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
			"config": map[string]any{"command": `sleep 1; ` + loadDone}}),
	}})
	return b
}

// soakDepth is the unfinished Work each soak scope keeps: its four slots
// and two waiting, so no slot idles and no backlog builds.
const soakDepth = 6

// TestLoadSoak is scenario 5: full default capacity with new Work topped up
// for OC_SOAK_DURATION, sampling every OC_SOAK_INTERVAL.
func TestLoadSoak(t *testing.T) {
	report := newLoadReport(t, "Soak")
	duration := loadDuration(t, "OC_SOAK_DURATION", 10*time.Minute)
	interval := loadDuration(t, "OC_SOAK_INTERVAL", 10*time.Second)
	d, stats := newLoadDrill(t, nil)
	binary := filepath.Join(d.root, "claude")
	if err := os.WriteFile(binary, []byte(loadClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, soakConfig(d.root, binary), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // Closed loop: each scope keeps a few more Work than slots.
		defer wg.Done()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		next := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			for _, scope := range []string{"agents", "jobs"} {
				page, err := d.c.Works(ctx, url.Values{"scope_id": {scope}, "state": {"ready,running"}, "limit": {"8"}})
				if err != nil {
					continue
				}
				for range soakDepth - len(page.Items) {
					next++
					id := "soak-" + strconv.Itoa(next)
					_, _ = d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: scope, Type: "load.soak", DeliveryID: id, CorrelationKey: &client.Key{Source: "load", ExternalID: id}}, id)
				}
			}
		}
	}()
	var samples []procSample
	var peakAgents, peakJobs int
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for running := true; running; {
		samples = append(samples, sampleDaemon(d, stats))
		// A soak that never fills its slots hasn't measured full capacity.
		peakAgents = max(peakAgents, runningIn(d, "agents"))
		peakJobs = max(peakJobs, runningIn(d, "jobs"))
		select {
		case <-ctx.Done():
			running = false
		case <-tick.C:
		}
	}
	wg.Wait()
	if peakAgents < 4 || peakJobs < 4 {
		t.Fatalf("the soak never reached full capacity: at most %d agent turns and %d jobs running", peakAgents, peakJobs)
	}
	report.note("%s at the default capacity (peak %d agent turns and %d jobs running), %d samples %s apart. CPU is the daemon's share of one core over the run.", duration, peakAgents, peakJobs, len(samples), interval)
	report.table([]string{"Series", "Start", "Peak", "End", "Slope per hour"}, soakRows(samples))
	report.note("Pass and phases (doctor): %s. Write transactions: %s.", doctorLines(t, d, "loop"), doctorLines(t, d, "transactions"))
	stopDaemon(d)
}

// runningIn counts a scope's running attempts.
func runningIn(d *drill, scope string) int {
	page, err := d.c.Executions(context.Background(), url.Values{"scope_id": {scope}, "state": {"running"}, "limit": {"50"}})
	if err != nil {
		return 0
	}
	return len(page.Items)
}

func soakRows(samples []procSample) [][]string {
	at := make([]time.Time, len(samples))
	for i, s := range samples {
		at[i] = s.at
	}
	series := func(name, unit string, scale float64, v func(procSample) float64) []string {
		ys := make([]float64, len(samples))
		peak := 0.0
		for i, s := range samples {
			ys[i] = v(s) / scale
			peak = max(peak, ys[i])
		}
		f := func(x float64) string { return fmt.Sprintf("%.1f%s", x, unit) }
		return []string{name, f(ys[0]), f(peak), f(ys[len(ys)-1]), f(slopePerHour(at, ys))}
	}
	const mib = 1 << 20
	rows := [][]string{
		series("Daemon RSS", " MiB", 1024, func(s procSample) float64 { return float64(s.rssKB) }),
		series("RSS, daemon and descendants", " MiB", 1024, func(s procSample) float64 { return float64(s.treeRSSKB) }),
		series("Processes", "", 1, func(s procSample) float64 { return float64(s.procs) }),
		series("Goroutines", "", 1, func(s procSample) float64 { return float64(s.goroutines) }),
		series("Open files", "", 1, func(s procSample) float64 { return float64(s.fds) }),
		series("`conduit.db`", " MiB", mib, func(s procSample) float64 { return float64(s.dbBytes) }),
		series("WAL", " MiB", mib, func(s procSample) float64 { return float64(s.walBytes) }),
		series("`artifacts/`", " MiB", mib, func(s procSample) float64 { return float64(s.artifactsB) }),
	}
	if n := len(samples); n > 1 {
		if elapsed := samples[n-1].at.Sub(samples[0].at); elapsed > 0 {
			rows = append(rows, []string{"Daemon CPU", "", "", fmt.Sprintf("%.1f%%", 100*float64(samples[n-1].cpu-samples[0].cpu)/float64(elapsed)), ""})
		}
	}
	return rows
}

// TestLoadIdle is what a daemon costs with nothing to do: start-up on a
// database with retained history, CPU while idle, each process's memory and
// the state directory's disk by part.
func TestLoadIdle(t *testing.T) {
	report := newLoadReport(t, "Idle daemon")
	window := loadDuration(t, "OC_LOAD_DURATION", time.Minute)
	var rows [][]string
	for _, retained := range loadEnv(t, "OC_LOAD_HISTORY", 1_000, 50_000) {
		d, stats := newLoadDrill(t, loadShellConfig(loadDone, map[string]any{}))
		d.start("")
		stopDaemon(d)
		seedRetained(t, d, retained)
		start := time.Now()
		d.start("")
		ready := time.Since(start)
		d.settle()
		before := sampleDaemon(d, stats)
		// Idle: nothing is submitted while the window passes.
		end := time.NewTimer(window)
		<-end.C
		after := sampleDaemon(d, stats)
		cpu := 100 * float64(after.cpu-before.cpu) / float64(after.at.Sub(before.at))
		rows = append(rows, []string{strconv.Itoa(retained), roundLatency(ready), fmt.Sprintf("%.1f%%", cpu), fmt.Sprintf("%.0f MiB", float64(after.rssKB)/1024), strconv.Itoa(after.procs), fmt.Sprintf("%.0f MiB", float64(after.treeRSSKB)/1024), stateParts(d.state)})
		stopDaemon(d)
	}
	report.table([]string{"Retained Events", "Start to API", "Idle CPU", "Daemon RSS", "Processes", "All RSS", "Disk by part"}, rows)
	report.note("Wake-ups per second are not sampled: macOS exposes them only to root.")
}

// stateParts is the state directory's size by top-level entry, largest first.
func stateParts(state string) string {
	entries, err := os.ReadDir(state)
	if err != nil {
		return ""
	}
	type part struct {
		name string
		n    int64
	}
	var parts []part
	for _, e := range entries {
		path := filepath.Join(state, e.Name())
		n := fileSize(path)
		if e.IsDir() {
			n = treeBytes(path)
		}
		if n >= 1<<20 {
			parts = append(parts, part{e.Name(), n})
		}
	}
	slices.SortFunc(parts, func(a, b part) int { return cmp.Compare(b.n, a.n) })
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = fmt.Sprintf("%s %.0f MiB", p.name, float64(p.n)/(1<<20))
	}
	return strings.Join(out, ", ")
}
