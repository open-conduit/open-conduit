package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
)

type fakeUpdates struct {
	latest    upgrade.Release
	homebrew  bool
	installed []string
}

func (f *fakeUpdates) Cached() (upgrade.Release, bool) { return f.latest, f.latest.Version != "" }
func (f *fakeUpdates) Latest(context.Context) (upgrade.Release, error) {
	return f.latest, nil
}
func (f *fakeUpdates) Homebrew() bool { return f.homebrew }
func (f *fakeUpdates) Install(_ context.Context, v string) error {
	f.installed = append(f.installed, v)
	return nil
}

type fakeService struct {
	running   bool
	restarted chan struct{}
}

func (f *fakeService) Running() bool { return f.running }
func (f *fakeService) Restart() error {
	close(f.restarted)
	return nil
}

func updateSetup(t *testing.T, u *fakeUpdates, svc *fakeService) (*fake, *Server, http.Handler) {
	t.Helper()
	f := newFake()
	o := Options{Addr: addr, NewKey: func() string { return "k" }, Now: func() time.Time { return time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC) }, Updates: u}
	if svc != nil {
		o.Service = svc
	}
	s, err := New(f, o)
	if err != nil {
		t.Fatal(err)
	}
	return f, s, s.Handler(addr)
}

func TestStatusLineOffersANewerRelease(t *testing.T) {
	u := &fakeUpdates{latest: upgrade.Release{Version: "1.0.0", URL: "https://example.test/v1.0.0"}}
	f, s, h := updateSetup(t, u, nil)
	if body := do(h, "GET", "/", nil, s).Body.String(); !strings.Contains(body, `href="/update">v1.0.0 available`) {
		t.Fatal("no notice for a newer release")
	}
	f.health = &client.Health{State: "ready", Version: "1.0.0"}
	if body := do(h, "GET", "/", nil, s).Body.String(); strings.Contains(body, "available</a>") {
		t.Fatal("notice while up to date")
	}
	f.health = &client.Health{State: "ready", Version: "dev"}
	if body := do(h, "GET", "/", nil, s).Body.String(); strings.Contains(body, "available</a>") {
		t.Fatal("notice on a dev build")
	}
	_, s2, h2 := setup(t)
	if body := do(h2, "GET", "/", nil, s2).Body.String(); strings.Contains(body, "available</a>") {
		t.Fatal("notice without an updater")
	}
}

func TestUpdatePageInstallsAndRestarts(t *testing.T) {
	u := &fakeUpdates{latest: upgrade.Release{Version: "1.0.0", URL: "https://example.test/v1.0.0"}}
	svc := &fakeService{running: true, restarted: make(chan struct{})}
	_, s, h := updateSetup(t, u, svc)
	body := do(h, "GET", "/update", nil, s).Body.String()
	for _, want := range []string{"1.0.0 is out", "0.9.0", `name="version" value="1.0.0"`, "Update to 1.0.0", "restarts on the new version", "https://example.test/v1.0.0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("update page lacks %q", want)
		}
	}
	if w := do(h, "POST", "/update", url.Values{"version": {"1.0.0"}}, s); w.Code != http.StatusForbidden || len(u.installed) != 0 {
		t.Fatalf("without CSRF: %d %v", w.Code, u.installed)
	}
	// A page that offered an older release installs nothing.
	w := do(h, "POST", "/update", url.Values{"csrf": {s.csrf}, "version": {"0.9.5"}}, s)
	if w.Code != http.StatusSeeOther || len(u.installed) != 0 {
		t.Fatalf("stale: %d %v", w.Code, u.installed)
	}
	w = do(h, "POST", "/update", url.Values{"csrf": {s.csrf}, "version": {"1.0.0"}}, s)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/update?restarting=1.0.0" || len(u.installed) != 1 {
		t.Fatalf("update: %d %s %v", w.Code, w.Header().Get("Location"), u.installed)
	}
	select {
	case <-svc.restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("service not restarted")
	}
	body = do(h, "GET", "/update?restarting=1.0.0", nil, s).Body.String()
	if !strings.Contains(body, "Restarting on 1.0.0") || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("restarting page")
	}
}

func TestUpdateWithoutAServiceLeavesTheRestart(t *testing.T) {
	u := &fakeUpdates{latest: upgrade.Release{Version: "1.0.0"}}
	_, s, h := updateSetup(t, u, nil)
	if body := do(h, "GET", "/update", nil, s).Body.String(); !strings.Contains(body, "restart <code>open-conduit serve</code> afterwards") {
		t.Fatal("no manual restart note")
	}
	w := do(h, "POST", "/update", url.Values{"csrf": {s.csrf}, "version": {"1.0.0"}}, s)
	if w.Header().Get("Location") != "/update" || !strings.Contains(w.Header().Get("Set-Cookie"), "Restart+open-conduit+serve") {
		t.Fatalf("no-service update: %s %s", w.Header().Get("Location"), w.Header().Get("Set-Cookie"))
	}
}

func TestUpdatePageLeavesHomebrewToBrew(t *testing.T) {
	u := &fakeUpdates{latest: upgrade.Release{Version: "1.0.0"}, homebrew: true}
	_, s, h := updateSetup(t, u, nil)
	body := do(h, "GET", "/update", nil, s).Body.String()
	if !strings.Contains(body, upgrade.BrewCommand) || strings.Contains(body, `action="/update"`) {
		t.Fatal("homebrew page")
	}
}
