package integration_test

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/bootstrap"
)

// TestOwnerConfigDiagnostics: offline, a bundled extension's and a bundled
// executor's refusals of their config come back as their own diagnostics,
// each at its field's full path with a stable code, across the protocol
// (issue #576), and never quote the refused value.
func TestOwnerConfigDiagnostics(t *testing.T) {
	releaseLayout(t)
	doc := `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"ops","name":"ops",
		"authorization":{"connections":[],"operations":[],"read_scopes":[]},
		"secret_refs":{"hook":"env:HOOK"},
		"extensions":[{"name":"jobs","type":"schedule","config":{"namespace":"schedule/ops","scope":"ops","jobs":[
			{"name":"tick","every_minutes":60},
			{"name":"report","cron":"not a cron","time_zone":"Europe/London"}]}}],
		"bindings":[{"name":"agent","executor":"claude-code","operations":[],"secret_names":["hook"],
			"config":{"extra_args":["--verbose","--mcp-config=/x.json"],"secret_env":{"missing":"API_KEY"}}}]}]}`
	diags, err := bootstrap.ValidateConfigAt("", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"scopes/0/extensions/0/config/jobs/1/cron":      "invalid_cron",
		"scopes/0/bindings/0/config/extra_args/1":       "reserved_argument",
		"scopes/0/bindings/0/config/secret_env/missing": "secret_not_selected",
	}
	for _, d := range diags {
		if strings.Contains(d.Message, "not a cron") || strings.Contains(d.Message, "/x.json") {
			t.Errorf("%s quotes the refused value: %s", d.Path, d.Message)
		}
		if want[d.Path] == d.Code {
			delete(want, d.Path)
		}
	}
	if len(want) > 0 {
		t.Fatalf("missing %v in %+v", want, diags)
	}
}
