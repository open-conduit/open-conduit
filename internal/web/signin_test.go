package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withSession sends a request carrying the session cookie value.
func withSession(h http.Handler, method, target, value string, form ...url.Values) *httptest.ResponseRecorder {
	var req *http.Request
	if len(form) > 0 {
		req = httptest.NewRequest(method, target, strings.NewReader(form[0].Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Host = "open-conduit.localhost:7420"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func sessionOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			return c.Value
		}
	}
	t.Fatalf("no session cookie set: %d %v", w.Code, w.Header())
	return ""
}

// Each sign-in gets a session of its own, which lapses once idle too long
// or old enough, however busy, and is renewed while in use.
func TestSessionsRotateAndExpire(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, err := New(newFake(), Options{Addr: addr, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler(addr)
	login := func() string { return sessionOf(t, do(h, "GET", "/login?token="+s.login, nil, nil)) }
	a, b := login(), login()
	if a == b {
		t.Fatal("two sign-ins shared a session")
	}
	// A forged or planted cookie gets nowhere.
	for _, bad := range []string{s.sess, "1.2.3.4", a[:len(a)-1] + "0", strings.Replace(a, ".", "9.", 1)} {
		if w := withSession(h, "GET", "/attention", bad); w.Code != http.StatusUnauthorized {
			t.Fatalf("forged %q: %d", bad, w.Code)
		}
	}
	// Idle past SessionIdle: signed out.
	now = now.Add(SessionIdle + time.Second)
	if w := withSession(h, "GET", "/attention", a); w.Code != http.StatusUnauthorized {
		t.Fatalf("idle session: %d", w.Code)
	}
	// In use, the session is renewed and outlives SessionIdle, but not
	// SessionLifetime.
	c := login()
	for elapsed := time.Duration(0); elapsed < SessionLifetime; elapsed += SessionIdle / 2 {
		w := withSession(h, "GET", "/attention", c)
		if w.Code != http.StatusOK {
			t.Fatalf("busy session after %s: %d", elapsed, w.Code)
		}
		if elapsed > 0 {
			c = sessionOf(t, w)
		}
		now = now.Add(SessionIdle / 2)
	}
	now = now.Add(time.Second)
	if w := withSession(h, "GET", "/attention", c); w.Code != http.StatusUnauthorized {
		t.Fatalf("session past its lifetime: %d", w.Code)
	}
}

// Signing out everywhere voids every session, keeps the new key, and
// leaves the sign-in URL working.
func TestSignOutEverywhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "web.signin")
	in, err := LoadSignIn(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(newFake(), Options{Addr: addr, SignIn: &in, SaveSignIn: func(n SignIn) error { return SaveSignIn(path, n) }})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler(addr)
	mine := sessionOf(t, do(h, "GET", "/login?token="+in.Login, nil, nil))
	other := sessionOf(t, do(h, "GET", "/login?token="+in.Login, nil, nil))
	if w := withSession(h, "POST", "/signout", mine, url.Values{}); w.Code != http.StatusForbidden {
		t.Fatalf("sign out without CSRF: %d", w.Code)
	}
	if w := withSession(h, "POST", "/signout", other, url.Values{"csrf": {s.csrf}}); w.Code != http.StatusOK {
		t.Fatalf("sign out: %d %s", w.Code, w.Body)
	}
	for _, c := range []string{mine, other} {
		if w := withSession(h, "GET", "/attention", c); w.Code != http.StatusUnauthorized {
			t.Fatalf("session survived signing out: %d", w.Code)
		}
	}
	saved, err := LoadSignIn(path)
	if err != nil || saved.Login != in.Login || saved.Session == in.Session || saved.Session != s.sess {
		t.Fatalf("saved %+v, %v", saved, err)
	}
	if w := withSession(h, "GET", "/attention", sessionOf(t, do(h, "GET", "/login?token="+in.Login, nil, nil))); w.Code != http.StatusOK {
		t.Fatalf("sign in again: %d", w.Code)
	}
}

// The sign-in is made once, private, and read back unchanged, so a browser
// signed in to one process is still signed in to the next.
func TestSignInLastsAcrossServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "web.signin")
	first, err := LoadSignIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("sign-in file: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("sign-in directory: %v %v", info, err)
	}
	again, err := LoadSignIn(path)
	if err != nil || again != first {
		t.Fatalf("reloaded %+v, %v; want %+v", again, err, first)
	}
	var cookie string
	for i := range 2 {
		s, err := New(newFake(), Options{Addr: addr, SignIn: &again})
		if err != nil {
			t.Fatal(err)
		}
		h := s.Handler(addr)
		if i == 0 {
			w := do(h, "GET", "/login?token="+first.Login, nil, nil)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("login: %d", w.Code)
			}
			cookie = w.Result().Cookies()[0].Value
		}
		// The browser signed in to the first process is still signed in.
		if w := withSession(h, "GET", "/attention", cookie); w.Code != http.StatusOK {
			t.Fatalf("kept session: %d", w.Code)
		}
	}
	if urls := SignInURLs(first, "", ""); urls[0] != "http://open-conduit.localhost:7420/login?token="+first.Login || urls[1] != "http://127.0.0.1:7420/login?token="+first.Login {
		t.Fatalf("urls: %v", urls)
	}
}

func TestSignInRefusesAFileItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.signin")
	if err := os.WriteFile(path, []byte(`{"login":"short"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSignIn(path); err == nil {
		t.Fatal("a malformed sign-in file loaded")
	}
}
