package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
)

type fakeUpdates struct {
	latest   upgrade.Release
	homebrew bool
}

func (f *fakeUpdates) Cached() (upgrade.Release, bool) { return f.latest, f.latest.Version != "" }
func (f *fakeUpdates) Latest(context.Context) (upgrade.Release, error) {
	return f.latest, nil
}
func (f *fakeUpdates) Homebrew() bool { return f.homebrew }

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
	f, s, h := updateSetup(t, u, svc)
	body := do(h, "GET", "/update", nil, s).Body.String()
	for _, want := range []string{"1.0.0 is out", "0.9.0", `name="version" value="1.0.0"`, "Update to 1.0.0", "restarts on the new version", "https://example.test/v1.0.0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("update page lacks %q", want)
		}
	}
	if w := do(h, "POST", "/update", url.Values{"version": {"1.0.0"}}, s); w.Code != http.StatusForbidden || len(f.installed) != 0 {
		t.Fatalf("without CSRF: %d %v", w.Code, f.installed)
	}
	// The daemon installs it, as the view's own credential here.
	w := do(h, "POST", "/update", url.Values{"csrf": {s.csrf}, "version": {"1.0.0"}}, s)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/update?restarting=1.0.0" || !slices.Equal(f.installed, []string{"1.0.0 as "}) {
		t.Fatalf("update: %d %s %v", w.Code, w.Header().Get("Location"), f.installed)
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

// The daemon decides who may install a release: a refusal (no admin role,
// or a stale page) is shown, and nothing restarts.
func TestUpdateShowsTheDaemonsRefusal(t *testing.T) {
	u := &fakeUpdates{latest: upgrade.Release{Version: "1.0.0"}}
	svc := &fakeService{running: true, restarted: make(chan struct{})}
	f, s, h := updateSetup(t, u, svc)
	f.installErr = &client.Error{Status: 403, Code: "forbidden_role", Message: "operator olly has role operate; this needs admin"}
	w := do(h, "POST", "/update", url.Values{"csrf": {s.csrf}, "version": {"1.0.0"}}, s)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/update" || !strings.Contains(w.Header().Get("Set-Cookie"), "this+needs+admin") {
		t.Fatalf("refused update: %d %s %s", w.Code, w.Header().Get("Location"), w.Header().Get("Set-Cookie"))
	}
	select {
	case <-svc.restarted:
		t.Fatal("restarted after a refused install")
	default:
	}
}

// Behind a sign-in proxy, the install is asked for as the person signed
// in, so the daemon checks their role, never the view's.
func TestUpdateActsForTheSignedInPerson(t *testing.T) {
	f := newFake()
	s, err := New(f, Options{Addr: addr, NewKey: func() string { return "k" }, SubjectHeader: "X-Forwarded-Access-Token", Updates: &fakeUpdates{latest: upgrade.Release{Version: "1.0.0"}}, Now: func() time.Time { return time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	session := testSession(s)
	form := url.Values{"csrf": {s.sessionToken(session)}, "version": {"1.0.0"}}
	req := httptest.NewRequest("POST", "/update", strings.NewReader(form.Encode()))
	req.Host = "open-conduit.localhost:7420"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Access-Token", "tok-alice")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	w := httptest.NewRecorder()
	s.Handler(addr).ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || !slices.Equal(f.installed, []string{"1.0.0 as tok-alice"}) {
		t.Fatalf("update as alice: %d %v", w.Code, f.installed)
	}
}

// With updates off, for a binary managed some other way, there is no
// update page, nothing to post to and no notice.
func TestUpdatesOff(t *testing.T) {
	f, s, h := setup(t)
	for _, method := range []string{"GET", "POST"} {
		var form url.Values
		if method == "POST" {
			form = url.Values{"csrf": {s.csrf}, "version": {"1.0.0"}}
		}
		if w := do(h, method, "/update", form, s); w.Code != http.StatusNotFound {
			t.Fatalf("%s /update with updates off: %d", method, w.Code)
		}
	}
	if len(f.installed) != 0 {
		t.Fatalf("installed with updates off: %v", f.installed)
	}
	if body := do(h, "GET", "/", nil, s).Body.String(); strings.Contains(body, `href="/update"`) || strings.Contains(body, `action="/update"`) {
		t.Fatal("an update control with updates off")
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
