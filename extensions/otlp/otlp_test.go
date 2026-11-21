package otlp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	colmetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

type collector struct {
	mu       sync.Mutex
	path     string
	auth     string
	requests []*colmetric.ExportMetricsServiceRequest
	status   int
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != 0 {
		http.Error(w, "secret-bearing collector detail", c.status)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req colmetric.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.path, c.auth = r.URL.Path, r.Header.Get("Authorization")
	c.requests = append(c.requests, &req)
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&colmetric.ExportMetricsServiceResponse{})
	_, _ = w.Write(out)
}

func configured(t *testing.T, url string, secrets map[string]string) *state {
	t.Helper()
	ext := Build(Options{
		Now: func() time.Time { return time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC) },
		Secrets: func(_ context.Context, _ *sdk.Host, _ domain.ID, names ...string) (map[string]string, error) {
			out := map[string]string{}
			for _, n := range names {
				if v, ok := secrets[n]; ok {
					out[n] = v
				}
			}
			return out, nil
		},
	})
	st := ext.Metrics.(*state)
	cfg, _ := json.Marshal(Settings{Endpoint: url, Headers: map[string]string{"Authorization": "otlp-token"}})
	if _, err := st.configure(context.Background(), nil, protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "s", Type: Name, Config: cfg, Secrets: []string{"otlp-token"}}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, d := range st.dests {
			d.close(context.Background())
		}
	})
	return st
}

func sample() []observability.Metric {
	start, at := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC), time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	lo, hi := 0.5, 4.0
	return []observability.Metric{
		{Name: "open_conduit.telemetry.export_failures", Kind: observability.MetricSum, Monotonic: true, Points: []observability.MetricPoint{{Start: start, Time: at, Value: 3}}},
		{Name: "open_conduit.capacity.in_use", Unit: "{slot}", Kind: observability.MetricGauge, Points: []observability.MetricPoint{{Attributes: map[string]string{"pool": "agents"}, Start: start, Time: at, Value: 2}}},
		{Name: "open_conduit.host.pass.duration", Unit: "s", Kind: observability.MetricHistogram, Points: []observability.MetricPoint{{Start: start, Time: at, Count: 3, Sum: 6, Bounds: []float64{1, 5}, Buckets: []uint64{1, 2, 0}, Min: &lo, Max: &hi}}},
	}
}

