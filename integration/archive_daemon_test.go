package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// archivedDeliveries is how many real deliveries the daemon archives. Their
// Events are small, so the test frees the space compaction returns itself
// (freeSpace) while the daemon is stopped.
const (
	archivedDeliveries    = 10
	archivedDeliveryBytes = 64 << 10
)

// freeSpace writes mib MiB to the stopped daemon's database and deletes it,
// leaving that much in free pages. With older set the database first goes
// back to the mode installs from before incremental compaction are in.
func freeSpace(t *testing.T, path string, mib int, older bool) {
	t.Helper()
	ctx := context.Background()
	if older {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, statement := range []string{"PRAGMA auto_vacuum=0", "VACUUM"} {
			if _, err = db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	value := bytes.Repeat([]byte("x"), 1<<20)
	for i := range mib {
		if err = s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			return tx.Put("test.free", fmt.Sprint(i), value)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range mib {
		if err = s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			return tx.Delete("test.free", fmt.Sprint(i))
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDaemonArchivesAndCompacts runs the real daemon with archiving on, on
// the traffic it exists for: GitHub deliveries that open no Work. It ages
// them by restarting with the clock ahead, then checks the file shrank and
// that nothing an operator or a redelivery relies on changed.
func TestDaemonArchivesAndCompacts(t *testing.T) {
	ctx := context.Background()
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	var cfg map[string]any
	if err := json.Unmarshal(webhookConfig(fake.URL), &cfg); err != nil {
		t.Fatal(err)
	}
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	scope["extensions"].([]any)[0].(map[string]any)["config"].(map[string]any)["manage_webhook"] = false
	settings := cfg["settings"].(map[string]any)
	settings["archive_finished_days"], settings["archive_unmatched_days"] = 30, 14
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := newDrill(t)
	if err = os.WriteFile(d.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--retention-interval", "100ms"}
	d.start("")
	// addr is the webhook listener's address. Only a first start can also
	// wait for a reconcile: a daemon killed mid-scan leaves the scan's lease
	// behind, and the next start can't scan until that lease runs out.
	addr := func(reconciled bool) string {
		t.Helper()
		var found, unmet string
		if !eventually(30*time.Second, func() bool {
			r, err := d.c.Doctor(ctx, false)
			if err != nil {
				unmet = "doctor failed: " + err.Error()
				return false
			}
			webhooks := check(r, "webhooks")
			m := listenerAddr.FindStringSubmatch(webhooks.Detail)
			if m == nil {
				unmet = "no listener in doctor's webhooks check: " + webhooks.Detail
				return false
			}
			found = m[1]
			lag := check(r, "reconcile_lag")
			if reconciled && !strings.Contains(strings.Join(lag.Items, " "), ": reconciled") {
				unmet = fmt.Sprintf("no completed reconcile: %s %v", lag.Code, lag.Items)
				return false
			}
			return true
		}) {
			t.Fatalf("timed out waiting for webhooks listening: %s", unmet)
		}
		return found
	}
	n := fake.OpenIssue("Archived", "", githubtest.User{Login: "alice", Type: "User"}, "backlog")
	body := paddedDelivery(t, fake.IssuesHook("edited", n), archivedDeliveryBytes)
	post := func(at, guid string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+at+"/github/eng/widgets", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = githubtest.Sign("hook-secret", "issues", guid, body)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("delivery %s: %d", guid, res.StatusCode)
		}
	}
	listening := addr(true)
	for i := range archivedDeliveries {
		post(listening, fmt.Sprintf("guid-%d", i))
	}
	deliveries := func() map[string]client.Event {
		out := map[string]client.Event{}
		for _, e := range gateEvents(d.c, url.Values{}) {
			if e.Type == "github.delivery" {
				out[e.DeliveryID] = e
			}
		}
		return out
	}
	d.until("the deliveries' Events", 60*time.Second, func() bool { return len(deliveries()) == archivedDeliveries })
	d.settle()
	originals := map[string]client.Event{}
	for delivery, listed := range deliveries() {
		e, err := d.c.Event(ctx, listed.ID)
		if err != nil {
			t.Fatal(err)
		}
		originals[delivery] = e
	}
	dbPath := filepath.Join(d.state, "conduit.db")
	size := func() int64 {
		info, err := os.Stat(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	d.settle()

	// Inside the horizon nothing moves.
	if got := archivedCount(t, d); got != 0 {
		t.Fatalf("%d Events archived inside the horizon", got)
	}

	// An install from before incremental compaction, with enough free to
	// rebuild: the rewrite happens at the start, before any package runs.
	d.kill()
	freeSpace(t, dbPath, 96, true)
	full := size()
	d.environ = []string{"OPEN_CONDUIT_CLOCK_SKEW=" + (40 * 24 * time.Hour).String()}
	d.start("")
	if got := size(); got > full-(64<<20) {
		t.Fatalf("after the rebuild the file is %d bytes, from %d", got, full)
	}
	log, err := os.ReadFile(d.log.Name())
	if err != nil {
		t.Fatal(err)
	}
	_, thisStart, _ := bytes.Cut(log, []byte("--- start 2"))
	rebuilt, started := bytes.Index(thisStart, []byte("database rebuilt")), bytes.Index(thisStart, []byte("extension started"))
	if rebuilt < 0 || started < 0 || rebuilt > started {
		t.Fatalf("rebuilt at %d, first package at %d in the start's log", rebuilt, started)
	}
	d.until("Events archived", 60*time.Second, func() bool { return archivedCount(t, d) >= archivedDeliveries })

	// Now incremental: the retention pass returns space while it serves.
	d.kill()
	freeSpace(t, dbPath, 48, false)
	full = size()
	d.start("")
	d.until("the file to shrink", 60*time.Second, func() bool { return size() < full-(16<<20) })

	check := func(when string) {
		t.Helper()
		for delivery, want := range originals {
			got, err := d.c.Event(ctx, want.ID)
			if err != nil {
				t.Fatalf("%s: %s: %v", when, delivery, err)
			}
			if got.Sequence != want.Sequence || !bytes.Equal(got.Data.Data, want.Data.Data) {
				t.Fatalf("%s: %s came back changed", when, delivery)
			}
		}
	}
	check("archived")

	// A redelivery is still a duplicate, with its original gone from the live log.
	listening = addr(false)
	for i := range archivedDeliveries {
		post(listening, fmt.Sprintf("guid-%d", i))
	}
	d.settle()
	if got := deliveries(); len(got) != archivedDeliveries {
		t.Fatalf("%d delivery Events after redelivery, want %d", len(got), archivedDeliveries)
	}

	// A kill and a restart change none of it.
	d.kill()
	d.start("")
	check("restarted")
	if status := archiveCheck(t, d).Status; status != "ok" {
		t.Fatalf("doctor's event_archive is %q", status)
	}
}

func archiveCheck(t *testing.T, d *drill) client.Check {
	t.Helper()
	report, err := d.c.Doctor(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Name == "event_archive" {
			return c
		}
	}
	t.Fatal("doctor has no event_archive check")
	return client.Check{}
}

// archivedCount is the count doctor reports.
func archivedCount(t *testing.T, d *drill) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(archiveCheck(t, d).Detail, "%d Events archived", &n); err != nil {
		return 0
	}
	return n
}
