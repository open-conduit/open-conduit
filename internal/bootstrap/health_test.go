package bootstrap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/internal/telemetry"
	"github.com/open-conduit/open-conduit/storage"
)

func probe(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

// The probes follow the daemon's phase and its host loop: alive but not
// ready while it starts, unready while it stops, and not alive once the
// loop has gone StallAfter without completing a pass. Readiness needs
// storage to answer and the configuration to be active.
func TestProbesFollowPhaseAndLoop(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	d := &daemon{o: Options{StallAfter: time.Minute}, now: func() time.Time { return now }}
	d.health.phase.Store(management.PhaseStarting)
	h := d.probeHandler()
	for path, want := range map[string]string{"/livez": "200 ok\n", "/readyz": "503 starting\n"} {
		if code, body := probe(t, h, path); strconv.Itoa(code)+" "+body != want {
			t.Fatalf("starting %s: %d %q", path, code, body)
		}
	}
	// The first pass makes it running. A degraded daemon (here, with no
	// configuration built or package started) is still ready while storage
	// answers, and unready once it doesn't.
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.coord = store
	d.passDone()
	if p := d.phase(); p != management.PhaseRunning {
		t.Fatalf("phase after a pass: %s", p)
	}
	if d.configState() == "ready" || d.packageState() == "ready" {
		t.Fatal("the daemon under test should be degraded")
	}
	if code, body := probe(t, h, "/readyz"); code != http.StatusOK || body != "ready\n" {
		t.Fatalf("degraded but serving: %d %q", code, body)
	}
	store.Close()
	if s := d.databaseState(context.Background()); s != "unavailable" {
		t.Fatalf("closed database: %s", s)
	}
	if code, body := probe(t, h, "/readyz"); code != http.StatusServiceUnavailable || body != "not_ready\n" {
		t.Fatalf("storage down: %d %q", code, body)
	}
	// Alive while passes complete; stalled once none has for StallAfter.
	now = now.Add(59 * time.Second)
	if code, body := probe(t, h, "/livez"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("recent pass: %d %q", code, body)
	}
	now = now.Add(2 * time.Second)
	if code, body := probe(t, h, "/livez"); code != http.StatusServiceUnavailable || body != "stalled\n" {
		t.Fatalf("stalled loop: %d %q", code, body)
	}
	if s := d.loopState(); s != "stalled" {
		t.Fatalf("host_loop: %s", s)
	}
	d.passDone()
	if s := d.loopState(); s != "ready" {
		t.Fatalf("host_loop after a pass: %s", s)
	}
	// Stopping: alive, not ready, and a pass on the way out doesn't make
	// it running again.
	d.health.phase.Store(management.PhaseStopping)
	d.passDone()
	if code, body := probe(t, h, "/readyz"); code != http.StatusServiceUnavailable || body != "stopping\n" {
		t.Fatalf("stopping readyz: %d %q", code, body)
	}
	if code, _ := probe(t, h, "/livez"); code != http.StatusOK {
		t.Fatalf("stopping livez: %d", code)
	}
	if code, _ := probe(t, h, "/v1/health"); code != http.StatusNotFound {
		t.Fatalf("management path on the probe listener: %d", code)
	}
}

// GET /v1/health's state is the phase while starting or stopping, and
// degraded while running with any component not ready.
func TestHealthState(t *testing.T) {
	ready := map[string]string{"database": "ready", "host_loop": "ready"}
	stalled := map[string]string{"database": "ready", "host_loop": "stalled"}
	for _, c := range []struct {
		phase string
		comp  map[string]string
		want  string
	}{
		{management.PhaseStarting, ready, "starting"},
		{management.PhaseRunning, ready, "ready"},
		{management.PhaseRunning, stalled, "degraded"},
		{management.PhaseStopping, ready, "stopping"},
	} {
		if got := management.HealthState(c.phase, c.comp); got != c.want {
			t.Fatalf("%s %v: %s, want %s", c.phase, c.comp, got, c.want)
		}
	}
}

// The host's instruments reach a reader: each pass's duration, and gauges
// of the loop, the unrouted backlog, reconcile, leases and the telemetry
// pipeline, none of them with an identifier as an attribute.
func TestHostMetrics(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	d := &daemon{o: Options{StallAfter: time.Minute}, now: func() time.Time { return now }, coord: store, inflight: map[attemptKey]domain.LeaseRef{{id: "work/w/attempt/1"}: {}}}
	if d.ingress, err = runtime.NewIngress(store, d.now, runtime.IngressOptions{}); err != nil {
		t.Fatal(err)
	}
	if d.pipeline, err = telemetry.New(telemetry.Options{}); err != nil {
		t.Fatal(err)
	}
	// Three unrouted Events over a cap of two: the count stops at the cap,
	// and the oldest's age is exact.
	old := unroutedCap
	unroutedCap = 2
	t.Cleanup(func() { unroutedCap = old })
	if err = store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		for i, at := range []time.Time{now.Add(-time.Minute), now, now} {
			e := domain.Event{ID: domain.ID("e" + strconv.Itoa(i)), Type: "fixture.changed", Source: "fixture", DeliveryID: strconv.Itoa(i), OccurredAt: at, ReceivedAt: at}
			if _, _, err := tx.AcceptEvent(e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reader := sdkmetric.NewManualReader()
	if err = d.openMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))); err != nil {
		t.Fatal(err)
	}
	d.passDone()
	now = now.Add(3 * time.Second)
	var rm metricdata.ResourceMetrics
	if err = reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = m.Data
		}
	}
	gauge := func(name string) float64 {
		t.Helper()
		switch g := got[name].(type) {
		case metricdata.Gauge[float64]:
			return g.DataPoints[0].Value
		case metricdata.Gauge[int64]:
			return float64(g.DataPoints[0].Value)
		}
		t.Fatalf("no gauge %s: %+v", name, got[name])
		return 0
	}
	if age := gauge("open_conduit.host.last_pass_age"); age != 3 {
		t.Fatalf("last pass age %v", age)
	}
	if n := gauge("open_conduit.events.unrouted"); n != 2 {
		t.Fatalf("unrouted %v", n)
	}
	if age := gauge("open_conduit.events.unrouted.oldest_age"); age != 63 {
		t.Fatalf("oldest unrouted age %v", age)
	}
	if n := gauge("open_conduit.leases.held"); n != 1 {
		t.Fatalf("leases %v", n)
	}
	if _, ok := got["open_conduit.reconcile.sources"]; !ok {
		t.Fatal("no reconcile sources")
	}
	if _, ok := got["open_conduit.telemetry.records"].(metricdata.Sum[int64]); !ok {
		t.Fatalf("telemetry records: %+v", got["open_conduit.telemetry.records"])
	}
	allowed := map[string]bool{"pool": true, "state": true, "outcome": true}
	for name, a := range got {
		var sets []attribute.Set
		switch v := a.(type) {
		case metricdata.Gauge[int64]:
			for _, p := range v.DataPoints {
				sets = append(sets, p.Attributes)
			}
		case metricdata.Gauge[float64]:
			for _, p := range v.DataPoints {
				sets = append(sets, p.Attributes)
			}
		case metricdata.Sum[int64]:
			for _, p := range v.DataPoints {
				sets = append(sets, p.Attributes)
			}
		}
		for _, set := range sets {
			for _, kv := range set.ToSlice() {
				if !allowed[string(kv.Key)] {
					t.Fatalf("%s has attribute %s", name, kv.Key)
				}
			}
		}
	}
}
