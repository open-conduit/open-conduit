package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// stopAndMove stops the daemon gracefully and renames its state directory
// into a new tree, as a move to another machine would land it.
func (d *drill) stopAndMove() {
	d.t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		d.t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(10 * time.Second):
		d.t.Fatal("daemon ignored SIGTERM")
	}
	moved := filepath.Join(d.root, "elsewhere", "tree", "state")
	if err := os.MkdirAll(filepath.Dir(moved), 0o700); err != nil {
		d.t.Fatal(err)
	}
	if err := os.Rename(d.state, moved); err != nil {
		d.t.Fatal(err)
	}
	d.state, d.c = moved, nil
}

func (d *drill) launchCount() int {
	d.t.Helper()
	return len(strings.Split(strings.TrimSpace(readFile(d.t, filepath.Join(d.root, "launches"))), "\n"))
}

func carries(s client.DrainStatus, code string) bool {
	return slices.ContainsFunc(s.Carried, func(r client.DrainReason) bool { return r.Code == code })
}

// TestDaemonDrainAndMove drains daemon A with a Work parked, moves its state
// to a new directory tree and starts daemon B on it. A wake that arrives
// during the drain starts nothing on A and runs on B, and a timer that comes
// due across the move fires once. drill.verify fails on a segment launched
// twice, an attempt left live or a lost root Event.
func TestDaemonDrainAndMove(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	drained := func(d *drill) client.DrainStatus {
		t.Helper()
		st, err := d.c.BeginDrain(ctx, "moving")
		if err != nil {
			t.Fatal(err)
		}
		d.until("safe to move", 20*time.Second, func() bool {
			st, err = d.c.DrainStatus(ctx)
			return err == nil && st.Safe
		})
		return st
	}
	t.Run("a wake during the drain runs on the new daemon", func(t *testing.T) {
		d := newDrill(t)
		d.start("")
		root, err := d.submit("drill.event")
		if err != nil {
			t.Fatal(err)
		}
		w := d.waitState("waiting")
		drained(d)
		before := d.launchCount()
		if err := d.wake(w); err != nil {
			t.Fatal(err)
		}
		d.until("the wake queued for the next daemon", 20*time.Second, func() bool {
			st, err := d.c.DrainStatus(ctx)
			return err == nil && st.Safe && carries(st, "unread_input")
		})
		// Full loop passes after the wake: a daemon that would start it has.
		d.settle()
		if got := d.launchCount(); got != before {
			t.Fatalf("a draining daemon launched %d attempts", got-before)
		}
		d.stopAndMove()
		d.start("")
		if st, err := d.c.DrainStatus(ctx); err != nil || st.Draining {
			t.Fatalf("the moved daemon is still draining: %+v, %v", st, err)
		}
		d.verify(root, "woken by drill.reply", false)
	})
	t.Run("a timer due across the move fires once", func(t *testing.T) {
		d := newDrill(t)
		d.start("")
		root, err := d.submit("drill.timer")
		if err != nil {
			t.Fatal(err)
		}
		d.waitState("waiting")
		drained(d)
		d.stopAndMove()
		d.start("")
		d.verify(root, "woken by core.wait.timeout", false)
		w, _ := d.work()
		timeouts, err := d.c.Events(ctx, map[string][]string{"work_id": {w.ID}, "type": {"core.wait.timeout"}})
		if err != nil || len(timeouts.Items) != 1 {
			t.Fatalf("timeouts fired: %d, %v", len(timeouts.Items), err)
		}
	})
}

// TestDaemonRestoreOntoAnotherPath takes a bundle of a drained daemon with a
// Claude Work handed back, and restores it under another path with a new
// config whose Claude config directory is empty, as on another machine. The
// daemon starts on that config, the Work is still the only one, and its next
// turn starts afresh from the handoff note because the transcript didn't
// travel.
func TestDaemonRestoreOntoAnotherPath(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	d := newDrill(t)
	binary := filepath.Join(d.root, "claude")
	if err := os.WriteFile(binary, []byte(daemonClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, claudeDrillConfig(d.root, binary), 0o600); err != nil {
		t.Fatal(err)
	}
	submit := func(typ, delivery string) {
		t.Helper()
		if _, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "drill", Type: typ, DeliveryID: delivery, CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-"+delivery); err != nil {
			t.Fatal(err)
		}
	}
	invocations := func() []string {
		b, _ := os.ReadFile(filepath.Join(d.root, "invocations"))
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	d.start("")
	submit("agent.start", "start-1")
	var w client.Work
	d.until("first turn handed back", 30*time.Second, func() bool {
		var ok bool
		if w, ok = d.work(); !ok {
			return false
		}
		x := d.executions(w.ID)
		return len(x) == 1 && x[0].State == "handoff"
	})
	session := strings.TrimPrefix(strings.Fields(invocations()[0])[1], "session=")
	if _, err := d.c.BeginDrain(ctx, "moving"); err != nil {
		t.Fatal(err)
	}
	d.until("safe to move", 20*time.Second, func() bool {
		st, err := d.c.DrainStatus(ctx)
		return err == nil && st.Safe
	})
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-d.exited

	bin := daemonBinary(t)
	bundle := filepath.Join(d.root, "bundle")
	if out, err := exec.Command(bin, "--data-dir", d.state, "backup", "create", bundle, "--unsigned", "--config", d.config).CombinedOutput(); err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	other := filepath.Join(d.root, "other-home")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := strings.ReplaceAll(string(claudeDrillConfig(d.root, binary)), filepath.Join(d.root, "claude-config"), filepath.Join(other, "claude-config"))
	config := filepath.Join(other, "config.json")
	if err := os.WriteFile(config, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(other, "state")
	if out, err := exec.Command(bin, "--data-dir", state, "backup", "restore", bundle, "--to", state, "--start-config", config, "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	d.state, d.config, d.c = state, config, nil
	d.start("")
	if got, ok := d.work(); !ok || got.ID != w.ID {
		t.Fatalf("after the restore: %+v (%v), want Work %s", got, ok, w.ID)
	}
	submit("agent.more", "more-1")
	d.until("the next turn handed back", 60*time.Second, func() bool {
		for _, x := range d.executions(w.ID) {
			if x.Attempt > 1 && x.State == "handoff" {
				return true
			}
		}
		return false
	})
	inv := invocations()
	// The transcript is gone, so the session can't be resumed: it starts
	// again under its ID, from the note.
	if len(inv) != 2 || !strings.Contains(inv[1], "session="+session+" resume=0") {
		t.Fatalf("the turn after the move should start afresh: %v", inv)
	}
	stdin := readFile(t, filepath.Join(d.root, "stdin"))
	if !strings.Contains(stdin, "note 1") {
		t.Fatalf("the new session wasn't given the handoff note: %s", stdin)
	}
	if got, ok := d.work(); !ok || got.ID != w.ID {
		t.Fatalf("a duplicate Work appeared: %+v", got)
	}
}
