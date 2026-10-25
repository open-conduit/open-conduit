package tunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func next(t *testing.T, urls <-chan string) string {
	t.Helper()
	select {
	case u := <-urls:
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("no URL reported")
	}
	return ""
}

// ngrok's JSON log announces the URL; when the process exits it reports
// down ("") and is restarted, announcing again.
func TestNgrokReportsURLsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	bin := script(t, `case "$1" in version) echo "ngrok version 3.0.0"; exit 0 ;; esac
echo "$@" >> `+dir+`/args
echo '{"lvl":"info","msg":"started tunnel","url":"https://abc.ngrok.app"}'
sleep 0.2
`)
	tun, err := New(Options{Kind: "ngrok", Binary: bin, Domain: "abc.ngrok.app", Backoff: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := tun.Check(context.Background()); err != nil || !strings.Contains(v, "3.0.0") {
		t.Fatalf("check: %q %v", v, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	urls := make(chan string)
	done := make(chan error, 1)
	go func() { done <- tun.Run(ctx, "127.0.0.1:8787", urls) }()
	for _, want := range []string{"https://abc.ngrok.app", "", "https://abc.ngrok.app"} {
		if got := next(t, urls); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "http 127.0.0.1:8787") || !strings.Contains(string(args), "--url abc.ngrok.app") {
		t.Fatalf("ngrok args: %s", args)
	}
}

func TestCloudflaredQuickAndNamed(t *testing.T) {
	quick := script(t, `echo "INF +----+"; echo "INF |  https://brave-fox-12.trycloudflare.com  |"; sleep 5`)
	tun, err := New(Options{Kind: "cloudflared", Binary: quick})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	urls := make(chan string)
	go func() { _ = tun.Run(ctx, "127.0.0.1:1", urls) }()
	if got := next(t, urls); got != "https://brave-fox-12.trycloudflare.com" {
		t.Fatalf("quick: %q", got)
	}
	cancel()
	named := script(t, `echo "INF Registered tunnel connection connIndex=0"; sleep 5`)
	tun, err = New(Options{Kind: "cloudflared", Binary: named, Name: "hooks", Hostname: "https://hooks.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	urls = make(chan string)
	go func() { _ = tun.Run(ctx, "127.0.0.1:1", urls) }()
	if got := next(t, urls); got != "https://hooks.example.com" {
		t.Fatalf("named: %q", got)
	}
	if _, err := New(Options{Kind: "cloudflared", Name: "x"}); err == nil {
		t.Fatal("named tunnel without hostname accepted")
	}
}

func TestStaticAndInvalid(t *testing.T) {
	tun, err := New(Options{Kind: "static", URL: "https://hooks.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	urls := make(chan string)
	go func() { _ = tun.Run(ctx, "", urls) }()
	if got := next(t, urls); got != "https://hooks.example.com" {
		t.Fatalf("static: %q", got)
	}
	for _, o := range []Options{{Kind: "static", URL: "ftp://x"}, {Kind: "static", URL: "https://user:pw@x"}, {Kind: "carrier-pigeon"}} {
		if _, err := New(o); err == nil {
			t.Fatalf("%+v accepted", o)
		}
	}
}
