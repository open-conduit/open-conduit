package runtime_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// A deferred page keeps its generation and checkpoint, comes back at the time
// the provider named, and is neither a failure nor a used-up page.
func TestIngressDeferredPageKeepsGeneration(t *testing.T) {
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	src := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", src, f)
	f.pages[""] = extension.Snapshot{ScanID: "scan", Complete: true, Checkpoint: "one"}
	reconcilePage(t, r, "source_a")

	clock.Set(clock.Now().Add(time.Minute))
	at := clock.Now().Add(90 * time.Second)
	f.pages[""] = extension.Snapshot{ScanID: "scan-2", NotBefore: at}
	result := reconcilePage(t, r, "source_a")
	if !result.Deferred.Equal(at) || result.Complete || result.ErrorCode != "" {
		t.Fatalf("deferred result: %+v", result)
	}
	s := ingressStatus(t, r, "source_a")
	if s.LastError != "" || s.Failures != 0 || !s.NextDue.Equal(at) || s.ScanID != "scan-2" {
		t.Fatalf("deferred status: %+v", s)
	}
	if due, err := r.DueSources(context.Background()); err != nil || len(due) != 0 {
		t.Fatalf("due while deferred: %v %v", due, err)
	}
	var lag runtime.Check
	for _, c := range runtime.Diagnose(context.Background(), db, clock.Now(), runtime.DoctorOptions{Ingress: r}) {
		if c.Name == "reconcile_lag" {
			lag = c
		}
	}
	if lag.Code != "current" || !strings.Contains(strings.Join(lag.Items, " "), "every 1m0s") || !strings.Contains(strings.Join(lag.Items, " "), "paced by the provider") {
		t.Fatalf("doctor while paced: %+v", lag)
	}

	clock.Set(at)
	f.pages[""] = extension.Snapshot{ScanID: "scan-2", Complete: true, Checkpoint: "two"}
	if result = reconcilePage(t, r, "source_a"); !result.Complete {
		t.Fatalf("resumed page: %+v", result)
	}
	if got := f.lastScan(t); got.ScanID != "scan-2" || got.Checkpoint != "one" {
		t.Fatalf("resumed scan: %+v", got)
	}
	if s = ingressStatus(t, r, "source_a"); !s.PacedSince.IsZero() || s.Checkpoint != "two" {
		t.Fatalf("completed status: %+v", s)
	}
}

// A deferred page may not carry anything: it would otherwise be a way to
// stage observations or move the cursor without the page checks.
func TestIngressDeferredPageMustBeEmpty(t *testing.T) {
	clock := &ingressClock{at: ingressTime}
	_, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	src := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", src, f)
	at := clock.Now().Add(time.Minute)
	for name, page := range map[string]extension.Snapshot{
		"observations": {ScanID: "s", NotBefore: at, Observations: []extension.Observation{fixtureObservation(src, "a", "1")}},
		"complete":     {ScanID: "s", NotBefore: at, Complete: true},
		"moved cursor": {ScanID: "s", NotBefore: at, NextCursor: "elsewhere"},
	} {
		f.pages[""] = page
		if result, _ := r.Reconcile(context.Background(), "source_a"); result.ErrorCode != "invalid_snapshot" {
			t.Fatalf("%s: %+v", name, result)
		}
	}
}

// A provider can't push a source's next read out without bound, or spin it.
func TestIngressDeferredPageIsBounded(t *testing.T) {
	clock := &ingressClock{at: ingressTime}
	_, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	f := newFixtureIngress()
	registerIngress(t, r, "source_a", extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}, f)
	f.pages[""] = extension.Snapshot{ScanID: "s", NotBefore: clock.Now().Add(1000 * time.Hour)}
	if got := reconcilePage(t, r, "source_a").Deferred; !got.Equal(clock.Now().Add(24 * time.Hour)) {
		t.Fatalf("far deferral: %s", got)
	}
	clock.Set(clock.Now().Add(24 * time.Hour))
	f.pages[""] = extension.Snapshot{ScanID: "s", NotBefore: clock.Now().Add(-time.Hour)}
	if got := reconcilePage(t, r, "source_a").Deferred; !got.Equal(clock.Now().Add(time.Second)) {
		t.Fatalf("past deferral: %s", got)
	}
}

// Shortening an entry's interval applies at the next registration, rather
// than after the wait the longer interval had scheduled.
func TestIngressShortenedIntervalPullsNextDueIn(t *testing.T) {
	clock := &ingressClock{at: ingressTime}
	db, r := openIngress(t, filepath.Join(t.TempDir(), "conduit.db"), clock)
	src := extension.Source{Namespace: "fixture/account-a", ScopeID: "scope_a"}
	f := newFixtureIngress()
	f.pages[""] = extension.Snapshot{ScanID: "s", Complete: true, Checkpoint: "c"}
	if err := r.Register(context.Background(), "source_a", src, f, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	reconcilePage(t, r, "source_a")

	again, err := runtime.NewIngress(db, clock.Now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Register(context.Background(), "source_a", src, f, time.Hour); err != nil {
		t.Fatal(err)
	}
	if s := ingressStatus(t, again, "source_a"); !s.NextDue.Equal(clock.Now().Add(time.Hour)) || s.Interval != time.Hour {
		t.Fatalf("after shortening: %+v", s)
	}
}
