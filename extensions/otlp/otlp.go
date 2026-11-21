// Package otlp is the OTLP metrics exporter package (#692). It receives each
// collection of the daemon's meter (telemetry.metrics, a declared permission) and
// sends it over OTLP/HTTP to the collector each otlp entry names: the
// lifecycle metrics (execution duration, start latency, tokens, cost) and
// the host's own (pass and phase durations, capacity, the unrouted backlog,
// reconcile lag, held leases, the telemetry pipeline's counts). It is an
// ordinary package on the public protocol, so any other exporter (a
// Prometheus scrape endpoint, a vendor's API) can be built the same way.
// open-conduit serves it as `open-conduit extension-run otlp`.
package otlp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's registration name and its entry type.
const Name = "otlp"

// Version is this package build's own version.
const Version = "0.1.0"

// Settings is an otlp entry's config.
type Settings struct {
	// Endpoint is the collector's OTLP/HTTP base URL, such as
	// https://collector.example.com:4318; /v1/metrics is added when it has
	// no path. Empty takes the standard OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
	// or OTEL_EXPORTER_OTLP_ENDPOINT from the package's environment
	// (settings.extensions.otlp.pass_env), else http://localhost:4318.
	Endpoint string `json:"endpoint,omitempty"`
	// Headers are sent with each export, by header name, each the name of
	// one of the entry's secrets (secret_names), such as
	// {"Authorization": "otlp-token"} for a secret holding "Bearer …".
	Headers map[string]string `json:"headers,omitempty"`
	// Insecure allows a plain http:// endpoint other than a loopback one.
	Insecure bool `json:"insecure,omitempty"`
}

// ParseSettings reads and checks an entry's config; secrets are the
// entry's secret names, or nil when they aren't known.
func ParseSettings(raw json.RawMessage, secrets []string) (Settings, error) {
	var s Settings
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return s, errors.New("an otlp entry's config takes endpoint, headers and insecure")
		}
	}
	if s.Endpoint != "" {
		u, err := url.Parse(s.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return s, errors.New("endpoint is an http or https URL with a host and no credentials, such as https://collector:4318")
		}
		if u.Scheme == "http" && !s.Insecure && !loopback(u.Hostname()) {
			return s, errors.New("endpoint is plain http to another host: use https, or set insecure")
		}
	}
	for header, secret := range s.Headers {
		if header == "" || strings.ContainsAny(header, " :\r\n") {
			return s, fmt.Errorf("header %q is not a header name", header)
		}
		if secrets != nil && !contains(secrets, secret) {
			return s, fmt.Errorf("header %s names %q, which is not one of the entry's secret_names", header, secret)
		}
	}
	return s, nil
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Options are the package's dependencies (tests replace them).
type Options struct {
	// Secrets resolves an entry's secrets in its scope (Host.Secrets).
	Secrets func(ctx context.Context, h *sdk.Host, scope domain.ID, names ...string) (map[string]string, error)
	// Now is the clock the check reports by.
	Now func() time.Time
}

type state struct {
	o    Options
	mu   sync.Mutex
	host *sdk.Host
	// dests are the configured collectors, by key: one per distinct
	// endpoint and headers, however many entries name it.
	dests map[string]*dest
}

type dest struct {
	key, label string
	scope      domain.ID
	settings   Settings

	mu       sync.Mutex
	headers  string // the resolved headers the exporter was made with
	exporter *otlpmetrichttp.Exporter
	last     time.Time
	metrics  int
	err      string
}

