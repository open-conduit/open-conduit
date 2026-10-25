// Package observability defines required lifecycle telemetry and optional export.
package observability

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
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

// Validate requires name, trusted time, scope, causal Event and trace. An
// ingress scan record ("ingress." prefix) precedes any accepted Event: it is
// anchored instead by its source namespace attribute (ingress.source) and a
// trace derived from the scan, and must not carry a fabricated Event ID.
func Validate(r Record) error {
	c := r.Correlation
	anchored := c.EventID != "" || strings.HasPrefix(r.Name, "ingress.") && r.Attributes["ingress.source"] != ""
	if r.Name == "" || r.At.IsZero() || c.ScopeID == "" || !anchored || c.TraceID == "" {
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