func TestExportSendsEveryKind(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	st := configured(t, srv.URL, map[string]string{"otlp-token": "Bearer abc"})
	if err := st.ExportMetrics(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	if c.path != "/v1/metrics" || c.auth != "Bearer abc" || len(c.requests) != 1 {
		t.Fatalf("path %q auth %q requests %d", c.path, c.auth, len(c.requests))
	}
	rm := c.requests[0].ResourceMetrics[0]
	var service string
	for _, a := range rm.Resource.Attributes {
		if a.Key == "service.name" {
			service = a.Value.GetStringValue()
		}
	}
	if service != "open-conduit" {
		t.Fatalf("service.name = %q", service)
	}
	got := map[string]*metricpb.Metric{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		got[m.Name] = m
	}
	sum := got["open_conduit.telemetry.export_failures"].GetSum()
	if sum == nil || !sum.IsMonotonic || sum.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE || sum.DataPoints[0].GetAsDouble() != 3 {
		t.Fatalf("sum = %v", sum)
	}
	gauge := got["open_conduit.capacity.in_use"].GetGauge()
	if gauge == nil || gauge.DataPoints[0].GetAsDouble() != 2 || gauge.DataPoints[0].Attributes[0].Value.GetStringValue() != "agents" {
		t.Fatalf("gauge = %v", gauge)
	}
	h := got["open_conduit.host.pass.duration"].GetHistogram()
	if h == nil || h.DataPoints[0].Count != 3 || h.DataPoints[0].GetSum() != 6 || len(h.DataPoints[0].BucketCounts) != 3 || h.DataPoints[0].GetMin() != 0.5 || h.DataPoints[0].GetMax() != 4 {
		t.Fatalf("histogram = %v", h)
	}
	detail, err := st.check(context.Background(), nil, false)
	if err != nil || !strings.Contains(detail, "3 metric(s)") {
		t.Fatalf("check = %q, %v", detail, err)
	}
}

func TestFailedExportKeepsCollectorTextOut(t *testing.T) {
	c := &collector{status: http.StatusBadRequest}
	srv := httptest.NewServer(c)
	defer srv.Close()
	st := configured(t, srv.URL, map[string]string{"otlp-token": "Bearer abc"})
	err := st.ExportMetrics(context.Background(), sample())
	if err == nil {
		t.Fatal("a refused export succeeded")
	}
	_, checkErr := st.check(context.Background(), nil, false)
	for _, e := range []error{err, checkErr} {
		if e == nil || strings.Contains(e.Error(), "secret-bearing") || strings.Contains(e.Error(), "Bearer") {
			t.Fatalf("error carries collector text: %v", e)
		}
	}
}

func TestUnresolvedHeaderSecretSendsNothing(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	st := configured(t, srv.URL, nil)
	if err := st.ExportMetrics(context.Background(), sample()); err == nil {
		t.Fatal("an export without its header secret succeeded")
	}
	if len(c.requests) != 0 {
		t.Fatalf("sent %d request(s) without the credential", len(c.requests))
	}
}

func TestParseSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		raw string
		ok  bool
	}{
		"empty":             {`{}`, true},
		"https":             {`{"endpoint":"https://collector:4318"}`, true},
		"loopback http":     {`{"endpoint":"http://127.0.0.1:4318"}`, true},
		"remote http":       {`{"endpoint":"http://collector:4318"}`, false},
		"remote insecure":   {`{"endpoint":"http://collector:4318","insecure":true}`, true},
		"url credentials":   {`{"endpoint":"https://u:p@collector:4318"}`, false},
		"unknown field":     {`{"url":"https://collector"}`, false},
		"bad header":        {`{"headers":{"Bad Header":"otlp-token"}}`, false},
		"undeclared secret": {`{"headers":{"Authorization":"other"}}`, false},
		"declared secret":   {`{"headers":{"Authorization":"otlp-token"}}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSettings(json.RawMessage(tc.raw), []string{"otlp-token"})
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestManifestAsksForMetricsPermission(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if !m.Allows(protocol.PermissionTelemetryMetrics) {
		t.Fatal("the package does not declare telemetry.metrics")
	}
}

func TestEnvironmentEndpointPassesThePlainHTTPRule(t *testing.T) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "http://collector.example.invalid:4318")
			st := configured(t, "", map[string]string{"otlp-token": "Bearer abc"})
			inner, resolved := st.o.Secrets, 0
			st.o.Secrets = func(ctx context.Context, h *sdk.Host, scope domain.ID, names ...string) (map[string]string, error) {
				resolved++
				return inner(ctx, h, scope, names...)
			}
			err := st.ExportMetrics(context.Background(), sample())
			if err == nil || !strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), "collector.example") {
				t.Fatalf("err = %v", err)
			}
			if resolved != 0 {
				t.Fatal("read the header secret for a refused endpoint")
			}
			for _, d := range st.dests {
				if d.exporter != nil {
					t.Fatal("made an exporter for a refused endpoint")
				}
			}
			if _, err := st.check(context.Background(), nil, false); err == nil || !strings.Contains(err.Error(), "endpoint refused") {
				t.Fatalf("check = %v", err)
			}
		})
	}
}

func TestResolveEndpoint(t *testing.T) {
	for name, tc := range map[string]struct {
		base, metrics string
		settings      Settings
		want          string
	}{
		"default":              {want: "http://localhost:4318/v1/metrics"},
		"configured":           {settings: Settings{Endpoint: "https://c:4318"}, base: "http://other:1", want: "https://c:4318/v1/metrics"},
		"configured path":      {settings: Settings{Endpoint: "https://c/custom"}, want: "https://c/custom"},
		"base":                 {base: "https://c:4318", want: "https://c:4318/v1/metrics"},
		"base with path":       {base: "https://c/otlp/", want: "https://c/otlp/v1/metrics"},
		"metrics as given":     {base: "https://other", metrics: "https://c/custom", want: "https://c/custom"},
		"metrics no path":      {metrics: "https://c:4318", want: "https://c:4318/"},
		"loopback":             {base: "http://127.0.0.1:4318", want: "http://127.0.0.1:4318/v1/metrics"},
		"remote with insecure": {base: "http://c:4318", settings: Settings{Insecure: true}, want: "http://c:4318/v1/metrics"},
		"remote http":          {base: "http://c:4318"},
		"metrics remote http":  {base: "https://c", metrics: "http://c:4318"},
		"credentials":          {base: "https://u:p@c:4318"},
		"not a URL":            {base: "collector:4318"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.base)
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", tc.metrics)
			u, err := resolveEndpoint(tc.settings)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted %s", u)
				}
				return
			}
			if err != nil || u.String() != tc.want {
				t.Fatalf("got %v, %v; want %s", u, err, tc.want)
			}
		})
	}
}

func TestEnvironmentEndpointExports(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL+"/base")
	st := configured(t, "", map[string]string{"otlp-token": "Bearer abc"})
	if err := st.ExportMetrics(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	if c.path != "/base/v1/metrics" || c.auth != "Bearer abc" {
		t.Fatalf("path %q auth %q", c.path, c.auth)
	}
}
