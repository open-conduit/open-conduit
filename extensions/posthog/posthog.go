// Package posthog is the first-party PostHog telemetry extension (06-01),
// ported from Open Supervisor's sink. It sends product telemetry to
// PostHog's capture endpoint directly (nothing else is installed): an
// `agent_turn` event for every finished execution (outcome, duration,
// tokens and cost, from the lifecycle records the observability pipeline
// delivers once each), the lifecycle records an entry selects, each as an
// event named after the record, and the host's telemetry events (reports,
// fixes, host readings) too. It is an exporter on
// the public observability contract: a failing or slow PostHog loses
// telemetry (counted by the pipeline) and never delays the runtime.
package posthog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/observability"
)

// Type is the registered connection and extension name.
const Type = "posthog"

// SecretToken is the connection secret: the project's public API key (phc_…).
const SecretToken = "token"

// DefaultHost is PostHog's US ingestion host.
const DefaultHost = "https://us.i.posthog.com"

// Event names.
const (
	EventTurn = "agent_turn"
)

// ConnectionConfig is the nonsecret config of a `posthog` connection.
type ConnectionConfig struct {
	// Host is the ingestion host (https://us.i.posthog.com or https://eu.i.posthog.com).
	Host string `json:"host,omitempty"`
	// DistinctID is who events are from (default open-conduit@<hostname>).
	DistinctID string `json:"distinct_id,omitempty"`
}

func strict(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

// ParseConnection decodes and defaults connection config.
func ParseConnection(data []byte) (ConnectionConfig, error) {
	var c ConnectionConfig
	if err := strict(data, &c); err != nil {
		return c, fmt.Errorf("posthog connection: %w", err)
	}
	if c.Host == "" {
		c.Host = DefaultHost
	}
	c.Host = strings.TrimRight(c.Host, "/")
	u, err := url.Parse(c.Host)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || (u.Path != "" && u.Path != "/") {
		return c, errors.New("posthog connection: host must be an http(s) ingestion host without credentials")
	}
	if c.DistinctID == "" {
		host, _ := os.Hostname()
		c.DistinctID = "open-conduit@" + host
	}
	if len(c.DistinctID) > 200 || strings.ContainsAny(c.DistinctID, "\r\n") {
		return c, errors.New("posthog connection: distinct_id must be one short line")
	}
	return c, nil
}

// ValidateConnection is the config.Registry connection validator.
func ValidateConnection(data json.RawMessage) error {
	_, err := ParseConnection(data)
	return err
}

// ExtensionConfig is a `posthog` extension entry: it sends the telemetry of
// its scope (and, with `telemetry_events`, the host's telemetry events)
// through its connection.
type ExtensionConfig struct {
	// Turns sends agent_turn events (default true).
	Turns *bool `json:"turns,omitempty"`
	// TelemetryEvents forwards the host's telemetry events (reports, fixes,
	// host readings) while the host writes them (default true).
	TelemetryEvents *bool `json:"telemetry_events,omitempty"`
	// Introspection is TelemetryEvents' old name, accepted for one release
	// with a doctor warning; `migrate upgrade` renames it.
	Introspection *bool `json:"introspection,omitempty"`
	// Records names the lifecycle records of the entry's scope to send, each
	// as a PostHog event named after the record: an exact name
	// ("work.transitioned"), a family ("execution.*") or every record ("*").
	// Default none.
	Records []string `json:"records,omitempty"`
}

// Renamed maps each entry setting renamed in this release to its new name,
// for `migrate upgrade`.
var Renamed = map[string]string{"introspection": "telemetry_events"}

// Settings is a parsed entry. OldNames lists the renamed settings it still
// uses.
type Settings struct {
	Turns, TelemetryEvents bool
	Records                []string
	OldNames               []string
}

// Sends reports whether the entry selects the lifecycle record name.
func (s Settings) Sends(name string) bool {
	for _, p := range s.Records {
		if p == name || strings.HasSuffix(p, "*") && strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// ParseExtension decodes and defaults an entry.
func ParseExtension(data []byte) (Settings, error) {
	var c ExtensionConfig
	if err := strict(data, &c); err != nil {
		return Settings{}, fmt.Errorf("posthog extension: %w", err)
	}
	s := Settings{Turns: true, TelemetryEvents: true}
	if c.Turns != nil {
		s.Turns = *c.Turns
	}
	if c.Introspection != nil {
		if c.TelemetryEvents != nil {
			return Settings{}, errors.New("posthog extension: introspection is the old name of telemetry_events; give only telemetry_events")
		}
		c.TelemetryEvents = c.Introspection
		s.OldNames = append(s.OldNames, "introspection")
	}
	if c.TelemetryEvents != nil {
		s.TelemetryEvents = *c.TelemetryEvents
	}
	for _, p := range c.Records {
		if p == "" || strings.Contains(strings.TrimSuffix(p, "*"), "*") || strings.ContainsAny(p, " \t\r\n") {
			return Settings{}, fmt.Errorf("posthog extension: records: %q is not a record name, a family such as \"execution.*\" or \"*\"", p)
		}
	}
	s.Records = c.Records
	return s, nil
}

// ValidateExtension is the config.Registry extension validator.
func ValidateExtension(data json.RawMessage, _ []string) error {
	_, err := ParseExtension(data)
	return err
}

// Sink is one PostHog project.
type Sink struct {
	conn   ConnectionConfig
	token  func(context.Context) (string, error)
	client *http.Client
}

// NewSink binds a connection; token resolves the project key lazily.
func NewSink(conn ConnectionConfig, token func(context.Context) (string, error), client *http.Client) *Sink {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Sink{conn: conn, token: token, client: client}
}

// ErrKey means the configured key is not a project API key.
var ErrKey = errors.New("posthog: that is not a project API key (they start phc_)")

// Check says where events go. PostHog's capture endpoint cannot be asked
// about a key without sending an event, so only the key's form is checked.
func (s *Sink) Check(ctx context.Context) (string, error) {
	key, err := s.token(ctx)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(key, "phc_") {
		return "", ErrKey
	}
	return "sending to " + s.conn.Host, nil
}

// StatusError is a rejected capture (never carries the body).
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return "posthog answered " + strconv.Itoa(e.Status) }

// Capture sends one event, with its scope (when it has one) as the `scope`
// property.
func (s *Sink) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	key, err := s.token(ctx)
	if err != nil {
		return err
	}
	all := map[string]any{"$lib": "open-conduit"}
	for k, v := range props {
		all[k] = v
	}
	if scope != "" {
		all["scope"] = scope
	}
	body, err := json.Marshal(map[string]any{"api_key": key, "event": name, "distinct_id": s.conn.DistinctID, "timestamp": at.UTC(), "properties": all})
	if err != nil {
		return err
	}
	return s.post(ctx, "/i/v0/e/", body)
}

// Event is one event of a batch.
type Event struct {
	Name       string
	At         time.Time
	Properties map[string]any
	// UUID, when set, lets PostHog drop a redelivered copy.
	UUID string
}

// Batch sends events in one request to the batch endpoint, each with its
// scope (when it has one) as the `scope` property.
func (s *Sink) Batch(ctx context.Context, scope string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	key, err := s.token(ctx)
	if err != nil {
		return err
	}
	batch := make([]map[string]any, 0, len(events))
	for _, e := range events {
		all := map[string]any{"$lib": "open-conduit"}
		for k, v := range e.Properties {
			all[k] = v
		}
		if scope != "" {
			all["scope"] = scope
		}
		one := map[string]any{"event": e.Name, "distinct_id": s.conn.DistinctID, "timestamp": e.At.UTC(), "properties": all}
		if e.UUID != "" {
			one["uuid"] = e.UUID
		}
		batch = append(batch, one)
	}
	body, err := json.Marshal(map[string]any{"api_key": key, "batch": batch})
	if err != nil {
		return err
	}
	return s.post(ctx, "/batch/", body)
}

func (s *Sink) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.conn.Host+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("posthog: invalid request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("posthog: unreachable")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Status: resp.StatusCode}
	}
	return nil
}

