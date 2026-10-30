package management

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

func openEndpoints(t *testing.T, state, short string) *Endpoints {
	t.Helper()
	lock, err := AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	e, err := NewEndpoints(lock, state, short, func(domain.ID) http.Handler { return http.NotFoundHandler() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	return e
}

func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "oc-ep-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// A run directory too deep for a Unix socket path keeps its token there and
// puts the socket in an owner-only directory keyed by execution under the
// short root; with no short path either, Serve fails and names the limit.
func TestEndpointsSocketPathFallback(t *testing.T) {
	base := shortTemp(t)
	deep := filepath.Join(base, strings.Repeat("d", 60), strings.Repeat("e", 30))
	short := filepath.Join(shortTemp(t), "s")
	e := openEndpoints(t, deep, short)
	endpoint, tokenFile, err := e.Serve(context.Background(), "execution/one", "token-one")
	if err != nil {
		t.Fatal(err)
	}
	socket := strings.TrimPrefix(endpoint, "unix://")
	if filepath.Dir(filepath.Dir(socket)) != short || len(socket) > maxSocketPath {
		t.Fatalf("socket not in the short root: %s", socket)
	}
	if filepath.Dir(filepath.Dir(tokenFile)) != e.Dir("execution/one") {
		t.Fatalf("token left the run directory: %s", tokenFile)
	}
	for _, d := range []string{short, filepath.Dir(socket)} {
		if info, err := os.Stat(d); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s is not owner-only: %v %v", d, info.Mode(), err)
		}
	}
	if err = e.Close("execution/one"); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{filepath.Dir(socket), e.Dir("execution/one")} {
		if _, err = os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s kept after close: %v", gone, err)
		}
	}

	long := openEndpoints(t, filepath.Join(base, "long"+strings.Repeat("l", 70)), filepath.Join(base, strings.Repeat("s", 90)))
	if _, _, err = long.Serve(context.Background(), "execution/two", "token-two"); err == nil || !strings.Contains(err.Error(), "use a shorter state directory") {
		t.Fatalf("an over-long socket path did not fail clearly: %v", err)
	}
}

// A short root someone else made readable is refused, never used.
func TestEndpointsRefuseUnsafeShortRoot(t *testing.T) {
	base := shortTemp(t)
	short := filepath.Join(shortTemp(t), "s")
	if err := os.Mkdir(short, 0o755); err != nil {
		t.Fatal(err)
	}
	e := openEndpoints(t, filepath.Join(base, strings.Repeat("d", 90)), short)
	if _, _, err := e.Serve(context.Background(), "execution/one", "token"); err == nil {
		t.Fatal("served a socket in a group-readable directory")
	}
}

// Rotation replaces the token file inside its directory, and a run directory
// that names another execution is refused.
func TestEndpointsRotateAndClaim(t *testing.T) {
	e := openEndpoints(t, shortTemp(t), "")
	_, file, err := e.Serve(context.Background(), "execution/one", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, again, err := e.Serve(context.Background(), "execution/one", "second"); err != nil || again != file {
		t.Fatalf("rotation: %s %v", again, err)
	}
	if b, _ := os.ReadFile(file); string(b) != "second\n" {
		t.Fatalf("token file after rotation: %q", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(file))
	if len(entries) != 1 {
		t.Fatalf("token directory holds more than the token: %v", entries)
	}
	if err = os.MkdirAll(e.Dir("execution/two"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Dir("execution/two"), endpointID), []byte("execution/other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = e.Serve(context.Background(), "execution/two", "token"); err == nil {
		t.Fatal("served in a run directory that names another execution")
	}
}

// ServeTCP serves the execution's handler on a loopback port named in
// toolbelt.port beside its socket, keeps it on a second call, and stops
// it with the socket on Close.
func TestEndpointsServeTCP(t *testing.T) {
	e := openEndpoints(t, shortTemp(t), "")
	if _, err := e.ServeTCP("execution/tcp", "127.0.0.1"); err == nil {
		t.Fatal("served a port for an execution whose socket isn't served")
	}
	endpoint, _, err := e.Serve(context.Background(), "execution/tcp", "token")
	if err != nil {
		t.Fatal(err)
	}
	addr, err := e.ServeTCP("execution/tcp", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := e.ServeTCP("execution/tcp", "127.0.0.1"); err != nil || again != addr {
		t.Fatalf("a second call served %s %v, want %s kept", again, err, addr)
	}
	file := filepath.Join(filepath.Dir(strings.TrimPrefix(endpoint, "unix://")), "toolbelt.port")
	raw, err := os.ReadFile(file)
	if err != nil || !strings.HasSuffix(addr, ":"+strings.TrimSpace(string(raw))) {
		t.Fatalf("port file %q %v for %s", raw, err, addr)
	}
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the port did not serve the execution's handler: %d", resp.StatusCode)
	}
	if err := e.Close("execution/tcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + addr + "/"); err == nil {
		t.Fatal("the port still serves after Close")
	}
}
