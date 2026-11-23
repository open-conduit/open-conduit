package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// takeoverTTL is the instance lease in the drill: long enough that a loaded
// machine still renews it, short enough to wait out.
const takeoverTTL = 3 * time.Second

// instanceDaemon is one real `serve` process on a state directory of its own
// over the drill's shared database, as a separate host would be.
type instanceDaemon struct {
	t      *testing.T
	state  string
	logs   string
	cmd    *exec.Cmd
	exited chan struct{}
	c      *client.Client
	// tokenDir is the state directory whose operator token this daemon
	// accepts. The credential is in the shared database, and only the
	// instance that first created it wrote the token to its own disk.
	tokenDir string
}

func startInstance(t *testing.T, d *drill, name, shared string) *instanceDaemon {
	t.Helper()
	state := filepath.Join(d.root, name)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	choice, _ := json.Marshal(map[string]string{"backend": "faultinject-shared", "location": shared})
	if err := os.WriteFile(filepath.Join(state, "storage.json"), choice, 0o600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(d.root, name+".log")
	out, err := os.Create(logs)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(daemonBinary(t), "--data-dir", state, "serve", "--config", d.config, "--poll-interval", "50ms", "--lease-duration", drillLease.String())
	cmd.Env = append(os.Environ(), "OPEN_CONDUIT_INSTANCE_TTL="+takeoverTTL.String())
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	i := &instanceDaemon{t: t, state: state, logs: logs, cmd: cmd, exited: make(chan struct{}), tokenDir: filepath.Join(d.root, "a")}
	go func() { _ = cmd.Wait(); out.Close(); close(i.exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-i.exited
		if t.Failed() {
			b, _ := os.ReadFile(logs)
			t.Logf("%s log:\n%s", name, b)
		}
	})
	return i
}

func (i *instanceDaemon) log() string {
	b, _ := os.ReadFile(i.logs)
	return string(b)
}

// serving reports whether the management API answers.
func (i *instanceDaemon) serving() bool {
	if i.c == nil {
		f, err := client.ReadTokenFile(filepath.Join(i.tokenDir, "private", "operator.token"))
		if err != nil {
			return false
		}
		if i.c, err = client.New("unix://"+filepath.Join(i.state, "management.sock"), f.Token); err != nil {
			return false
		}
	}
	_, err := i.c.Health(context.Background())
	return err == nil
}

// tenure is the instance lease as the shared database records it.
type tenure struct {
	owner   string
	epoch   int64
	expires int64
}

func readTenure(t *testing.T, db *sql.DB) tenure {
	t.Helper()
	var r tenure
	var expires sql.NullInt64
	if err := db.QueryRow(`SELECT owner, epoch, expires_at FROM instance_lease WHERE id=1`).Scan(&r.owner, &r.epoch, &expires); err != nil {
		t.Fatal(err)
	}
	r.expires = expires.Int64
	return r
}

// takeoverFixture has a holder running one in-flight attempt, a standby
// waiting behind it, and a read connection on the shared database.
type takeoverFixture struct {
	d               *drill
	holder, standby *instanceDaemon
	db              *sql.DB
	work            client.Work
	attempt         client.Execution
	before          tenure
}

func newTakeover(t *testing.T) *takeoverFixture {
	t.Helper()
	d := newDrill(t)
	shared := filepath.Join(d.root, "shared.db")
	holder := startInstance(t, d, "a", shared)
	waitFor(t, "the holder serving", holder.serving)
	if _, err := holder.c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: "drill", Type: "drill.slow", DeliveryID: "takeover-1", CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-takeover-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the attempt running", func() bool {
		_, err := os.Stat(filepath.Join(d.root, "pid"))
		return err == nil
	})
	page, err := holder.c.Works(context.Background(), url.Values{"scope_id": {"drill"}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("works: %+v %v", page.Items, err)
	}
	xs, err := holder.c.Executions(context.Background(), url.Values{"work_id": {page.Items[0].ID}})
	if err != nil || len(xs.Items) != 1 || xs.Items[0].State != "running" {
		t.Fatalf("executions: %+v %v", xs.Items, err)
	}
	db, err := sql.Open("sqlite", "file:"+shared+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	standby := startInstance(t, d, "b", shared)
	waitFor(t, "the second daemon waiting as a standby", func() bool {
		return strings.Contains(standby.log(), "another instance holds this state")
	})
	if standby.serving() {
		t.Fatal("the standby served while the holder's lease was live")
	}
	return &takeoverFixture{d: d, holder: holder, standby: standby, db: db, work: page.Items[0], attempt: xs.Items[0], before: readTenure(t, db)}
}

// awaitTakeover watches the lease and fails if it changes hands before the
// expiry the old holder last wrote. The old holder must already be unable to
// renew. A successor's acquire precedes the observation, so observing the new
// epoch before the expiry proves an early takeover.
func (f *takeoverFixture) awaitTakeover(t *testing.T) tenure {
	t.Helper()
	last := readTenure(t, f.db)
	if last.epoch != f.before.epoch {
		t.Fatalf("the lease moved before the holder stopped: %+v -> %+v", f.before, last)
	}
	var now tenure
	waitFor(t, "the standby acquiring the lease", func() bool {
		now = readTenure(t, f.db)
		if now.epoch == last.epoch {
			return false
		}
		if at := time.Now().UnixNano(); at < last.expires {
			t.Fatalf("the standby took over %s before the lease expired", time.Duration(last.expires-at))
		}
		return true
	})
	if now.epoch <= last.epoch || now.owner == last.owner {
		t.Fatalf("takeover: %+v -> %+v", last, now)
	}
	waitFor(t, "the standby serving", f.standby.serving)
	return now
}

// The holder is killed with no release. The standby waits out the lease, fences
// the holder's Execution lease, and the attempt fails and retries under it.
func TestDaemonTakeoverFromKilledHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	f := newTakeover(t)
	if err := f.holder.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-f.holder.exited
	f.awaitTakeover(t)

	ctx := context.Background()
	var xs []client.Execution
	waitFor(t, "the attempt failing and retrying", func() bool {
		page, err := f.standby.c.Executions(ctx, url.Values{"work_id": {f.work.ID}})
		if err != nil {
			return false
		}
		xs = page.Items
		return len(xs) >= 2
	})
	for _, x := range xs {
		if x.ID == f.attempt.ID {
			if x.State == "running" && x.Lease.Epoch == f.attempt.Lease.Epoch {
				t.Fatalf("the old attempt kept its lease: %+v", x)
			}
			if x.Lease.Owner == f.attempt.Lease.Owner {
				t.Fatalf("the old holder still owns the attempt: %+v", x.Lease)
			}
		}
	}
	waitFor(t, "the retry parking", func() bool {
		w, err := f.standby.c.Work(ctx, f.work.ID)
		return err == nil && w.State == "waiting"
	})
}