// Target is one enabled entry: the scope whose turns it sends and its sink.
type Target struct {
	Scope    string
	Settings Settings
	Sink     *Sink
}

// Exporter maps lifecycle records onto agent_turn events and sends them to
// every enabled entry of the record's scope. Targets is read per batch, so
// a config apply takes effect without a restart.
type Exporter struct {
	Targets func() []Target
	// Diagnostic receives a safe code when a capture fails (optional).
	Diagnostic func(code string)
}

var _ observability.Exporter = (*Exporter)(nil)

// TurnEvent maps a finished execution record onto agent_turn properties.
func TurnEvent(r observability.Record) (map[string]any, bool) {
	switch r.Name {
	case "execution.completed", "execution.failed", "execution.handoff", "execution.waiting", "execution.cancelled":
	default:
		return nil, false
	}
	props := map[string]any{"work_id": string(r.Correlation.WorkID),
		"execution_id": string(r.Correlation.ExecutionID), "outcome": strings.TrimPrefix(r.Name, "execution.")}
	if b := r.Correlation.ExecutorBinding; b != "" {
		props["executor_binding"] = b
	}
	for attr, prop := range map[string]string{"execution.attempt": "attempt", "execution.duration_ms": "duration_ms"} {
		if n, err := strconv.ParseInt(r.Attributes[attr], 10, 64); err == nil {
			props[prop] = n
		}
	}
	// status, trigger_type and trigger_at let a consumer measure time in
	// each state: the gap from a turn that ended in "review" to the next
	// turn's trigger_at is how long the Work waited on a person. A comment
	// trigger's type says who wrote it (<source>.comment.human, .bot or
	// .agent).
	for attr, prop := range map[string]string{"executor": "executor", "work.kind": "work_kind", "turn.status": "status",
		"trigger.type": "trigger_type", "trigger.occurred_at": "trigger_at", "execution.started_at": "started_at"} {
		if v := r.Attributes[attr]; v != "" {
			props[prop] = v
		}
	}
	if u := r.Usage; u != nil {
		if u.InputTokens != nil {
			props["input_tokens"] = *u.InputTokens
		}
		if u.OutputTokens != nil {
			props["output_tokens"] = *u.OutputTokens
		}
		if u.CachedTokens != nil {
			props["cached_tokens"] = *u.CachedTokens
		}
		if u.Cost != nil {
			props["cost"] = *u.Cost
			props["currency"] = u.Currency
		}
	}
	return props, true
}

