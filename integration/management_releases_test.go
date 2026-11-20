package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/management"
)

// fakeReleases is the daemon's release installer: latest is the latest
// release, installed what Install was asked to put in place.
type fakeReleases struct {
	mu        sync.Mutex
	latest    string
	installed []string
}

func (f *fakeReleases) Latest(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest, nil
}

func (f *fakeReleases) Install(_ context.Context, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, v)
	return nil
}

func (f *fakeReleases) installs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.installed...)
}

// Installing a release replaces the daemon's binary, so the host checks it
// as it checks the other administrative routes: view and operate are
// refused before anything is installed, admin installs, a stale version
// installs nothing, and every call that reaches it is in the operator
// trail.
func TestManagementInstallReleaseNeedsAdmin(t *testing.T) {
	ctx := context.Background()
	h := newManagementHarness(t)
	for _, role := range []string{"view", "operate"} {
		path := filepath.Join(h.state, role+".token")
		h.mustCLI(nil, "auth", "add", "olly-"+role, "--role", role, "--out", path)
		f, err := client.ReadTokenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(h.endpoint, f.Token)
		if err != nil {
			t.Fatal(err)
		}
		var api *client.Error
		if _, err = c.InstallRelease(ctx, "1.0.0", "k-"+role); !errors.As(err, &api) || api.Status != 403 || api.Code != "forbidden_role" {
			t.Fatalf("%s install: %v", role, err)
		}
		if n := h.releases.installs(); len(n) != 0 {
			t.Fatalf("%s install put %v in place before it was refused", role, n)
		}
	}
	admin := h.operatorClient()
	if _, err := admin.InstallRelease(ctx, "0.9.5", "k-stale"); apiCode(err) != "stale_release" || !strings.Contains(err.Error(), "1.0.0") || len(h.releases.installs()) != 0 {
		t.Fatalf("stale install = %v, installed %v", err, h.releases.installs())
	}
	got, err := admin.InstallRelease(ctx, "1.0.0", "k-admin")
	if err != nil || got.Version != "1.0.0" {
		t.Fatalf("admin install = %+v, %v", got, err)
	}
	if n := h.releases.installs(); len(n) != 1 || n[0] != "1.0.0" {
		t.Fatalf("installed %v", n)
	}
	outcomes := map[string]int{}
	for _, r := range h.observed.named("operator.release_installed") {
		if r.Attributes["operator.actor"] != "operator:local" {
			t.Fatalf("actor: %+v", r)
		}
		outcomes[r.Attributes["operator.target"]+" "+r.Attributes["operator.outcome"]]++
	}
	if outcomes["1.0.0 ok"] != 1 || outcomes["0.9.5 stale_release"] != 1 || len(outcomes) != 2 {
		t.Fatalf("operator trail: %v", outcomes)
	}
}

// With updates off the daemon installs nothing, even for an admin.
func TestManagementInstallReleaseUpdatesOff(t *testing.T) {
	h := newHarness(t, managementConfig, nil, nil, false)
	if _, err := h.operatorClient().InstallRelease(context.Background(), "1.0.0", "k"); apiCode(err) != "updates_off" {
		t.Fatalf("install with updates off = %v", err)
	}
	if n := h.releases.installs(); len(n) != 0 {
		t.Fatalf("installed %v with updates off", n)
	}
}

var _ management.Releases = (*fakeReleases)(nil)
