package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

// An entry's reconcile_seconds reaches the running source: doctor reports
// that interval, and judges the entry's lag against it.
func TestDaemonEntryReconcileInterval(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	fake.SetPermissions(map[string]bool{"pull": true, "push": true, "admin": false})
	d := newDrill(t)
	o := newOperatorCLI(t, d)
	o.must(nil, "init", "github", "--repo", "octo/widgets", "--scope", "eng", "--no-workspace", "--token", "file:github-token")
	o.stdin = "tok\n"
	o.must(nil, "secret", "set", "github-token", "--store", "file")
	o.stdin = ""
	o.must(nil, "config", "set", "/connections/github/config", `{"api_url":"`+fake.URL+`"}`)
	config := filepath.Join(d.root, "xdg", "open-conduit", "config.json")
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Scopes []struct {
			ID         string `json:"id"`
			Extensions []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"extensions"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	entry := ""
	for _, sc := range doc.Scopes {
		for _, e := range sc.Extensions {
			if sc.ID == "eng" && e.Type == "github" {
				entry = e.Name
			}
		}
	}
	if entry == "" {
		t.Fatalf("no github entry in %s", raw)
	}
	o.must(nil, "config", "set", "/scopes/eng/extensions/"+entry+"/config/reconcile_seconds", "7200")
	d.config = config
	d.start("")
	var report client.Doctor
	d.until("the entry's interval in doctor", 30*time.Second, func() bool {
		var err error
		report, err = d.c.Doctor(context.Background(), false)
		return err == nil && strings.Contains(strings.Join(check(report, "reconcile_lag").Items, " "), ": reconciled")
	})
	lag := check(report, "reconcile_lag")
	if lag.Code != "current" || !strings.Contains(strings.Join(lag.Items, " "), "(every 2h0m0s)") {
		t.Fatalf("reconcile_lag: %+v", lag)
	}
}
