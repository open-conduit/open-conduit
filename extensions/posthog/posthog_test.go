package posthog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/observability"
)

func TestConfig(t *testing.T) {
	for _, bad := range []string{`{"host":"ftp://x"}`, `{"host":"https://u:p@x"}`, `{"host":"https://x/path"}`, `{"token":"x"}`} {
		if ValidateConnection(json.RawMessage(bad)) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	c, err := ParseConnection([]byte(`{}`))
	if err != nil || c.Host != DefaultHost || c.DistinctID == "" {
		t.Fatal(c, err)
	}
	s, err := ParseExtension([]byte(`{"turns":false}`))
	if err != nil || s.Turns || !s.TelemetryEvents || len(s.OldNames) != 0 {
		t.Fatal(s, err)
	}
	// The old name is accepted for one release and reported; both is a mistake.
	if s, err = ParseExtension([]byte(`{"introspection":false}`)); err != nil || s.TelemetryEvents || len(s.OldNames) != 1 || Renamed[s.OldNames[0]] != "telemetry_events" {
		t.Fatal(s, err)
	}
	if ValidateExtension(json.RawMessage(`{"introspection":true,"telemetry_events":true}`), nil) == nil {
		t.Fatal("old and new name together")
	}
	if ValidateExtension(json.RawMessage(`{"other":1}`), nil) == nil {
		t.Fatal("unknown field")
	}
}

type capture struct {
	mu     sync.Mutex
	events []map[string]any
	status int
}

func (c *capture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var e map[string]any
		_ = json.Unmarshal(b, &e)
		c.mu.Lock()
		c.events = append(c.events, e)
		status := c.status
		c.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
	}))
}