// dataVersion changes when any other connection commits to the database.
func dataVersion(t *testing.T, c *sql.Conn) int64 {
	t.Helper()
	var v int64
	if err := c.QueryRowContext(context.Background(), `PRAGMA data_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// pauseIdle stops i between write transactions. SQLite's write lock is a file
// lock that a stopped process keeps, which a database server would time out,
// so a daemon stopped inside a transaction would block everyone else.
func (f *takeoverFixture) pauseIdle(t *testing.T, i *instanceDaemon) {
	t.Helper()
	writer, err := sql.Open("sqlite", "file:"+filepath.Join(f.d.root, "shared.db")+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	waitFor(t, "a daemon paused between transactions", func() bool {
		if err := i.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		// The signal is asynchronous: probe only once it has landed.
		waitUntil(t, "the process stopping", func() bool {
			out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(i.cmd.Process.Pid)).Output()
			return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "T")
		})
		if _, err := writer.Exec(`BEGIN IMMEDIATE; ROLLBACK`); err != nil {
			_ = i.cmd.Process.Signal(syscall.SIGCONT)
			return false
		}
		return true
	})
}

// A paused holder that wakes after the takeover stops and commits nothing. The
// successor is paused too, between transactions, so any commit in that window
// can only be the old holder's.
func TestDaemonPausedHolderCommitsNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	f := newTakeover(t)
	f.pauseIdle(t, f.holder)
	f.awaitTakeover(t)

	ctx := context.Background()
	conn, err := f.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The detector's control: it sees the successor's own commits.
	seen := dataVersion(t, conn)
	waitFor(t, "a commit by the successor", func() bool { return dataVersion(t, conn) != seen })

	f.pauseIdle(t, f.standby)
	before := dataVersion(t, conn)
	if err := f.holder.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.holder.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the superseded holder kept running")
	}
	if !strings.Contains(f.holder.log(), "another instance took over this state") {
		t.Fatalf("the holder did not stop as superseded:\n%s", f.holder.log())
	}
	if after := dataVersion(t, conn); after != before {
		t.Fatalf("the superseded holder committed: data_version %d -> %d", before, after)
	}
	if err := f.standby.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the successor serving again", f.standby.serving)
}
