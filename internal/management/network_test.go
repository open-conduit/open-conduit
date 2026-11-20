package management

import (
	"bytes"
	"fmt"
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
	var wait time.Duration
	for i := 0; i < AuthFailureLimit+3; i++ {
		var first bool
		wait, first = l.fail("192.0.2.1")
		if first {
			reported++
		}
		if i < AuthFailureLimit && wait != 0 {
			t.Fatalf("refused after %d failures", i+1)
		}
	}
	if reported != 1 || wait <= 0 {
		t.Fatalf("reported %d times, refused for %v", reported, wait)
	}
	if wait, _ = l.fail("192.0.2.2"); wait != 0 {
		t.Fatal("another client was refused")
	}
	now = now.Add(AuthFailureWindow)
	if wait, _ = l.fail("192.0.2.1"); wait != 0 {
		t.Fatal("still refused after the window")
	}
}

// IPv6 clients are keyed by their /64, so rotating addresses within it
// doesn't reset the count.
func TestFailureLimiterIPv6Prefix(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := &failureLimiter{now: func() time.Time { return now }, seen: map[string]*failures{}}
	var wait time.Duration
	for i := 0; i <= AuthFailureLimit; i++ {
		wait, _ = l.fail(fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if wait <= 0 {
		t.Fatal("rotating addresses in one /64 escaped the limit")
	}
	if wait, _ = l.fail("2001:db8:1:3::1"); wait != 0 {
		t.Fatal("another /64 was refused")
	}
}

// With the table full of clients at their limit, a blocked client stays
// blocked and new clients share one overflow window, so a flood of
// addresses neither evicts a blocked client nor gets unlimited failures.
// A client under its limit is evicted before one at it.
func TestFailureLimiterFullTable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := &failureLimiter{now: func() time.Time { return now }, seen: map[string]*failures{}}
	for i := 0; i < maxTrackedClients; i++ {
		for j := 0; j <= AuthFailureLimit; j++ {
			l.fail(fmt.Sprintf("10.%d.%d.1", i/256, i%256))
		}
	}
	var wait time.Duration
	for i := 0; i <= AuthFailureLimit; i++ {
		wait, _ = l.fail(fmt.Sprintf("172.16.%d.%d", i/256, i%256))
	}
	if wait <= 0 {
		t.Fatal("new clients in a full table escaped the limit")
	}
	if wait, _ = l.fail("10.0.0.1"); wait <= 0 {
		t.Fatal("a blocked client was evicted")
	}
	if len(l.seen) > maxTrackedClients+1 {
		t.Fatalf("tracked %d clients", len(l.seen))
	}

	// One client under its limit in a full table is the one evicted.
	l = &failureLimiter{now: func() time.Time { return now }, seen: map[string]*failures{}}
	l.fail("192.0.2.9")
	for i := 1; i < maxTrackedClients; i++ {
		for j := 0; j <= AuthFailureLimit; j++ {
			l.fail(fmt.Sprintf("10.%d.%d.1", i/256, i%256))
		}
	}
	l.fail("198.51.100.1")
	if _, ok := l.seen["192.0.2.9"]; ok {
		t.Fatal("the client under its limit was kept")
	}
	if _, ok := l.seen["198.51.100.1"]; !ok {
		t.Fatal("the new client was not tracked")
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
