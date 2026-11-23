package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// signalWriter collects a daemon's log and closes seen at the first line
// holding want, so a test waits on the event rather than on a clock.
type signalWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	seen chan struct{}
	once sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

const takeoverConfig = `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"org","name":"org"}]}`

func (w *signalWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type servedDaemon struct {
	state  string
	log    *signalWriter
	ready  chan struct{}
	done   chan error
	cancel context.CancelFunc
}

// serveShared runs a real daemon on its own state directory over the shared
// database at location, as a separate host would.
func serveShared(t *testing.T, root, name, location string, onReady func()) *servedDaemon {
	t.Helper()
	state := filepath.Join(root, name)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	choice := `{"backend":"` + sharedTestBackend + `","location":"` + location + `"}`
	if err := os.WriteFile(filepath.Join(state, storageFile), []byte(choice), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(takeoverConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &servedDaemon{state: state, ready: make(chan struct{}), done: make(chan error, 1), cancel: cancel,
		log: &signalWriter{want: "another instance holds this state", seen: make(chan struct{})}}
	t.Cleanup(cancel)
	go func() {
		d.done <- Serve(ctx, Options{
			StateDir: state, ConfigPath: filepath.Join(state, "config.json"), Socket: filepath.Join(state, "m.sock"),
			ToolbeltDir: state, Log: d.log, NoTunnel: true, NoTelemetry: true, BackupInterval: -1,
			PollInterval: 10 * time.Millisecond, instanceRenew: 10 * time.Millisecond, instanceRetry: 10 * time.Millisecond,
			Ready: func() {
				if onReady != nil {
					onReady()
				}
				close(d.ready)
			},
		})
	}()
	return d
}

func await[T any](t *testing.T, d *servedDaemon, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case err := <-d.done:
		t.Fatalf("%s: the daemon exited: %v\n%s", what, err, d.log.text())
	case <-time.After(time.Minute):
		t.Fatalf("%s: timed out\n%s", what, d.log.text())
	}
	panic("unreachable")
}

// Two real daemons on one shared database: the second waits as a standby,
// serves once the first stops, and stops itself when it is superseded.
func TestStandbyTakesOverThroughServe(t *testing.T) {
	// A short root: a Unix socket path has a small limit.
	root, err := os.MkdirTemp("", "oc-takeover-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	location := filepath.Join(root, "shared.db")

	first := serveShared(t, root, "a", location, nil)
	await(t, first, first.ready, "the first daemon serving")

	var firstStopped atomic.Bool
	var servedEarly atomic.Bool
	second := serveShared(t, root, "b", location, func() { servedEarly.Store(!firstStopped.Load()) })
	await(t, second, second.log.seen, "the second daemon waiting as a standby")

	firstStopped.Store(true)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first daemon's clean stop: %v\n%s", err, first.log.text())
	}
	await(t, second, second.ready, "the standby taking over")
	if servedEarly.Load() {
		t.Fatal("the standby served while the first daemon held the state")
	}

	// A third party takes the lease: the serving daemon must stop on its own.
	ctx := context.Background()
	direct, err := sqlite.Open(ctx, location)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	owner, err := instanceOwner(second.state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.AcquireInstance(ctx, owner, time.Hour); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-second.done:
		if !errors.Is(err, storage.ErrInstanceFenced) {
			t.Fatalf("the superseded daemon exited with %v\n%s", err, second.log.text())
		}
	case <-time.After(time.Minute):
		t.Fatalf("the superseded daemon kept running\n%s", second.log.text())
	}
}
