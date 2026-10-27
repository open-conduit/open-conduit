package posthog

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

func TestExtensionConfigureAndChecks(t *testing.T) {
	c := &capture{}
	srv := c.server()
	defer srv.Close()
	var asked []string
	x := New()
	_ = x.Start(context.Background(), inproc.Host{Secrets: func(_ context.Context, scope domain.ID, conn string, names []string) (map[string]string, error) {
		asked = append(asked, string(scope)+"/"+conn)
		return map[string]string{SecretToken: "phc_x"}, nil
	}})
	conn := &protocol.ConnectionEntry{ID: "ph", Type: Type, Config: json.RawMessage(`{"host":"` + srv.URL + `"}`)}
	params := protocol.ConfigureParams{Entries: []protocol.ConfigEntry{
		{ScopeID: "eng", Type: Type, Name: "telemetry", Connection: conn},
		{ScopeID: "ops", Type: Type, Name: "telemetry"},
	}}
	x.Configure(context.Background(), params)
	if r := x.check(context.Background()); r.Status != inproc.CheckFail || r.Code != "posthog_connection_type" || len(r.Items) != 2 {
		t.Fatalf("check %+v", r)
	}
	if r := x.authCheck(context.Background()); r.Code != "posthog_connection_type" || len(asked) != 1 || asked[0] != "eng/ph" {
		t.Fatalf("auth check %+v, secrets asked %v", r, asked)
	}
	if err := x.Capture(context.Background(), "host_metrics", time.Now(), map[string]any{"scope": "eng"}); err != nil || len(c.events) != 1 {
		t.Fatalf("capture: %v, %d sent", err, len(c.events))
	}
	c.status = http.StatusBadRequest
	_ = x.Capture(context.Background(), "host_metrics", time.Now(), map[string]any{"scope": "eng"})
	params.Entries, params.DryRun = params.Entries[:1], false
	x.Configure(context.Background(), params)
	if r := x.check(context.Background()); r.Status != inproc.CheckWarn || r.Code != "capture_failing" {
		t.Fatalf("a failed capture warns: %+v", r)
	}
	params.DryRun = true
	x.Configure(context.Background(), params)
	if len(x.targets()) != 0 {
		t.Fatal("a dry run sends nothing")
	}
}
