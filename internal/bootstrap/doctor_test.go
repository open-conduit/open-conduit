package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/runtime"
)

// OfflineChecks validates the config without a daemon and names the
// daemon's checks it skipped; an invalid config stops at its diagnostics.
// The packages' offline checks (claude-code's, GitHub's) need the release
// layout: integration's TestOfflineDoctorRunsPackageChecks.
func TestOfflineChecks(t *testing.T) {
	config := `{
  "version": 1, "settings": {}, "connections": [],
  "scopes": [{"id": "ops", "name": "Operations", "authorization": {"connections": [], "operations": [], "read_scopes": []}, "secret_refs": {}}]
}`
	checks := OfflineChecks(context.Background(), "/cfg/config.json", []byte(config))
	got := map[string]runtime.Check{}
	for _, c := range checks {
		got[c.Name] = c
	}
	if got["config"].Status != runtime.CheckOK || !strings.Contains(checks[len(checks)-1].Detail, "storage") {
		t.Fatalf("offline checks: %+v", checks)
	}

	checks = OfflineChecks(context.Background(), "/cfg/config.json", []byte(`{"version": 1}`))
	if len(checks) != 2 || checks[1].Name != "skipped" || checks[0].Name != "config" || checks[0].Status != runtime.CheckFail || len(checks[0].Items) == 0 {
		t.Fatalf("invalid config: %+v", checks)
	}
}
