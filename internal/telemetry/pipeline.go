// Package telemetry delivers lifecycle records to optional exporters without
// ever blocking durable runtime progress. It owns validation, redaction,
// bounded buffering with drop accounting, per-exporter isolation and the
// OpenTelemetry mapping. Exporter configuration (OTLP endpoints, PostHog and
// host watches) belongs to extensions; this package uses only the public
// observability contract and the OpenTelemetry API.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alsiola/open-conduit/observability"
)

// Options configures a Pipeline. Zero values select bounded defaults.
type Options struct {
	Exporters []observability.Exporter
	// Buffer is the per-exporter queue capacity (default 4096). A full queue
	// drops its oldest record and counts the drop.
	Buffer int
	// Batch is the maximum records per Export call (default 128).
	Batch int
	// FlushInterval bounds how long a partial batch waits (default 1s).
	FlushInterval time.Duration
	// ExportTimeout bounds each Export call (default 10s).
	ExportTimeout time.Duration
	// Attempts is the number of tries per batch before it is dropped (default 3).
	Attempts int
	// Secrets returns currently known secret values for final redaction.
	Secrets func() []string
	// Diagnostic receives safe exporter fault codes; it must not block.
	Diagnostic func(Diagnostic)
}

// Diagnostic is a safe exporter fault report; it never carries error text.
type Diagnostic struct {
	Exporter int
	Code     string
	Records  int
}

// Stats is cumulative pipeline accounting.
type Stats struct {
	Observed, Invalid, Dropped, Exported, ExportFailures uint64
	LastFailure                                          time.Time
	LastCode                                             string
}

// Pipeline implements observability.Observer. Observe validates, redacts and
// enqueues in constant time; exporters run on their own goroutines, so a
// failing, slow or hung exporter loses telemetry (counted) but never delays a
// coordination commit, scheduling decision or execution outcome.
type Pipeline struct {
	o      Options
	sinks  []*sink
	closed atomic.Bool

	observed, invalid, dropped, exported, failures atomic.Uint64
	mu                                             sync.Mutex
	lastFailure                                    time.Time
	lastCode                                       string
}

type sink struct {
	index    int
	exporter observability.Exporter
	mu       sync.Mutex
	queue    []observability.Record
	inflight int
	notify   chan struct{}
	flushes  chan chan struct{}
	stop     chan struct{}
	done     chan struct{}
}

func New(o Options) (*Pipeline, error) {
	if o.Buffer == 0 {
		o.Buffer = 4096
	}
	if o.Batch == 0 {
		o.Batch = 128
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = time.Second
	}
	if o.ExportTimeout == 0 {
		o.ExportTimeout = 10 * time.Second
	}
	if o.Attempts == 0 {
		o.Attempts = 3
	}
	if o.Buffer < 1 || o.Batch < 1 || o.FlushInterval <= 0 || o.ExportTimeout <= 0 || o.Attempts < 1 {
		return nil, errors.New("telemetry: invalid options")
	}
	p := &Pipeline{o: o}
	for i, e := range o.Exporters {
		if e == nil {
			return nil, errors.New("telemetry: nil exporter")
		}
		s := &sink{index: i, exporter: e, notify: make(chan struct{}, 1), flushes: make(chan chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
		p.sinks = append(p.sinks, s)
		go p.run(s)
	}
	return p, nil
}

// Observe never blocks on an exporter. It returns an error only for a record
// that fails lifecycle validation, which is never delivered.
func (p *Pipeline) Observe(_ context.Context, r observability.Record) error {
	if err := observability.Validate(r); err != nil {
		p.invalid.Add(1)
		return err
	}
	if p.closed.Load() {
		p.dropped.Add(uint64(max(len(p.sinks), 1)))
		return nil
	}
	var secrets []string
	if p.o.Secrets != nil {
		secrets = p.o.Secrets()
	}
	r = observability.Redact(r, secrets)
	p.observed.Add(1)
	for _, s := range p.sinks {
		s.mu.Lock()
		if len(s.queue) >= p.o.Buffer {
			s.queue = s.queue[1:]
			p.dropped.Add(1)
		}
		s.queue = append(s.queue, r)
		full := len(s.queue) >= p.o.Batch
		s.mu.Unlock()
		if full {
			select {
			case s.notify <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

func (p *Pipeline) fault(s *sink, code string, records int) {
	p.mu.Lock()
	p.lastFailure, p.lastCode = time.Now(), code
	p.mu.Unlock()
	if p.o.Diagnostic != nil {
		p.o.Diagnostic(Diagnostic{Exporter: s.index, Code: code, Records: records})
	}
}

func (p *Pipeline) take(s *sink) []observability.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := min(len(s.queue), p.o.Batch)
	if n == 0 {
		return nil
	}
	batch := append([]observability.Record(nil), s.queue[:n]...)
	s.queue = s.queue[n:]
	s.inflight = n
	return batch
}

func (p *Pipeline) export(s *sink, batch []observability.Record) {
	defer func() {
		s.mu.Lock()
		s.inflight = 0
		s.mu.Unlock()
	}()
	delay := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := p.call(s, batch)
		if err == nil {
			p.exported.Add(uint64(len(batch)))
			return
		}
		p.failures.Add(1)
		code := "export_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "export_timeout"
		}
		if attempt >= p.o.Attempts {
			p.dropped.Add(uint64(len(batch)))
			p.fault(s, code, len(batch))
			return
		}
		p.fault(s, code+"_retrying", len(batch))
		select {
		case <-time.After(delay):
			delay *= 2
		case <-s.stop:
			p.dropped.Add(uint64(len(batch)))
			return
		}
	}
}

// call isolates exporter panics and bounds the call. An exporter that ignores
// its context keeps only its own goroutine busy; Observe keeps enqueueing and
// the bounded queue drops oldest records.
func (p *Pipeline) call(s *sink, batch []observability.Record) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.o.ExportTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			err = errors.New("exporter panic")
		}
	}()
	return s.exporter.Export(ctx, batch)
}

