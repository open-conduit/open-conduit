package bootstrap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// DefaultStallAfter is how long the host loop may go without completing a
// pass before liveness fails (Options.StallAfter). A pass waits at most a
// poll interval to begin, and the calls it makes into extensions are each
// bounded (CallTimeout, the ingress page timeout), so a loop this far
// behind is stuck rather than busy.
const DefaultStallAfter = 2 * time.Minute

// healthState is the daemon's own account of whether it is alive and
// ready, which GET /v1/health and the probe listener both read.
type healthState struct {
	// phase is management.PhaseStarting until the host loop completes its
	// first pass, then PhaseRunning, and PhaseStopping once the loop has
	// returned.
	phase atomic.Value
	// lastPass is when the host loop last completed a pass (UnixNano).
	lastPass atomic.Int64
}

func (d *daemon) phase() string {
	if p, ok := d.health.phase.Load().(string); ok {
		return p
	}
	return management.PhaseStarting
}

// passDone records a completed host loop pass: the loop is alive, and the
// daemon is running once its first pass is done. passTimes records how
// long it took.
func (d *daemon) passDone() {
	now := d.now()
	d.health.lastPass.Store(now.UnixNano())
	d.health.phase.CompareAndSwap(management.PhaseStarting, management.PhaseRunning)
	d.passed()
}

// loopState is the host_loop component: ready while a pass completed
// within StallAfter, stalled once none has.
func (d *daemon) loopState() string {
	last := d.health.lastPass.Load()
	switch {
	case last == 0:
		return "starting"
	case d.now().Sub(time.Unix(0, last)) > d.o.StallAfter:
		return "stalled"
	}
	return "ready"
}

// configState is the configuration component: ready while the generation
// the configuration service holds is the one this process runs, and
// not_activated after a committed generation failed to build here (the
// daemon goes on with the one before until it restarts).
func (d *daemon) configState() string {
	c := d.current.Load()
	if c == nil || d.configs == nil {
		return "starting"
	}
	if _, gen := d.configs.Current(); strconv.FormatUint(gen, 10) != c.generation {
		return "not_activated"
	}
	return "ready"
}

// packageState is the packages component: ready while every package the
// active generation uses is running, not_started when one of them failed
// to start, and stopped when one has failed for good (its supervisor gave
// up restarting it).
func (d *daemon) packageState() string {
	if d.configs == nil {
		return "starting"
	}
	s, _ := d.configs.Current()
	state := "ready"
	for _, c := range d.catalogued() {
		if !packageUsed(s, c.m) {
			continue
		}
		r := d.runningRemote(c.Name())
		switch {
		case r == nil:
			return "not_started"
		case r.Process().Err() != nil:
			state = "stopped"
		}
	}
	return state
}

// reconcileState is the reconcile component: ready while every ingress
// source is within its reconcile interval, lagging when one is behind and
// failing when one's last scan failed (doctor's reconcile_lag lists them).
func (d *daemon) reconcileState(ctx context.Context) string {
	if d.ingress == nil {
		return "starting"
	}
	statuses, err := d.ingress.Inspect(ctx)
	if err != nil {
		return "unavailable"
	}
	state, now := "ready", d.now()
	for _, s := range statuses {
		switch failing, lagging := runtime.SourceLag(s, now, 2); {
		case failing:
			return "failing"
		case lagging:
			state = "lagging"
		}
	}
	return state
}

// databaseState is the database component: whether coordination storage
// answers a read.
func (d *daemon) databaseState(ctx context.Context) string {
	if err := storage.View(ctx, d.coord, d.now(), func(tx storage.Tx) error { _, e := tx.ListScopes(); return e }); err != nil {
		return "unavailable"
	}
	return "ready"
}

// healthComponents are GET /v1/health's components beyond the database,
// which the management server reads itself.
func (d *daemon) healthComponents(ctx context.Context) map[string]string {
	return map[string]string{"host_loop": d.loopState(), "configuration": d.configState(), "packages": d.packageState(), "reconcile": d.reconcileState(ctx)}
}

// alive is /livez: the process answers and its host loop is not stalled.
// A daemon still starting, or stopping, is alive; a startup probe on
// /readyz covers a start that never finishes.
func (d *daemon) alive() (bool, string) {
	if d.phase() != management.PhaseRunning {
		return true, "ok"
	}
	if s := d.loopState(); s != "ready" {
		return false, s
	}
	return true, "ok"
}

// probeReadTimeout bounds /readyz's storage read.
const probeReadTimeout = 2 * time.Second

// ready is /readyz: the daemon is running, storage answers, the
// configuration generation is active in this process and every package it
// uses is running. Reconcile lag and a slow host loop don't make it
// unready: webhooks are still accepted durably, and a probe that took the
// daemon out of service for an upstream's outage would only add to it.
func (d *daemon) ready(ctx context.Context) (bool, string) {
	if p := d.phase(); p != management.PhaseRunning {
		return false, p
	}
	ctx, cancel := context.WithTimeout(ctx, probeReadTimeout)
	defer cancel()
	for _, s := range []string{d.databaseState(ctx), d.configState(), d.packageState()} {
		if s != "ready" {
			return false, "not_ready"
		}
	}
	return true, "ready"
}

// probeHandler serves /livez and /readyz and nothing else: no credential,
// and no body beyond a one-word state, so it can face an orchestrator's
// network. It never mounts the management API.
func (d *daemon) probeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var check func() (bool, string)
		switch r.URL.Path {
		case "/livez":
			check = d.alive
		case "/readyz":
			check = func() (bool, string) { return d.ready(r.Context()) }
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ok, state := check()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		code := http.StatusOK
		if !ok {
			code = http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(state + "\n"))
		}
	})
}

// startProbes binds the probe listener on Options.ProbeAddr (off when
// empty). It is bound before the packages start, so an orchestrator sees
// the daemon starting, and stopped last, so it sees it stopping. A failed
// bind stops the start.
func (d *daemon) startProbes() (func(), error) {
	if d.o.ProbeAddr == "" {
		return func() {}, nil
	}
	ln, err := net.Listen("tcp", d.o.ProbeAddr)
	if err != nil {
		return nil, errors.New("probe listener: cannot bind " + d.o.ProbeAddr + " (from --probe-addr): " + err.Error())
	}
	srv := &http.Server{Handler: d.probeHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("probe listener stopped", "error", err)
		}
	}()
	d.log.Info("probe listener serving", "address", ln.Addr().String())
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
	}, nil
}