// RecordEvent maps a lifecycle record onto an event named after it. Its
// properties are the record's correlation, transition and attributes
// (integers as numbers) and usage; the pipeline has already redacted them.
// The UUID derives from the scope, record and record_id, so a redelivered
// record is one event in PostHog.
func RecordEvent(r observability.Record) Event {
	c := r.Correlation
	props := map[string]any{"record": r.Name, "trace_id": c.TraceID}
	add := func(k, v string) {
		if v != "" {
			props[k] = v
		}
	}
	add("event_id", string(c.EventID))
	add("work_id", string(c.WorkID))
	add("execution_id", string(c.ExecutionID))
	add("executor_binding", c.ExecutorBinding)
	add("from", r.From)
	add("to", r.To)
	for k, v := range r.Attributes {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && strconv.FormatInt(n, 10) == v {
			props[k] = n
		} else {
			props[k] = v
		}
	}
	if u := r.Usage; u != nil {
		if u.InputTokens != nil {
			props["input_tokens"] = *u.InputTokens
		}
		if u.OutputTokens != nil {
			props["output_tokens"] = *u.OutputTokens
		}
		if u.CachedTokens != nil {
			props["cached_tokens"] = *u.CachedTokens
		}
		if u.Cost != nil {
			props["cost"] = *u.Cost
			props["currency"] = u.Currency
		}
	}
	e := Event{Name: r.Name, At: r.At, Properties: props}
	if id := r.Attributes["record_id"]; id != "" {
		h := sha256.Sum256([]byte(string(c.ScopeID) + "\x00" + r.Name + "\x00" + id))
		h[6] = h[6]&0x0f | 0x80 // version 8 (custom)
		h[8] = h[8]&0x3f | 0x80 // RFC 9562 variant
		e.UUID = fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
	}
	return e
}

// Export sends one batch. A failed capture is reported and the batch moves
// on: the pipeline already isolates a failing exporter, and a retry would
// send the events that succeeded twice.
func (x *Exporter) Export(ctx context.Context, records []observability.Record) error {
	if x.Targets == nil {
		return nil
	}
	targets := x.Targets()
	if len(targets) == 0 {
		return nil
	}
	var failed error
	report := func(err error) {
		failed = err
		if x.Diagnostic != nil {
			x.Diagnostic(code(err))
		}
	}
	for _, t := range targets {
		if len(t.Settings.Records) == 0 {
			continue
		}
		var events []Event
		for _, r := range records {
			if t.Scope == string(r.Correlation.ScopeID) && t.Settings.Sends(r.Name) {
				events = append(events, RecordEvent(r))
			}
		}
		if err := t.Sink.Batch(ctx, t.Scope, events); err != nil {
			report(err)
		}
	}
	for _, r := range records {
		props, ok := TurnEvent(r)
		if !ok {
			continue
		}
		for _, t := range targets {
			if !t.Settings.Turns || t.Scope != string(r.Correlation.ScopeID) {
				continue
			}
			if err := t.Sink.Capture(ctx, t.Scope, EventTurn, r.At, props); err != nil {
				report(err)
			}
		}
	}
	if failed != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// Shutdown has nothing to flush: every event is sent as it comes.
func (x *Exporter) Shutdown(context.Context) error { return nil }

func code(err error) string {
	var s *StatusError
	switch {
	case errors.As(err, &s):
		return "http_" + strconv.Itoa(s.Status)
	case errors.Is(err, ErrKey):
		return "invalid_key"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	return "capture_failed"
}

// Code maps a capture failure onto a safe diagnostic code.
func Code(err error) string { return code(err) }

// Fanout forwards the host's named telemetry events to every enabled entry
// that takes them (all of them for an event with no scope, else that
// scope's), besides an optional primary sink. Forwarding failures never
// fail the primary.
type Fanout struct {
	Primary interface {
		Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error
	}
	Targets    func() []Target
	Diagnostic func(code string)
	mu         sync.Mutex
}

// Capture writes to the primary, then forwards.
func (f *Fanout) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	var err error
	if f.Primary != nil {
		err = f.Primary.Capture(ctx, scope, name, at, props)
	}
	if f.Targets == nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.Targets() {
		if !t.Settings.TelemetryEvents || (scope != "" && t.Scope != scope) {
			continue
		}
		cx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if e := t.Sink.Capture(cx, scope, name, at, props); e != nil && f.Diagnostic != nil {
			f.Diagnostic(code(e))
		}
		cancel()
	}
	return err
}