func TestExporterAndFanout(t *testing.T) {
	c := &capture{}
	srv := c.server()
	defer srv.Close()
	key := func(context.Context) (string, error) { return "phc_x", nil }
	sink := NewSink(ConnectionConfig{Host: srv.URL, DistinctID: "me"}, key, nil)
	if where, err := sink.Check(context.Background()); err != nil || where != "sending to "+srv.URL {
		t.Fatal(where, err)
	}
	bad := NewSink(ConnectionConfig{Host: srv.URL}, func(context.Context) (string, error) { return "sk_x", nil }, nil)
	if _, err := bad.Check(context.Background()); !errors.Is(err, ErrKey) {
		t.Fatal("non-project key")
	}
	in, out, cached := uint64(10), uint64(5), uint64(2)
	cost := 0.25
	rec := func(name, scope string) observability.Record {
		return observability.Record{Name: name, At: time.Unix(100, 0), Correlation: observability.Correlation{ScopeID: "eng", WorkID: "w", ExecutionID: "x", ExecutorBinding: "opus-agent"},
			Attributes: map[string]string{"execution.duration_ms": "1500", "executor": "claude-code", "execution.attempt": "2", "work.kind": "github.issue",
				"turn.status": "review", "trigger.type": "github.comment.human", "trigger.occurred_at": "1970-01-01T00:01:00Z", "execution.started_at": "1970-01-01T00:01:05Z"},
			Usage: &observability.Measurement{InputTokens: &in, OutputTokens: &out, CachedTokens: &cached, Cost: &cost, Currency: "USD"}}
	}
	other := rec("execution.completed", "eng")
	other.Correlation.ScopeID = "ops"
	var codes []string
	x := &Exporter{Targets: func() []Target { return []Target{{Scope: "eng", Settings: Settings{Turns: true}, Sink: sink}} }, Diagnostic: func(c string) { codes = append(codes, c) }}
	if err := x.Export(context.Background(), []observability.Record{rec("execution.completed", "eng"), rec("execution.started", "eng"), other}); err != nil {
		t.Fatal(err)
	}
	if len(c.events) != 1 {
		t.Fatalf("events %v", c.events)
	}
	e := c.events[0]
	props := e["properties"].(map[string]any)
	if e["event"] != EventTurn || e["api_key"] != "phc_x" || e["distinct_id"] != "me" || props["outcome"] != "completed" || props["duration_ms"] != 1500.0 ||
		props["input_tokens"] != 10.0 || props["cost"] != 0.25 || props["executor"] != "claude-code" || props["$lib"] != "open-conduit" || props["scope"] != "eng" ||
		props["attempt"] != 2.0 || props["work_kind"] != "github.issue" || props["status"] != "review" || props["trigger_type"] != "github.comment.human" || props["executor_binding"] != "opus-agent" ||
		props["trigger_at"] != "1970-01-01T00:01:00Z" || props["started_at"] != "1970-01-01T00:01:05Z" {
		t.Fatalf("event %v", e)
	}
	c.status = http.StatusBadRequest
	if err := x.Export(context.Background(), []observability.Record{rec("execution.failed", "eng")}); err != nil || len(codes) != 1 || codes[0] != "http_400" {
		t.Fatalf("a failed capture is reported, not retried: %v %v", err, codes)
	}
	c.status = 0
	primary := &capture{}
	f := &Fanout{Primary: sinkFunc(func(name string) { primary.events = append(primary.events, map[string]any{"event": name}) }),
		Targets: func() []Target {
			return []Target{{Scope: "eng", Settings: Settings{TelemetryEvents: true}, Sink: sink}, {Scope: "ops", Settings: Settings{TelemetryEvents: false}, Sink: sink},
				{Scope: "web", Settings: Settings{TelemetryEvents: true}, Sink: sink}}
		}}
	before := len(c.events)
	if err := f.Capture(context.Background(), "", "host_metrics", time.Now(), map[string]any{"load": 1}); err != nil {
		t.Fatal(err)
	}
	if len(primary.events) != 1 || len(c.events) != before+1 {
		t.Fatalf("an event with no scope goes once to each project that takes them, though two scopes' entries share it: primary %d posthog %d", len(primary.events), len(c.events)-before)
	}
	otherProject := NewSink(ConnectionConfig{Host: srv.URL, DistinctID: "me"}, func(context.Context) (string, error) { return "phc_other", nil }, nil)
	two := &Fanout{Targets: func() []Target {
		return []Target{{Scope: "eng", Settings: Settings{TelemetryEvents: true}, Sink: sink}, {Scope: "web", Settings: Settings{TelemetryEvents: true}, Sink: otherProject}}
	}}
	before = len(c.events)
	if err := two.Capture(context.Background(), "", "host_metrics", time.Now(), map[string]any{}); err != nil || len(c.events) != before+2 {
		t.Fatalf("another project still gets it: %v %d", err, len(c.events)-before)
	}
	before = len(c.events)
	if err := f.Capture(context.Background(), "", "host_metrics", time.Now(), map[string]any{"load": 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.events[before]["properties"].(map[string]any)["scope"]; ok {
		t.Fatal("no scope, no scope property")
	}
	before = len(c.events)
	if err := f.Capture(context.Background(), "web", "introspection_report", time.Now(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if len(c.events) != before+1 || c.events[before]["properties"].(map[string]any)["scope"] != "web" {
		t.Fatalf("a scoped event goes to its scope's entries, with its scope: %v", c.events[before:])
	}
}

type sinkFunc func(string)

func (s sinkFunc) Capture(_ context.Context, _, name string, _ time.Time, _ map[string]any) error {
	s(name)
	return nil
}

func TestRecordsSetting(t *testing.T) {
	s, err := ParseExtension([]byte(`{}`))
	if err != nil || len(s.Records) != 0 || s.Sends("work.created") {
		t.Fatal("no records by default", s, err)
	}
	if s, err = ParseExtension([]byte(`{"records":["work.transitioned","execution.*"]}`)); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"work.transitioned": true, "work.created": false, "execution.queued": true, "executionx": false, "lease.renewed": false} {
		if s.Sends(name) != want {
			t.Errorf("Sends(%q) = %v", name, !want)
		}
	}
	if s, _ = ParseExtension([]byte(`{"records":["*"]}`)); !s.Sends("lease.renewed") {
		t.Fatal(`"*" sends every record`)
	}
	for _, bad := range []string{`{"records":[""]}`, `{"records":["exec*.started"]}`, `{"records":["work created"]}`, `{"records":"*"}`} {
		if ValidateExtension(json.RawMessage(bad), nil) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestExporterRecords(t *testing.T) {
	type request struct {
		path string
		body map[string]any
	}
	var mu sync.Mutex
	var got []request
	status := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		got = append(got, request{r.URL.Path, body})
		s := status
		mu.Unlock()
		if s != 0 {
			w.WriteHeader(s)
		}
	}))
	defer srv.Close()
	sink := NewSink(ConnectionConfig{Host: srv.URL, DistinctID: "me"}, func(context.Context) (string, error) { return "phc_x", nil }, nil)
	in := uint64(7)
	cost := 0.5
	rec := func(name, scope, id string) observability.Record {
		return observability.Record{Name: name, At: time.Unix(100, 0), From: "open", To: "running",
			Correlation: observability.Correlation{ScopeID: domain.ID(scope), EventID: "e1", WorkID: "w1", TraceID: "t1"},
			Attributes:  map[string]string{"record_id": id, "work.revision": "3", "work.kind": "github.issue", "padded": "007"}}
	}
	done := rec("execution.completed", "eng", "r3")
	done.Correlation.ExecutionID, done.Correlation.ExecutorBinding = "x1", "opus-agent"
	done.Attributes["execution.duration_ms"] = "1500"
	done.Usage = &observability.Measurement{InputTokens: &in, Cost: &cost, Currency: "USD"}
	batch := []observability.Record{rec("work.transitioned", "eng", "r1"), rec("lease.renewed", "eng", "r2"), done, rec("work.transitioned", "ops", "r4")}
	var codes []string
	x := &Exporter{Targets: func() []Target {
		return []Target{{Scope: "eng", Settings: Settings{Records: []string{"work.*", "execution.*"}}, Sink: sink}, {Scope: "web", Settings: Settings{Records: []string{"*"}}, Sink: sink}}
	}, Diagnostic: func(c string) { codes = append(codes, c) }}
	if err := x.Export(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].path != "/batch/" || got[0].body["api_key"] != "phc_x" {
		t.Fatalf("one batch request for the entry with records in its scope, none for an empty one, no agent_turn without turns: %+v", got)
	}
	events := got[0].body["batch"].([]any)
	if len(events) != 2 {
		t.Fatalf("the selected records of the entry's scope only: %v", events)
	}
	first, second := events[0].(map[string]any), events[1].(map[string]any)
	p := first["properties"].(map[string]any)
	if first["event"] != "work.transitioned" || first["distinct_id"] != "me" || first["uuid"] == nil || p["scope"] != "eng" || p["$lib"] != "open-conduit" ||
		p["from"] != "open" || p["to"] != "running" || p["work_id"] != "w1" || p["event_id"] != "e1" || p["trace_id"] != "t1" ||
		p["work.revision"] != 3.0 || p["padded"] != "007" || p["work.kind"] != "github.issue" || p["execution_id"] != nil {
		t.Fatalf("record event %v", first)
	}
	p = second["properties"].(map[string]any)
	if second["event"] != "execution.completed" || p["execution_id"] != "x1" || p["executor_binding"] != "opus-agent" || p["execution.duration_ms"] != 1500.0 ||
		p["input_tokens"] != 7.0 || p["cost"] != 0.5 || p["currency"] != "USD" || p["output_tokens"] != nil {
		t.Fatalf("execution event %v", second)
	}
	// A redelivered record keeps its UUID; another record, or the same one in another scope, does not share it.
	again, other, elsewhere := RecordEvent(batch[0]), RecordEvent(batch[2]), RecordEvent(batch[3])
	if again.UUID != first["uuid"] || other.UUID == again.UUID || elsewhere.UUID == again.UUID || len(again.UUID) != 36 {
		t.Fatalf("uuids %q %q %q", again.UUID, other.UUID, elsewhere.UUID)
	}
	if noID := RecordEvent(observability.Record{Name: "work.created"}); noID.UUID != "" {
		t.Fatal("no record_id, no uuid")
	}
	status = http.StatusServiceUnavailable
	if err := x.Export(context.Background(), batch); err != nil || len(codes) != 1 || codes[0] != "http_503" {
		t.Fatalf("a failed batch is reported, not retried: %v %v", err, codes)
	}
}
