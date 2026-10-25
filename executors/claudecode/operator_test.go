package claudecode_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/executors/claudecode"
)

// A dry run runs the turn as a stub on the cheap model, told to do nothing.
func TestDryRunTurnIsAStub(t *testing.T) {
	h := newHarness(t)
	h.mode("review")
	bin := filepath.Join(h.root, "bin")
	_ = os.MkdirAll(bin, 0o700)
	e, err := claudecode.New(claudecode.Options{StateDir: h.state, Store: h.store, Artifacts: h.art, ToolbeltDir: bin, Now: h.clock.Now, DryRun: true,
		Environ: func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.root, "CLAUDE_CONFIG_DIR=" + h.config}
		},
		PollInterval: 20 * time.Millisecond, Draw: func() float64 { return 0.99 }})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := h.request("work/dry", "exec-dry", 1, nil)
	h.run(e, r)
	models, _ := os.ReadFile(filepath.Join(h.fake, "models"))
	prompts := h.prompts()
	if strings.TrimSpace(string(models)) != "haiku" || len(prompts) != 1 || !strings.HasPrefix(prompts[0], "This is an Open Conduit dry run: do nothing and call no tools.") || !strings.Contains(prompts[0], "/open-conduit:work-issue 12") {
		t.Fatalf("dry run: model %q prompt %q", models, prompts)
	}
}

func TestVersionAndUsageWindows(t *testing.T) {
	h := newHarness(t)
	if v, err := claudecode.Version(context.Background(), h.binary); err != nil || v != "9.9.9 (Claude Code)" {
		t.Fatalf("version: %q %v", v, err)
	}
	if _, err := claudecode.Version(context.Background(), filepath.Join(h.root, "missing")); err == nil || strings.Contains(err.Error(), h.root) {
		t.Fatalf("missing binary: %v", err)
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization") + "|" + r.Header.Get("anthropic-beta")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":42.5,"resets_at":"2026-10-25T15:00:00Z"},"seven_day":{"utilization":91,"resets_at":"2026-10-30T00:00:00Z"},"extra":{}}`))
	}))
	defer srv.Close()
	windows, err := claudecode.Usage(context.Background(), claudecode.UsageOptions{URL: srv.URL, Token: func(context.Context) (string, error) { return "oauth-tok", nil }})
	if err != nil || len(windows) != 2 || windows[0].Name != "5h" || windows[0].Used != 42.5 || windows[1].Name != "7d" || windows[1].ResetsAt.Day() != 30 {
		t.Fatalf("windows: %+v %v", windows, err)
	}
	if auth != "Bearer oauth-tok|oauth-2025-04-20" {
		t.Fatalf("request headers: %q", auth)
	}
	// (LoginToken is not exercised: on macOS it reads the real Keychain.)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	defer failing.Close()
	if _, err := claudecode.Usage(context.Background(), claudecode.UsageOptions{URL: failing.URL, Token: func(context.Context) (string, error) { return "x", nil }}); err == nil {
		t.Fatal("a failing endpoint returned windows")
	}
}
