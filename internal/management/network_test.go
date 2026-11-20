package management

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The listener is off unless asked for, and a half-configured or plaintext
// one is refused before anything is bound.
func TestNetworkConfigCheck(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  NetworkConfig
		want string // "" passes
	}{
		{"off", NetworkConfig{}, ""},
		{"tls", NetworkConfig{Addr: "0.0.0.0:8443", CertFile: "c", KeyFile: "k"}, ""},
		{"mtls", NetworkConfig{Addr: "0.0.0.0:8443", CertFile: "c", KeyFile: "k", ClientCAFile: "ca"}, ""},
		{"proxy on loopback", NetworkConfig{Addr: "127.0.0.1:8443", TrustedProxy: true}, ""},
		{"proxy on ipv6 loopback", NetworkConfig{Addr: "[::1]:8443", TrustedProxy: true}, ""},
		{"plaintext", NetworkConfig{Addr: "0.0.0.0:8443"}, "never serves plaintext"},
		{"plaintext on loopback", NetworkConfig{Addr: "127.0.0.1:8443"}, "never serves plaintext"},
		{"cert without key", NetworkConfig{Addr: "0.0.0.0:8443", CertFile: "c"}, "give both"},
		{"key without cert", NetworkConfig{Addr: "0.0.0.0:8443", KeyFile: "k"}, "give both"},
		{"tls without addr", NetworkConfig{CertFile: "c", KeyFile: "k"}, "need --management-addr"},
		{"client CA without tls", NetworkConfig{Addr: "0.0.0.0:8443", ClientCAFile: "ca"}, "needs --management-tls-cert"},
		{"proxy off loopback", NetworkConfig{Addr: "0.0.0.0:8443", TrustedProxy: true}, "needs a loopback"},
		{"proxy by name", NetworkConfig{Addr: "localhost:8443", TrustedProxy: true}, "needs a loopback"},
		{"proxy and tls", NetworkConfig{Addr: "127.0.0.1:8443", TrustedProxy: true, CertFile: "c", KeyFile: "k"}, "not both"},
		{"proxy without addr", NetworkConfig{TrustedProxy: true}, "need --management-addr"},
		{"bad addr", NetworkConfig{Addr: "8443", CertFile: "c", KeyFile: "k"}, "not host:port"},
	} {
		err := c.cfg.Check()
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: Check() = %v, want %q", c.name, err, c.want)
		}
	}
	// A certificate that does not load is a startup error.
	dir := t.TempDir()
	if _, err := ListenNetwork(NetworkConfig{Addr: "127.0.0.1:0", CertFile: filepath.Join(dir, "c"), KeyFile: filepath.Join(dir, "k")}); err == nil || !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("a missing certificate did not stop the listener: %v", err)
	}
}

// A client is refused once it reaches the limit of failed authentications,
// until its window ends; others are unaffected, and the limit is reported
// once per window.
func TestFailureLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := &failureLimiter{now: func() time.Time { return now }, seen: map[string]*failures{}}
	reported := 0
	for i := 0; i < AuthFailureLimit+3; i++ {
		if l.blocked("a") > 0 {
			break
		}
		if l.fail("a") {
			reported++
		}
	}
	if reported != 1 || l.blocked("a") <= 0 {
		t.Fatalf("reported %d times, blocked %v", reported, l.blocked("a"))
	}
	if l.blocked("b") != 0 {
		t.Fatal("another client was refused")
	}
	now = now.Add(AuthFailureWindow)
	if l.blocked("a") != 0 {
		t.Fatal("still refused after the window")
	}
	// Tracking is bounded.
	for i := 0; i < maxTrackedClients+10; i++ {
		l.fail(strings.Repeat("x", 1) + time.Duration(i).String())
	}
	if len(l.seen) > maxTrackedClients {
		t.Fatalf("tracked %d clients", len(l.seen))
	}
}

// The cursor key is made once, owner-only, and read back on the next start;
// a readable or malformed key file is refused.
func TestCursorKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "cursor.key")
	first, err := CursorKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file not owner-only: %v %v", info.Mode(), err)
	}
	second, err := CursorKey(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("key not kept across starts: %v", err)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = CursorKey(path); err == nil {
		t.Fatal("a readable key file was used")
	}
	if err = os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = CursorKey(path); err == nil {
		t.Fatal("a malformed key file was used")
	}
}
