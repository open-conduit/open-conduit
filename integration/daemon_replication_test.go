package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// litestreamEnv names the Litestream binary the replication drill runs. `make
// drill-replication` builds the pinned version and sets it.
const litestreamEnv = "OPEN_CONDUIT_LITESTREAM"

// TestDaemonReplicationDrill is the supported WAL replication setup end to
// end: replicate a real daemon through commits, Reclaim and Rebuild, lose the
// host's disk, restore what the replica holds and start on it. The restored
// daemon launches nothing until the drain ends, then finishes the Work.
func TestDaemonReplicationDrill(t *testing.T) {
	litestream := os.Getenv(litestreamEnv)
	if litestream == "" {
		t.Skip(litestreamEnv + " is unset; run make drill-replication")
	}
	ctx := context.Background()
	d := newDrill(t)
	database := filepath.Join(d.state, "conduit.db")
	config := filepath.Join(d.root, "litestream.yml")
	if err := os.WriteFile(config, fmt.Appendf(nil, `logging: {level: info}
dbs:
  - path: %s
    truncate-page-n: 0
    checkpoint-interval: 1s
    min-checkpoint-page-count: 64
    replica:
      type: file
      path: %s
      sync-interval: 100ms
`, database, filepath.Join(d.root, "replica")), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := os.Create(filepath.Join(d.root, "litestream.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	// Started first: it waits for the daemon to create the database.
	replicator := exec.Command(litestream, "replicate", "-config", config)
	replicator.Stdout, replicator.Stderr = logs, logs
	if err := replicator.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { _ = replicator.Wait(); close(stopped) }()
	stopReplicator := func() {
		_ = replicator.Process.Signal(syscall.SIGKILL)
		<-stopped
	}
	defer stopReplicator()
	defer func() {
		if t.Failed() {
			out, _ := os.ReadFile(logs.Name())
			t.Logf("litestream log:\n%s", out)
		}
	}()

	d.start("")
	root, err := d.submit("drill.event")
	if err != nil {
		t.Fatal(err)
	}
	w := d.waitState("waiting")

	// What a daemon does to the file on its own only when archiving has left
	// it enough free space, driven directly while the replicator runs.
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-d.exited
	store, err := sqlite.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	guard := storage.Guard{Now: time.Now()}
	value := make([]byte, 8<<10)
	if err := store.Transact(ctx, guard, func(tx storage.Tx) error {
		for i := range 256 {
			if err := tx.Put("drill.replication", fmt.Sprint(i), value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, guard, func(tx storage.Tx) error {
		for i := range 192 {
			if err := tx.Delete("drill.replication", fmt.Sprint(i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reclaim(ctx, 512<<10); err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, guard, func(tx storage.Tx) error {
		return tx.Put("drill.replication", "last", []byte("committed before the disk was lost"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	d.start("")

	// The replica has caught up once a restore of it holds the last commit.
	recovered := filepath.Join(d.root, "recovered", "conduit.db")
	if err := os.MkdirAll(filepath.Dir(recovered), 0o700); err != nil {
		t.Fatal(err)
	}
	clear := func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(recovered + suffix)
		}
	}
	restored := func() bool {
		clear()
		if err := exec.Command(litestream, "restore", "-config", config, "-o", recovered, database).Run(); err != nil {
			return false
		}
		db, err := sql.Open("sqlite", "file:"+recovered+"?mode=ro")
		if err != nil {
			return false
		}
		defer db.Close()
		var kv, works int
		if err := db.QueryRow(`SELECT count(*) FROM kv WHERE namespace='drill.replication' AND key='last'`).Scan(&kv); err != nil {
			return false
		}
		if err := db.QueryRow(`SELECT count(*) FROM works WHERE id=?`, w.ID).Scan(&works); err != nil {
			return false
		}
		return kv == 1 && works == 1
	}
	d.until("the replica to catch up", 60*time.Second, restored)

	// The host goes: both processes die and the disk is gone.
	d.kill()
	stopReplicator()
	before := d.launchCount()
	if err := os.RemoveAll(d.state); err != nil {
		t.Fatal(err)
	}
	clear()
	if out, err := exec.Command(litestream, "restore", "-config", config, "-o", recovered, database).CombinedOutput(); err != nil {
		t.Fatalf("litestream restore: %v\n%s", err, out)
	}
	state := filepath.Join(d.root, "restored", "state")
	out, err := exec.Command(daemonBinary(t), "--data-dir", state, "backup", "restore-replica", recovered, "--to", state, "--config", d.config).CombinedOutput()
	if err != nil {
		t.Fatalf("restore-replica: %v\n%s", err, out)
	}
	var report struct {
		Drained bool `json:"drained"`
	}
	if err := json.Unmarshal(out, &report); err != nil || !report.Drained {
		t.Fatalf("restore-replica reported %s: %v", strings.TrimSpace(string(out)), err)
	}

	d.state, d.c = state, nil
	d.start("")
	got, ok := d.work()
	if !ok || got.ID != w.ID {
		t.Fatalf("after the restore: %+v (%v), want Work %s", got, ok, w.ID)
	}
	if st, err := d.c.DrainStatus(ctx); err != nil || !st.Draining {
		t.Fatalf("the restored daemon isn't draining: %+v, %v", st, err)
	}
	if err := d.wake(got); err != nil {
		t.Fatal(err)
	}
	d.settle()
	if n := d.launchCount(); n != before {
		t.Fatalf("a drained restore launched %d attempts", n-before)
	}
	if _, err := d.c.EndDrain(ctx); err != nil {
		t.Fatal(err)
	}
	d.verify(root, "woken by drill.reply", false)
}
