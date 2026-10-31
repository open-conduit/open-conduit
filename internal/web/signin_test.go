package web

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

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
	for range 2 {
		s, err := New(newFake(), Options{Addr: addr, SignIn: &again})
		if err != nil {
			t.Fatal(err)
		}
		h := s.Handler(addr)
		if w := do(h, "GET", "/login?token="+first.Login, nil, nil); w.Code != http.StatusSeeOther {
			t.Fatalf("login: %d", w.Code)
		}
		req := do(h, "GET", "/attention", nil, s)
		if req.Code != http.StatusOK || s.sess != first.Session {
			t.Fatalf("kept session: %d", req.Code)
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
