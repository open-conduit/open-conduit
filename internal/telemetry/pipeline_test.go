package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/observability"
)

func record(name string, i int) observability.Record {
	return observability.Record{Name: name, At: time.Date(2026, 10, 25, 12, 0, i, 0, time.UTC), Correlation: observability.Correlation{WorkID: "work", EventID: "event", ExecutionID: "work/attempt/1", ScopeID: "scope", ExecutorBinding: "run", TraceID: "trace"}, Attributes: map[string]string{"record_id": name}}
}

func TestPipelineSatisfiesObserverContract(t *testing.T) {
	memory := &Memory{}
	p, err := New(Options{Exporters: []observability.Exporter{memory}, Secrets: func() []string { return []string{"secret-value"} }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	conformance.RunObserver(t, p, func() []observability.Record {
		if err := p.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		return memory.Records()
	})
	if st := p.Stats(); st.Invalid == 0 || st.Observed != 1 || st.Exported != 1 {
		t.Fatalf("stats %+v", st)
	}
}

type blockingExporter struct {
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingExporter) Export(context.Context, []observability.Record) error {
	b.calls.Add(1)
	<-b.release
	return nil
}
func (b *blockingExporter) Shutdown(context.Context) error { return nil }

// A hung exporter that ignores its context must not delay Observe, and a full
// queue drops its oldest records with accounting.
func TestPipelineNeverBlocksOnHungExporter(t *testing.T) {
	hung := &blockingExporter{release: make(chan struct{})}
	healthy := &Memory{}
	p, err := New(Options{Exporters: []observability.Exporter{hung, healthy}, Buffer: 4, Batch: 2, FlushInterval: time.Millisecond, ExportTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if err := p.Observe(context.Background(), record("execution.started", i%60)); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Observe blocked behind exporter: %s", elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(healthy.Records()) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if st := p.Stats(); st.Dropped == 0 || st.Observed != 1000 {
		t.Fatalf("drop accounting %+v", st)
	}
	if len(healthy.Records()) == 0 {
		t.Fatal("healthy exporter starved by hung sibling")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with hung exporter: %v", err)
	}
	close(hung.release)
	// Records after shutdown are dropped, never delivered or blocking.
	before := p.Stats().Dropped
	if err := p.Observe(context.Background(), record("execution.started", 1)); err != nil || p.Stats().Dropped <= before {
		t.Fatalf("observe after shutdown: %v %+v", err, p.Stats())
	}
}

type flakyExporter struct {
	mu       sync.Mutex
	fail     int
	panicked bool
	got      []observability.Record
}

func (f *flakyExporter) Export(_ context.Context, rs []observability.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.panicked {
		f.panicked = true
		panic("exporter bug")
	}
	if f.fail > 0 {
		f.fail--
		return errors.New("collector said: secret detail")
	}
	f.got = append(f.got, rs...)
	return nil
}
func (f *flakyExporter) Shutdown(context.Context) error { return nil }

func TestPipelineRetriesIsolatesPanicsAndReportsSafeDiagnostics(t *testing.T) {
	flaky := &flakyExporter{fail: 1}
	var mu sync.Mutex
	var codes []string
	p, err := New(Options{Exporters: []observability.Exporter{flaky}, Attempts: 3, FlushInterval: time.Millisecond, Diagnostic: func(d Diagnostic) {
		mu.Lock()
		codes = append(codes, d.Code)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Observe(context.Background(), record("execution.completed", 1))
	if err = p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	flaky.mu.Lock()
	got := len(flaky.got)
	flaky.mu.Unlock()
	if got != 1 {
		t.Fatalf("record not delivered after retries: %d", got)
	}
	mu.Lock()
	joined := strings.Join(codes, ",")
	mu.Unlock()
	if joined != "export_failed_retrying,export_failed_retrying" || strings.Contains(joined, "secret") {
		t.Fatalf("diagnostics %q", joined)
	}
	if st := p.Stats(); st.ExportFailures != 2 || st.Exported != 1 || st.Dropped != 0 {
		t.Fatalf("stats %+v", st)
	}
	if status, code, _ := p.Health(); status != "warn" || code != "export_degraded" {
		t.Fatalf("health %s %s", status, code)
	}
	_ = p.Shutdown(context.Background())
}

func TestPipelineDropsBatchAfterAttempts(t *testing.T) {
	flaky := &flakyExporter{fail: 100, panicked: true}
	p, _ := New(Options{Exporters: []observability.Exporter{flaky}, Attempts: 2, FlushInterval: time.Millisecond})
	_ = p.Observe(context.Background(), record("execution.failed", 1))
	_ = p.Flush(context.Background())
	if st := p.Stats(); st.Dropped != 1 || st.ExportFailures != 2 {
		t.Fatalf("stats %+v", st)
	}
	_ = p.Shutdown(context.Background())
}

func TestLogExporterWritesRedactedJSONLines(t *testing.T) {
	var buf bytes.Buffer
	p, _ := New(Options{Exporters: []observability.Exporter{NewLogExporter(&buf)}, Secrets: func() []string { return []string{"s3cr3t"} }})
	r := record("execution.failed", 3)
	r.Attributes["outcome.summary"] = "failed with s3cr3t"
	in := uint64(5)
	r.Usage = &observability.Measurement{InputTokens: &in}
	_ = p.Observe(context.Background(), r)
	_ = p.Shutdown(context.Background())
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	attrs := line["attributes"].(map[string]any)
	if line["msg"] != "execution.failed" || line["level"] != "WARN" || line["execution_id"] != "work/attempt/1" || attrs["outcome.summary"] != "failed with [REDACTED]" || line["usage"].(map[string]any)["input_tokens"] != float64(5) {
		t.Fatalf("log line %+v", line)
	}
}
