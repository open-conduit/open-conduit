package integration_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDaemonProbes runs the daemon with --probe-addr: /livez and /readyz
// answer an orchestrator with no credential and a one-word body, the
// listener serves nothing else (never the management API), and GET
// /v1/health reports the host loop, configuration, packages and reconcile
// components beside the database.
func TestDaemonProbes(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	d := newDrill(t)
	d.extra = []string{"--probe-addr", addr}
	d.start("")
	get := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+addr+path, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	d.until("ready", 20*time.Second, func() bool { code, _ := get("GET", "/readyz"); return code == http.StatusOK })
	if code, body := get("GET", "/readyz"); code != http.StatusOK || body != "ready\n" {
		t.Fatalf("readyz: %d %q", code, body)
	}
	if code, body := get("GET", "/livez"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("livez: %d %q", code, body)
	}
	if code, body := get("HEAD", "/livez"); code != http.StatusOK || body != "" {
		t.Fatalf("livez HEAD: %d %q", code, body)
	}
	for _, path := range []string{"/v1/health", "/v1/capacity", "/", "/metrics"} {
		if code, _ := get("GET", path); code != http.StatusNotFound {
			t.Fatalf("%s on the probe listener: %d", path, code)
		}
	}
	if code, _ := get("POST", "/readyz"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST readyz: %d", code)
	}
	h, err := d.c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"database", "host_loop", "configuration", "packages", "reconcile"} {
		if h.Components[c] != "ready" {
			t.Fatalf("health component %s: %+v", c, h)
		}
	}
	if h.State != "ready" {
		t.Fatalf("health: %+v", h)
	}
	// A stopping daemon is not ready: once SIGTERM arrives, it leaves
	// service before its listener closes.
	if err = d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon ignored SIGTERM")
	}
	if code, body := get("GET", "/readyz"); code != 0 || !strings.Contains(body, "refused") {
		t.Fatalf("probe listener still answering after stop: %d %q", code, body)
	}
}
