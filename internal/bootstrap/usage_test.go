package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/observability"
)

type captured struct {
	Key        string         `json:"api_key"`
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
}

// fakePostHog records the events sent to its capture endpoint.
func fakePostHog(t *testing.T, onEvent func(captured)) (*httptest.Server, func() []captured) {
	t.Helper()
	var mu sync.Mutex
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c captured
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Errorf("capture body: %v", err)
		}
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
		onEvent(c)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		return append([]captured(nil), got...)
	}
}

func TestUsageReporterOff(t *testing.T) {
	dir := t.TempDir()
	for name, o := range map[string]Options{
		"opted out":   {StateDir: dir, Version: "1.2.3", NoTelemetry: true, TelemetryKey: "phc_test"},
		"dev build":   {StateDir: dir, Version: "dev", TelemetryKey: "phc_test"},
		"unversioned": {StateDir: dir, TelemetryKey: "phc_test"},
		"no key":      {StateDir: dir, Version: "1.2.3", TelemetryURL: "http://127.0.0.1:1"},
	} {
		if u, err := newUsageReporter(o); err != nil || u != nil {
			t.Errorf("%s: reporter %v, %v", name, u, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "telemetry-id")); !os.IsNotExist(err) {
		t.Fatalf("an off reporter wrote an install ID: %v", err)
	}
}

func TestUsageReporterReports(t *testing.T) {
	// run sends the install and the start before it waits on its ticker;
	// the start ends it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, events := fakePostHog(t, func(c captured) {
		if c.Event == "daemon_started" {
			cancel()
		}
	})
	dir := t.TempDir()
	o := Options{StateDir: dir, Version: "1.2.3", Commit: "abc", TelemetryURL: srv.URL, TelemetryKey: "phc_test"}
	u, err := newUsageReporter(o)
	if err != nil || u == nil {
		t.Fatalf("reporter: %v, %v", u, err)
	}
	u.known = map[string]bool{"github": true, "claude-code": true, "shell": true}
	snap := &config.Snapshot{
		Scopes: map[domain.ID]*config.EffectiveScope{
			"a": {Enabled: true, Extensions: []config.Extension{{Name: "gh", Type: "github"}, {Name: "mine", Type: "acme-private"}}, Bindings: []config.Binding{{Name: "agent", Executor: "claude-code"}}},
			"b": {Extensions: []config.Extension{{Name: "gh2", Type: "github"}}, Bindings: []config.Binding{{Name: "x", Executor: "shell"}}},
		},
		Connections: []config.Connection{{ID: "secret-name", Type: "github"}},
	}
	_ = u.Export(context.Background(), []observability.Record{{Name: "work.created"}, {Name: "execution.completed"}, {Name: "execution.completed"}, {Name: "execution.failed"}, {Name: "execution.started"}})

	u.run(ctx, time.Hour, func() *config.Snapshot { return snap })
	u.stop(snap, errors.New("boom"))

	got := events()
	if len(got) != 3 || got[0].Event != "install" || got[1].Event != "daemon_started" || got[2].Event != "daemon_stopped" {
		t.Fatalf("events: %+v", got)
	}
	id, _ := os.ReadFile(filepath.Join(dir, "telemetry-id"))
	for _, e := range got {
		if e.Key != "phc_test" || e.DistinctID+"\n" != string(id) || e.Properties["version"] != "1.2.3" || e.Properties["$process_person_profile"] != false || e.Properties["$geoip_disable"] != true {
			t.Fatalf("event: %+v", e)
		}
	}
	started := got[1].Properties
	if started["scopes"] != float64(2) || started["scopes_enabled"] != float64(1) || started["third_party_types"] != float64(1) {
		t.Fatalf("shape: %+v", started)
	}
	if b, _ := json.Marshal([]any{started["extensions"], started["connections"], started["executors"]}); string(b) != `[["github"],["github"],["claude-code","shell"]]` {
		t.Fatalf("types: %s", b)
	}
	stopped := got[2].Properties
	if stopped["runs"] != float64(3) || stopped["runs_completed"] != float64(2) || stopped["runs_failed"] != float64(1) || stopped["work_created"] != float64(1) || stopped["error"] != "other" {
		t.Fatalf("stopped: %+v", stopped)
	}
	raw, _ := json.Marshal(got)
	for _, private := range []string{"acme-private", "secret-name", "boom", "agent", "mine"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("report names %q: %s", private, raw)
		}
	}

	// A second start keeps the install ID and reports no new install.
	again, err := newUsageReporter(o)
	if err != nil || again.installed || again.id != u.id {
		t.Fatalf("restart: %+v, %v", again, err)
	}
}
