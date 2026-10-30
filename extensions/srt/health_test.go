package srt_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/open-conduit/open-conduit/extensions/srt"
)

func TestHealth(t *testing.T) {
	l := newLab(t, "")
	if h := l.provider("darwin").Health(context.Background()); h.Status != "ok" || h.Code != "ok" {
		t.Errorf("a working srt: %+v", h)
	}
	if h := newLab(t, `[ "$1" = --version ] && { echo 0.0.99; exit 0; }; exit 0`).provider("darwin").Health(context.Background()); h.Status != "warn" || h.Code != "untested_version" {
		t.Errorf("a newer srt: %+v", h)
	}
	if h := srt.New(srt.Options{Binary: "/nonexistent/srt"}).Health(context.Background()); h.Status != "fail" || h.Code != "srt_unavailable" {
		t.Errorf("no srt: %+v", h)
	}
	if h := newLab(t, `[ "$1" = --version ] && { echo 0.0.81; exit 0; }; exit 1`).provider("darwin").Health(context.Background()); h.Status != "fail" || h.Code != "preflight_failed" {
		t.Errorf("srt that can't sandbox: %+v", h)
	}
	if h := srt.New(srt.Options{GOOS: "windows"}).Health(context.Background()); h.Status != "fail" {
		t.Errorf("an unsupported platform: %+v", h)
	}
	// The real srt, where the sandbox CI jobs install it.
	bin := os.Getenv("OPEN_CONDUIT_SRT")
	if bin == "" {
		bin, _ = exec.LookPath("srt")
	}
	if bin == "" {
		if os.Getenv("OPEN_CONDUIT_SANDBOX_TESTS") == "1" {
			t.Fatal("OPEN_CONDUIT_SANDBOX_TESTS=1, but srt is not installed")
		}
		return
	}
	if h := srt.New(srt.Options{Binary: bin}).Health(context.Background()); h.Status == "fail" {
		t.Errorf("real srt on %s: %+v", runtime.GOOS, h)
	}
}
