//go:build faultinject

package bootstrap

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// The kill-drill crash injector exists only in binaries built with the
// faultinject tag. OPEN_CONDUIT_FAULT=<boundary>:<phase>[:<event type>]
// SIGKILLs the daemon at the first matching coordination write:
//
//	accept   AcceptEvent (optionally of the given event type)
//	route    MarkRouted (optionally of an Event of the given type)
//	claim    Claim of an execution lease
//	outcome  PutExecution entering completed
//	wait     PutWait registering an active Wait
//	timer    FireTimer (optionally for a timer of the given event type)
//	kv       an extension KV Put (optionally in the given namespace)
//
// Phase "before" kills inside the open transaction, so SQLite must roll it
// back; "after" kills immediately after the commit is acknowledged, before
// the caller can act on it. Either way nothing else in the process runs.
type fault struct {
	boundary, phase, match string
	fired                  atomic.Bool
	log                    *slog.Logger
}

func faultStore(s storage.Store, log *slog.Logger) storage.Store {
	spec := os.Getenv("OPEN_CONDUIT_FAULT")
	if spec == "" {
		return s
	}
	parts := strings.SplitN(spec, ":", 3)
	f := &fault{boundary: parts[0], log: log}
	if len(parts) > 1 {
		f.phase = parts[1]
	}
	if len(parts) > 2 {
		f.match = parts[2]
	}
	if f.phase != "before" && f.phase != "after" {
		log.Error("invalid OPEN_CONDUIT_FAULT phase; fault injection disabled")
		return s
	}
	log.Warn("fault injection armed", "boundary", f.boundary, "phase", f.phase, "match", f.match)
	return &faultyStore{inner: s, f: f}
}

func (f *fault) kill() {
	f.log.Warn("fault injected: SIGKILL", "boundary", f.boundary, "phase", f.phase, "match", f.match)
	_ = os.Stderr.Sync()
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	for {
		time.Sleep(time.Hour)
	}
}

type faultyStore struct {
	inner storage.Store
	f     *fault
}

func (s *faultyStore) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	var hit bool
	err := s.inner.Transact(ctx, g, func(tx storage.Tx) error {
		hit = false
		return fn(&faultyTx{Tx: tx, f: s.f, hit: &hit})
	})
	if err == nil && hit {
		s.f.kill()
	}
	return err
}

type faultyTx struct {
	storage.Tx
	f   *fault
	hit *bool
}

// trigger reports a matching write; "before" kills while the transaction is open.
func (t *faultyTx) trigger(boundary, typ string) {
	f := t.f
	if f.boundary != boundary || f.match != "" && f.match != typ || f.fired.Load() {
		return
	}
	if !f.fired.CompareAndSwap(false, true) {
		return
	}
	if f.phase == "before" {
		f.kill()
	}
	*t.hit = true
}

func (t *faultyTx) AcceptEvent(e domain.Event) (domain.AcceptedEvent, bool, error) {
	a, added, err := t.Tx.AcceptEvent(e)
	if err == nil && added {
		t.trigger("accept", e.Type)
	}
	return a, added, err
}

func (t *faultyTx) MarkRouted(eventID, workID domain.ID, disposition string) error {
	err := t.Tx.MarkRouted(eventID, workID, disposition)
	if err == nil {
		var typ string
		if a, e := t.Tx.Event(eventID); e == nil {
			typ = a.Event.Type
		}
		t.trigger("route", typ)
	}
	return err
}

func (t *faultyTx) Claim(id domain.ID, owner string, d time.Duration) (domain.LeaseRef, error) {
	ref, err := t.Tx.Claim(id, owner, d)
	if err == nil {
		t.trigger("claim", "")
	}
	return ref, err
}

func (t *faultyTx) PutExecution(x domain.Execution, expected domain.Revision) error {
	err := t.Tx.PutExecution(x, expected)
	if err == nil && x.State == domain.ExecutionCompleted {
		t.trigger("outcome", "")
	}
	return err
}

func (t *faultyTx) PutWait(w domain.Wait, expected domain.Revision) error {
	err := t.Tx.PutWait(w, expected)
	if err == nil && w.State == domain.WaitActive {
		t.trigger("wait", "")
	}
	return err
}

func (t *faultyTx) FireTimer(id domain.ID, expected domain.Revision, replacement *domain.Timer) error {
	var typ string
	if timer, e := t.Tx.Timer(id); e == nil {
		typ = timer.Event.Type
	}
	err := t.Tx.FireTimer(id, expected, replacement)
	if err == nil {
		t.trigger("timer", typ)
	}
	return err
}

// passes counts the host loop's completed passes in this process.
var passes atomic.Uint64

// passed is the integration tests' loop barrier, also only in faultinject
// builds. With OPEN_CONDUIT_PASS_FILE set, each completed pass of the host
// loop (routing, starts, projections, reconciles) atomically rewrites that
// file with the number of passes this process has completed. A test that
// must show something did not happen waits for passes that began after its
// point of interest to complete, instead of sleeping.
func (d *daemon) passed() {
	path := os.Getenv("OPEN_CONDUIT_PASS_FILE")
	if path == "" {
		return
	}
	n := passes.Add(1)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(n, 10)), 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

func (t *faultyTx) Put(namespace, key string, value []byte) error {
	err := t.Tx.Put(namespace, key, value)
	if err == nil {
		t.trigger("kv", namespace)
	}
	return err
}