// Build returns the served extension.
func Build(o Options) *sdk.Extension {
	if o.Secrets == nil {
		o.Secrets = func(ctx context.Context, h *sdk.Host, scope domain.ID, names ...string) (map[string]string, error) {
			return h.Secrets(ctx, scope, names...)
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	st := &state{o: o, dests: map[string]*dest{}}
	ext := sdk.New(Name, sdk.Version(Version), sdk.Command(protocol.HostRunCommand(Name)...),
		sdk.Summary("Sends the daemon's metrics (lifecycle, capacity, backlog, reconcile lag, loop timings) to an OpenTelemetry collector over OTLP/HTTP."))
	ext.Manifest.Title = "OpenTelemetry (OTLP)"
	ext.Manifest.Permissions = []string{protocol.PermissionTelemetryMetrics}
	ext.Manifest.Provides.Extensions = []protocol.ExtensionDecl{{Type: Name, Fields: []protocol.FieldDecl{
		{Name: "endpoint", Help: "the collector's OTLP/HTTP base URL, such as https://collector:4318 (/v1/metrics is added when it has no path); empty takes OTEL_EXPORTER_OTLP_METRICS_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT from the package's pass_env, else http://localhost:4318"},
		{Name: "headers", Help: `{"Authorization": "otlp-token"}: headers sent with each export, each value the name of one of the entry's secret_names`},
		{Name: "insecure", Help: "allow a plain http:// endpoint other than a loopback one", Default: "false"},
	}}}
	ext.Metrics = st
	ext.Check(Name, st.check)
	ext.Validate = func(_ context.Context, p protocol.ValidateParams) error {
		if p.Kind != protocol.ValidateExtension {
			return nil
		}
		if _, err := ParseSettings(p.Config, p.SecretNames); err != nil {
			return extension.Invalid("/config", "invalid_otlp_entry", err.Error())
		}
		return nil
	}
	ext.Configure = st.configure
	return ext
}

// Manifest is the package manifest.
func Manifest() protocol.Manifest { return Build(Options{}).Manifest }

// configure takes the generation's otlp entries; a collector named before
// keeps its exporter.
func (st *state) configure(ctx context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	next := map[string]*dest{}
	for _, e := range p.Entries {
		if e.Type != Name {
			continue
		}
		s, err := ParseSettings(e.Config, e.Secrets)
		if err != nil {
			return protocol.ConfigureResult{}, protocol.Errorf(protocol.CodeInvalid, "invalid otlp entry")
		}
		key := s.Endpoint
		if len(s.Headers) > 0 {
			// Header secrets resolve in the entry's scope.
			b, _ := json.Marshal(s.Headers)
			key += "\x00" + e.ScopeID + "\x00" + string(b)
		}
		if s.Insecure {
			key += "\x00insecure"
		}
		if _, ok := next[key]; ok {
			continue
		}
		label := s.Endpoint
		if label == "" {
			label = "the environment's endpoint"
		}
		next[key] = &dest{key: key, label: label, scope: domain.ID(e.ScopeID), settings: s}
	}
	st.mu.Lock()
	prev := st.dests
	st.host = h
	for key := range next {
		if old, ok := prev[key]; ok {
			next[key] = old
		}
	}
	st.dests = next
	st.mu.Unlock()
	for key, old := range prev {
		if _, kept := next[key]; !kept {
			old.close(ctx)
		}
	}
	return protocol.ConfigureResult{}, nil
}

// ExportMetrics sends one collection to every configured collector at
// once.
func (st *state) ExportMetrics(ctx context.Context, metrics []observability.Metric) error {
	st.mu.Lock()
	dests := make([]*dest, 0, len(st.dests))
	for _, d := range st.dests {
		dests = append(dests, d)
	}
	h := st.host
	st.mu.Unlock()
	if len(dests) == 0 {
		return nil
	}
	rm, err := resourceMetrics(ctx, metrics)
	if err != nil {
		return err
	}
	errs := make([]error, len(dests))
	var wg sync.WaitGroup
	for i, d := range dests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.export(ctx, st, h, rm, len(metrics))
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (d *dest) export(ctx context.Context, st *state, h *sdk.Host, rm *metricdata.ResourceMetrics, n int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.exportLocked(ctx, st, h, rm)
	d.last, d.metrics, d.err = st.o.Now(), n, ""
	if err != nil {
		// The collector's error text can carry a URL or a response body; the
		// check shows only that it failed.
		d.err = "export failed"
		return errors.New("otlp: export to " + d.label + " failed")
	}
	return nil
}

func (d *dest) exportLocked(ctx context.Context, st *state, h *sdk.Host, rm *metricdata.ResourceMetrics) error {
	headers := map[string]string{}
	if len(d.settings.Headers) > 0 {
		names := make([]string, 0, len(d.settings.Headers))
		for _, secret := range d.settings.Headers {
			names = append(names, secret)
		}
		values, err := st.o.Secrets(ctx, h, d.scope, names...)
		if err != nil {
			return err
		}
		for header, secret := range d.settings.Headers {
			v, ok := values[secret]
			if !ok || v == "" {
				return errors.New("a header secret did not resolve")
			}
			headers[header] = v
		}
	}
	digest, _ := json.Marshal(headers)
	if d.exporter == nil || d.headers != string(digest) {
		if d.exporter != nil {
			_ = d.exporter.Shutdown(ctx)
		}
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithHeaders(headers)}
		if e := d.settings.Endpoint; e != "" {
			u, _ := url.Parse(e)
			if u.Path == "" || u.Path == "/" {
				u.Path = "/v1/metrics"
			}
			opts = append(opts, otlpmetrichttp.WithEndpointURL(u.String()))
			if u.Scheme == "http" {
				opts = append(opts, otlpmetrichttp.WithInsecure())
			}
		}
		x, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return err
		}
		d.exporter, d.headers = x, string(digest)
	}
	return d.exporter.Export(ctx, rm)
}

func (d *dest) close(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exporter != nil {
		_ = d.exporter.Shutdown(ctx)
		d.exporter = nil
	}
}

// check lists each collector and how its last export went.
func (st *state) check(context.Context, *sdk.Host, bool) (string, error) {
	st.mu.Lock()
	dests := make([]*dest, 0, len(st.dests))
	for _, d := range st.dests {
		dests = append(dests, d)
	}
	st.mu.Unlock()
	if len(dests) == 0 {
		return "no otlp entries are configured", nil
	}
	sort.Slice(dests, func(i, j int) bool { return dests[i].key < dests[j].key })
	var items []string
	var failed bool
	for _, d := range dests {
		d.mu.Lock()
		switch {
		case d.last.IsZero():
			items = append(items, d.label+": no export yet")
		case d.err != "":
			failed = true
			items = append(items, fmt.Sprintf("%s: %s at %s", d.label, d.err, d.last.UTC().Format(time.RFC3339)))
		default:
			items = append(items, fmt.Sprintf("%s: %d metric(s) at %s", d.label, d.metrics, d.last.UTC().Format(time.RFC3339)))
		}
		d.mu.Unlock()
	}
	if failed {
		return "", errors.New(strings.Join(items, "; "))
	}
	return strings.Join(items, "; "), nil
}

// resourceMetrics maps a collection back onto the OpenTelemetry data
// model, under a resource naming the daemon (service.name open-conduit,
// which OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES from the package's
// environment override).
func resourceMetrics(ctx context.Context, metrics []observability.Metric) (*metricdata.ResourceMetrics, error) {
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", "open-conduit")), resource.WithFromEnv())
	if err != nil && res == nil {
		return nil, err
	}
	sm := metricdata.ScopeMetrics{Scope: instrumentation.Scope{Name: "github.com/open-conduit/open-conduit"}}
	for _, m := range metrics {
		x := metricdata.Metrics{Name: m.Name, Description: m.Description, Unit: m.Unit}
		switch m.Kind {
		case observability.MetricSum:
			d := metricdata.Sum[float64]{Temporality: metricdata.CumulativeTemporality, IsMonotonic: m.Monotonic}
			for _, p := range m.Points {
				d.DataPoints = append(d.DataPoints, metricdata.DataPoint[float64]{Attributes: set(p.Attributes), StartTime: p.Start, Time: p.Time, Value: p.Value})
			}
			x.Data = d
		case observability.MetricGauge:
			d := metricdata.Gauge[float64]{}
			for _, p := range m.Points {
				d.DataPoints = append(d.DataPoints, metricdata.DataPoint[float64]{Attributes: set(p.Attributes), StartTime: p.Start, Time: p.Time, Value: p.Value})
			}
			x.Data = d
		case observability.MetricHistogram:
			d := metricdata.Histogram[float64]{Temporality: metricdata.CumulativeTemporality}
			for _, p := range m.Points {
				hp := metricdata.HistogramDataPoint[float64]{Attributes: set(p.Attributes), StartTime: p.Start, Time: p.Time, Count: p.Count, Sum: p.Sum, Bounds: p.Bounds, BucketCounts: p.Buckets}
				if p.Min != nil {
					hp.Min = metricdata.NewExtrema(*p.Min)
				}
				if p.Max != nil {
					hp.Max = metricdata.NewExtrema(*p.Max)
				}
				d.DataPoints = append(d.DataPoints, hp)
			}
			x.Data = d
		default:
			continue
		}
		sm.Metrics = append(sm.Metrics, x)
	}
	return &metricdata.ResourceMetrics{Resource: res, ScopeMetrics: []metricdata.ScopeMetrics{sm}}, nil
}

func set(attrs map[string]string) attribute.Set {
	kv := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		kv = append(kv, attribute.String(k, v))
	}
	return attribute.NewSet(kv...)
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run otlp` does. Its subcommands package-manifest
// and package-certification print the package files.
func Main(args []string) int {
	if sdk.PackageCommand(args, Manifest(), protocol.CertificationFile{}, os.Stdout) {
		return 0
	}
	_ = sdk.Serve(Build(Options{}))
	return 0
}
