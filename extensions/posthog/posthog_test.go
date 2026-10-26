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

	"github.com/alsiola/open-conduit/observability"
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
	if err != nil || s.Turns || !s.Introspection {
		t.Fatal(s, err)
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
		return observability.Record{Name: name, At: time.Unix(100, 0), Correlation: observability.Correlation{ScopeID: "eng", WorkID: "w", ExecutionID: "x"},
			Attributes: map[string]string{"execution.duration_ms": "1500", "executor": "claude-code", "execution.attempt": "2"},
			Usage:      &observability.Measurement{InputTokens: &in, OutputTokens: &out, CachedTokens: &cached, Cost: &cost, Currency: "USD"}}
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
		props["input_tokens"] != 10.0 || props["cost"] != 0.25 || props["executor"] != "claude-code" || props["$lib"] != "open-conduit" {
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
			return []Target{{Scope: "eng", Settings: Settings{Introspection: true}, Sink: sink}, {Scope: "ops", Settings: Settings{Introspection: false}, Sink: sink}}
		}}
	before := len(c.events)
	if err := f.Capture(context.Background(), "host_metrics", time.Now(), map[string]any{"load": 1}); err != nil {
		t.Fatal(err)
	}
	if len(primary.events) != 1 || len(c.events) != before+1 {
		t.Fatalf("fanout: primary %d posthog %d", len(primary.events), len(c.events)-before)
	}
}

type sinkFunc func(string)

func (s sinkFunc) Capture(_ context.Context, name string, _ time.Time, _ map[string]any) error {
	s(name)
	return nil
}
