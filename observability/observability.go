// Package observability defines required lifecycle telemetry and optional export.
package observability

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

type Correlation struct {
	WorkID, EventID, ExecutionID, ScopeID domain.ID
	ExecutorBinding, TraceID              string
}

// Record is a transition, not an artifact/log stream. Before an attempt exists,
// ExecutionID/binding may be absent; all execution transitions require them.
type Record struct {
	Name        string
	At          time.Time
	Correlation Correlation
	From, To    string
	Attributes  map[string]string
	Usage       *Measurement
}
type Measurement struct {
	InputTokens, OutputTokens, CachedTokens *uint64
	Cost                                    *float64
	Currency                                string
}
type Observer interface {
	Observe(context.Context, Record) error
}
type Exporter interface {
	Export(context.Context, []Record) error
	Shutdown(context.Context) error
}

// Metric kinds (Metric.Kind).
const (
	MetricSum       = "sum"
	MetricGauge     = "gauge"
	MetricHistogram = "histogram"
)

// Metric is one instrument's data at one collection of the host's meter:
// the lifecycle metrics and the host's own gauges and histograms. Sums and
// histograms are cumulative from Start. Point attributes are small fixed
// sets (a pool, a state, an outcome, an executor binding); they never carry
// an identifier.
type Metric struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Unit        string `json:"unit,omitempty"`
	// Kind is MetricSum, MetricGauge or MetricHistogram; Monotonic marks a
	// sum that only grows (a counter).
	Kind      string        `json:"kind"`
	Monotonic bool          `json:"monotonic,omitempty"`
	Points    []MetricPoint `json:"points"`
}

// MetricPoint is one series of a Metric: Value for a sum or gauge, and
// Count, Sum, Bounds (explicit bucket upper bounds) and Buckets (one more
// count than Bounds) for a histogram.
type MetricPoint struct {
	Attributes map[string]string `json:"attributes,omitempty"`
	Start      time.Time         `json:"start,omitempty"`
	Time       time.Time         `json:"time"`
	Value      float64           `json:"value,omitempty"`
	Count      uint64            `json:"count,omitempty"`
	Sum        float64           `json:"sum,omitempty"`
	Bounds     []float64         `json:"bounds,omitempty"`
	Buckets    []uint64          `json:"buckets,omitempty"`
	Min        *float64          `json:"min,omitempty"`
	Max        *float64          `json:"max,omitempty"`
}

// MetricExporter receives each collection of the host's meter. The host
// collects on an interval only while an exporter is running, and bounds
// each call; a failed or slow export loses that collection, never the
// next, since sums and histograms are cumulative.
type MetricExporter interface {
	ExportMetrics(context.Context, []Metric) error
}

// Validate requires name, trusted time, scope, causal Event and trace. An
// ingress scan record ("ingress." prefix) precedes any accepted Event: it is
// anchored instead by its source namespace attribute (ingress.source) and a
// trace derived from the scan, and must not carry a fabricated Event ID. A
// provider health record ("provider." prefix) is host-observed in the same
// way, anchored by the action provider's name (provider.name). An operator
// record ("operator." prefix) is a daemon-wide administrative call (a secret
// stored, say): no Event or scope precedes it, so it is anchored by the
// operator who made it (operator.actor) and needs no scope. A credential
// record ("auth." prefix) is the host checking a bearer credential with a
// trusted issuer: it precedes any operator, so it is anchored by the
// issuer (auth.issuer) and needs no scope either. A secret record
// ("secret." prefix) is the host resolving a provider-backed secret
// reference, by reference and outcome, never value: it may precede any
// Event (an attempt's start, doctor), so it is anchored by the reference's
// scheme (secret.scheme) and needs no scope. An audit record ("audit."
// prefix) is an entry of the durable audit ledger, which may describe a
// refused request that authenticated no one: it is anchored by its ledger
// sequence (audit.sequence) and needs no scope.
func Validate(r Record) error {
	c := r.Correlation
	operator := strings.HasPrefix(r.Name, "operator.") && r.Attributes["operator.actor"] != "" ||
		strings.HasPrefix(r.Name, "auth.") && r.Attributes["auth.issuer"] != "" ||
		strings.HasPrefix(r.Name, "secret.") && r.Attributes["secret.scheme"] != "" ||
		strings.HasPrefix(r.Name, "audit.") && r.Attributes["audit.sequence"] != ""
	anchored := c.EventID != "" || strings.HasPrefix(r.Name, "ingress.") && r.Attributes["ingress.source"] != "" ||
		strings.HasPrefix(r.Name, "provider.") && r.Attributes["provider.name"] != "" || operator
	if r.Name == "" || r.At.IsZero() || c.ScopeID == "" && !operator || !anchored || c.TraceID == "" {
		return fmt.Errorf("missing lifecycle correlation")
	}
	if strings.HasPrefix(r.Name, "execution.") && (c.WorkID == "" || c.ExecutionID == "" || c.ExecutorBinding == "") {
		return fmt.Errorf("missing execution correlation")
	}
	if r.Usage != nil && r.Usage.Cost != nil && (math.IsNaN(*r.Usage.Cost) || math.IsInf(*r.Usage.Cost, 0) || *r.Usage.Cost < 0 || r.Usage.Currency == "") {
		return fmt.Errorf("invalid cost")
	}
	return nil
}

// Redact copies attributes and removes known secret values even inside strings.
// Producers must also omit bodies, credentials and opaque executor state; this
// is a final boundary defense, not permission to log arbitrary request data.
func Redact(r Record, secrets []string) Record {
	clean := func(s string) string {
		for _, secret := range secrets {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[REDACTED]")
			}
		}
		return s
	}
	r.Name, r.From, r.To = clean(r.Name), clean(r.From), clean(r.To)
	attrs := make(map[string]string, len(r.Attributes))
	for k, v := range r.Attributes {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "authorization") || strings.Contains(lower, "password") {
			v = "[REDACTED]"
		}
		attrs[clean(k)] = clean(v)
	}
	r.Attributes = attrs
	if r.Usage != nil {
		usage := *r.Usage
		if usage.InputTokens != nil {
			v := *usage.InputTokens
			usage.InputTokens = &v
		}
		if usage.OutputTokens != nil {
			v := *usage.OutputTokens
			usage.OutputTokens = &v
		}
		if usage.CachedTokens != nil {
			v := *usage.CachedTokens
			usage.CachedTokens = &v
		}
		if usage.Cost != nil {
			v := *usage.Cost
			usage.Cost = &v
		}
		r.Usage = &usage
	}
	return r
}