func (p *Pipeline) run(s *sink) {
	defer close(s.done)
	ticker := time.NewTicker(p.o.FlushInterval)
	defer ticker.Stop()
	drain := func() {
		for {
			batch := p.take(s)
			if batch == nil {
				return
			}
			p.export(s, batch)
		}
	}
	for {
		select {
		case <-s.notify:
			drain()
		case <-ticker.C:
			drain()
		case reply := <-s.flushes:
			drain()
			close(reply)
		case <-s.stop:
			drain()
			return
		}
	}
}

// Flush exports everything queued before the call, or returns ctx.Err().
func (p *Pipeline) Flush(ctx context.Context) error {
	for _, s := range p.sinks {
		reply := make(chan struct{})
		select {
		case s.flushes <- reply:
		case <-s.done:
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-reply:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Shutdown stops accepting records, drains within ctx and shuts exporters
// down. Records still queued when ctx expires are counted as dropped.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	if p.closed.Swap(true) {
		return nil
	}
	var errs []error
	for _, s := range p.sinks {
		close(s.stop)
	}
	for _, s := range p.sinks {
		select {
		case <-s.done:
		case <-ctx.Done():
			s.mu.Lock()
			p.dropped.Add(uint64(len(s.queue)))
			s.queue = nil
			s.mu.Unlock()
			errs = append(errs, ctx.Err())
			continue
		}
		if err := s.exporter.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Stats returns cumulative counters, including drops while queued.
func (p *Pipeline) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Observed: p.observed.Load(), Invalid: p.invalid.Load(), Dropped: p.dropped.Load(), Exported: p.exported.Load(), ExportFailures: p.failures.Load(), LastFailure: p.lastFailure, LastCode: p.lastCode}
}

// Health summarizes delivery for doctor as (status, code, detail) using the
// runtime check vocabulary: ok, or warn after drops or export failures.
func (p *Pipeline) Health() (string, string, string) {
	st := p.Stats()
	switch {
	case st.ExportFailures > 0 || st.Dropped > 0:
		return "warn", "export_degraded", fmt.Sprintf("%d record(s) observed, %d exported, %d dropped, %d failed export attempt(s); last fault %s", st.Observed, st.Exported, st.Dropped, st.ExportFailures, st.LastCode)
	case len(p.sinks) == 0:
		return "info", "no_exporters", fmt.Sprintf("%d record(s) observed; no exporter is configured", st.Observed)
	}
	return "ok", "exporting", fmt.Sprintf("%d record(s) observed, %d exported", st.Observed, st.Exported)
}

// Pending returns queued plus in-flight records across exporters.
func (p *Pipeline) Pending() int {
	n := 0
	for _, s := range p.sinks {
		s.mu.Lock()
		n += len(s.queue) + s.inflight
		s.mu.Unlock()
	}
	return n
}

// Memory is an in-process exporter retaining exported records, for local
// inspection and tests.
type Memory struct {
	mu      sync.Mutex
	records []observability.Record
}

func (m *Memory) Export(_ context.Context, records []observability.Record) error {
	m.mu.Lock()
	m.records = append(m.records, records...)
	m.mu.Unlock()
	return nil
}
func (m *Memory) Shutdown(context.Context) error { return nil }

// Records returns a copy of every exported record.
func (m *Memory) Records() []observability.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]observability.Record(nil), m.records...)
}
